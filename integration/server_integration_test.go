package integration

// Phase 6: an end-to-end test that boots the *real* HTTP stack — the same
// wiring cmd/server/main.go performs — against a real Postgres. It exercises
// the target endpoints over HTTP, watches the monitor engine persist checks
// into that same database, and confirms /metrics and the /events SSE stream
// reflect the engine's output.
//
// It runs only when TEST_DATABASE_URL is set (see compose.test.yaml and
// `make test-integration`), so the CI `go test -race ./...` job stays green
// without a database. The DSN is intentionally its own variable rather than
// reusing DATABASE_URL: the dotenv DATABASE_URL points at production
// (Neon), and this test truncates its tables — it must never run against
// the real project.

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmer-bell/pulse-monitor/internal/handlers"
	"github.com/programmer-bell/pulse-monitor/internal/metrics"
	"github.com/programmer-bell/pulse-monitor/internal/monitor"
	"github.com/programmer-bell/pulse-monitor/internal/ratelimit"
	"github.com/programmer-bell/pulse-monitor/internal/sse"
	"github.com/programmer-bell/pulse-monitor/internal/store"
	"github.com/programmer-bell/pulse-monitor/migrations"
)

// testServer mirrors main.go's wiring: handlers double as the engine's
// Publisher, the metrics recorder backs /metrics, and the pool runs on a
// tight interval so tests observe engine output quickly.
type testServer struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	store    *store.Store
	metrics  *metrics.Metrics
	stopMon  context.CancelFunc
	monDone  chan struct{}
	upstream *httptest.Server
}

// mustServer boots a fresh full-stack server and throws away the test data
// that any previous run left behind. Each test gets its own database state
// and its own engine, so none of them can interfere.
func mustServer(t *testing.T) *testServer {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — run `make test-db-up && make test-integration`")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		t.Fatalf("ping test postgres: %v", err)
	}

	// This is a dedicated throwaway database: apply the real schema, then
	// start from a clean slate so every run is idempotent.
	if _, err := pool.Exec(ctx, migrations.InitSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE targets CASCADE`); err != nil {
		t.Fatalf("truncate targets: %v", err)
	}

	dbStore := store.New(pool)
	hub := sse.New()
	metricsRecorder := metrics.New()

	h, err := handlers.New(dbStore, hub)
	if err != nil {
		t.Fatalf("init handlers: %v", err)
	}

	// A live upstream for the engine to check — the same httptest warhorse
	// the unit tests use; here it is one more local server.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	// Generous single-domain budget: every test target shares the local
	// httptest host, so the domain limiter must not pace the test. This
	// mirrors the load config, not the default 5 rps profile.
	limiter := ratelimit.NewManager(500)
	t.Cleanup(limiter.Close)

	mon := monitor.New(nil, limiter, dbStore, 64, 2*time.Second, h)
	mon.SetMetrics(metricsRecorder)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("GET /metrics", metricsRecorder.Handler())
	h.Register(mux)

	monCtx, stopMon := context.WithCancel(ctx)
	monDone := make(chan struct{})
	go func() {
		defer close(monDone)
		mon.Run(monCtx, 500*time.Millisecond)
	}()
	t.Cleanup(func() {
		stopMon()
		<-monDone
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &testServer{
		ts:       ts,
		pool:     pool,
		store:    dbStore,
		metrics:  metricsRecorder,
		stopMon:  stopMon,
		monDone:  monDone,
		upstream: upstream,
	}
}

// healthz replicates the entrypoint's handler (it lives in package main and
// is not importable).
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func TestHealthz(t *testing.T) {
	srv := mustServer(t)
	resp := doGet(t, srv.ts.URL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if got := responseBody(t, resp); got != "ok\n" {
		t.Fatalf("GET /healthz body = %q, want %q", got, "ok\n")
	}
}

func TestIndexServesDashboard(t *testing.T) {
	srv := mustServer(t)
	resp := doGet(t, srv.ts.URL+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	body := responseBody(t, resp)
	if !strings.Contains(body, "Pulse Monitor") {
		t.Fatal("dashboard HTML does not contain the page title")
	}
	if !strings.Contains(body, `sse-connect="/events"`) {
		t.Fatal("dashboard HTML does not wire the SSE connection")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv := mustServer(t)
	resp := doGet(t, srv.ts.URL+"/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	body := responseBody(t, resp)
	for _, line := range []string{"checks_total", "checks_in_flight", "checks_failures", "uptime_seconds"} {
		if !strings.Contains(body, line) {
			t.Fatalf("GET /metrics missing %q in:\n%s", line, body)
		}
	}
}

// TestTargetEndpoints walks the full HTTP CRUD path against the real server
// and database: invalid input is rejected, a valid target persists and
// renders, a duplicate is a 409, and deletion removes the row.
func TestTargetEndpoints(t *testing.T) {
	srv := mustServer(t)

	// Invalid URL -> 400, nothing persisted.
	resp := doPost(t, srv.ts.URL+"/targets", "url=javascript:alert(1)")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST invalid = %d, want 400", resp.StatusCode)
	}

	// Valid URL -> 200 and the row appears in GET /targets.
	targetURL := srv.ts.URL + "/crud/1"
	resp = doPost(t, srv.ts.URL+"/targets", "url="+targetURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST valid = %d, want 200", resp.StatusCode)
	}
	rows := responseBody(t, doGet(t, srv.ts.URL+"/targets"))
	if !strings.Contains(rows, targetURL) {
		t.Fatal("GET /targets does not render the just-added target")
	}

	// Duplicate URL -> 409 with an explanatory message.
	resp = doPost(t, srv.ts.URL+"/targets", "url="+targetURL)
	dupBody := responseBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST duplicate = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(dupBody, "already being monitored") {
		t.Fatalf("409 body = %q, want an explanatory conflict message", dupBody)
	}

	// Delete by ID -> 200 and the row disappears.
	id := targetIDFromRow(t, rows, targetURL)
	resp = doDelete(t, srv.ts.URL+"/targets/"+id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", resp.StatusCode)
	}
	if rows = responseBody(t, doGet(t, srv.ts.URL+"/targets")); strings.Contains(rows, targetURL) {
		t.Fatal("GET /targets still renders a deleted target")
	}
}

// TestEnginePersistsToDatabase lets the monitor engine tick against a live
// upstream and asserts the check actually lands in the shared Postgres
// database, and that RecentStats and the /metrics counters move in lockstep.
func TestEnginePersistsToDatabase(t *testing.T) {
	srv := mustServer(t)

	targetURL := srv.upstream.URL + "/engine"
	resp := doPost(t, srv.ts.URL+"/targets", "url="+targetURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST target = %d, want 200", resp.StatusCode)
	}
	id := targetIDFromRow(t, responseBody(t, doGet(t, srv.ts.URL+"/targets")), targetURL)

	// The pool ticks every 500ms starting immediately; poll until at least
	// one check for our target has been recorded through the real store.
	deadline := time.Now().Add(15 * time.Second)
	for {
		stats, err := srv.store.RecentStats(context.Background(), id)
		if err != nil {
			t.Fatalf("RecentStats: %v", err)
		}
		if stats.TotalChecks > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine never persisted a check to the database within 15s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// /metrics must move in lockstep through the real recorder.
	deadline = time.Now().Add(5 * time.Second)
	for {
		vals := parseMetrics(t, doGet(t, srv.ts.URL+"/metrics"))
		if vals["checks_total"] > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("/metrics checks_total never incremented")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestEventsStreamDeliversStats opens a real SSE connection and expects a
// "stats" frame for the freshly added target within a few ticks — the
// end-to-end path from engine -> handlers.PublishStats -> sse.Hub -> HTTP.
func TestEventsStreamDeliversStats(t *testing.T) {
	srv := mustServer(t)

	targetURL := srv.upstream.URL + "/stream"
	resp := doPost(t, srv.ts.URL+"/targets", "url="+targetURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST target = %d, want 200", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, srv.ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("new events request: %v", err)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open /events: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d, want 200", stream.StatusCode)
	}

	scanner := bufio.NewScanner(stream.Body)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		if strings.HasPrefix(scanner.Text(), "event: stats") {
			return
		}
	}
	t.Fatal("no SSE stats event arrived within 10s")
}

// ----- http helpers -----

func doGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func doPost(t *testing.T, url, form string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func doDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(b)
}

// targetIDFromRow extracts the row's id from its <tr id="target-..."> for
// the row that contains targetURL.
func targetIDFromRow(t *testing.T, rows, targetURL string) string {
	t.Helper()
	idx := strings.Index(rows, targetURL)
	if idx < 0 {
		t.Fatalf("row for %q not found in GET /targets", targetURL)
	}
	trStart := strings.LastIndex(rows[:idx], "<tr")
	if trStart < 0 {
		t.Fatalf("no <tr before %q in GET /targets output", targetURL)
	}
	row := rows[trStart:idx]
	const marker = `id="target-`
	pos := strings.Index(row, marker)
	if pos < 0 {
		t.Fatalf("no target id in row fragment %q", row)
	}
	rest := row[pos+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("malformed target id in row fragment %q", row)
	}
	return rest[:end]
}

// parseMetrics returns the /metrics plain-text body as a map of name -> value.
func parseMetrics(t *testing.T, resp *http.Response) map[string]int64 {
	t.Helper()
	vals := map[string]int64{}
	for _, line := range strings.Split(responseBody(t, resp), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		vals[parts[0]] = v
	}
	return vals
}
