# packrat

**Keeps your MyAnonamouse ratio working for you, automatically.**

MAM lets you have a certain number of unsatisfied torrents at once, based on
your rank. Staying near that number is how you build ratio — but it means
remembering to go find new torrents every time a few finish. Miss a few days
and you've drifted down to nothing.

packrat watches that number for you. When you have room, it searches MAM for
torrents matching what you want, hands them to your download client, and
gets on with it. You set it up once.

---

## Quick start

```yaml
services:
  packrat:
    image: ghcr.io/miista/packrat:latest
    container_name: packrat
    ports:
      - "8766:8766"
    volumes:
      - ./data:/app/data
    environment:
      PACKRAT_SETTING_MAM_ID: "your_mam_id_cookie_value"
    restart: unless-stopped
```

```sh
docker compose up -d
```

Open **http://localhost:8766**, create your admin account, point it at your
download client, and choose what you want it to look for.

That's it. It'll check in every 30 minutes.

## What you can tune

**Reserve** — how much headroom to leave. If MAM allows you 150 and you set a
reserve of 5, packrat fills up to 145 and leaves the rest for torrents you
grab yourself.

**What to search for** — free text, ebooks or audiobooks or both, freeleech
only, minimum seeders, size range, sort order.

**How often** — every 30 minutes by default.

**What your download client does with them** — category, tags, speed limits,
and whether to seed forever or stop at a ratio. Defaults to seeding
indefinitely, since that's rather the point.

There's a **dry run** button that shows you exactly what it would grab,
without grabbing anything. Worth using before you turn it loose.

## Good to know

**Your MAM cookie is best set as an environment variable** (as in the
quick-start above) rather than typed into the UI — that way it stays out of
the config file on disk. packrat will show the field as locked when it's set
this way.

**It skips things you've already snatched.** No duplicates.

**It never spends your freeleech wedges.** MAM's API offers a way to do that
automatically; packrat doesn't touch it, by design.

**It's gentle with MAM.** Downloads are spaced out so a big top-up doesn't
look like a hammering.

**Nothing leaves your machine** except requests to MAM and your own
download client.

## Settings reference

These four are also settable in the UI. Setting one here overrides the UI
and shows the field as locked:

| Variable | What it does | Default |
| --- | --- | --- |
| `PACKRAT_SETTING_MAM_ID` | Your MAM session cookie | — |
| `PACKRAT_SETTING_RESERVE` | Headroom below MAM's limit | `5` |
| `PACKRAT_SETTING_NEXT_RUN_DELAY_MINUTES` | Minutes between runs | `30` |
| `PACKRAT_SETTING_DOWNLOAD_DELAY_SECONDS` | Pause between downloads | `2` |

These are container-level only:

| Variable | What it does | Default |
| --- | --- | --- |
| `PACKRAT_DATA_DIR` | Where settings are saved inside the container | `/app/data` |
| `PACKRAT_AUTH_DISABLED` | Turn off the login screen | `false` |
| `LOG_LEVEL` | How chatty the logs are | `info` |

Your settings file holds your MAM cookie and download client password. It's
locked down to your user, but it isn't encrypted — keep the `data/` volume
somewhere you're comfortable with.

## Status

Early days, but genuinely in use — running daily against a real account
since September 2026.

One download client is supported so far. Search filters and the download
client connection have to be set in the UI rather than by environment
variable.

## Building it yourself

```sh
make docker    # -> packrat:local
```

Then point the compose file at `packrat:local` instead of the ghcr image.

Developer notes — architecture, MAM's API quirks, test fixtures — are in
[CLAUDE.md](CLAUDE.md).

## Related

Sibling to [AutoMouse](../automouse), which spends MAM bonus points.
