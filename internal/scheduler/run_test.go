package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/miista/packrat/internal/downloadclient"
	"github.com/miista/packrat/internal/mamclient"
	"github.com/miista/packrat/internal/store"
)

// fakeMAM stands in for MAM. Each field controls one response; the counters
// record what the run loop actually asked for, which is how the pagination
// and backoff invariants are asserted.
type fakeMAM struct {
	mu sync.Mutex

	unsat    mamclient.UnsatStatus
	unsatErr error

	// searchFn answers one search call, given the filters it was called
	// with. Returning rawCount separately from the results is the point:
	// client-side filtering means they differ.
	searchFn func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error)

	// downloadFn answers one torrent-file fetch.
	downloadFn func(url string) ([]byte, error)

	searchCalls   []mamclient.SearchFilters
	downloadCalls []string
}

func (f *fakeMAM) UnsatStatus() (mamclient.UnsatStatus, error) {
	return f.unsat, f.unsatErr
}

func (f *fakeMAM) Search(filters mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
	f.mu.Lock()
	f.searchCalls = append(f.searchCalls, filters)
	f.mu.Unlock()
	if f.searchFn == nil {
		return nil, 0, nil
	}
	return f.searchFn(filters)
}

func (f *fakeMAM) DownloadTorrentFile(url string) ([]byte, error) {
	f.mu.Lock()
	f.downloadCalls = append(f.downloadCalls, url)
	f.mu.Unlock()
	if f.downloadFn == nil {
		return []byte("torrent"), nil
	}
	return f.downloadFn(url)
}

func (f *fakeMAM) searchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.searchCalls)
}

func (f *fakeMAM) downloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.downloadCalls)
}

// fakeDownloadClient records what was added.
type fakeDownloadClient struct {
	mu    sync.Mutex
	added []string
	err   error
}

func (c *fakeDownloadClient) AddTorrent(torrentFile []byte, name string) error {
	if c.err != nil {
		return c.err
	}
	c.mu.Lock()
	c.added = append(c.added, name)
	c.mu.Unlock()
	return nil
}

func (c *fakeDownloadClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.added)
}

// newTestScheduler builds a scheduler backed by a temp-dir store and the
// given fakes, with settings configured enough for a run to proceed.
func newTestScheduler(t *testing.T, mam *fakeMAM, dl *fakeDownloadClient) (*Scheduler, *store.Store) {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.Update(func(s *store.State) {
		s.Settings.MamID = "test-cookie"
		s.Settings.Reserve = 5
		s.Settings.DownloadDelaySeconds = 0 // no real waiting in tests
		s.Settings.DownloadClient.Type = store.ClientQBittorrent
		s.Settings.DownloadClient.URL = "http://localhost:8080"
	}); err != nil {
		t.Fatalf("seeding settings: %v", err)
	}

	sched := New(st, zerolog.New(io.Discard),
		func(mamclient.Secret) MamAPI { return mam },
		func(store.DownloadClient) (downloadclient.Client, error) { return dl, nil },
	)
	return sched, st
}

func results(n int, startID int) []mamclient.SearchResult {
	out := make([]mamclient.SearchResult, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%d", startID+i)
		out = append(out, mamclient.SearchResult{
			ID:          id,
			Title:       "Torrent " + id,
			SizeBytes:   1024,
			DownloadURL: "https://mam.test/tor/download.php?tid=" + id,
		})
	}
	return out
}

// At or above target, a run must stop before searching and report itself
// idle — the signal the backoff depends on.
func TestRunOnceIdleWhenAtTarget(t *testing.T) {
	mam := &fakeMAM{unsat: mamclient.UnsatStatus{Count: 145, Limit: 150}}
	dl := &fakeDownloadClient{}
	sched, st := newTestScheduler(t, mam, dl)

	idle := sched.runOnce(context.Background(), false)

	if !idle {
		t.Error("a run that found the account at target must report idle")
	}
	if mam.searchCount() != 0 {
		t.Errorf("searched %d times despite being at target", mam.searchCount())
	}
	var last store.HistoryEntry
	st.View(func(s store.State) { last = s.History[len(s.History)-1] })
	if last.NeededCount != 0 {
		t.Errorf("NeededCount = %d, want 0", last.NeededCount)
	}
}

// An error reaching MAM is not an idle run: the account might be wide open,
// we simply don't know, so the backoff must not treat it as quiet.
func TestRunOnceErrorIsNotIdle(t *testing.T) {
	mam := &fakeMAM{unsatErr: errors.New("mam http 503")}
	sched, _ := newTestScheduler(t, mam, &fakeDownloadClient{})

	if sched.runOnce(context.Background(), false) {
		t.Error("a failed status fetch must not count as an idle run")
	}
}

// A run that adds torrents is not idle either.
func TestRunOnceAddingIsNotIdle(t *testing.T) {
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 140, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(5, 1), 5, nil
		},
	}
	dl := &fakeDownloadClient{}
	sched, _ := newTestScheduler(t, mam, dl)

	if sched.runOnce(context.Background(), false) {
		t.Error("a run that added torrents must not report idle")
	}
	if dl.count() != 5 {
		t.Errorf("added %d torrents, want 5 (target 145 - count 140)", dl.count())
	}
}

// The pagination invariant that caused real "no more candidates" bugs: a
// page can filter down to nothing while MAM still has rows, so the cursor
// must advance by the raw page size and exhaustion is rawCount == 0 only.
func TestRunOncePaginatesByRawCountNotResults(t *testing.T) {
	var cursors []int
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 143, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			cursors = append(cursors, f.StartNumber)
			switch len(cursors) {
			case 1:
				// A full raw page that filtered down to nothing usable.
				return nil, 100, nil
			case 2:
				// Still nothing usable, but MAM keeps returning rows.
				return nil, 100, nil
			default:
				return results(2, 1), 100, nil
			}
		},
	}
	dl := &fakeDownloadClient{}
	sched, _ := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), false)

	if len(cursors) < 3 {
		t.Fatalf("stopped after %d searches; empty-but-nonzero pages must not end the search", len(cursors))
	}
	for i, got := range cursors[:3] {
		if want := i * 100; got != want {
			t.Errorf("search %d used StartNumber=%d, want %d (advance by raw page size)", i+1, got, want)
		}
	}
	if dl.count() != 2 {
		t.Errorf("added %d, want the 2 candidates found on the third page", dl.count())
	}
}

// A genuinely empty raw page means MAM has nothing more; stop there.
func TestRunOnceStopsOnZeroRawCount(t *testing.T) {
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 100, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return nil, 0, nil
		},
	}
	sched, st := newTestScheduler(t, mam, &fakeDownloadClient{})

	sched.runOnce(context.Background(), false)

	if mam.searchCount() != 1 {
		t.Errorf("made %d searches, want 1: a zero-row page means MAM is exhausted", mam.searchCount())
	}
	var last store.HistoryEntry
	st.View(func(s store.State) { last = s.History[len(s.History)-1] })
	if last.Result == "" {
		t.Error("expected a recorded result")
	}
}

// Phase 1 must finish collecting before phase 2 adds anything, so a run
// never adds a partial batch just because a later page came up short.
func TestRunOnceCollectsBeforeAdding(t *testing.T) {
	var addedDuringSearch int
	dl := &fakeDownloadClient{}
	mam := &fakeMAM{unsat: mamclient.UnsatStatus{Count: 140, Limit: 150}}
	mam.searchFn = func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
		addedDuringSearch = dl.count()
		return results(5, 1), 5, nil
	}
	sched, _ := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), false)

	if addedDuringSearch != 0 {
		t.Errorf("%d torrents were added while still searching; phase 1 must complete first", addedDuringSearch)
	}
	if dl.count() == 0 {
		t.Error("nothing was added after searching completed")
	}
}

// A dry run does real status and search work but must not download, add, or
// touch cumulative totals.
func TestRunOnceDryRunAddsNothing(t *testing.T) {
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 140, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(5, 1), 5, nil
		},
	}
	dl := &fakeDownloadClient{}
	sched, st := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), true)

	if dl.count() != 0 {
		t.Errorf("dry run added %d torrents to the client", dl.count())
	}
	if mam.downloadCount() != 0 {
		t.Errorf("dry run downloaded %d torrent files", mam.downloadCount())
	}
	var state store.State
	st.View(func(s store.State) { state = s })
	if state.Totals.CumulativeTorrentsAdded != 0 {
		t.Errorf("dry run updated cumulative totals to %d", state.Totals.CumulativeTorrentsAdded)
	}
	last := state.History[len(state.History)-1]
	if !last.DryRun {
		t.Error("history entry not marked as a dry run")
	}
	if len(last.AddedTorrents) != 5 {
		t.Errorf("dry run recorded %d would-add torrents, want 5", len(last.AddedTorrents))
	}
}

// A 429 must make the run retry the SAME candidate rather than skip it,
// and give up after repeated rate limits instead of hammering MAM once per
// remaining candidate.
func TestRunOnceRetriesSameCandidateOnRateLimit(t *testing.T) {
	var attempts int
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 144, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(1, 7), 1, nil
		},
		downloadFn: func(url string) ([]byte, error) {
			attempts++
			if attempts == 1 {
				return nil, &mamclient.RateLimitError{}
			}
			return []byte("torrent"), nil
		},
	}
	dl := &fakeDownloadClient{}
	sched, _ := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), false)

	if attempts != 2 {
		t.Errorf("download attempted %d times, want 2 (one 429 then a retry)", attempts)
	}
	if dl.count() != 1 {
		t.Errorf("added %d torrents, want 1: the rate-limited candidate must be retried, not skipped", dl.count())
	}
	if mam.downloadCalls[0] != mam.downloadCalls[1] {
		t.Errorf("retry hit a different URL (%s then %s); it must retry the same candidate",
			mam.downloadCalls[0], mam.downloadCalls[1])
	}
}

// Persistent rate limiting must stop the run early, keeping what it got,
// rather than requesting every remaining candidate.
func TestRunOnceStopsAfterRepeatedRateLimits(t *testing.T) {
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 130, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(15, 1), 15, nil
		},
		downloadFn: func(url string) ([]byte, error) {
			return nil, &mamclient.RateLimitError{}
		},
	}
	dl := &fakeDownloadClient{}
	sched, st := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), false)

	// Three retries then a stop: far fewer than the 15 candidates.
	if got := mam.downloadCount(); got > 5 {
		t.Errorf("made %d download attempts against a rate-limiting server, want it to give up quickly", got)
	}
	if dl.count() != 0 {
		t.Errorf("added %d torrents despite every download being rate limited", dl.count())
	}
	var last store.HistoryEntry
	st.View(func(s store.State) { last = s.History[len(s.History)-1] })
	if last.Result == "" {
		t.Error("expected the rate-limited outcome to be recorded in history")
	}
}

// An ordinary download failure skips that candidate and carries on — the
// opposite of the rate-limit response.
func TestRunOnceSkipsCandidateOnOrdinaryError(t *testing.T) {
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 142, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(3, 1), 3, nil
		},
		downloadFn: func(url string) ([]byte, error) {
			if url == "https://mam.test/tor/download.php?tid=1" {
				return nil, errors.New("mam http 404 fetching torrent file")
			}
			return []byte("torrent"), nil
		},
	}
	dl := &fakeDownloadClient{}
	sched, _ := newTestScheduler(t, mam, dl)

	sched.runOnce(context.Background(), false)

	if dl.count() != 2 {
		t.Errorf("added %d, want 2: a dead torrent is skipped, the rest still added", dl.count())
	}
	if mam.downloadCount() != 3 {
		t.Errorf("made %d download attempts, want 3: a 404 must not be retried", mam.downloadCount())
	}
}

// Cancelling mid-run stops between torrents rather than finishing the batch.
func TestRunOnceStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mam := &fakeMAM{
		unsat: mamclient.UnsatStatus{Count: 130, Limit: 150},
		searchFn: func(f mamclient.SearchFilters) ([]mamclient.SearchResult, int, error) {
			return results(15, 1), 15, nil
		},
	}
	dl := &fakeDownloadClient{}
	mam.downloadFn = func(url string) ([]byte, error) {
		if mam.downloadCount() >= 3 {
			cancel()
		}
		return []byte("torrent"), nil
	}
	sched, _ := newTestScheduler(t, mam, dl)

	sched.runOnce(ctx, false)

	if dl.count() > 5 {
		t.Errorf("added %d torrents after cancellation, want it to stop promptly", dl.count())
	}
}

// Without a MAM cookie there is nothing to do, and it must not be mistaken
// for an idle (at-target) run.
func TestRunOnceWithoutCookie(t *testing.T) {
	mam := &fakeMAM{}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	sched := New(st, zerolog.New(io.Discard),
		func(mamclient.Secret) MamAPI { return mam },
		func(store.DownloadClient) (downloadclient.Client, error) { return &fakeDownloadClient{}, nil },
	)

	if sched.runOnce(context.Background(), false) {
		t.Error("a run with no cookie configured must not count as idle")
	}
	if mam.searchCount() != 0 {
		t.Error("searched MAM without a cookie")
	}
}
