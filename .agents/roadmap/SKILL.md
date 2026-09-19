---
name: roadmap
description: Phased build roadmap for the Concurrent Health Monitor (Go + htmx, deployed on Render with Neon Postgres). Load when planning work, resuming after a break, or deciding what to build next — breaks a 1-2 week build into ordered phases with concrete deliverables and exit criteria. Always read .agents/instruction/SKILL.md alongside this file.
---

# Roadmap — Concurrent Health Monitor

Target: a working, deployed, portfolio-quality build in 10-14 days of part-time work.
Each phase should be its own set of commits. Do not start a phase whose prerequisites
aren't checked off.

## Phase 0 — Foundations (Day 1)

Goal: an empty-but-real Go service that boots, is dockerized, and is provisioned.

- [ ] `go.mod`, base package layout per the instruction file
- [ ] `internal/config`: load `PORT`, `DATABASE_URL`, `MAX_WORKERS`, `CHECK_INTERVAL_SECONDS`
      from env with sane defaults
- [ ] `GET /healthz` returning 200 — this is what Render's health check will hit
- [ ] Dockerfile with `dev` (air, live-reload) and `prod` (distroless, compiled binary)
      stages; `docker-compose.yml` for local dev
- [ ] Provision a free Neon Postgres project and branch; confirm `psql`/`pgx` can reach
      it from your machine
- [ ] `.env.example` committed, real `.env` gitignored

**Exit criteria:** `docker compose up` serves `/healthz` with live reload on file save.

## Phase 1 — Core concurrency engine (Day 2-3)

Goal: the worker pool and rate limiter — the actual showcase piece — proven correct in
isolation, with no HTTP or DB involved yet.

- [ ] `internal/ratelimit`: hand-rolled per-key token bucket (channel + ticker refill)
- [ ] `internal/monitor`: bounded worker pool (semaphore channel) that takes a list of
      URLs and an `http.Client` and checks them concurrently with per-target timeout
- [ ] Unit tests for both, run under `-race`, including a test that floods the limiter
      from many goroutines and asserts the rate is actually respected
- [ ] A throwaway `cmd/loadtest` (or a `go test -bench`) that checks a few thousand
      dummy targets and reports checks/sec — first draft of the number that goes in
      the README

**Exit criteria:** `go test -race ./internal/...` green; you have a real checks/sec
number, even against a mock target.

## Phase 2 — Persistence (Day 4)

Goal: targets and check results survive a restart.

- [x] `migrations/001_init.sql`: `targets`, `checks` tables
- [x] `internal/store`: `CreateTarget`, `ListTargets`, `DeleteTarget`, `RecordCheck`,
      `RecentStats` — one parameterized query per method, `pgxpool.Pool` injected in
      the constructor
- [x] Migration runs automatically on boot (idempotent `IF NOT EXISTS`)
- [x] `internal/monitor` writes results through `store.RecordCheck` instead of discarding them

**Exit criteria:** restart the service, previously-added targets and their history are
still there.

## Phase 3 — API + htmx UI (Day 5-6)

Goal: a person can add/remove monitored URLs and see status from a browser.

- [x] `internal/handlers`: `GET /`, `POST /targets`, `DELETE /targets/{id}`,
      `GET /targets` (partial)
- [x] `web/templates/index.html` + `partials/target_row.html`, `partials/stats.html`
- [x] `web/static/css/style.css` — the vercel-style dark theme (see design notes in README)
- [x] Add-target form submits via htmx (`hx-post`), row deletion via `hx-delete`, no
      full page reloads

**Exit criteria:** you can add a URL, watch it appear, delete it, all without JS you
wrote by hand beyond what htmx attributes need.

## Phase 4 — Real-time updates (Day 7)

Goal: the dashboard updates itself as checks complete, no polling.

- [x] `internal/sse`: broadcast hub, `Subscribe()`/`Publish()`, mutex-protected
      client set
- [x] `GET /events` SSE endpoint, monitor engine publishes a result event per check and
      a stats event per tick
- [x] htmx SSE extension on the page wires row/stat updates to incoming events;
      `web/static/js/app.js` only handles the small bits htmx attributes can't

**Exit criteria:** open the dashboard in two tabs, add a target in one, watch it and
its status appear live in both without a manual refresh.

## Phase 5 — Observability & resilience (Day 8-9)

Goal: it behaves like a service someone else could operate.

- [x] `log/slog` structured logs on startup, shutdown, and every check failure
- [x] `GET /metrics` — plain-text counters (checks total, in-flight, failures, uptime)
- [x] Graceful shutdown: `signal.NotifyContext`, in-flight checks allowed to finish,
      `http.Server.Shutdown` with timeout
- [x] Panic-recovery middleware on the HTTP server so one bad handler can't take down
      the process

**Exit criteria:** `docker compose kill -s SIGTERM app` shows a clean shutdown log, no
dropped connections mid-request.

## Phase 6 — Testing & load numbers (Day 10-11)

Goal: numbers and coverage you can put in the README without flinching.

- [ ] Integration test spinning up the real HTTP server against a local/test Postgres
- [ ] Full `-race` run across the whole module in CI (GitHub Actions)
- [ ] A real load test against the deployed or local instance; record checks/sec,
      p50/p99 check latency, memory footprint at N targets

**Exit criteria:** CI is green with a race-detector job; the README's performance
section has real, reproducible numbers with the command used to get them.

## Phase 7 — Ship (Day 12-14)

Goal: it's live, documented, and postable.

- [ ] Prod image builds and runs on Render free tier, pointed at the Neon `DATABASE_URL`
- [ ] `render.yaml` blueprint checked in
- [ ] README finished: architecture diagram (ASCII is fine), setup, deploy steps,
      performance numbers, design trade-offs section
- [ ] Record a short GIF/screen capture of the live dashboard for the README and the
      LinkedIn post
- [ ] Publish the LinkedIn post

**Exit criteria:** a stranger can `git clone`, read the README, and have it running
locally in under 10 minutes — and the deployed link works.
