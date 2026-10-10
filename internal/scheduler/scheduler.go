// Package scheduler runs the torrent top-up loop on a timer and exposes
// start/pause/run-now controls. Pausing is a real state transition — the
// background ticker keeps polling every tick, but never fires a run while
// paused.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/packrat/internal/downloadclient"
	"github.com/miista/packrat/internal/mamclient"
	"github.com/miista/packrat/internal/settings"
	"github.com/miista/packrat/internal/store"
)

const pollInterval = 5 * time.Second

// postAddRefreshDelay is how long after a run that added torrents we re-read
// the unsat status. A var so tests can shorten it. The 3 minutes is a guess at
// MAM's lag, not a measured value — adjust once observed live.
var postAddRefreshDelay = 3 * time.Minute

// MamAPI is the slice of MAM's client that the scheduler actually uses.
// Declared here, in the consumer, rather than in mamclient: *mamclient.Client
// satisfies it implicitly, and tests can substitute a fake without touching
// the real client or reaching MAM's servers.
type MamAPI interface {
	UnsatStatus() (mamclient.UnsatStatus, error)
	Search(mamclient.SearchFilters) ([]mamclient.SearchResult, int, error)
	DownloadTorrentFile(downloadURL string) ([]byte, error)
}

// NewMamFunc builds a MAM client for one run. It is a factory rather than a
// single long-lived client because the session cookie comes from settings
// and can change while the app is running — a client built once at startup
// would keep using a stale cookie until the next restart.
type NewMamFunc func(mamclient.Secret) MamAPI

// NewDownloadClientFunc builds a download client from the current settings,
// per run, for the same reason: the user can change the connection details
// at any time. downloadclient.New satisfies this directly.
type NewDownloadClientFunc func(store.DownloadClient) (downloadclient.Client, error)

// Scheduler owns the background run loop.
type Scheduler struct {
	store *store.Store
	log   zerolog.Logger

	// Dependencies are injected at construction (see main.go, the
	// composition root) rather than built inline, so the run loop can be
	// exercised against fakes.
	newMAM NewMamFunc
	newDL  NewDownloadClientFunc

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

// New creates a scheduler bound to st. newMAM and newDL build the per-run
// clients; passing nil for either uses the real implementation, which keeps
// the common case at the call site short.
func New(st *store.Store, log zerolog.Logger, newMAM NewMamFunc, newDL NewDownloadClientFunc) *Scheduler {
	if newMAM == nil {
		newMAM = func(secret mamclient.Secret) MamAPI { return mamclient.New(secret) }
	}
	if newDL == nil {
		newDL = downloadclient.New
	}
	return &Scheduler{
		store:  st,
		log:    log.With().Str("component", "scheduler").Logger(),
		newMAM: newMAM,
		newDL:  newDL,
	}
}

// Start begins the background polling loop. Call once at startup.
func (s *Scheduler) Start() {
	go s.loop()
	s.RefreshUnsat()
}

// RefreshUnsat does a single read-only unsat-status check against MAM —
// no search, no adding, no history entry — so the dashboard has real
// numbers to show immediately (on app startup, and right after login)
// instead of "—" until the scheduler's first actual run completes, which
// might be minutes or hours away (or never, while paused).
func (s *Scheduler) RefreshUnsat() {
	go func() {
		var cfg store.Settings
		s.store.View(func(st store.State) {
			cfg = settings.Resolve(st.Settings).Settings
		})
		if cfg.MamID == "" {
			return
		}
		mam := s.newMAM(mamclient.Secret(cfg.MamID))
		unsat, err := mam.UnsatStatus()
		if err != nil {
			return
		}
		s.recordUnsat(unsat)
	}()
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

// ErrNoDownloadClient is returned by StartSchedule and RunNow when no
// download client has been configured yet — activating or running without
// one would just fail deep inside the run, once per candidate torrent.
var ErrNoDownloadClient = errors.New("no download client configured")

// StartSchedule enables the scheduler and arms the next run.
func (s *Scheduler) StartSchedule() error {
	var configured bool
	s.store.View(func(st store.State) { configured = st.Settings.DownloadClient.Configured() })
	if !configured {
		return ErrNoDownloadClient
	}
	return s.store.Update(func(st *store.State) {
		delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
		next := time.Now().Add(delay)
		st.SchedulerOn = true
		st.Paused = false
		st.NextRunTime = &next
		// Starting the scheduler is an explicit "go now" — drop any idle
		// backoff so the user gets the configured interval, not a 6-hour
		// gap inherited from before they paused it.
		st.ConsecutiveIdleRuns = 0
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
// executing or no download client has been configured yet.
func (s *Scheduler) RunNow() (bool, error) {
	var configured bool
	s.store.View(func(st store.State) { configured = st.Settings.DownloadClient.Configured() })
	if !configured {
		return false, ErrNoDownloadClient
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, nil
	}
	s.running = true
	s.mu.Unlock()
	go s.runAndReschedule()
	return true, nil
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
		_ = s.runOnce(context.Background(), true)
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

	addedBefore := s.cumulativeAdded()
	idle := s.runOnce(ctx, false)

	// The unsat count recorded at the start of the run is stale once torrents
	// have been added, and MAM doesn't reflect new snatches immediately, so
	// reading it straight away would show the old number too. Do one
	// read-only refresh a few minutes later instead. Detected via the
	// cumulative total so runOnce's signature stays untouched.
	if s.cumulativeAdded() > addedBefore {
		time.AfterFunc(postAddRefreshDelay, s.RefreshUnsat)
	}

	_ = s.store.Update(func(st *store.State) {
		if idle {
			st.ConsecutiveIdleRuns++
		} else {
			st.ConsecutiveIdleRuns = 0
		}
		if st.SchedulerOn && !st.Paused {
			next := time.Now().Add(nextDelay(st.Settings.NextRunDelayMinutes, st.ConsecutiveIdleRuns))
			st.NextRunTime = &next
		}
	})
}

// Idle backoff. An unsatisfied torrent only frees up its slot after
// seeding for 72 hours, so once the account sits at target there is
// genuinely nothing for a run to do until one of those crosses the line —
// hours away, not minutes. Polling every 30 minutes through that window is
// pure waste: the live instance recorded 48 consecutive runs over 24 hours
// reporting the identical "already at target" result (2026-10-02).
//
// So after idleThreshold consecutive idle runs, start doubling the gap,
// capped at maxIdleDelay. Any run that actually adds something resets the
// streak immediately, because torrents turning over means slots are
// opening again. Note the signal is "needed <= 0", not "the numbers didn't
// change" — unsat.count sits pinned at its target the whole time, so it
// never changes either way.
const (
	// Back off only after this many consecutive idle runs, so a brief
	// at-target moment doesn't immediately slow the schedule down.
	idleThreshold = 3
	// Ceiling on the backed-off interval. Well under the 72-hour seeding
	// window, so packrat still notices slots opening reasonably promptly.
	maxIdleDelay = 6 * time.Hour
)

// nextDelay returns how long to wait before the next run, given the
// configured base interval and how many consecutive idle runs precede it.
func nextDelay(baseMinutes, consecutiveIdle int) time.Duration {
	base := time.Duration(baseMinutes) * time.Minute
	if base <= 0 {
		base = 30 * time.Minute
	}
	if consecutiveIdle < idleThreshold {
		return base
	}
	// Double once per idle run past the threshold: 2x, 4x, 8x, ...
	delay := base
	for i := idleThreshold; i < consecutiveIdle; i++ {
		delay *= 2
		if delay >= maxIdleDelay {
			return maxIdleDelay
		}
	}
	if delay > maxIdleDelay {
		return maxIdleDelay
	}
	return delay
}

// runOnce executes one top-up pass. It returns true when the run was
// "idle": it reached MAM successfully and found nothing to add because the
// account is already at target. Only that specific outcome feeds the idle
// backoff — an error, a cancellation or a run that genuinely added
// something must not be mistaken for a quiet account.
func (s *Scheduler) runOnce(ctx context.Context, dryRun bool) (idle bool) {
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
		return false
	}

	mam := s.newMAM(mamclient.Secret(cfg.MamID))

	unsat, err := mam.UnsatStatus()
	if err != nil {
		entry.Result = fmt.Sprintf("Failed to fetch unsatisfied-torrent status: %v", err)
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return false
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
		// The one genuinely idle outcome: MAM answered, and there is
		// simply no room to add anything.
		return true
	}
	entry.NeededCount = needed

	var dlClient downloadclient.Client
	if !dryRun {
		var err error
		dlClient, err = s.newDL(cfg.DownloadClient)
		if err != nil {
			entry.Result = fmt.Sprintf("Download client not configured: %v", err)
			s.log.Warn().Err(err).Msg(entry.Result)
			s.appendHistory(entry)
			return false
		}
	}

	// Candidates are fetched from MAM on demand rather than all up front:
	// a page is only requested when the add loop below is about to run out
	// of untried candidates and still needs more. This lets a run recover
	// from per-candidate failures (duplicate/already-in-qBittorrent 409s,
	// transient download errors) by pulling in replacements instead of
	// ending early just because the originally fetched batch didn't pan
	// out — confirmed live, 2026-10-04: a run needing 6 drew 6 candidates
	// that were all duplicates of torrents already in qBittorrent under
	// different filenames (MAM's own my_snatched flag is per-torrent-ID and
	// doesn't catch a different release of a work already held), and ended
	// with 0 added instead of searching further for genuinely new ones.
	//
	// rawPageSize/rawCursor track MAM's own result-set position, which is
	// NOT the same as len(candidates): Search() applies client-side filters
	// (seeders/leechers/size range, freeleech, already-snatched — see
	// mamclient.Search) after fetching one raw page, so a raw page can
	// return fewer matches than requested, or even zero, while MAM still
	// has more matching rows further on. Advancing StartNumber by however
	// many candidates were accepted (instead of by the raw page size
	// actually requested) would desync the cursor from MAM's real result
	// set and could skip straight past still-matching rows — which is
	// exactly what caused runs to wrongly report "no more candidates"
	// after less than the needed count. A raw page only means genuine
	// exhaustion when MAM returns zero raw rows for it — Search's rawCount
	// return value, not len(results).
	//
	// maxPages bounds total search pages fetched across the whole run, and
	// maxAttempts (needed * MaxAddAttemptsPerNeeded, configurable) bounds
	// total add attempts — together they guarantee the run terminates even
	// if every candidate MAM offers turns out to be unusable. Hitting
	// maxAttempts is reported distinctly in the result message because it
	// will most likely recur on the next run too: the fix is widening the
	// search filters, not retrying.
	const maxPages = 10
	const rawPageSize = 100
	maxAttempts := needed * cfg.MaxAddAttemptsPerNeeded
	if maxAttempts <= 0 {
		maxAttempts = needed
	}
	cancelled := false
	noMoreResults := false
	searchFailed := false
	attemptsExhausted := false
	candidates := make([]mamclient.SearchResult, 0, needed)
	rawCursor := 0
	page := 0
	added := make([]store.AddedTorrent, 0, needed)
	nextCandidate := 0 // index into candidates of the next untried one

	fetchMore := func() {
		// Keep fetching until there are enough untried candidates left to
		// plausibly reach `needed` additions, not just until the candidate
		// slice has ever reached `needed` entries — otherwise a run whose
		// early candidates failed would never top up again.
		for page < maxPages && len(candidates)-nextCandidate < needed-len(added) && !noMoreResults && !searchFailed {
			if ctx.Err() != nil {
				cancelled = true
				return
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
				Category:      mamCategoryFor(cfg.SearchFilters.Category),
				SortType:      cfg.SearchFilters.SortType,
				PerPage:       rawPageSize,
				StartNumber:   rawCursor,
			}
			page++
			results, rawCount, err := mam.Search(filters)
			if err != nil {
				entry.Result = fmt.Sprintf("Search failed: %v", err)
				s.log.Warn().Err(err).Msg(entry.Result)
				searchFailed = true
				return
			}
			rawCursor += rawPageSize
			if rawCount == 0 {
				// MAM returned zero raw rows for this page — genuinely
				// nothing more to offer, not just a page that got filtered
				// down to nothing.
				noMoreResults = true
				return
			}

			for _, res := range results {
				if res.DownloadURL == "" {
					s.log.Warn().Str("title", res.Title).Msg("search result had no download URL, skipping")
					continue
				}
				candidates = append(candidates, res)
			}
		}
	}

	fetchMore()

	// downloadDelay throttles consecutive MAM download requests. Without
	// it, a batch of ~90 downloads fired back-to-back in under a second
	// tripped MAM's rate limiting (HTTP 429) after roughly the first 10 —
	// confirmed live, 2026-10-01, once logging was fixed enough to actually
	// show the error. Configurable via settings (default 2s); ctx-aware so
	// Pause() still interrupts promptly instead of waiting out the delay
	// first.
	//
	// A 429 on top of that delay means MAM is still asking us to slow down,
	// so the response is to back off — NOT to skip the candidate and
	// immediately request the next one, which is what a plain "continue"
	// would do, hammering the tracker once per remaining candidate. Each
	// 429 instead doubles the delay (or honours Retry-After when MAM sends
	// one), waits, and retries the same candidate. After
	// maxRateLimitRetries consecutive 429s the run stops adding entirely
	// and keeps whatever it already got; the next scheduled run picks up
	// the remaining shortfall. None of this is user-configurable on
	// purpose: backing off politely is not a preference.
	const (
		maxRateLimitRetries = 3
		maxBackoff          = 5 * time.Minute
	)
	baseDelay := time.Duration(cfg.DownloadDelaySeconds) * time.Second
	downloadDelay := baseDelay
	rateLimited := false
	retries := 0
	attempts := 0
	firstAttempt := true
	// retrying tracks whether this iteration is a backoff retry of the same
	// candidate rather than a move to the next one (nextCandidate is only
	// advanced on a non-retry outcome). Without it, a 429 right after the
	// very first attempt would skip the delay on its retry, since
	// firstAttempt would otherwise already be false — hammering MAM exactly
	// when it asked us to stop.
	retrying := false
	for {
		if len(added) >= needed {
			break
		}
		if attempts >= maxAttempts {
			attemptsExhausted = true
			break
		}
		if !retrying && nextCandidate >= len(candidates) {
			// Out of untried candidates — top up before continuing.
			fetchMore()
			if cancelled {
				break
			}
			if nextCandidate >= len(candidates) {
				// fetchMore couldn't produce any more (exhausted, failed,
				// or hit maxPages) — nothing left to try.
				break
			}
		}
		res := candidates[nextCandidate]
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if (!firstAttempt || retrying) && !dryRun {
			select {
			case <-time.After(downloadDelay):
			case <-ctx.Done():
				cancelled = true
			}
			if cancelled {
				break
			}
		}

		firstAttempt = false

		if dryRun {
			// Dry run: candidate is a real search result that passed
			// every filter, but nothing is downloaded or added — just
			// recorded as what this run would have picked.
			attempts++
			nextCandidate++
			added = append(added, store.AddedTorrent{ID: res.ID, Title: res.Title, Size: res.SizeBytes})
			s.log.Info().Str("title", res.Title).Int64("size", res.SizeBytes).Msg("dry run: would add torrent")
			continue
		}

		torrentFile, err := mam.DownloadTorrentFile(res.DownloadURL)
		if err != nil {
			var rl *mamclient.RateLimitError
			if errors.As(err, &rl) {
				retries++
				if retries > maxRateLimitRetries {
					// Still rate limited after backing off repeatedly.
					// Stop asking: keep what we have and let the next run
					// try again later.
					rateLimited = true
					s.log.Warn().Int("added", len(added)).Int("needed", needed).
						Msg("still rate limited after backing off, stopping this run")
					break
				}
				// Honour MAM's own Retry-After when it sends one, otherwise
				// double our delay. Either way, retry this same candidate
				// (nextCandidate is NOT advanced) rather than burning it.
				if rl.HasRetryAfter && rl.RetryAfter > downloadDelay {
					downloadDelay = rl.RetryAfter
				} else {
					downloadDelay *= 2
				}
				if downloadDelay > maxBackoff {
					downloadDelay = maxBackoff
				}
				s.log.Warn().Dur("delay", downloadDelay).Int("attempt", retries).
					Str("title", res.Title).Msg("rate limited by MAM, backing off")
				retrying = true
				continue
			}
			attempts++
			nextCandidate++
			s.log.Warn().Err(err).Str("title", res.Title).Msg("failed to fetch torrent file, skipping")
			continue
		}
		// A clean download means the backoff worked; reset so one blip
		// doesn't slow the whole remaining batch to a crawl.
		retries = 0
		retrying = false
		downloadDelay = baseDelay
		attempts++
		nextCandidate++
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
	case rateLimited:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents — MAM rate limited us, so this run stopped early. The next run will continue.", verb, len(added), needed)
	case searchFailed:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents before a search request failed.", verb, len(added), needed)
	case attemptsExhausted:
		entry.Result = fmt.Sprintf("%s %d of %d needed torrents — hit the attempt limit (%d) after too many duplicate or failed candidates; this will likely recur until search filters are widened.", verb, len(added), needed, maxAttempts)
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
	return false
}

func (s *Scheduler) cumulativeAdded() (n int) {
	s.store.View(func(st store.State) { n = st.Totals.CumulativeTorrentsAdded })
	return n
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

// mamCategoryFor maps the persisted category string to MAM's actual
// main_cat ID (see mamclient.CategoryAudiobook/CategoryEbook). Any
// unrecognized value (including "" and "all") means no restriction.
func mamCategoryFor(category string) int {
	switch category {
	case "audiobook":
		return mamclient.CategoryAudiobook
	case "ebook":
		return mamclient.CategoryEbook
	default:
		return 0
	}
}
