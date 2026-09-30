// Package store persists packrat's full application state to a single
// JSON file under a data directory. All writes go through Save, which
// rewrites the whole file with 0600 permissions since it may contain a
// secret (the MAM session cookie, if entered via the UI rather than env
// var, and download client credentials).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Download client backends. Only qBittorrent is supported for now.
const (
	ClientQBittorrent = "qbittorrent"
)

// DownloadClient holds connection details for the configured torrent
// client. Password is a persisted secret, masked whenever rendered to the
// API.
type DownloadClient struct {
	Type     string `json:"type"` // qbittorrent
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`

	// AddOptions are applied to every torrent this app adds. All fields are
	// optional (zero value = leave qBittorrent's own default behavior
	// alone) — e.g. RatioLimit/SeedingTimeLimitMinutes of 0 means "use the
	// client's global limit", not "no limit".
	AddOptions AddOptions `json:"add_options"`
}

// AddOptions are the qBittorrent /api/v2/torrents/add fields this app lets
// the user pin on every torrent it adds, rather than leaving them at
// qBittorrent's own defaults.
//
// RatioLimit and SeedingTimeLimitMinutes store qBittorrent's own sentinel
// convention directly, matching the "Use global / Unlimited / Custom"
// picker qBittorrent's own UI (and qui's) uses for these two fields:
//   - -2 = use the client's global default limit
//   - -1 = no limit, seed forever
//   - >=0 = an explicit custom limit
//
// Storing the raw qBittorrent sentinel (rather than this app inventing its
// own "0 means default" mapping) removes any ambiguity between "unset" and
// "a real custom value of 0" — both are meaningful, distinct qBittorrent
// states in their own right.
type AddOptions struct {
	Category                string  `json:"category"`
	Tags                    string  `json:"tags"` // comma-separated, per qBittorrent's API
	UploadLimitKBs          int     `json:"upload_limit_kbs"`
	DownloadLimitKBs        int     `json:"download_limit_kbs"`
	RatioLimit              float64 `json:"ratio_limit"`
	SeedingTimeLimitMinutes int     `json:"seeding_time_limit_minutes"`
}

const (
	QbitLimitUseGlobal = -2
	QbitLimitUnlimited = -1
)

// Configured reports whether enough connection detail has been entered to
// attempt using this download client. URL is the only real signal — Type
// defaults to qbittorrent out of the box (it's the only option, purely to
// pre-select it in the UI), so its presence alone doesn't mean the user
// has actually set anything up.
func (c DownloadClient) Configured() bool {
	return c.URL != ""
}

// SearchFilters is the curated set of MAM search criteria used when
// looking for top-up candidates.
type SearchFilters struct {
	Text          string `json:"text"`
	MinSeeders    int    `json:"min_seeders"`
	MaxSeeders    int    `json:"max_seeders"` // 0 = no max
	MinLeechers   int    `json:"min_leechers"`
	MaxLeechers   int    `json:"max_leechers"` // 0 = no max
	MinSizeMB     int    `json:"min_size_mb"`
	MaxSizeMB     int    `json:"max_size_mb"` // 0 = no max
	FreeleechOnly bool   `json:"freeleech_only"`
	SortType      string `json:"sort_type"` // default | seeders | size | ...
}

// Settings holds every user-configurable value. MamID is the MAM session
// cookie; it is a persisted secret and is masked whenever rendered to the
// API (see internal/api).
type Settings struct {
	MamID               string         `json:"mam_id"`
	Reserve             int            `json:"reserve"` // buffer kept below MAM's unsat.limit
	NextRunDelayMinutes int            `json:"next_run_delay_minutes"`
	SearchFilters       SearchFilters  `json:"search_filters"`
	DownloadClient      DownloadClient `json:"download_client"`
}

// DefaultSettings returns the baseline settings for a fresh install.
func DefaultSettings() Settings {
	return Settings{
		Reserve:             5,
		NextRunDelayMinutes: 30,
		SearchFilters: SearchFilters{
			SortType: "default",
		},
		DownloadClient: DownloadClient{
			Type: ClientQBittorrent,
			// Ratio/seeding-time limit default to Unlimited rather than Use
			// global — packrat exists to build ratio, so torrents it adds
			// shouldn't stop seeding on their own unless the user opts into
			// a limit explicitly.
			AddOptions: AddOptions{
				RatioLimit:              QbitLimitUnlimited,
				SeedingTimeLimitMinutes: QbitLimitUnlimited,
			},
		},
	}
}

// Totals tracks cumulative activity across all runs.
type Totals struct {
	CumulativeTorrentsAdded int `json:"cumulative_torrents_added"`
	CumulativeRuns          int `json:"cumulative_runs"`
}

// AddedTorrent records one torrent added to the download client during a
// run.
type AddedTorrent struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Size  int64  `json:"size_bytes"`
}

// HistoryEntry records the outcome of one scheduled or manual top-up run.
type HistoryEntry struct {
	CreatedAt     time.Time      `json:"created_at"`
	StartedAt     time.Time      `json:"started_at"`
	Result        string         `json:"result"`
	UnsatCount    int            `json:"unsat_count"`
	UnsatLimit    int            `json:"unsat_limit"`
	TargetCount   int            `json:"target_count"` // limit - reserve
	NeededCount   int            `json:"needed_count"`
	AddedTorrents []AddedTorrent `json:"added_torrents"`
	// DryRun marks an entry from a manual dry-run trigger: real MAM status
	// and search, but nothing was actually downloaded or added, and
	// cumulative totals were not updated.
	DryRun bool `json:"dry_run"`
}

// Admin holds the single admin account (Sonarr/Radarr first-launch pattern).
type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// Auth holds session-auth state that must survive a process restart: the
// HMAC key used to sign session tokens, and the set of currently valid
// sessions (id -> expiry).
type Auth struct {
	HMACKey  []byte           `json:"hmac_key,omitempty"`
	Sessions map[string]int64 `json:"sessions,omitempty"` // sessionID -> expiry (unix seconds)
}

// State is the full persisted document.
type State struct {
	Admin       *Admin         `json:"admin,omitempty"`
	Auth        Auth           `json:"auth"`
	Settings    Settings       `json:"settings"`
	Totals      Totals         `json:"totals"`
	SchedulerOn bool           `json:"scheduler_enabled"`
	Paused      bool           `json:"paused"`
	NextRunTime *time.Time     `json:"next_run_time,omitempty"`
	History     []HistoryEntry `json:"history"`
}

const maxHistory = 300

// Store guards State with a mutex and persists it to dataDir/config.json.
type Store struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	state   State
}

// Open loads state from dataDir/config.json, creating the directory and a
// fresh default document if none exists yet.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	s := &Store{
		path:    filepath.Join(dataDir, "config.json"),
		dataDir: dataDir,
		state: State{
			Settings: DefaultSettings(),
		},
	}
	if _, err := os.Stat(s.path); err == nil {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return nil, fmt.Errorf("reading state file: %w", err)
		}
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("parsing state file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat state file: %w", err)
	} else {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// DataDir returns the directory state is persisted under.
func (s *Store) DataDir() string {
	return s.dataDir
}

// View runs fn with a read lock held over a copy of the current state.
func (s *Store) View(fn func(State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.state)
}

// Update runs fn with a write lock held, letting it mutate state in place,
// then persists the result to disk.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	if len(s.state.History) > maxHistory {
		s.state.History = s.state.History[len(s.state.History)-maxHistory:]
	}
	return s.saveLocked()
}

// saveLocked writes state to disk. Caller must hold s.mu.
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}
	return nil
}
