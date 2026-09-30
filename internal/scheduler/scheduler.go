// Package scheduler runs the torrent top-up loop on a timer and exposes
// start/pause/run-now controls. Pausing is a real state transition — the
// background ticker keeps polling every tick, but never fires a run while
// paused.
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/mam-ratio/internal/downloadclient"
	"github.com/miista/mam-ratio/internal/mamclient"
	"github.com/miista/mam-ratio/internal/settings"
	"github.com/miista/mam-ratio/internal/store"
)

const pollInterval = 5 * time.Second

// Scheduler owns the background run loop.
type Scheduler struct {
	store *store.Store
	log   zerolog.Logger

	mu      sync.Mutex
	running bool
	// cancelRun stops the currently in-flight run's add-torrent loop
	// between iterations, when set. Non-nil only while a run is active.
	cancelRun context.CancelFunc

	// lastUnsat is the most recently observed unsat status. Kept in memory
	// only, deliberately not persisted — a restart just means it's unknown
	// until the next successful run.
	lastUnsat mamclient.UnsatStatus
	haveUnsat bool
}

// New creates a scheduler bound to st.
func New(st *store.Store, log zerolog.Logger) *Scheduler {
	return &Scheduler{store: st, log: log.With().Str("component", "scheduler").Logger()}
}

// Start begins the background polling loop. Call once at startup.
func (s *Scheduler) Start() {
	go s.loop()
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		due := false
		s.store.View(func(st store.State) {
			due = st.SchedulerOn && !st.Paused && st.NextRunTime != nil && !time.Now().Before(*st.NextRunTime)
		})
		if !due {
			continue
		}
		s.mu.Lock()
		alreadyRunning := s.running
		if !alreadyRunning {
			s.running = true
		}
		s.mu.Unlock()
		if alreadyRunning {
			continue
		}
		go s.runAndReschedule()
	}
}

// StartSchedule enables the scheduler and arms the next run.
func (s *Scheduler) StartSchedule() error {
	return s.store.Update(func(st *store.State) {
		delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
		next := time.Now().Add(delay)
		st.SchedulerOn = true
		st.Paused = false
		st.NextRunTime = &next
	})
}

// Pause disables the scheduler and, if a run is currently adding torrents,
// stops it between torrents rather than letting it finish its full batch.
// Search/download for the torrent already in flight completes (it's not
// interrupted mid-request), but no further torrents are added after that.
// No new run will be triggered until StartSchedule is called again.
func (s *Scheduler) Pause() error {
	s.mu.Lock()
	if s.cancelRun != nil {
		s.cancelRun()
	}
	s.mu.Unlock()
	return s.store.Update(func(st *store.State) {
		st.Paused = true
		st.SchedulerOn = false
	})
}

// RunNow triggers an immediate out-of-band run, unless one is already
// executing.
func (s *Scheduler) RunNow() bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.running = true
	s.mu.Unlock()
	go s.runAndReschedule()
	return true
}

// RunDryNow triggers an immediate, one-off dry run: fetches the real
// unsat status and runs a real search, but stops before downloading or
// adding anything. This is a manual, on-demand action only — the scheduler
// itself never does a dry run on its own, and a dry run never reschedules
// the next real run or updates cumulative totals.
func (s *Scheduler) RunDryNow() bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.running = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.runOnce(context.Background(), true)
	}()
	return true
}

// IsRunning reports whether a top-up pass is currently executing.
func (s *Scheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// UnsatStatus reports the most recently observed unsat count/limit. ok is
// false until at least one successful run has completed.
func (s *Scheduler) UnsatStatus() (status mamclient.UnsatStatus, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUnsat, s.haveUnsat
}

func (s *Scheduler) recordUnsat(status mamclient.UnsatStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUnsat = status
	s.haveUnsat = true
}

func (s *Scheduler) runAndReschedule() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelRun = cancel
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancelRun = nil
		s.mu.Unlock()
		cancel()
	}()

	s.runOnce(ctx, false)

	_ = s.store.Update(func(st *store.State) {
		if st.SchedulerOn && !st.Paused {
			delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
			next := time.Now().Add(delay)
			st.NextRunTime = &next
		}
	})
}

func (s *Scheduler) runOnce(ctx context.Context, dryRun bool) {
	startedAt := time.Now()
	entry := store.HistoryEntry{StartedAt: startedAt, CreatedAt: startedAt, Result: "Completed", DryRun: dryRun}

	var resolved settings.Resolved
	s.store.View(func(st store.State) {
		resolved = settings.Resolve(st.Settings)
	})
	cfg := resolved.Settings

	if cfg.MamID == "" {
		entry.Result = "No Mam Session_ID configured."
		s.log.Warn().Msg(entry.Result)
		s.appendHistory(entry)
		return
	}

	mam := mamclient.New(mamclient.Secret(cfg.MamID))

	unsat, err := mam.UnsatStatus()
	if err != nil {
		entry.Result = fmt.Sprintf("Failed to fetch unsatisfied-torrent status: %v", err)
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return
	}
	s.recordUnsat(unsat)
	entry.UnsatCount = unsat.Count
	entry.UnsatLimit = unsat.Limit

	target := unsat.Limit - cfg.Reserve
	if target < 0 {
		target = 0
	}
	entry.TargetCount = target

	needed := target - unsat.Count
	if needed <= 0 {
		entry.Result = "Already at or above target; nothing to add."
		entry.NeededCount = 0
		s.appendHistory(entry)
		s.log.Info().Int("unsat_count", unsat.Count).Int("target", target).Msg("top-up run complete, nothing needed")
		return
	}
	if cfg.MaxAddPerRun > 0 && needed > cfg.MaxAddPerRun {
		needed = cfg.MaxAddPerRun
	}
	entry.NeededCount = needed

	var dlClient downloadclient.Client
	if !dryRun {
		var err error
		dlClient, err = downloadclient.New(cfg.DownloadClient)
		if err != nil {
			entry.Result = fmt.Sprintf("Download client not configured: %v", err)
			s.log.Warn().Err(err).Msg(entry.Result)
			s.appendHistory(entry)
			return
		}
	}

	// Phase 1: collect candidates across as many pages as it takes to reach
	// `needed`, WITHOUT downloading or adding anything yet. Only once this
	// phase has run its course (either it collected enough, or genuinely
	// ran out of search results / hit an error) does phase 2 below start
	// downloading and adding — so a run never ends up having added a
	// partial batch just because a later search page came back short or
	// failed. maxPages bounds phase 1 so an exhausted search can't loop
	// forever.
	const maxPages = 10
	cancelled := false
	noMoreResults := false
	searchFailed := false
	candidates := make([]mamclient.SearchResult, 0, needed)

	for page := 0; page < maxPages && len(candidates) < needed; page++ {
		if ctx.Err() != nil {
			cancelled = true
			break
		}

		filters := mamclient.SearchFilters{
			Text:          cfg.SearchFilters.Text,
			MinSeeders:    cfg.SearchFilters.MinSeeders,
			MaxSeeders:    cfg.SearchFilters.MaxSeeders,
			MinLeechers:   cfg.SearchFilters.MinLeechers,
			MaxLeechers:   cfg.SearchFilters.MaxLeechers,
			MinSizeMB:     cfg.SearchFilters.MinSizeMB,
			MaxSizeMB:     cfg.SearchFilters.MaxSizeMB,
			FreeleechOnly: cfg.SearchFilters.FreeleechOnly,
			SortType:      cfg.SearchFilters.SortType,
			PerPage:       needed - len(candidates),
			StartNumber:   page * needed,
		}
		results, err := mam.Search(filters)
		if err != nil {
			entry.Result = fmt.Sprintf("Search failed: %v", err)
			s.log.Warn().Err(err).Msg(entry.Result)
			searchFailed = true
			break
		}
		if len(results) == 0 {
			// MAM has nothing more to offer for these filters; further
			// pages would be empty too.
			noMoreResults = true
			break
		}

		for _, res := range results {
			if len(candidates) >= needed {
				break
			}
			if res.DownloadURL == "" {
				s.log.Warn().Str("title", res.Title).Msg("search result had no download URL, skipping")
				continue
			}
			candidates = append(candidates, res)
		}
	}

	// Phase 2: only now, with candidates collected (however many phase 1
	// managed), actually download and add them. Cancellation is checked
	// here too — Pause() mid-phase-2 still stops before starting a new
	// add, same as before.
	added := make([]store.AddedTorrent, 0, len(candidates))
	for _, res := range candidates {
		if ctx.Err() != nil {
			cancelled = true
			break
		}

		if dryRun {
			// Dry run: candidate is a real search result that passed
			// every filter, but nothing is downloaded or added — just
			// recorded as what this run would have picked.
			added = append(added, store.AddedTorrent{ID: res.ID, Title: res.Title, Size: res.SizeBytes})
			s.log.Info().Str("title", res.Title).Int64("size", res.SizeBytes).Msg("dry run: would add torrent")
			continue
		}

		torrentFile, err := mam.DownloadTorrentFile(res.DownloadURL)
		if err != nil {
			s.log.Warn().Err(err).Str("title", res.Title).Msg("failed to fetch torrent file, skipping")
			continue
		}
		if err := dlClient.AddTorrent(torrentFile, res.Title); err != nil {
			s.log.Warn().Err(err).Str("title", res.Title).Msg("failed to add torrent to download client, skipping")
			continue
		}
		added = append(added, store.AddedTorrent{ID: res.ID, Title: res.Title, Size: res.SizeBytes})
		s.log.Info().Str("title", res.Title).Int64("size", res.SizeBytes).Msg("added torrent")
	}

	entry.AddedTorrents = added
	verb := "Added"
	if dryRun {
		verb = "Would add"
	}
	switch {
	case cancelled:
		entry.Result = fmt.Sprintf("Stopped: %s %d of %d needed torrents before the scheduler was deactivated.", strings.ToLower(verb), len(added), needed)
	case searchFailed:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents before a search request failed.", verb, len(added), needed)
	case noMoreResults && len(added) < needed:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents — MAM has no more matching candidates for the current filters.", verb, len(added), needed)
	case len(added) < needed:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents (hit the page-fetch limit or per-candidate errors).", verb, len(added), needed)
	default:
		entry.Result = fmt.Sprintf("%s %d torrents.", verb, len(added))
	}

	if !dryRun {
		s.updateTotals(len(added))
	}
	s.appendHistory(entry)
	s.log.Info().Int("added", len(added)).Int("needed", needed).Bool("dry_run", dryRun).Msg("run complete")
}

func (s *Scheduler) updateTotals(added int) {
	_ = s.store.Update(func(st *store.State) {
		st.Totals.CumulativeTorrentsAdded += added
		st.Totals.CumulativeRuns++
	})
}

func (s *Scheduler) appendHistory(entry store.HistoryEntry) {
	_ = s.store.Update(func(st *store.State) {
		st.History = append(st.History, entry)
	})
}
