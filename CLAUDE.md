# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go service that keeps a MyAnonamouse (MAM) account's *unsatisfied torrent*
count topped up to `unsat.limit - reserve`, by searching MAM for candidates
matching configured filters and adding them to qBittorrent. Single binary +
petite-vue frontend, served on `:8766` (hardcoded in `main.go`).

Sibling project to `../automouse`; both share conventions (zerolog
ConsoleWriter, first-launch admin auth, `PACKRAT_SETTING_<KEY>` env overrides
with locked-field UI, scratch Docker image).

## Commands

```sh
make build                 # CGO_ENABLED=0 GOOS=linux build -> ./packrat
ARCH=amd64 make build      # cross-build for a different host arch
make docker                # make build, then docker build -t packrat:local
go test ./... -run TestName ./internal/scheduler   # single test
gofmt -l . && go vet ./... && go build ./...       # what CI enforces
```

Local run without Docker: `PACKRAT_DATA_DIR=./data go run .` (also set
`PACKRAT_STATIC_DIR=web/static` if not running from the repo root).

CI (`.github/workflows/ci.yml`) fails on any unformatted file, `go vet`, or
`go test ./... -shuffle=on -race`. Tests live in `internal/mamclient` so far; they run under `-race` by default.

Releases are goreleaser-driven to `ghcr.io/miista/packrat`. Note `Dockerfile`
assumes a *prebuilt* binary in the build context (it's `FROM scratch` + COPY,
no in-image compile) — both `make docker` and goreleaser rely on that.

## Architecture

`main.go` wires four long-lived pieces and nothing else:

- **`internal/store`** — the single source of truth. One mutex-guarded
  `State` struct persisted as `dataDir/config.json` (0600, write-to-tmp +
  rename). Everything goes through `store.View(fn)` / `store.Update(fn)`;
  there is no partial write path. Holds admin creds, auth HMAC key +
  sessions, settings, totals, scheduler on/paused/next-run, and a history
  ring capped at `maxHistory` (300).
- **`internal/auth`** — Sonarr/Radarr-style first-launch admin setup, bcrypt
  password, opaque HMAC-signed session cookie. The HMAC key and session set
  live in the store so restarts don't log the user out.
- **`internal/scheduler`** — owns the run loop. A 5s ticker checks whether
  `NextRunTime` is due; runs are serialized by `s.running`. `Pause()` both
  flips state *and* cancels the in-flight run's context so the add loop stops
  between torrents.
- **`internal/api`** — `net/http` mux. Every route except `/healthz`,
  `/api/setup`, `/api/login` is wrapped in `guarded`; state-changing routes
  additionally get `csrfGuard` (Origin vs Host).

Plus `internal/mamclient` (MAM HTTP) and `internal/downloadclient` (a
`Client` interface with one qBittorrent implementation).

### The run loop (`scheduler.runOnce`)

Deliberately two-phase, and the comments there explain why — read them before
editing:

1. **Collect** candidates across up to `maxPages` (10) raw pages of 100,
   downloading/adding nothing. This prevents a run from adding a partial
   batch because a later page came back short or errored.
2. **Add**: download each `.torrent` from MAM and push it to the download
   client, sleeping `DownloadDelaySeconds` between each.

Two cursor traps already fixed and easy to reintroduce:

- `Search` applies **client-side** filters, so results-returned ≠ rows-consumed.
  Advance the MAM cursor by the *raw* page size, not `len(candidates)`.
  Exhaustion is `rawCount == 0`, never `len(results) == 0`.
- MAM rate-limits (429) bursts of torrent downloads; `DownloadDelaySeconds`
  (default 2) exists because ~90 back-to-back downloads tripped it after ~10.

### MAM API quirks

`internal/mamclient/mamclient.go` documents, inline, several places where
MAM's own docs disagree with reality (`size` is a human string like
`"528.3 MiB"`; `id`/`seeders`/`leechers`/`free` are numbers not strings; the
title field is `title` not `name`). Keep those comments with the code they
explain. There are **no server-side seeders/leechers/size filters** — MAM only
supports text/category/searchType/sort server-side; the rest is `passesFilters`
client-side.

Downloads use `/tor/download.php?tid={id}`. Never set the `fl` flag — it
spends a freeleech wedge with no refund.

### MAM response fixtures

`internal/mamclient/testdata/*.json` are **real MAM responses**, captured live
and scrubbed. `parse_test.go` replays them through the actual `UnsatStatus`/
`Search` code paths via `httptest` (hence `baseURL` being a var, not a const),
so the documented docs-vs-reality corrections above are regression-tested
rather than just commented.

Fixtures are **whole captured responses**: every field MAM returns is kept,
with its original type and position — all 35 search-row fields, all 12
snatch_summary top-level fields, envelope included. Only account-identifying
*values* are replaced, never fields removed. Trimming a fixture to just the
fields the parser reads would destroy what it exists to prove (that the parser
copes with a real, complete payload), so `TestFixturesAreCompleteResponses`
fails if a recapture does that.

Replaced values: `username`, `uid`, `ratio`, `seedbonus`, `wedges`,
`vip_until`, up/downloaded totals, account `created`/`created_at`, and
`unsat` count/limit (rank-derived) in snatch_summary; torrent `id`, `title`,
`tags`, `ownership` (which embeds a **third-party uploader's** uid and
name), the `author_info`/`narrator_info`/`series_info` blobs, and inside
`mediainfo` the `General.Title` and `menu.extra` chapter list. That last one
is easy to miss — `mediainfo` looks like pure codec metadata, but it names
the real work and lists its chapters, which de-anonymizes the scrubbed
`Fixture Title N` sitting next to it. Its technical keys (Format, BitRate,
Channels, SamplingRate, Duration, CodecID) are kept, and the chapter *count*
is preserved so the blob stays realistically sized. Everything else — `my_snatched`, `free`/`fl_vip`, sizes, seeders,
categories, `added` timestamps — is real, because the tests assert against it.

To recapture, run the curl calls against `jsonLoad.php?snatch_summary` and
`loadSearchJSONbasic.php` on the host where a configured instance already
holds the cookie — read `mam_id` from that host's `config.json` into a shell
variable, never copy the file. Scrub before the fixture leaves that host, and
verify two ways: diff raw vs scrubbed so every JSON path is identical with
only intended values changed, AND sweep every string in the result against an
allowlist of known-safe patterns. The path diff alone would not have caught
the `mediainfo` leak, because that field was never on the replace list — only
the exhaustive string sweep surfaced it. Tests count fixture rows at runtime rather than
hardcoding totals, so a recapture with different data still passes; they fail
loudly if a fixture stops containing the cases it exists to cover (e.g. no
already-snatched rows).

Note the current `search_all.json` capture happens to contain only
`main_cat=14` rows, so it does *not* prove that an empty `main_cat` really
means "all categories".

### Settings and secrets

`internal/settings` implements the override convention: `PACKRAT_SETTING_<KEY>`
beats the persisted value and is reported to the UI as env-managed so the field
renders read-only. Only `mam_id`, `reserve`, `next_run_delay_minutes`,
`download_delay_seconds` are overridable (see `definitions`) — adding a new one
means adding to `definitions` *and* to the `apply(...)` calls in `Resolve`.

Secrets: the MAM cookie is wrapped in `mamclient.Secret`, whose `String()` and
`MarshalJSON()` always redact — use `.Reveal()` only at the HTTP call site.
Values rendered to the API go through `settings.MaskSecret`. `/data/` is
gitignored and holds live credentials.

qBittorrent `AddOptions` store qBittorrent's own sentinels directly
(`-2` = use global, `-1` = unlimited, `>=0` = custom) rather than inventing a
mapping. Defaults are unlimited ratio/seeding time, on purpose. qBittorrent
login success varies by version — detect the session cookie, not a status/body
combination.

## Conventions

Comments in this codebase explain *why*, often citing a live verification with
a date (e.g. the `LOG_LEVEL` empty-string zerolog trap in `main.go:newLogger`).
Match that: when fixing something non-obvious, record what was observed, not
just what changed. Keep README's "Status" section honest about what is actually
verified live versus merely compiled.
