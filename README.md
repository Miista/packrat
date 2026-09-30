# mam-ratio

*Working name — not final.*

Automates topping up a MyAnonamouse account's number of unsatisfied
torrents to a target (the account's rank-based limit minus a configurable
reserve), by searching MAM for candidates matching configurable criteria
and adding them to a download client. Only qBittorrent is supported.

Sibling project to [AutoMouse](../automouse) (MAM bonus-point spending) —
same conventions: Go, zerolog, first-launch admin auth with persistent
sessions, `MAMRATIO_SETTING_<KEY>` env-var overrides with a locked-field UI,
petite-vue frontend, scratch Docker image.

## How the target works

MAM reports both numbers directly via its API (`jsonLoad.php?snatch_summary`
→ `unsat.count` / `unsat.limit`) — no need to hardcode or infer your
account's rank-based limit:

```
target = unsat.limit - reserve
needed = target - unsat.count
```

Each run fetches the current status, and if `needed > 0`, searches MAM
(paginating as needed) until it has collected `needed` candidates or
genuinely runs out of results, then adds them.

## Status

Early scaffold. Verified against real MAM data (not just compiled):
search, result parsing (title/size/seeders/leechers/freeleech), and
downloading an actual valid `.torrent` file all confirmed working live
(2026-09-30). Along the way, MAM's own API docs turned out to disagree with
reality in several places — corrected in `internal/mamclient/mamclient.go`,
each with a comment explaining the discrepancy:

- `size` is a human-formatted string (`"528.3 MiB"`), not a raw byte count
  as MAM's docs example implies. Parsed via `parseSizeString`.
- `id`, `seeders`, `leechers`, `free`, `fl_vip` are real JSON numbers, not
  strings.
- The title field is `title`, not `name` as MAM's own worked example
  showed.
- Downloads use MAM's documented `/tor/download.php?tid={id}` endpoint
  directly, rather than the search response's `dl` hash field. Both work
  (confirmed the `dl` hash also downloads successfully, and already
  embeds its own `?tid=...`), but `tid`-based download.php is MAM's
  stable documented contract — including an optional `fl` flag to spend a
  freeleech wedge on the torrent, which this app never sets (MAM's docs
  warn "no refunds available" for automated use of that flag).
- The search API has **no server-side min/max seeders/leechers/size
  parameters** — MAM's docs only support text/category/searchType/sort
  filtering server-side. Those four filters are applied **client-side**,
  as a post-filter over search results (`passesFilters`).

Only qBittorrent is supported as a download client (no qui, for now).

qBittorrent login and add-torrent are verified working against a real
instance (v5.2.3 / WebAPI 2.15.1, 2026-09-30) — a real MAM search result
was downloaded and successfully added. One thing this caught: qBittorrent's
login success response is inconsistent across versions (some return 200
with body "Ok.", this one returned 204 with an empty body) — fixed by
checking for a session cookie directly instead of trusting a specific
status/body combination.

Still not verified:

- The full scheduler run loop (search → download → add to client → record
  history) hasn't been exercised end-to-end yet — only its individual
  pieces (MAM search/download, qBittorrent login/add, the app's own HTTP
  layer) have been verified in isolation.

## Running

```sh
make build
make docker
docker compose up -d
```

Or without Docker:

```sh
go build -o mam-ratio .
MAMRATIO_ADDR=127.0.0.1:8766 MAMRATIO_DATA_DIR=./data ./mam-ratio
```

## Environment variables

- `MAMRATIO_ADDR` — listen address (default `127.0.0.1:8766`).
- `MAMRATIO_DATA_DIR` — where `config.json` is persisted (default `/app/data`).
- `MAMRATIO_AUTH_DISABLED=true` — disables the admin login entirely.
- `MAMRATIO_SETTING_<KEY>` — env-var overrides for `mam_id`, `reserve`,
  `next_run_delay_minutes`. Search filters and download client config are
  not yet env-overridable (only settable via the UI).
- `LOG_LEVEL` — zerolog level (default `info`).
