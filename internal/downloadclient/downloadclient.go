// Package downloadclient abstracts over torrent client backends
// (qBittorrent, qui) so the scheduler can add torrents without knowing
// which one is configured.
package downloadclient

import (
	"fmt"

	"github.com/miista/mam-ratio/internal/store"
)

// Client adds torrents to a download client backend.
type Client interface {
	// AddTorrent submits a .torrent file's raw bytes to the client.
	AddTorrent(torrentFile []byte, name string) error
}

// New constructs a Client for the given configuration. Returns an error for
// an unconfigured or unrecognized client type.
func New(cfg store.DownloadClient) (Client, error) {
	switch cfg.Type {
	case store.ClientQBittorrent:
		return newQBittorrentClient(cfg), nil
	case store.ClientQui:
		return newQuiClient(cfg), nil
	default:
		return nil, fmt.Errorf("unknown download client type %q", cfg.Type)
	}
}
