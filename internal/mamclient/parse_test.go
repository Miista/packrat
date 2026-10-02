package mamclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real UnsatStatus/Search code paths against captured
// live MAM responses (testdata/*.json, scrubbed — see the package docs in
// CLAUDE.md). Their job is to pin down the places where MAM's actual
// response shape disagrees with MAM's own documentation, each of which has
// already caused a real bug: a "cleanup" that re-aligns this code with the
// docs should fail here rather than in production.

// serveFixture starts an httptest server returning the named fixture for
// every request, and points the package's baseURL at it for the duration
// of the test.
func serveFixture(t *testing.T, name string) *Client {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	orig := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = orig })

	return New(Secret("fixture-cookie"))
}

// TestUnsatStatusParsesNestedShape pins the nesting that the live response
// actually uses: top-level "unsat" is null and the real data sits under
// "snatch_summary.unsat". At least two other MAM tools broke on this exact
// change, which is why the parser checks the nested location first.
func TestUnsatStatusParsesNestedShape(t *testing.T) {
	c := serveFixture(t, "snatch_summary.json")

	got, err := c.UnsatStatus()
	if err != nil {
		t.Fatalf("UnsatStatus: %v", err)
	}
	if got.Count != 42 || got.Limit != 50 {
		t.Errorf("got count=%d limit=%d, want count=42 limit=50", got.Count, got.Limit)
	}
}

// TestUnsatStatusParsesLegacyTopLevelShape covers the fallback branch. This
// shape is synthesized rather than captured: MAM no longer serves it, but
// the parser still supports it, so the branch needs its own coverage.
func TestUnsatStatusParsesLegacyTopLevelShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unsat":{"count":7,"limit":20}}`))
	}))
	defer srv.Close()

	orig := baseURL
	baseURL = srv.URL
	defer func() { baseURL = orig }()

	got, err := New(Secret("x")).UnsatStatus()
	if err != nil {
		t.Fatalf("UnsatStatus: %v", err)
	}
	if got.Count != 7 || got.Limit != 20 {
		t.Errorf("got count=%d limit=%d, want count=7 limit=20", got.Count, got.Limit)
	}
}

func TestUnsatStatusErrorsWhenUnsatMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"snatch_summary":{}}`))
	}))
	defer srv.Close()

	orig := baseURL
	baseURL = srv.URL
	defer func() { baseURL = orig }()

	if _, err := New(Secret("x")).UnsatStatus(); err == nil {
		t.Fatal("expected an error when the response carries no unsat field")
	}
}

// TestSearchParsesDocumentedDiscrepancies is the core regression test for
// the four places MAM's docs disagree with reality. Each assertion here
// corresponds to a comment on searchResponseItem.
func TestSearchParsesDocumentedDiscrepancies(t *testing.T) {
	c := serveFixture(t, "search_all.json")

	results, rawCount, err := c.Search(SearchFilters{PerPage: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rawCount != 20 {
		t.Fatalf("rawCount = %d, want 20 (the raw row count MAM returned)", rawCount)
	}
	if len(results) == 0 {
		t.Fatal("no results parsed from a 20-row fixture")
	}

	first := results[0]

	// "title", not "name" as MAM's worked example showed. A title that
	// failed to bind would be the empty string.
	if first.Title == "" {
		t.Error("Title is empty: the field is named \"title\", not \"name\"")
	}

	// id is a real JSON number, not a string — but SearchResult.ID is a
	// string, so a binding failure shows up as "0" or "".
	if first.ID == "" || first.ID == "0" {
		t.Errorf("ID = %q: id is a JSON number and must still decode via json.Number", first.ID)
	}

	// size is a human-formatted string ("528.3 MiB"), not a raw byte count.
	// A row that failed to parse is skipped entirely, so a zero size here
	// means the human-string path broke.
	if first.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d: size is a human-formatted string and must be parsed, not cast", first.SizeBytes)
	}

	// Every row in the fixture carries a download URL built from tid, not
	// from the response's "dl" hash field.
	for _, r := range results {
		if want := "/tor/download.php?tid=" + r.ID; !strings.Contains(r.DownloadURL, want) {
			t.Errorf("DownloadURL = %q, want it to contain %q", r.DownloadURL, want)
		}
		// The fl flag spends a freeleech wedge with no refund. It must
		// never appear in a URL this app builds.
		if strings.Contains(r.DownloadURL, "&fl") || strings.HasSuffix(r.DownloadURL, "?fl") {
			t.Errorf("DownloadURL = %q must never carry the fl flag", r.DownloadURL)
		}
	}
}

// TestSearchSkipsAlreadySnatched uses the freeleech fixture, which is the
// one capture containing rows with my_snatched set (12 of 20). Those rows
// must never reach the download client, where they would be rejected as
// duplicates without freeing a slot toward the target.
func TestSearchSkipsAlreadySnatched(t *testing.T) {
	snatched := countFixtureRows(t, "search_freeleech.json", func(r map[string]any) bool {
		return asNumber(r["my_snatched"]) != 0
	})
	if snatched == 0 {
		t.Fatal("fixture no longer contains any already-snatched rows; it cannot test the skip")
	}

	c := serveFixture(t, "search_freeleech.json")
	results, rawCount, err := c.Search(SearchFilters{PerPage: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if want := rawCount - snatched; len(results) != want {
		t.Errorf("got %d results from %d raw rows with %d snatched, want %d",
			len(results), rawCount, snatched, want)
	}
}

// TestSearchRawCountIsIndependentOfFiltering pins the pagination invariant
// that caused runs to wrongly report "no more candidates": rawCount tracks
// MAM's own result-set position and must stay at the raw row count even
// when client-side filters reject every row.
func TestSearchRawCountIsIndependentOfFiltering(t *testing.T) {
	c := serveFixture(t, "search_all.json")

	// A size floor no fixture row can satisfy, so every row is filtered out.
	results, rawCount, err := c.Search(SearchFilters{PerPage: 20, MinSizeMB: 1 << 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected every row to be filtered out, got %d results", len(results))
	}
	if rawCount != 20 {
		t.Errorf("rawCount = %d, want 20: a fully filtered page still consumed 20 raw rows, "+
			"and callers must advance their cursor by that, not by len(results)", rawCount)
	}
}

// TestSearchFreeleechDetection covers the free || fl_vip branch. In the
// all-categories fixture many rows have free=0 but fl_vip set, which is
// exactly the case a naive "free != 0" check would miss.
func TestSearchFreeleechDetection(t *testing.T) {
	flVipOnly := countFixtureRows(t, "search_all.json", func(r map[string]any) bool {
		return asNumber(r["free"]) == 0 && asNumber(r["fl_vip"]) != 0
	})
	if flVipOnly == 0 {
		t.Skip("fixture contains no free=0/fl_vip!=0 rows; nothing to assert here")
	}

	c := serveFixture(t, "search_all.json")
	results, _, err := c.Search(SearchFilters{PerPage: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var freeleech int
	for _, r := range results {
		if r.Freeleech {
			freeleech++
		}
	}
	if freeleech < flVipOnly {
		t.Errorf("counted %d freeleech results but the fixture has %d rows with fl_vip set and free=0; "+
			"freeleech must be free != 0 || fl_vip != 0", freeleech, flVipOnly)
	}
}

// TestSearchFreeleechOnlyFilter checks the client-side FreeleechOnly
// post-filter against the capture where every row is freeleech.
func TestSearchFreeleechOnlyFilter(t *testing.T) {
	c := serveFixture(t, "search_freeleech.json")

	results, _, err := c.Search(SearchFilters{PerPage: 20, FreeleechOnly: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if !r.Freeleech {
			t.Errorf("result %s passed a FreeleechOnly search without being freeleech", r.ID)
		}
	}
}

// TestSearchParsesAllSizeUnits walks every fixture and asserts each row's
// size string decoded to a positive byte count. Across the four captures
// this covers KiB, MiB and GiB; a unit the parser cannot handle makes the
// row vanish, which this catches as a count mismatch.
func TestSearchParsesAllSizeUnits(t *testing.T) {
	for _, name := range []string{"search_all.json", "search_audiobook.json", "search_ebook.json", "search_freeleech.json"} {
		t.Run(name, func(t *testing.T) {
			// Only unsnatched rows survive Search, so compare against that.
			eligible := countFixtureRows(t, name, func(r map[string]any) bool {
				return asNumber(r["my_snatched"]) == 0
			})

			c := serveFixture(t, name)
			results, _, err := c.Search(SearchFilters{PerPage: 20})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(results) != eligible {
				t.Errorf("parsed %d results from %d eligible rows: a row was dropped, "+
					"most likely a size unit parseSizeString does not handle", len(results), eligible)
			}
			for _, r := range results {
				if r.SizeBytes <= 0 {
					t.Errorf("result %s parsed to %d bytes", r.ID, r.SizeBytes)
				}
			}
		})
	}
}

// TestSearchSendsPerPageInBody pins the request-side contract: perpage must
// be a top-level JSON body field. As a query-string parameter MAM silently
// ignores it and returns its own default page size, which broke pagination
// in production.
func TestSearchSendsPerPageInBody(t *testing.T) {
	type capture struct {
		PerPage json.Number `json:"perpage"`
		Tor     struct {
			MainCat     []int  `json:"main_cat"`
			SearchType  string `json:"searchType"`
			SortType    string `json:"sortType"`
			StartNumber string `json:"startNumber"`
		} `json:"tor"`
	}

	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if r.URL.RawQuery != "" {
			t.Errorf("request carried a query string %q; search parameters belong in the body", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	orig := baseURL
	baseURL = srv.URL
	defer func() { baseURL = orig }()

	_, _, err := New(Secret("x")).Search(SearchFilters{
		PerPage:     37,
		Category:    CategoryAudiobook,
		SortType:    "sizeAsc",
		StartNumber: 100,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got.PerPage.String() != "37" {
		t.Errorf("perpage = %s, want 37 as a top-level body field", got.PerPage)
	}
	if len(got.Tor.MainCat) != 1 || got.Tor.MainCat[0] != CategoryAudiobook {
		t.Errorf("main_cat = %v, want [%d]", got.Tor.MainCat, CategoryAudiobook)
	}
	if got.Tor.SortType != "sizeAsc" {
		t.Errorf("sortType = %q, want sizeAsc", got.Tor.SortType)
	}
	// startNumber is a string in MAM's API, not a number.
	if got.Tor.StartNumber != "100" {
		t.Errorf("startNumber = %q, want \"100\" as a string", got.Tor.StartNumber)
	}
}

// TestSearchFreeleechSetsSearchType confirms FreeleechOnly narrows
// server-side via searchType=fl in addition to the client-side post-filter.
func TestSearchFreeleechSetsSearchType(t *testing.T) {
	var searchType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tor struct {
				SearchType string `json:"searchType"`
			} `json:"tor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		searchType = body.Tor.SearchType
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	orig := baseURL
	baseURL = srv.URL
	defer func() { baseURL = orig }()

	if _, _, err := New(Secret("x")).Search(SearchFilters{FreeleechOnly: true}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if searchType != "fl" {
		t.Errorf("searchType = %q, want fl", searchType)
	}
}

func TestSearchErrorsOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	orig := baseURL
	baseURL = srv.URL
	defer func() { baseURL = orig }()

	if _, _, err := New(Secret("x")).Search(SearchFilters{}); err == nil {
		t.Fatal("expected an error on HTTP 429")
	}
}

// --- helpers ---

// countFixtureRows counts raw fixture rows matching pred, so tests can
// assert against what the capture actually contains rather than against a
// number hardcoded here that would silently rot if a fixture is recaptured.
func countFixtureRows(t *testing.T, name string, pred func(map[string]any) bool) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var parsed struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	var n int
	for _, r := range parsed.Data {
		if pred(r) {
			n++
		}
	}
	return n
}

func asNumber(v any) float64 {
	f, _ := v.(float64)
	return f
}

// TestFixturesAreCompleteResponses guards the fixtures themselves. They are
// whole captured responses: every field MAM returns is present, with only
// account-identifying VALUES replaced (see CLAUDE.md). A future recapture
// that trims the payload down to the fields the parser happens to read
// would quietly destroy the thing these fixtures exist to prove — that the
// parser copes with a real, complete response — so assert the full shape.
func TestFixturesAreCompleteResponses(t *testing.T) {
	// Fields MAM returns on every search row, well beyond the eight the
	// parser binds. Their presence is the point.
	wantRowFields := []string{
		"id", "language", "lang_code", "main_cat", "category", "mediatype",
		"maincat", "categories", "catname", "size", "numfiles", "vip",
		"vip_expire", "free", "personal_freeleech", "fl_vip", "title", "w",
		"tags", "author_info", "narrator_info", "series_info", "filetype",
		"seeders", "leechers", "added", "browseflags", "times_completed",
		"comments", "bookmarked", "my_snatched", "poster_type", "mediainfo",
		"ownership", "cat",
	}

	for _, name := range []string{"search_all.json", "search_audiobook.json", "search_ebook.json", "search_freeleech.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			var parsed struct {
				Data    []map[string]any `json:"data"`
				Found   *int             `json:"found"`
				PerPage *int             `json:"perpage"`
				Start   *int             `json:"start"`
				Total   *int             `json:"total"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			// The search envelope around "data" must survive too.
			if parsed.Found == nil || parsed.PerPage == nil || parsed.Start == nil || parsed.Total == nil {
				t.Error("fixture lost part of the response envelope (found/perpage/start/total)")
			}
			if len(parsed.Data) == 0 {
				t.Fatal("fixture has no rows")
			}
			for _, field := range wantRowFields {
				if _, ok := parsed.Data[0][field]; !ok {
					t.Errorf("row is missing %q: fixtures must stay complete responses, not trimmed to parsed fields", field)
				}
			}
		})
	}

	// The snatch_summary envelope likewise keeps every field, including the
	// account-level ones the parser ignores (scrubbed to fake values).
	raw, err := os.ReadFile(filepath.Join("testdata", "snatch_summary.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var snatch map[string]any
	if err := json.Unmarshal(raw, &snatch); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	for _, field := range []string{"classname", "downloaded", "downloaded_bytes", "ratio",
		"seedbonus", "snatch_summary", "uid", "uploaded", "uploaded_bytes", "username",
		"vip_until", "wedges"} {
		if _, ok := snatch[field]; !ok {
			t.Errorf("snatch_summary fixture is missing %q", field)
		}
	}
	if snatch["username"] != "FixtureUser" {
		t.Errorf("username = %v, want the scrubbed placeholder FixtureUser", snatch["username"])
	}
}
