# mam-ratio

*Working name — not final.*

Automates topping up a MyAnonamouse account's number of unsatisfied
torrents to a target (the account's rank-based limit minus a configurable
reserve), by searching MAM for candidates matching configurable criteria
and adding them to a download client (qBittorrent, or qui).

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

Each run fetches the current status, and if `needed > 0`, searches MAM and
adds up to `min(needed, max_add_per_run)` matching torrents.

## Status

Early scaffold. Built and verified to compile/run/serve, but the following
are NOT yet verified against real MAM/download-client behavior:

- **Freeleech search flag**: the `browseFlags` value used for
  freeleech-only filtering (`internal/mamclient/mamclient.go`) is a
  placeholder, not confirmed against a real MAM response. Do not trust the
  "Freeleech only" filter until this is checked.
- **qui download client**: not implemented at all — `internal/downloadclient/qui.go`
  is a stub that always errors. Only qBittorrent's WebUI API is wired up
  (and that itself hasn't been tested against a real qBittorrent instance
  yet, only written against its documented API contract).
- No end-to-end test has been run against the real MAM search API or a
  real download client — only the app's own HTTP layer (auth, settings,
  scheduler state) has been smoke-tested.

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
  `next_run_delay_minutes`, `max_add_per_run`. Search filters and download
  client config are not yet env-overridable (only settable via the UI).
- `LOG_LEVEL` — zerolog level (default `info`).
