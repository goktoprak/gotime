# GoTime

A local, personal-use TV show/episode tracker: one Go binary rendering
plain HTML, with everything it knows in a single SQLite file.

Nothing is sent anywhere except to `api.themoviedb.org`.

## Running it

```bash
go run .
```

Then open http://localhost:3000.

The database is created at `./gotime.db` on first run. Two flags, both
with environment-variable equivalents:

| Flag | Environment | Default | |
|---|---|---|---|
| `-db` | `GOTIME_DB` | `gotime.db` | path to the SQLite database |
| `-bind` | `GOTIME_BIND` | `0.0.0.0:3000` | address to listen on |

```bash
go run . -db /var/lib/gotime/gotime.db -bind 127.0.0.1:8080
```

To build a single binary instead — the stylesheet, htmx and every template
are compiled into it, so there is nothing to deploy beside it:

```bash
go build -o gotime .
```

There is no cgo, no build step, and no Node. `go build` on any platform Go
supports produces a working binary, including cross-compiled ones:

```bash
GOOS=linux GOARCH=arm64 go build -o gotime-pi .
```

## Behind a reverse proxy

The server speaks plain HTTP and terminates nothing — TLS belongs to the
proxy in front of it. It reads no `X-Forwarded-*` headers and builds no
absolute URLs, so there is nothing to configure on this side beyond pointing
the proxy at it; every redirect it issues is a bare path.

`docker-compose.yml` assumes that shape: it joins an existing `proxy` network
and publishes no port, so Nginx Proxy Manager forwards to `gotime:3000` over
the docker network and the container is unreachable from anywhere else.
Rename the network to match yours, and uncomment the `ports:` block if you
also want to reach it directly from the host.

One request is long: **Refresh All** re-reads every tracked show from TMDB in
a single POST. That is roughly 32 seconds for 261 shows — the client holds
the connection open throughout, so it has to fit inside the proxy's read
timeout (Nginx Proxy Manager's default is 60 seconds). If your library is
large enough to approach that, raise `proxy_read_timeout` in the proxy host's
Advanced tab. The rate the app fetches at is deliberate and explained in
[tmdb.go](tmdb.go); refreshing faster would mean TMDB refusing requests.

```bash
docker compose up -d
```

The database lives on the `gotime_data` volume at `/data/gotime.db`. To
start from a database you already have, copy it in before the first start:

```bash
docker compose cp gotime.db gotime:/data/gotime.db
```

## First run

Open the app, go to **Settings**, and paste in a free TMDB API key
(https://www.themoviedb.org/settings/api). It's stored in the `settings`
table in the database.

Then use **+ Add Show** and enter a TMDB TV show ID — the number in a show's
URL on themoviedb.org, e.g. `1399` for
`themoviedb.org/tv/1399-game-of-thrones`. That pulls in the show, all
seasons, and all episodes.

## How categories work

- **Watch List** — show added, nothing marked watched yet
- **Watching** — at least one episode watched, but not all of them
- **Ongoing** — every currently-known episode watched, and TMDB reports the
  show is still airing/in production/planned
- **Finished** — every currently-known episode watched, and TMDB reports the
  show has ended or been canceled

If a show is `Ongoing` or `Finished` and you hit **Refresh Metadata** and
TMDB has added episodes since you last checked, it drops back to `Watching`
automatically, since not everything is watched anymore.

A refresh also removes seasons and episodes TMDB has *stopped* reporting, so
what's stored matches what TMDB currently says. This deletes whatever you'd
marked watched on them, which can't be undone — the alternative was episodes
that no longer exist lingering forever, inflating a season's progress and
counting toward its category. Two exceptions: a refresh that comes back with
no seasons at all is treated as a bad answer and removes nothing, and a
season TMDB lists with no episodes yet is honoured as the real state it is.

**Specials** (TMDB season 0) are excluded from these rules entirely. They're
stored, shown, and individually checkable, but whether they're watched has no
bearing on the category — a show with every regular episode watched counts as
Ongoing or Finished even with unwatched specials.

Nothing refreshes automatically. Episode and season data is only re-pulled
when you click **Refresh Metadata** on a show, use **Refresh All Shows** in
Settings, or add a show for the first time.

## Backing up your data

The database — TMDB key, shows, and all watched progress — is a single SQLite
file. Settings → **Download Backup** streams a consistent snapshot taken with
`VACUUM INTO`, so it's safe even with uncheckpointed WAL writes.

## Resetting data

Delete `gotime.db` (and any `gotime.db-shm` / `gotime.db-wal` files
beside it) and restart. A fresh empty database is created on boot.

---

# Development

```bash
go test ./...
```

The tests cover the category rules against a real SQLite file, the store's
mutations, the TMDB client against a fake TMDB, every page and htmx fragment
over `httptest`, and the schema compatibility described below.

## Project layout

```
gotime/
├── main.go            -- flags, routes, server, shutdown
├── db.go              -- opening the database; schema and migrations
├── model.go           -- the domain types and the category rules
├── store.go           -- every query; nothing here knows about HTTP or TMDB
├── tmdb.go            -- the TMDB client
├── handlers.go        -- one handler per route
├── render.go          -- template loading, fragments, flash messages
├── templates/
│   ├── layout.html    -- the page shell every page fills in
│   ├── fragments.html -- the pieces htmx swaps on their own
│   └── *.html         -- one file per page
└── static/
    ├── style.css      -- hand-written, no framework
    └── htmx.min.js    -- vendored, so nothing is fetched from a CDN
```

## Notes on the design

**The schema is one idempotent script.** `db.go` holds the whole database
as `CREATE TABLE IF NOT EXISTS` statements, so first run and every restart
after it take the same code path and there is no migration ordering to get
wrong. `TestReopeningADatabaseKeepsEverything` runs it back over a database
that already has data and checks that nothing was disturbed.

**HTML is the API.** There is no JSON API and no client-side router: every
page is rendered server-side and every mutation is a form POST that
redirects. The `Show`, `Season` and `Episode` types never have to cross a
wire, so they're just Go structs.

**htmx does exactly one job.** Ticking episodes off is the one interaction
where a full page reload would be intolerable, so the three mark-watched
actions answer with the fragments they changed — every season they touched,
each one's progress line and episode list, plus the show's category badge —
marked `hx-swap-oob`. The season is the unit even for a single tick, which
is why one code path serves all three. Everything else on the show page
(refresh, delete, add, settings) is a plain form. With JavaScript switched
off, those three actions fall back to a redirect and a full re-render, which
lands on the same state.

**Seasons are `<details>` elements.** Opening and closing a season is the
browser's job, which is why an htmx swap of a season's contents doesn't
disturb which panels are open. The "Mark season watched" button sits *over*
the header rather than inside it, so clicking it can't also collapse the
season.

**The client never decides a category.** Same as the original: the rules
exclude specials and depend on the raw TMDB production status, so they live
in one place — `deriveCategory` in `model.go`, called only from
`recomputeCategory`, inside the same transaction as the mutation that
triggered it.

**One connection to SQLite.** This is a single-user app whose writes are all
short, and a pool of one means a write can never meet "database is locked"
from another request mid-transaction.
