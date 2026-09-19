<div align="center">
  <img src="web/static/images/icon.jpg" alt="Pulse Monitor logo" width="96" height="96" />
</div>

<h1 align="center">Concurrent Health Monitor</h1>

A bounded worker pool checks every URL you give it, in parallel, on a timer —
with a per-domain rate limit so it never hammers a single host — and streams
live results to a dashboard over Server-Sent Events. Go on the backend
(standard library only, with one dependency for the Postgres driver), htmx +
vanilla JS on the frontend, no build step anywhere.

This exists as a portfolio piece: the point is the concurrency engine in
[`internal/monitor`](internal/monitor) and [`internal/ratelimit`](internal/ratelimit),
not the CRUD around it.

> **Status / numbers:** see [Performance](#performance) — that section is
> intentionally left with real, reproducible commands rather than made-up
> figures. Fill it in from your own run before you link this in an application.

---

## What it looks like

_Add a screenshot or a short GIF of the running dashboard here before you
publish this repo — it's the single highest-leverage thing you can add for a
recruiter skimming GitHub._

## Why these choices

A few decisions in this repo are deliberate and worth explaining, since a
reviewer's first question is usually "did they actually understand this, or
did they copy a tutorial":

- **Hand-rolled rate limiter, not `golang.org/x/time/rate`.** [`internal/ratelimit`](internal/ratelimit/limiter.go)
  is a token bucket built from a buffered channel and a `time.Ticker`. It's
  the file most worth opening first — and it's tested under `-race` with
  concurrent producers, not just a single-threaded happy path.
- **`net/http`'s default `MaxIdleConnsPerHost` is 2.** Left alone, that
  throttles throughput to any single domain no matter how large the worker
  pool is. `internal/monitor` raises it and lets the rate limiter — not
  connection starvation — be the thing actually governing per-domain
  throughput. See the `Transport` setup in `monitor.New`.
- **Interfaces defined at the point of use, not the point of implementation.**
  `monitor.Store` and `monitor.Publisher` are declared in `internal/monitor`,
  satisfied structurally by `*store.Store` and `*handlers.Handlers`. Neither
  of those packages imports `monitor`. This is what makes
  [`internal/monitor/monitor_test.go`](internal/monitor/monitor_test.go)
  possible without a real Postgres connection — it hands the pool a fake
  in-memory `Store` and a real `httptest.Server`, then asserts under `-race`
  that the semaphore actually caps concurrent in-flight requests.
- **Server-Sent Events instead of WebSockets.** The dashboard only needs
  server → browser updates, and SSE is one HTTP response plus `net/http`'s
  `http.Flusher` — no dependency, no upgrade handshake. `internal/sse` is
  about 90 lines. Paired with htmx's out-of-band swaps
  (`hx-swap-oob="true"`), a check result renders itself into the one table
  row that changed, without a page reload or a hand-written diffing layer —
  see `PublishCheck`/`PublishStats` in `internal/handlers`.
- **`html/template`, not a frontend framework.** Every template is
  server-rendered, contextually auto-escaped (the URLs stored here are
  user-supplied — this is a real XSS boundary, not a formality), and shared
  between normal HTTP responses and SSE push via named `{{define}}` blocks.
- **Go 1.22's `http.ServeMux` patterns** (`"DELETE /targets/{id}"`,
  `"GET /{$}"`) instead of a router dependency.
- **One external dependency, on purpose.** `github.com/jackc/pgx/v5` is the
  only non-stdlib import in the module — Postgres wire protocol isn't
  something to hand-roll. Everything else (routing, rate limiting, SSE,
  logging via `log/slog`, metrics via `sync/atomic`) is standard library.
  See [`.agents/instruction/SKILL.md`](.agents/instruction/SKILL.md) for the
  full dependency policy this repo holds itself to.

## Architecture

```
                         ┌──────────────────────────┐
   ticker (interval) ──▶ │   monitor.tick(ctx)       │
                         │   ListTargets()           │
                         └─────────────┬─────────────┘
                                       │ fan out, one goroutine/target
                                       ▼
                         ┌──────────────────────────┐
                         │  semaphore (MaxWorkers)   │◀── bounds concurrency
                         │  ratelimit.Manager        │◀── bounds per-domain rps
                         │  http.Client.Do(check)    │
                         └─────────────┬─────────────┘
                                       │ store.CheckResult
                    ┌──────────────────┼──────────────────┐
                    ▼                                      ▼
         store.RecordCheck (Postgres)          handlers.Publisher (Handlers)
                                                    │ renders OOB HTML
                                                    ▼
                                              sse.Hub.Publish
                                                    │
                                                    ▼
                                    every open browser tab (GET /events)
```

`cmd/server/main.go` is the only place these concrete types are wired
together — see [`.agents/instruction/SKILL.md`](.agents/instruction/SKILL.md)
§2 for the package boundaries this repo enforces.

## Performance

The rate limiter's own concurrency test is real and reproducible right now:

```
$ go test -race ./internal/ratelimit/... -v
=== RUN   TestBucket_RespectsRate
--- PASS: TestBucket_RespectsRate (0.35s)
=== RUN   TestManager_PerKeyIsolation
--- PASS: TestManager_PerKeyIsolation (0.00s)
=== RUN   TestManager_ConcurrentCreation
--- PASS: TestManager_ConcurrentCreation (0.20s)
PASS
```

The worker pool's concurrency bound is verified the same way in
[`internal/monitor/monitor_test.go`](internal/monitor/monitor_test.go)
(`TestMonitor_BoundsConcurrency`), against a real `httptest.Server`.

What's *not* filled in yet, on purpose — this is Phase 6 of
[`.agents/roadmap/SKILL.md`](.agents/roadmap/SKILL.md), do it against your own
deployed instance and paste real numbers here before you publish:

- [ ] Sustained checks/sec at your chosen `MAX_WORKERS`
- [ ] p50 / p99 check latency
- [ ] Memory footprint at N monitored targets
- [ ] The exact command you used to produce the above (e.g. `hey`, `vegeta`,
      or a small Go load-test script) so it's reproducible

## Getting started

**Prerequisites:** Go 1.22+, Docker (optional but recommended), a free
[Neon](https://neon.tech) Postgres project.

```bash
git clone https://github.com/programmer-bell/pulse-monitor.git
cd pulse-monitor

cp .env.example .env
# edit .env — paste your Neon pooled connection string into DATABASE_URL

go mod tidy   # resolves and locks dependencies; pgx is added in Phase 2
```

**Option A — Docker, with live reload (recommended for day-to-day dev):**

```bash
docker compose up --build
```

This builds the Dockerfile's `dev` stage, which runs the app under
[`air`](https://github.com/air-verse/air) via `go run` — air is never
installed on your machine, only fetched into the dev image at build time
(`go install github.com/air-verse/air@v1.61.1`), and never shipped to the
`prod` image. Air runs the compiled binary as a child and restarts it on file
change: edit a `.go`, `.html`, `.css`, or `.js` file and the server rebuilds
and restarts automatically. The dev image runs air as PID 1 so a
`docker compose kill -s SIGTERM app` reaches the server and drains cleanly.

**Option B — no Docker:**

```bash
make dev
```

Does the same thing, straight on your machine.

Either way, open **http://localhost:8080**.

**Production build** (what actually ships — a compiled binary on a
distroless base image, no `air`, no Go toolchain in the final image):

```bash
make docker-build   # docker build --target prod
make docker-run
```

## Configuration

All configuration is environment variables — see [`.env.example`](.env.example).

| Variable                  | Default  | Meaning                                             |
| :------------------------ | :------- | :-------------------------------------------------- |
| `PORT`                    | `8080`   | HTTP listen port                                     |
| `DATABASE_URL`            | *(required)* | Postgres connection string (Neon pooled URL)     |
| `MAX_WORKERS`             | `64`     | Upper bound on simultaneous in-flight checks          |
| `CHECK_INTERVAL_SECONDS`  | `30`     | How often every target is re-checked                  |
| `CHECK_TIMEOUT_SECONDS`   | `10`     | Per-check HTTP timeout                                |
| `DOMAIN_RPS`               | `5`      | Max requests/sec sent to any single domain            |

## Testing

```bash
go test -race ./...     # everything, race detector on — this is the bar
go vet ./...
gofmt -l .               # should print nothing
```

`internal/ratelimit` and `internal/monitor` are the packages that matter most
here; both have tests that actually exercise concurrent access rather than
single-threaded happy paths — see
[`.agents/instruction/SKILL.md`](.agents/instruction/SKILL.md) §6 for why
that distinction is a hard requirement in this repo, not a nice-to-have.

## Deploying to Render

The short version: connect the repo, point `DATABASE_URL` at Neon, deploy.
The [`render.yaml`](render.yaml) Blueprint in this repo does the rest —
Render builds the Dockerfile's final (`prod`) stage automatically. Full
walkthrough in the PR/chat this repo came from, or just:

1. Push this repo to GitHub.
2. On Render, **New → Blueprint**, pick the repo — it reads `render.yaml`.
3. Set `DATABASE_URL` to your Neon pooled connection string when prompted.
4. Deploy. `/healthz` is what Render polls to know the instance is ready.

## Project structure

```
cmd/server/          entrypoint — wiring only, see the instruction file's §2
internal/config/      env-based config, the only package allowed to read env vars
internal/ratelimit/   hand-rolled token-bucket limiter (start here)
internal/monitor/      the worker-pool/scheduler — the concurrency core
internal/store/        Postgres access via pgx, one method per query
internal/sse/           Server-Sent Events pub/sub hub
internal/handlers/      HTTP layer + template rendering (also the SSE Publisher)
internal/metrics/       atomic counters behind /metrics
internal/logging/       log/slog setup
web/templates/           html/template files (index + OOB partials)
web/static/               CSS (vercel-style dark theme) + the one JS file
migrations/               reference copy of the schema (applied automatically at boot)
.agents/instruction/       engineering standards — read this before touching code
.agents/roadmap/            phased build plan (what's done, what's next)
```

## Roadmap

This was built in phases — see
[`.agents/roadmap/SKILL.md`](.agents/roadmap/SKILL.md) for the full plan and
exit criteria for each one. Short version: foundations → concurrency engine →
persistence → API/UI → real-time → observability → testing → ship.

## License

MIT — see [LICENSE](LICENSE).
