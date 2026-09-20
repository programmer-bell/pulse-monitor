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

> **Status / numbers:** see [Performance](#performance) — every figure there
> is real and reproducible, with the exact command that produced it (run it
> yourself: `make load-up && go run ./cmd/loadtest -targets 500 -duration 180s`).

---

## What it looks like

_Add a screenshot or a short GIF of the running dashboard here before you
publish this repo — it's the single highest-leverage thing you can add for a
recruiter skimming GitHub._

![Pulse Monitor dashboard — concurrent health checks at a glance](web/static/images/screenshort.png)

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
  satisfied structurally by `*store.Store` and `*handlers.Handlers`. The
  dependency runs one way: `internal/monitor` never imports either package.
  `*store.Store` and `*handlers.Handlers` do import `internal/monitor`, but
  only for the small shared value types (`Target`, `Stats`, `Result`) and one
  sentinel error. This is what makes
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

Two kinds of real number, each with the exact command that produced it.

**Engine ceiling** — the pure worker pool against a single fast local
upstream, no database involved (a `go test` micro-benchmark):

```
$ go test ./internal/monitor/... -run '^$' -bench BenchmarkPool_Check -benchtime 2s
BenchmarkPool_Check-12  692  3221233 ns/op  62088 checks/sec
```

**Sustained throughput through the whole pipeline** — the prod Docker image
writing every check to Neon Postgres, monitoring 500 targets served by a
throwaway internal target server on the same Docker network, so no external
site gets hammered for the numbers. Reproduce with:

```
$ make load-up                          # prod app (localhost:8080) + internal target server
$ go run ./cmd/loadtest -targets 500 -duration 180s
$ make load-down
```

Load profile (see [`compose.load.yaml`](compose.load.yaml)): `MAX_WORKERS=256`,
`CHECK_INTERVAL_SECONDS=5`, `CHECK_TIMEOUT_SECONDS=3`, `DOMAIN_RPS=500`.

Output of the run (this machine, Go 1.25, 12 cores, Neon `ap-southeast-1`):

```
pulse-monitor load report
-------------------------
targets monitored      500
sampling window        3m0s
checks exercised       1004
sustained checks/sec   5.6
burst checks/sec       100 (min 0, avg 5.4)
p50 check latency      11.0 ms
p99 check latency      50.0 ms
failed checks          0 (0.0%)
```

Reading those honestly, because a reviewer will ask:

- **5.6 checks/sec sustained is a *database-write* bound, not an engine
  bound.** The pool fans 500 requests out across 256 workers in a couple of
  seconds — but each result is then persisted with one `INSERT` and each
  target's stats recomputed with one aggregate query, executed *sequentially*
  from `monitor.tick` (one store method per query, no batching — a deliberate
  simplicity trade-off documented in `internal/store`). Against remote Neon
  that is roughly 1000 sequential round trips per tick, and tick duration —
  not request fan-out — is what limits checks/sec. Same stack pointed at a
  local Postgres shifts the number sharply upward; the engine's own ceiling
  is the 62k checks/sec micro-benchmark above.
- **p50 11 ms / p99 50 ms** are the latencies of the checks themselves
  against the in-network target server. Against real internet targets these
  are shaped by the upstream's response time, not by this engine.
- **0 failed checks** — every seeded target answered HTTP 200; this run
  exercised the happy path end to end. Failures are recorded and streamed to
  the dashboard the same way (see the `/metrics` failure counter).
- **Memory at 500 targets: ≈ 16 MiB RSS** (app container, sampled with
  `docker stats` during the run). Footprint is dominated by the in-memory
  target list and template pool — it tracks row count, not worker count.

The concurrency guarantees this whole section is built on are themselves
tested, under the race detector, in
[`internal/ratelimit`](internal/ratelimit) and
[`internal/monitor`](internal/monitor) — see the Testing section below.

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

**Integration test** — boots the *real* HTTP stack (the same wiring
`cmd/server/main.go` performs) against a real Postgres and walks the full
path: target CRUD over HTTP (`400` bad input, `409` duplicate, `DELETE`), the
monitor engine persisting checks into that same database, `/metrics` moving
in lockstep, and a live `/events` SSE stream delivering a stats frame.

```bash
make test-db-up          # throwaway Postgres on :5433 (compose.test.yaml)
make test-integration    # TEST_DATABASE_URL=… go test -race ./integration/
make test-db-down        # stop and wipe it
```

It reads `TEST_DATABASE_URL` — deliberately *not* the production
`DATABASE_URL` — and skips when it is unset, which is exactly how CI stays
green without a database.

**CI** — `.github/workflows/ci.yml` runs `gofmt`, `go vet ./...`, and
`go test -race ./...` on every push and pull request.

## Deploying to Render

The live instance runs at **[https://pulse-monitor-vqwz.onrender.com/](https://pulse-monitor-vqwz.onrender.com/)** — open it to explore the running dashboard without any setup.

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
cmd/targetsrv/       throwaway upstream for the Phase 6 load test (never shipped)
cmd/loadtest/        Phase 6 load runner: seeds targets, samples /metrics, reports
internal/config/      env-based config, the only package allowed to read env vars
internal/ratelimit/   hand-rolled token-bucket limiter (start here)
internal/monitor/      the worker-pool/scheduler — the concurrency core
internal/store/        Postgres access via pgx, one method per query
internal/sse/           Server-Sent Events pub/sub hub
internal/handlers/      HTTP layer + template rendering (also the SSE Publisher)
internal/metrics/       atomic counters behind /metrics
web/templates/           html/template files (index + OOB partials)
web/static/               CSS (vercel-style dark theme) + the one JS file
integration/             end-to-end test: real HTTP stack + real Postgres
migrations/               reference copy of the schema (applied automatically at boot)
compose.test.yaml         dockerized Postgres for the integration test
compose.load.yaml         load-test stack: prod app + internal target server
.github/workflows/        CI: gofmt + go vet + go test -race ./...
.agents/instruction/      engineering standards — read this before touching code
.agents/roadmap/          phased build plan (what's done, what's next)
```

## Roadmap

This was built in phases — see
[`.agents/roadmap/SKILL.md`](.agents/roadmap/SKILL.md) for the full plan and
exit criteria for each one. Short version: foundations → concurrency engine →
persistence → API/UI → real-time → observability → testing → ship.

## License

MIT — see [LICENSE](LICENSE).
