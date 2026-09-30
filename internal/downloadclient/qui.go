package downloadclient

import (
	"fmt"

	"github.com/miista/mam-ratio/internal/store"
)

// quiClient talks to qui's (ghcr.io/autobrr/qui) API.
//
// NOT YET IMPLEMENTED: qui's actual add-torrent API shape (auth mechanism,
// endpoint path, request format) has not been verified against real
// documentation or source, unlike qBittorrent's WebUI API above. Writing a
// plausible-looking implementation without checking would repeat exactly
// the kind of unverified-assumption mistake this project has been careful
// to avoid elsewhere (the mam_id cookie, the 50 GiB purchase floor, the
// unsat.count/limit API shape — all confirmed against real sources before
// being relied on). Research qui's actual API (its GitHub repo/docs) before
// filling this in.
type quiClient struct {
	cfg store.DownloadClient
}

func newQuiClient(cfg store.DownloadClient) *quiClient {
	return &quiClient{cfg: cfg}
}

func (c *quiClient) AddTorrent(torrentFile []byte, name string) error {
	return fmt.Errorf("qui download client support is not yet implemented")
}
