---
name: instruction
description: Core engineering standards for the Concurrent Health Monitor (Go + htmx). Load before writing, editing, or reviewing any code in this repo — covers dependency policy, package layout, concurrency rules, error handling, testing, and commit conventions. Applies to every phase in .agents/roadmap/SKILL.md.
---

# Engineering Standards — Concurrent Health Monitor

This project exists to demonstrate production-grade Go concurrency to backend hiring
managers. Every decision should be defensible in a code review, not just "made to work."

## 1. Dependency policy

- Standard library first, always. Reach for `net/http`, `html/template`, `context`,
  `sync`, `log/slog`, `database/sql` idioms before considering a module.
- Exactly one dependency is pre-approved: `github.com/jackc/pgx/v5` (Postgres driver +
  pool). Postgres wire protocol cannot reasonably be hand-rolled.
- No web framework (no gin/echo/fiber). `net/http` + `http.ServeMux` (Go 1.22+ pattern
  routing) is sufficient for this surface area.
- No ORM. Hand-written SQL in `internal/store`, every query parameterized.
- No frontend build step. htmx is loaded from a CDN `<script>` tag. Any other JS is
  vanilla, in `web/static/js`, no bundler, no npm.
- Before adding any new dependency, write one sentence in the PR/commit body justifying
  why the standard library cannot do it. If you can't write that sentence, don't add it.

## 2. Package layout

```
cmd/server        entrypoint only — wiring, flag/env parsing, graceful shutdown
cmd/loadtest      throwaway Phase 6 load runner (seed targets, sample /metrics, report)
cmd/targetsrv     throwaway Phase 6 load upstream (ships only in compose.load.yaml)
integration/      end-to-end test: real HTTP stack + real Postgres (skips unless
                  TEST_DATABASE_URL is set)
internal/config    env-based config struct, no globals
internal/store      Postgres access, one method per query, no business logic
internal/ratelimit  hand-rolled token-bucket limiter (this is a showcase piece — keep it
                     readable, it's the file a reviewer will open first)
internal/monitor    the worker-pool/scheduler engine — the concurrency core
internal/sse        pub/sub hub for Server-Sent Events, no external pubsub
internal/handlers   HTTP handlers; thin — parse request, call one collaborator, render
internal/metrics    atomic counters behind /metrics (slog is configured inline in
                    cmd/server — there is no logging package)
web/                templates + static assets, served directly, no templating engine
                     beyond html/template
```

Nothing outside `cmd/server` may call `os.Exit`, read env vars directly, or import
`net/http` route-registration helpers. Keep the entrypoint the only place that wires
concrete implementations together.

## 3. Concurrency rules (non-negotiable)

- Every goroutine's lifetime is owned by a `context.Context` derived from the process's
  root shutdown context. No goroutine may outlive the request/tick that spawned it
  without an explicit reason documented in a comment.
- Never start a goroutine without a plan for how it stops. If you write `go func() {`,
  the next line should make clear how that func returns.
- Bound concurrency explicitly (buffered channel used as a semaphore, or
  `sync.WaitGroup` + limiter) — never fan out one goroutine per item with no cap.
- Shared mutable state is guarded by a `sync.Mutex`/`RWMutex` with a comment stating
  exactly what it protects, or is only ever touched by a single owning goroutine that
  receives work over a channel. No "probably fine" shared state.
- All I/O (HTTP calls, DB queries) takes a `context.Context` and a timeout. No
  unbounded blocking calls.
- Run `go test -race ./...` before considering any concurrency-touching change done.
  A change to `internal/monitor` or `internal/ratelimit` without a `-race`-clean test
  run is not complete.

## 4. Error handling & logging

- Errors are wrapped with `fmt.Errorf("...: %w", err)` at each boundary that adds
  context (which target, which query).
- No `panic` in request-handling or monitor-loop code paths. Panics are only
  acceptable for programmer-error assertions during startup (e.g. bad config).
- Structured logging via `log/slog` only. Never log a secret (DB password, connection
  string) — log the sanitized host/db name if needed.
- HTTP handlers return the right status code (400 for bad input, 404 unknown target,
  500 only for genuinely unexpected failure) — never swallow an error into a silent 200.

## 5. HTTP / htmx conventions

- Handlers that back an htmx interaction return an HTML fragment (a `partials/*.html`
  template), not JSON. JSON is only for `/metrics`-style machine endpoints, if any.
- Every mutating endpoint (`POST`, `DELETE`) validates input server-side — never trust
  that the browser form validation ran.
- Keep templates free of business logic; compute display values in the handler and
  pass a plain struct to `html/template`.

## 6. Testing

- Table-driven tests for anything with more than one branch.
- `internal/ratelimit` and `internal/monitor` require tests that run under `-race` and
  actually exercise concurrent calls (spin up N goroutines against the same limiter),
  not just single-threaded happy-path checks.
- Prefer a real (short-lived) `httptest.Server` over mocking `http.Client` by hand.

## 7. Commits

- Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`, `refactor:`).
- One phase from `.agents/roadmap/SKILL.md` = one or more commits, never one giant
  commit spanning multiple phases.

## 8. Definition of done (applies to every phase)

A phase is not done until:
1. `go vet ./...` and `go build ./...` are clean.
2. `go test -race ./...` passes.
3. `gofmt -l .` prints nothing.
4. The phase's exit criteria in `.agents/roadmap/SKILL.md` are checked off.
5. README reflects any new setup step introduced by the phase.
