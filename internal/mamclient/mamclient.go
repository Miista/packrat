// Package mamclient talks to MyAnonamouse's JSON endpoints: account
// unsatisfied-torrent status and the torrent search API.
package mamclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	baseURL   = "https://www.myanonamouse.net"
	userAgent = "mam-ratio-go"
)

// Client makes authenticated requests to MAM using the mam_id session
// cookie. The cookie is held as a Secret so it can never be accidentally
// logged in full.
type Client struct {
	cookie Secret
	http   *http.Client
}

// New creates a client authenticated with the given mam_id cookie value.
func New(mamID Secret) *Client {
	return &Client{
		cookie: mamID,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	req.Header.Set("Cookie", "mam_id="+url.QueryEscape(c.cookie.Reveal()))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mam http %d", resp.StatusCode)
	}
	return body, nil
}

// UnsatStatus is the account's unsatisfied-torrent count and rank-based
// limit, as reported by MAM itself.
type UnsatStatus struct {
	Count int
	Limit int
}

type unsatObj struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

// snatchSummaryResponse models the two known shapes of the unsat field:
// nested under snatch_summary (current, as of the 2026-09-23 API change) or
// top-level (older/fallback). Both are parsed defensively since at least
// two other MAM tools broke on this exact nesting change.
type snatchSummaryResponse struct {
	Unsat         *unsatObj `json:"unsat"`
	SnatchSummary *struct {
		Unsat *unsatObj `json:"unsat"`
	} `json:"snatch_summary"`
}

// UnsatStatus fetches the account's current unsatisfied-torrent count and
// rank-based limit. Validates the session cookie as a side effect (an
// invalid cookie surfaces as an HTTP or parse error here).
func (c *Client) UnsatStatus() (UnsatStatus, error) {
	body, err := c.get("/jsonLoad.php?snatch_summary")
	if err != nil {
		return UnsatStatus{}, err
	}
	var data snatchSummaryResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return UnsatStatus{}, fmt.Errorf("mam returned non-JSON response")
	}
	if data.SnatchSummary != nil && data.SnatchSummary.Unsat != nil {
		u := data.SnatchSummary.Unsat
		return UnsatStatus{Count: u.Count, Limit: u.Limit}, nil
	}
	if data.Unsat != nil {
		return UnsatStatus{Count: data.Unsat.Count, Limit: data.Unsat.Limit}, nil
	}
	return UnsatStatus{}, fmt.Errorf("mam response did not include an unsat field")
}

// SearchFilters is the set of criteria used to find top-up candidates.
//
// Per MAM's official search API documentation, the server side only
// supports text/category/searchType/sort filtering — there is no
// server-side min/max seeders, leechers, or size parameter. MinSeeders,
// MaxSeeders, MinLeechers, MaxLeechers, MinSizeMB, and MaxSizeMB are
// therefore applied CLIENT-SIDE, as a post-filter over the search
// response, not sent to MAM at all.
type SearchFilters struct {
	Text          string
	MinSeeders    int
	MaxSeeders    int // 0 = unset
	MinLeechers   int
	MaxLeechers   int // 0 = unset
	MinSizeMB     int
	MaxSizeMB     int // 0 = unset
	FreeleechOnly bool
	// SortType is one of MAM's real sort enum values, e.g. "seedersDesc",
	// "sizeAsc", "dateDesc", "default". See MAM's search API docs for the
	// full list; an empty value means "default".
	SortType string
	PerPage  int
}

// SearchResult is one torrent returned by MAM's search API.
type SearchResult struct {
	ID          string
	Title       string
	SizeBytes   int64
	Seeders     int
	Leechers    int
	Freeleech   bool
	DownloadURL string
}

// searchResponseItem models MAM's real response shape, confirmed against a
// live call (2026-09-30) — this differs from both MAM's own docs table and
// its worked example, which disagree with each other and with reality:
//   - id, seeders, leechers, free, fl_vip are genuine JSON numbers.
//   - size is a human-formatted string, e.g. "528.3 MiB" or "211.9 KiB" —
//     NOT a raw byte count despite what MAM's docs example shows
//     ("size": "6324306932"). Must be parsed with parseSizeString.
//   - the title field is actually named "title", not "name" as MAM's own
//     worked example implied.
type searchResponseItem struct {
	ID       json.Number `json:"id"`
	Title    string      `json:"title"`
	Size     string      `json:"size"`
	Seeders  json.Number `json:"seeders"`
	Leechers json.Number `json:"leechers"`
	Free     json.Number `json:"free"`
	FLVip    json.Number `json:"fl_vip"`
	// MySnatched is present in the real response (confirmed live,
	// 2026-09-30) despite not being listed in MAM's own output-parameters
	// table — another gap between that table and reality, same category as
	// size/title/etc above.
	MySnatched json.Number `json:"my_snatched"`
}

type searchResponse struct {
	Data []searchResponseItem `json:"data"`
}

// Search queries MAM's torrent search API and returns matching results,
// each with a ready-to-download URL, already filtered by the seeders/
// leechers/size criteria in f (applied client-side — see SearchFilters).
//
// The download URL uses MAM's documented download endpoint directly —
// /tor/download.php?tid={id} — rather than the "dl" hash field from the
// search response. Both work (the dl hash already carries its own
// embedded ?tid=..., confirmed live 2026-09-30), but tid-based download.php
// is the one MAM's own docs describe as the stable, documented contract,
// including the optional "fl" flag to spend a freeleech wedge on the
// torrent. This app never sets fl (see DownloadTorrentFile) — MAM's docs
// warn "no refunds available" for automated use of that flag.
func (c *Client) Search(f SearchFilters) ([]SearchResult, error) {
	payload := map[string]any{
		"tor": map[string]any{
			"text":       f.Text,
			"srchIn":     []string{"title"},
			"searchType": searchTypeFor(f),
			"main_cat":   []string{}, // empty = all categories
			"sortType":   sortTypeOrDefault(f.SortType),
		},
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 50
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/tor/js/loadSearchJSONbasic.php?perpage="+strconv.Itoa(perPage), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	req.Header.Set("Cookie", "mam_id="+url.QueryEscape(c.cookie.Reveal()))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mam http %d", resp.StatusCode)
	}

	var parsed searchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("mam returned non-JSON search response")
	}

	results := make([]SearchResult, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		size, err := parseSizeString(item.Size)
		if err != nil {
			// Skip results we can't size rather than silently treating
			// them as 0 bytes, which would let them slip past a MinSizeMB
			// filter that should have excluded them.
			continue
		}
		seedersN, _ := item.Seeders.Int64()
		leechersN, _ := item.Leechers.Int64()
		freeN, _ := item.Free.Int64()
		flVipN, _ := item.FLVip.Int64()
		mySnatchedN, _ := item.MySnatched.Int64()
		freeleech := freeN != 0 || flVipN != 0
		seeders := int(seedersN)
		leechers := int(leechersN)

		if mySnatchedN != 0 {
			// Already snatched by this account — skip it rather than let
			// it reach the download client, where it would just be
			// rejected as a duplicate (wasting a download+add attempt)
			// without freeing up a slot toward the top-up target.
			continue
		}
		if !passesFilters(f, size, seeders, leechers) {
			continue
		}
		if f.FreeleechOnly && !freeleech {
			continue
		}

		// DO NOT add "&fl" here. Per MAM's own docs, fl is presence-triggered
		// (any request carrying the parameter fires it, regardless of
		// value) and spends a freeleech wedge unconditionally — including
		// on VIP torrents, with no refund. This app never sets it.
		downloadURL := fmt.Sprintf("%s/tor/download.php?tid=%s", baseURL, item.ID.String())

		results = append(results, SearchResult{
			ID:          item.ID.String(),
			Title:       item.Title,
			SizeBytes:   size,
			Seeders:     seeders,
			Leechers:    leechers,
			Freeleech:   freeleech,
			DownloadURL: downloadURL,
		})
	}
	return results, nil
}

// searchTypeFor maps FreeleechOnly to MAM's searchType enum ("fl") when
// set, so the freeleech filter narrows results server-side rather than
// relying solely on the client-side post-filter — more efficient, and
// matches how MAM's own docs describe filtering for freeleech.
func searchTypeFor(f SearchFilters) string {
	if f.FreeleechOnly {
		return "fl"
	}
	return "all"
}

func sortTypeOrDefault(sortType string) string {
	if sortType == "" {
		return "default"
	}
	return sortType
}

// parseSizeString parses MAM's human-formatted size strings ("528.3 MiB",
// "211.9 KiB", "1.2 GiB") into bytes. Confirmed against live search
// responses (2026-09-30) that this is binary units (MiB/KiB, base 1024),
// not decimal (MB/KB, base 1000) — MAM's own docs incorrectly show size as
// a raw byte-count string in their worked example, which does not match
// reality.
func parseSizeString(s string) (int64, error) {
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, fmt.Errorf("unrecognized size format: %q", s)
	}
	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognized size value: %q", s)
	}
	var multiplier float64
	switch strings.ToUpper(parts[1]) {
	case "B":
		multiplier = 1
	case "KIB":
		multiplier = 1024
	case "MIB":
		multiplier = 1024 * 1024
	case "GIB":
		multiplier = 1024 * 1024 * 1024
	case "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unrecognized size unit: %q", s)
	}
	return int64(value * multiplier), nil
}

// passesFilters applies the client-side seeders/leechers/size range checks
// that MAM's search API does not support server-side.
func passesFilters(f SearchFilters, sizeBytes int64, seeders, leechers int) bool {
	sizeMB := sizeBytes / (1024 * 1024)
	if f.MinSeeders > 0 && seeders < f.MinSeeders {
		return false
	}
	if f.MaxSeeders > 0 && seeders > f.MaxSeeders {
		return false
	}
	if f.MinLeechers > 0 && leechers < f.MinLeechers {
		return false
	}
	if f.MaxLeechers > 0 && leechers > f.MaxLeechers {
		return false
	}
	if f.MinSizeMB > 0 && sizeMB < int64(f.MinSizeMB) {
		return false
	}
	if f.MaxSizeMB > 0 && sizeMB > int64(f.MaxSizeMB) {
		return false
	}
	return true
}

// DownloadTorrentFile fetches the .torrent file bytes for a search result's
// download URL, using the same authenticated session.
func (c *Client) DownloadTorrentFile(downloadURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "mam_id="+url.QueryEscape(c.cookie.Reveal()))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mam http %d fetching torrent file", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
