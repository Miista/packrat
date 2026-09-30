function App() {
  return {
    view: 'loading', // 'loading' | 'setup' | 'login' | 'app'
    tab: 'dashboard',
    loggedIn: false,
    runMenuOpen: false,

    setupForm: { username: '', password: '' },
    setupError: '',
    loginForm: { username: '', password: '' },
    loginError: '',

    state: {
      totals: { cumulative_torrents_added: 0, cumulative_runs: 0 },
      scheduler_enabled: false,
      paused: false,
      running: false,
      next_run_time: null,
      have_unsat: false,
      unsat_count: 0,
      unsat_limit: 0,
    },
    runHistory: {
      entries: [],
      page: 1,
      page_size: 20,
      total: 0,
      total_pages: 1,
    },
    settings: {
      values: {
        search_filters: {},
        download_client: {},
      },
      env_managed: {},
      mam_id_set: false,
    },
    mamIdInput: '',
    downloadClientPasswordInput: '',
    settingsError: '',
    settingsSaved: false,
    schedulerError: '',
    pollTimer: null,

    async mounted() {
      // Closes the dry-run dropdown on any click outside the split-button.
      // Wired once here (a known-good lifecycle hook — the root #app's own
      // @vue:mounted) rather than via a nested @vue:mounted on the
      // split-button div itself, which failed to bind reliably in Safari
      // (ReferenceError on runMenuOpen/registerRunMenu — the split-button
      // subtree's scope bindings never got set up).
      document.addEventListener('click', (e) => {
        if (!e.target.closest('.split-button')) this.runMenuOpen = false;
      });

      const setupRes = await fetch('/api/setup');
      const setupData = await setupRes.json();
      if (!setupData.admin_exists) {
        this.view = 'setup';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async tryLoadState() {
      const res = await fetch('/api/state');
      if (res.status === 401 || res.status === 412) return false;
      if (!res.ok) return false;
      this.state = await res.json();
      this.loggedIn = true;
      await this.loadSettings();
      await this.loadHistory(1);
      return true;
    },

    async loadHistory(page) {
      const res = await fetch(`/api/history?page=${page}&page_size=${this.runHistory.page_size}`);
      if (!res.ok) return;
      this.runHistory = await res.json();
    },

    async loadSettings() {
      const res = await fetch('/api/settings');
      if (!res.ok) return;
      this.settings = await res.json();
      this.mamIdInput = '';
      this.downloadClientPasswordInput = '';
    },

    startPolling() {
      if (this.pollTimer) return;
      this.pollTimer = setInterval(async () => {
        const res = await fetch('/api/state');
        if (res.ok) this.state = await res.json();
        // Only auto-refresh history while sitting on the first page — a
        // completed run shows up there, but jumping the user back to page 1
        // out from under them while they're browsing older pages would be
        // disruptive.
        if (this.runHistory.page === 1) await this.loadHistory(1);
      }, 5000);
    },

    async submitSetup() {
      this.setupError = '';
      const res = await fetch('/api/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(this.setupForm),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.setupError = data.error || 'Setup failed.';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async submitLogin() {
      this.loginError = '';
      const res = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(this.loginForm),
      });
      if (!res.ok) {
        this.loginError = 'Invalid username or password.';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async logout() {
      await fetch('/api/logout', { method: 'POST' });
      if (this.pollTimer) {
        clearInterval(this.pollTimer);
        this.pollTimer = null;
      }
      this.loggedIn = false;
      this.view = 'login';
    },

    async startSchedule() {
      this.schedulerError = '';
      const res = await fetch('/api/start', { method: 'POST' });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.schedulerError = data.error || 'Failed to activate the scheduler.';
        return;
      }
      await this.tryLoadState();
    },

    async pauseSchedule() {
      await fetch('/api/pause', { method: 'POST' });
      await this.tryLoadState();
    },

    async runNow(dryRun) {
      this.schedulerError = '';
      const endpoint = dryRun ? '/api/dry-run' : '/api/run';
      const res = await fetch(endpoint, { method: 'POST' });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.schedulerError = data.error || 'Failed to start the run.';
        return;
      }
      await this.tryLoadState();
    },

    async saveSettings() {
      this.settingsError = '';
      this.settingsSaved = false;
      const payload = { ...this.settings.values };
      if (this.mamIdInput) {
        payload.mam_id = this.mamIdInput;
      } else {
        delete payload.mam_id;
      }
      payload.download_client = { ...this.settings.values.download_client };
      if (this.downloadClientPasswordInput) {
        payload.download_client.password = this.downloadClientPasswordInput;
      } else {
        delete payload.download_client.password;
      }

      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.settingsError = data.error || 'Failed to save settings.';
        return;
      }
      this.settings = await res.json();
      this.mamIdInput = '';
      this.downloadClientPasswordInput = '';
      this.settingsSaved = true;
      setTimeout(() => { this.settingsSaved = false; }, 2500);
    },

    get statusLabel() {
      if (this.state.running) return 'Running now';
      if (this.state.paused || !this.state.scheduler_enabled) return 'Paused';
      return 'Scheduled';
    },

    formatTime(value) {
      if (!value) return '—';
      const d = new Date(value);
      if (isNaN(d.getTime())) return String(value);
      return d.toLocaleString();
    },

    // limitMode/setLimitMode drive the "Use global / Unlimited / Custom"
    // picker for ratio_limit and seeding_time_limit_minutes, which store
    // qBittorrent's own sentinel values directly (-2 = use global, -1 =
    // unlimited, >=0 = a real custom value — see store.AddOptions on the
    // backend). The picker derives its mode from the stored number rather
    // than tracking separate UI state, so switching tabs or reloading
    // settings never gets out of sync with what's actually persisted.
    limitMode(key) {
      const v = this.settings.values.download_client.add_options[key];
      if (v === -2) return 'global';
      if (v === -1) return 'unlimited';
      return 'custom';
    },

    setLimitMode(key, mode) {
      const ao = this.settings.values.download_client.add_options;
      if (mode === 'global') ao[key] = -2;
      else if (mode === 'unlimited') ao[key] = -1;
      else if (mode === 'custom' && (ao[key] === -2 || ao[key] === -1)) ao[key] = 0;
    },

    minutesAsDuration(minutes) {
      if (!Number.isFinite(minutes) || minutes < 0) return '';
      const days = Math.floor(minutes / 1440);
      const hours = Math.floor((minutes % 1440) / 60);
      const mins = minutes % 60;
      const parts = [];
      if (days) parts.push(`${days}d`);
      if (hours) parts.push(`${hours}h`);
      if (mins || parts.length === 0) parts.push(`${mins}m`);
      return `Minutes (${minutes} = ${parts.join(' ')})`;
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');
