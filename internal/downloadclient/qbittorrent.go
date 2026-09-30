package downloadclient

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miista/mam-ratio/internal/store"
)

// qbittorrentClient talks to qBittorrent's WebUI API (v2). Auth is
// cookie-based: POST /api/v2/auth/login with username/password sets a
// session cookie (SID) that subsequent requests must carry — held via a
// cookiejar rather than manual header handling.
type qbittorrentClient struct {
	baseURL  string
	username string
	password string
	addOpts  store.AddOptions

	http *http.Client

	mu       sync.Mutex
	loggedIn bool
}

func newQBittorrentClient(cfg store.DownloadClient) *qbittorrentClient {
	jar, _ := cookiejar.New(nil)
	return &qbittorrentClient{
		baseURL:  strings.TrimRight(cfg.URL, "/"),
		username: cfg.Username,
		password: cfg.Password,
		addOpts:  cfg.AddOptions,
		http:     &http.Client{Timeout: 30 * time.Second, Jar: jar},
	}
}

func (c *qbittorrentClient) login() error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// qBittorrent's WebUI checks Referer/Origin against its configured host
	// allowlist for CSRF protection; without it, login can be rejected even
	// with correct credentials.
	req.Header.Set("Referer", c.baseURL)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qbittorrent login request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// qBittorrent's login success response is not consistent across
	// versions: some return 200 with body "Ok.", others (confirmed live
	// against a real instance, 2026-09-30) return 204 with an empty body.
	// The one reliable signal of success is whether a session cookie
	// (QBT_SID_*) was actually issued — check the cookie jar directly
	// rather than trust a specific status/body combination.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qbittorrent login failed: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !c.hasSessionCookie() {
		return fmt.Errorf("qbittorrent login did not return a session cookie (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	c.loggedIn = true
	return nil
}

// hasSessionCookie reports whether the client's cookie jar holds a
// qBittorrent session cookie (QBT_SID_*) for baseURL.
func (c *qbittorrentClient) hasSessionCookie() bool {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return false
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if strings.HasPrefix(ck.Name, "QBT_SID") {
			return true
		}
	}
	return false
}

// AddTorrent uploads a .torrent file to qBittorrent via multipart form,
// matching the WebUI API's /api/v2/torrents/add contract. Retries once
// with a fresh login if the session had expired. Torrents start
// immediately (qBittorrent's default add behavior) — this app does not
// add torrents paused.
func (c *qbittorrentClient) AddTorrent(torrentFile []byte, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.loggedIn {
		if err := c.login(); err != nil {
			return err
		}
	}

	status, respBody, err := c.uploadTorrent(torrentFile, name)
	if err != nil {
		return err
	}

	if status == http.StatusForbidden {
		// Session likely expired; retry once with a fresh login.
		c.loggedIn = false
		if err := c.login(); err != nil {
			return err
		}
		status, respBody, err = c.uploadTorrent(torrentFile, name)
		if err != nil {
			return err
		}
	}

	if status != http.StatusOK {
		return fmt.Errorf("qbittorrent add-torrent failed: http %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// uploadTorrent performs one multipart upload attempt and returns the raw
// status/body for the caller to interpret (including retry decisions).
func (c *qbittorrentClient) uploadTorrent(torrentFile []byte, name string) (status int, respBody []byte, err error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("torrents", name+".torrent")
	if err != nil {
		return 0, nil, err
	}
	if _, err := part.Write(torrentFile); err != nil {
		return 0, nil, err
	}
	if err := c.writeAddOptionFields(writer); err != nil {
		return 0, nil, err
	}
	if err := writer.Close(); err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v2/torrents/add", body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Referer", c.baseURL)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qbittorrent add-torrent request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ = io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// writeAddOptionFields adds the optional qBittorrent /api/v2/torrents/add
// form fields this app lets the user pin (category, tags, speed/ratio/time
// limits). Every field is omitted when unset, matching qBittorrent's own
// "leave at client default" behavior rather than sending an explicit zero,
// which for the limits below wouldn't mean "no limit" but "the client's
// current global limit".
func (c *qbittorrentClient) writeAddOptionFields(writer *multipart.Writer) error {
	o := c.addOpts
	fields := map[string]string{}
	if o.Category != "" {
		fields["category"] = o.Category
	}
	if o.Tags != "" {
		fields["tags"] = o.Tags
	}
	if o.UploadLimitKBs > 0 {
		fields["upLimit"] = strconv.Itoa(o.UploadLimitKBs * 1024)
	}
	if o.DownloadLimitKBs > 0 {
		fields["dlLimit"] = strconv.Itoa(o.DownloadLimitKBs * 1024)
	}
	if o.RatioLimit > 0 {
		fields["ratioLimit"] = strconv.FormatFloat(o.RatioLimit, 'f', -1, 64)
	}
	if o.SeedingTimeLimitMinutes > 0 {
		fields["seedingTimeLimit"] = strconv.Itoa(o.SeedingTimeLimitMinutes)
	}
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return err
		}
	}
	return nil
}
