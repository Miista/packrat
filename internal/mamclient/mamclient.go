// Package mamclient talks to MyAnonamouse's JSON endpoints: account
// unsatisfied-torrent status and the torrent search API.
package mamclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// SearchFilters is the set of criteria sent to MAM's search endpoint.
type SearchFilters struct {
	Text          string
	MinSeeders    int
	MaxSeeders    int // 0 = unset
	MinLeechers   int
	MaxLeechers   int // 0 = unset
	MinSizeMB     int
	MaxSizeMB     int // 0 = unset
	FreeleechOnly bool
	SortType      string
	PerPage       int
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

type searchResponseItem struct {
	ID       json.Number `json:"id"`
	Title    string      `json:"title"`
	Size     json.Number `json:"size"`
	Seeders  json.Number `json:"seeders"`
	Leechers json.Number `json:"leechers"`
	Free     json.Number `json:"free"`
	FLVip    json.Number `json:"fl_vip"`
	DL       string      `json:"dl"`
}

type searchResponse struct {
	Data []searchResponseItem `json:"data"`
}

// Search queries MAM's torrent search API and returns matching results,
// each with a ready-to-download URL.
//
// The download URL is NOT simply the "dl" field from the response — it
// must be built as /tor/download.php/{dl} with the torrent's id
// re-appended as a "tid" query parameter, or MAM 403s with "Invalid
// download link: missing tid" (confirmed against a working reference
// implementation, not guessed).
func (c *Client) Search(f SearchFilters) ([]SearchResult, error) {
	q := url.Values{}
	if f.Text != "" {
		q.Set("tor[text]", f.Text)
	}
	if f.MinSeeders > 0 {
		q.Set("tor[minSeeders]", strconv.Itoa(f.MinSeeders))
	}
	if f.MaxSeeders > 0 {
		q.Set("tor[maxSeeders]", strconv.Itoa(f.MaxSeeders))
	}
	if f.MinLeechers > 0 {
		q.Set("tor[minLeechers]", strconv.Itoa(f.MinLeechers))
	}
	if f.MaxLeechers > 0 {
		q.Set("tor[maxLeechers]", strconv.Itoa(f.MaxLeechers))
	}
	if f.MinSizeMB > 0 {
		q.Set("tor[minSize]", strconv.Itoa(f.MinSizeMB))
		q.Set("tor[unit]", "MB")
	}
	if f.MaxSizeMB > 0 {
		q.Set("tor[maxSize]", strconv.Itoa(f.MaxSizeMB))
		q.Set("tor[unit]", "MB")
	}
	if f.FreeleechOnly {
		// TODO(unverified): flag id "3" for freeleech is a placeholder, not
		// confirmed against MAM's real browseFlags values — research only
		// established that browseFlags is the right *mechanism* (an array
		// of flag IDs + a show/hide toggle), not which ID means freeleech.
		// Verify this against a real search response (or MAM's own search
		// form HTML/JS) before relying on it — do not treat this as correct.
		q.Set("tor[browseFlags][]", "3")
		q.Set("tor[browseFlagsHideVsShow]", "0")
	}
	sortType := f.SortType
	if sortType == "" {
		sortType = "default"
	}
	q.Set("tor[sortType]", sortType)
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	q.Set("perpage", strconv.Itoa(perPage))
	q.Set("dlLink", "true")

	body, err := c.get("/tor/js/loadSearchJSONbasic.php?" + q.Encode())
	if err != nil {
		return nil, err
	}
	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("mam returned non-JSON search response")
	}

	results := make([]SearchResult, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		size, _ := item.Size.Int64()
		seeders, _ := item.Seeders.Int64()
		leechers, _ := item.Leechers.Int64()
		free, _ := item.Free.Int64()
		flVip, _ := item.FLVip.Int64()

		downloadURL := ""
		if item.DL != "" {
			downloadURL = fmt.Sprintf("%s/tor/download.php/%s?tid=%s", baseURL, item.DL, item.ID.String())
		}

		results = append(results, SearchResult{
			ID:          item.ID.String(),
			Title:       item.Title,
			SizeBytes:   size,
			Seeders:     int(seeders),
			Leechers:    int(leechers),
			Freeleech:   free != 0 || flVip != 0,
			DownloadURL: downloadURL,
		})
	}
	return results, nil
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
