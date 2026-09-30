package downloadclient

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbittorrent login failed: http %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("qbittorrent login rejected: %s", strings.TrimSpace(string(body)))
	}
	c.loggedIn = true
	return nil
}

// AddTorrent uploads a .torrent file to qBittorrent via multipart form,
// matching the WebUI API's /api/v2/torrents/add contract. Retries once
// with a fresh login if the session had expired.
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
