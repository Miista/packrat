// Package api implements the HTTP handlers for packrat: first-launch
// admin setup, login/logout, settings (with env-override reporting), and
// scheduler control.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog"

	"github.com/miista/packrat/internal/auth"
	"github.com/miista/packrat/internal/scheduler"
	"github.com/miista/packrat/internal/settings"
	"github.com/miista/packrat/internal/store"
)

// Server wires the store, auth manager, and scheduler into HTTP handlers.
type Server struct {
	store     *store.Store
	auth      *auth.Manager
	scheduler *scheduler.Scheduler
	log       zerolog.Logger
	authOff   bool
}

// New builds a Server.
func New(st *store.Store, am *auth.Manager, sc *scheduler.Scheduler, log zerolog.Logger) *Server {
	return &Server{
		store:     st,
		auth:      am,
		scheduler: sc,
		log:       log.With().Str("component", "api").Logger(),
		authOff:   settings.AuthDisabled(),
	}
}

// Routes registers all handlers on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.guarded(s.handleLogout))
	mux.HandleFunc("/api/state", s.guarded(s.handleState))
	mux.HandleFunc("/api/settings", s.guarded(s.handleSettings))
	mux.HandleFunc("/api/start", s.guarded(s.csrfGuard(s.handleStart)))
	mux.HandleFunc("/api/pause", s.guarded(s.csrfGuard(s.handlePause)))
	mux.HandleFunc("/api/run", s.guarded(s.csrfGuard(s.handleRun)))
	mux.HandleFunc("/api/dry-run", s.guarded(s.csrfGuard(s.handleDryRun)))
}

// guarded enforces that, once an admin account exists (and auth isn't
// explicitly disabled), a request carries a valid session — on every
// route, including read-only state.
func (s *Server) guarded(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authOff {
			next(w, r)
			return
		}
		if !s.auth.HasAdmin() {
			writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "Admin account setup required."})
			return
		}
		if !s.auth.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Login required."})
			return
		}
		next(w, r)
	}
}

// csrfGuard rejects state-changing requests whose Origin header doesn't
// match the request Host, as defense-in-depth alongside session auth.
func (s *Server) csrfGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if u, err := url.Parse(origin); err == nil && u.Host != r.Host {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "Cross-origin request rejected."})
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]bool{"admin_exists": s.auth.HasAdmin()})
	case http.MethodPost:
		if s.auth.HasAdmin() {
			writeJSON(w, http.StatusConflict, map[string]string{"error": auth.ErrAdminExists.Error()})
			return
		}
		var req struct{ Username, Password string }
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.auth.CreateAdmin(req.Username, req.Password); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		token, err := s.auth.Authenticate(req.Username, req.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.auth.SetSessionCookie(w, r, token)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct{ Username, Password string }
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	token, err := s.auth.Authenticate(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password."})
		return
	}
	s.auth.SetSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.ClearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var st store.State
	s.store.View(func(state store.State) { st = state })
	resolved := settings.Resolve(st.Settings)

	unsat, haveUnsat := s.scheduler.UnsatStatus()

	writeJSON(w, http.StatusOK, map[string]any{
		"settings":          publicSettings(resolved),
		"totals":            st.Totals,
		"history":           reversed(st.History),
		"scheduler_enabled": st.SchedulerOn,
		"paused":            st.Paused,
		"running":           s.scheduler.IsRunning(),
		"next_run_time":     st.NextRunTime,
		"unsat_count":       unsat.Count,
		"unsat_limit":       unsat.Limit,
		"have_unsat":        haveUnsat,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var st store.State
		s.store.View(func(state store.State) { st = state })
		writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var incoming map[string]any
	if err := readJSON(r, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	unsat, haveUnsat := s.scheduler.UnsatStatus()

	err := s.store.Update(func(st *store.State) {
		applySettingsPatch(&st.Settings, incoming, unsat.Limit, haveUnsat)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var st store.State
	s.store.View(func(state store.State) { st = state })
	writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.StartSchedule(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, scheduler.ErrNoDownloadClient) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.Pause(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	started, err := s.scheduler.RunNow()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, scheduler.ErrNoDownloadClient) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": started})
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	started := s.scheduler.RunDryNow()
	writeJSON(w, http.StatusOK, map[string]bool{"started": started})
}

// applySettingsPatch applies whitelisted, validated fields from incoming
// onto st. Fields that are currently env-managed are silently ignored so a
// client can't override an operator-pinned value via the API.
func applySettingsPatch(st *store.Settings, incoming map[string]any, unsatLimit int, haveUnsat bool) {
	resolved := settings.Resolve(*st)

	setInt := func(key string, dst *int, min, max int) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(float64); ok {
			n := int(v)
			if n < min {
				n = min
			}
			if max > 0 && n > max {
				n = max
			}
			*dst = n
		}
	}
	setString := func(key string, dst *string) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(string); ok {
			*dst = strings.TrimSpace(v)
		}
	}

	setString("mam_id", &st.MamID)
	// Reserve can never legitimately exceed MAM's current unsatisfied-torrent
	// limit — that would make the target negative, which the scheduler
	// already clamps to zero, but it's a nonsensical setting so it's
	// rejected here too when the limit is known. Not yet knowing the limit
	// (no run has completed yet) leaves reserve unbounded above, same as
	// before.
	reserveMax := 0
	if haveUnsat {
		reserveMax = unsatLimit
	}
	setInt("reserve", &st.Reserve, 0, reserveMax)
	setInt("next_run_delay_minutes", &st.NextRunDelayMinutes, 2, 0)

	if sf, ok := incoming["search_filters"].(map[string]any); ok {
		applySearchFiltersPatch(&st.SearchFilters, sf)
	}
	if dc, ok := incoming["download_client"].(map[string]any); ok {
		applyDownloadClientPatch(&st.DownloadClient, dc)
	}
}

func applySearchFiltersPatch(f *store.SearchFilters, incoming map[string]any) {
	if v, ok := incoming["text"].(string); ok {
		f.Text = strings.TrimSpace(v)
	}
	if v, ok := incoming["min_seeders"].(float64); ok && v >= 0 {
		f.MinSeeders = int(v)
	}
	if v, ok := incoming["max_seeders"].(float64); ok && v >= 0 {
		f.MaxSeeders = int(v)
	}
	if v, ok := incoming["min_leechers"].(float64); ok && v >= 0 {
		f.MinLeechers = int(v)
	}
	if v, ok := incoming["max_leechers"].(float64); ok && v >= 0 {
		f.MaxLeechers = int(v)
	}
	if v, ok := incoming["min_size_mb"].(float64); ok && v >= 0 {
		f.MinSizeMB = int(v)
	}
	if v, ok := incoming["max_size_mb"].(float64); ok && v >= 0 {
		f.MaxSizeMB = int(v)
	}
	if v, ok := incoming["freeleech_only"].(bool); ok {
		f.FreeleechOnly = v
	}
	if v, ok := incoming["sort_type"].(string); ok {
		f.SortType = strings.TrimSpace(v)
	}
}

func applyDownloadClientPatch(dc *store.DownloadClient, incoming map[string]any) {
	if v, ok := incoming["type"].(string); ok {
		// Only qBittorrent is supported for now.
		if v == store.ClientQBittorrent {
			dc.Type = v
		}
	}
	if v, ok := incoming["url"].(string); ok {
		dc.URL = strings.TrimSpace(v)
	}
	if v, ok := incoming["username"].(string); ok {
		dc.Username = strings.TrimSpace(v)
	}
	// Password: only overwritten when a non-empty value is sent, matching
	// the mam_id UI pattern — an empty field means "leave unchanged", not
	// "clear it".
	if v, ok := incoming["password"].(string); ok && v != "" {
		dc.Password = v
	}
	if ao, ok := incoming["add_options"].(map[string]any); ok {
		applyAddOptionsPatch(&dc.AddOptions, ao)
	}
}

func applyAddOptionsPatch(ao *store.AddOptions, incoming map[string]any) {
	if v, ok := incoming["category"].(string); ok {
		ao.Category = strings.TrimSpace(v)
	}
	if v, ok := incoming["tags"].(string); ok {
		ao.Tags = strings.TrimSpace(v)
	}
	setNonNegInt := func(key string, dst *int) {
		if v, ok := incoming[key].(float64); ok {
			n := int(v)
			if n < 0 {
				n = 0
			}
			*dst = n
		}
	}
	setNonNegInt("upload_limit_kbs", &ao.UploadLimitKBs)
	setNonNegInt("download_limit_kbs", &ao.DownloadLimitKBs)
	setNonNegInt("seeding_time_limit_minutes", &ao.SeedingTimeLimitMinutes)
	if v, ok := incoming["ratio_limit"].(float64); ok {
		if v < 0 {
			v = 0
		}
		ao.RatioLimit = v
	}
}

// publicSettings renders resolved settings for API responses: env-managed
// fields are flagged, and secrets (MAM cookie, download client password)
// are masked once saved.
func publicSettings(resolved settings.Resolved) map[string]any {
	s := resolved.Settings
	fields := map[string]any{
		"mam_id":                 settings.MaskSecret(s.MamID),
		"reserve":                s.Reserve,
		"next_run_delay_minutes": s.NextRunDelayMinutes,
		"search_filters": map[string]any{
			"text":           s.SearchFilters.Text,
			"min_seeders":    s.SearchFilters.MinSeeders,
			"max_seeders":    s.SearchFilters.MaxSeeders,
			"min_leechers":   s.SearchFilters.MinLeechers,
			"max_leechers":   s.SearchFilters.MaxLeechers,
			"min_size_mb":    s.SearchFilters.MinSizeMB,
			"max_size_mb":    s.SearchFilters.MaxSizeMB,
			"freeleech_only": s.SearchFilters.FreeleechOnly,
			"sort_type":      s.SearchFilters.SortType,
		},
		"download_client": map[string]any{
			"type":         s.DownloadClient.Type,
			"url":          s.DownloadClient.URL,
			"username":     s.DownloadClient.Username,
			"password":     settings.MaskSecret(s.DownloadClient.Password),
			"password_set": s.DownloadClient.Password != "",
			"add_options": map[string]any{
				"category":                   s.DownloadClient.AddOptions.Category,
				"tags":                       s.DownloadClient.AddOptions.Tags,
				"upload_limit_kbs":           s.DownloadClient.AddOptions.UploadLimitKBs,
				"download_limit_kbs":         s.DownloadClient.AddOptions.DownloadLimitKBs,
				"ratio_limit":                s.DownloadClient.AddOptions.RatioLimit,
				"seeding_time_limit_minutes": s.DownloadClient.AddOptions.SeedingTimeLimitMinutes,
			},
		},
	}
	envManaged := map[string]string{}
	for k, v := range resolved.Managed {
		envManaged[k] = v
	}
	return map[string]any{
		"values":      fields,
		"env_managed": envManaged,
		"mam_id_set":  s.MamID != "",
	}
}

func reversed(entries []store.HistoryEntry) []store.HistoryEntry {
	out := make([]store.HistoryEntry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}
	return out
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
