package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/programmer-bell/pulse-monitor/internal/ratelimit"
)

// TestMonitor_BoundsConcurrency is the test the README already advertises:
// against a real httptest.Server, assert the semaphore actually caps the
// number of simultaneous in-flight requests at MaxWorkers, even when far
// more targets than that are checked at once.
//
// The limiter is nil on purpose. Every target here hits the same host, and
// a real per-domain limiter would *also* cap concurrency — which would make
// it impossible to tell whether the semaphore specifically is doing its
// job. This test isolates that one mechanism; TestMonitor_UsesRateLimiterPerDomain
// below covers the limiter.
func TestMonitor_BoundsConcurrency(t *testing.T) {
	const (
		maxWorkers  = 4
		numTargets  = 20
		handlerWait = 40 * time.Millisecond
	)

	var inFlight, maxSeen int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)

		for {
			seen := atomic.LoadInt32(&maxSeen)
			if cur <= seen || atomic.CompareAndSwapInt32(&maxSeen, seen, cur) {
				break
			}
		}

		time.Sleep(handlerWait)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	targets := make([]Target, numTargets)
	for i := range targets {
		targets[i] = Target{URL: srv.URL}
	}

	pool := New(nil, nil, nil, maxWorkers, time.Second, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := pool.Check(ctx, targets)

	if len(results) != numTargets {
		t.Fatalf("got %d results, want %d", len(results), numTargets)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d]: unexpected error: %v", i, r.Err)
		}
		if r.StatusCode != http.StatusOK {
			t.Fatalf("results[%d]: status = %d, want 200", i, r.StatusCode)
		}
	}

	got := atomic.LoadInt32(&maxSeen)
	if got > maxWorkers {
		t.Fatalf("observed %d concurrent in-flight requests, want at most %d (maxWorkers)", got, maxWorkers)
	}
	if got < maxWorkers {
		// numTargets is far larger than maxWorkers and the handler blocks
		// for handlerWait, so the pool should reliably saturate. If it
		// didn't, this test isn't actually exercising the bound it claims to.
		t.Fatalf("observed only %d concurrent in-flight requests, want exactly %d (pool never saturated)", got, maxWorkers)
	}
}

// TestMonitor_PerTargetTimeout verifies a slow target is cut off at
// checkTimeout rather than being allowed to block a worker indefinitely.
func TestMonitor_PerTargetTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := New(nil, nil, nil, 2, 20*time.Millisecond, nil)

	results := pool.Check(context.Background(), []Target{{URL: srv.URL}})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected a timeout error for a target slower than checkTimeout, got nil")
	}
}

// TestMonitor_UsesRateLimiterPerDomain verifies the Pool actually consults
// the supplied ratelimit.Manager before each request, not just stores it
// unused. It compares against the limiter's own known rate rather than
// against a separate unlimited run, so it can't flake on a slow machine —
// only on a limiter that isn't being enforced at all.
func TestMonitor_UsesRateLimiterPerDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const (
		n    = 6
		rate = 10 // one token every 100ms
	)
	targets := make([]Target, n)
	for i := range targets {
		targets[i] = Target{URL: srv.URL}
	}

	limiter := ratelimit.NewManager(rate)
	defer limiter.Close()

	// maxWorkers == n so the semaphore never makes anything queue; any
	// slowdown we see below is attributable to the limiter alone.
	pool := New(nil, limiter, nil, n, time.Second, nil)

	start := time.Now()
	results := pool.Check(context.Background(), targets)
	elapsed := time.Since(start)

	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d]: unexpected error: %v", i, r.Err)
		}
	}

	// n tokens from an empty bucket refilling one every 100ms takes at
	// least (n-1)*100ms. Half that floor leaves headroom for scheduler
	// jitter while still catching a limiter that's constructed but never
	// actually consulted (which would finish in well under a millisecond).
	minExpected := time.Duration(n-1) * 100 * time.Millisecond / 2
	if elapsed < minExpected {
		t.Fatalf("checked %d targets on a rate-limited domain in %s, want at least %s — limiter does not appear to be enforced", n, elapsed, minExpected)
	}
}

// TestMonitor_BoundRateLimitWait verifies a slow per-domain limiter cannot
// stall a worker past the per-check timeout. With an empty bucket refilling
// once per second and a 100ms checkTimeout, every waiter must give up with a
// rate-limit error promptly instead of blocking until a token appears.
// Without the timeout on limiter.Wait this test would hang the pool for
// seconds and only finish because the limiter eventually refills.
func TestMonitor_BoundRateLimitWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	limiter := ratelimit.NewManager(1) // one token/sec; bucket starts empty
	defer limiter.Close()

	pool := New(nil, limiter, nil, 8, 100*time.Millisecond, nil)

	targets := []Target{
		{URL: srv.URL},
		{URL: srv.URL},
		{URL: srv.URL},
	}

	start := time.Now()
	results := pool.Check(context.Background(), targets)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("check took %s, want bounded by checkTimeout (100ms) — limiter wait is not time-boxed", elapsed)
	}
	for i, r := range results {
		if r.Err == nil {
			t.Fatalf("results[%d] succeeded; expected a rate-limit timeout with no token within checkTimeout", i)
		}
		if !strings.Contains(r.Err.Error(), "rate limit wait") {
			t.Fatalf("results[%d].Err = %v, want a rate-limit wait error", i, r.Err)
		}
	}
}

// TestMonitor_MalformedURL verifies a bad target produces an error Result
// instead of a panic or a dropped result.
func TestMonitor_MalformedURL(t *testing.T) {
	pool := New(nil, nil, nil, 2, time.Second, nil)

	results := pool.Check(context.Background(), []Target{{URL: "://not-a-valid-url"}})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected an error for a malformed URL, got nil")
	}
}

// TestMonitor_ResultsPreserveOrder verifies results[i] always corresponds to
// targets[i], regardless of which goroutine finishes first.
func TestMonitor_ResultsPreserveOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	targets := []Target{
		{URL: srv.URL + "/a"},
		{URL: srv.URL + "/b"},
		{URL: srv.URL + "/c"},
		{URL: srv.URL + "/d"},
	}

	pool := New(nil, nil, nil, 8, time.Second, nil)
	results := pool.Check(context.Background(), targets)

	if len(results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(results), len(targets))
	}
	for i, r := range results {
		if r.Target.URL != targets[i].URL {
			t.Fatalf("results[%d].Target.URL = %q, want %q", i, r.Target.URL, targets[i].URL)
		}
	}
}

// TestMonitor_CanceledContextFailsFast verifies targets queued behind an
// already-canceled context don't block waiting for a worker slot — they
// fail immediately with the context's error.
func TestMonitor_CanceledContextFailsFast(t *testing.T) {
	pool := New(nil, nil, nil, 1, time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := pool.Check(ctx, []Target{{URL: "http://example.invalid"}, {URL: "http://example.invalid"}})
	for i, r := range results {
		if r.Err == nil {
			t.Fatalf("results[%d]: expected an error for an already-canceled context, got nil", i)
		}
	}
}

// fakeStore implements Store for the publisher test: it hands back a fixed
// target set and returns canned stats.
type fakeStore struct {
	targets []Target
	stats   Stats
	checks  int
}

func (s *fakeStore) ListTargets(context.Context) ([]Target, error) { return s.targets, nil }
func (s *fakeStore) RecordCheck(context.Context, string, int, int, string, time.Time) error {
	s.checks++
	return nil
}
func (s *fakeStore) RecentStats(_ context.Context, targetID string) (Stats, error) {
	stats := s.stats
	stats.TargetID = targetID
	return stats, nil
}

// fakePublisher implements Publisher, recording every published event so the
// test can assert on what the engine actually broadcasts.
type fakePublisher struct {
	mu     sync.Mutex
	checks []Result
	stats  []Stats
}

func (p *fakePublisher) PublishCheck(r Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, r)
}

func (p *fakePublisher) PublishStats(targetID string, totalChecks, failures, p50MS, p99MS int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats = append(p.stats, Stats{
		TargetID:     targetID,
		TotalChecks:  totalChecks,
		Failures:     failures,
		P50LatencyMS: p50MS,
		P99LatencyMS: p99MS,
	})
}

// TestMonitor_TickPublishesCheckAndStats verifies Phase 4's contract: one
// result event per completed check and one stats event per target per tick,
// in the order the engine produces them.
func TestMonitor_TickPublishesCheckAndStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A closed server makes the second check fail at the transport layer,
	// producing a Result with a non-nil Err (HTTP status codes don't make
	// checkOne return an error — up/down classification is the renderer's job).
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	good := Target{ID: "t-1", URL: srv.URL}
	bad := Target{ID: "t-2", URL: deadURL}

	store := &fakeStore{targets: []Target{good, bad}}
	store.stats = Stats{TotalChecks: 7, Failures: 1, P50LatencyMS: 120, P99LatencyMS: 900}
	pub := &fakePublisher{}

	pool := New(nil, nil, store, 4, time.Second, pub)
	pool.tick()

	pub.mu.Lock()
	defer pub.mu.Unlock()

	if len(pub.checks) != 2 {
		t.Fatalf("published %d check events, want 2 (one per target)", len(pub.checks))
	}
	if got := pub.checks[0].Target.ID; got != "t-1" {
		t.Fatalf("first check event target = %q, want t-1", got)
	}
	if pub.checks[0].StatusCode != http.StatusOK {
		t.Fatalf("first check status = %d, want 200", pub.checks[0].StatusCode)
	}
	if pub.checks[1].Err == nil {
		t.Fatal("second check should carry an error result for a 503 response")
	}

	if len(pub.stats) != 2 {
		t.Fatalf("published %d stats events, want 2 (one per target per tick)", len(pub.stats))
	}
	want := Stats{TargetID: "t-1", TotalChecks: 7, Failures: 1, P50LatencyMS: 120, P99LatencyMS: 900}
	if got := pub.stats[0]; got != want {
		t.Fatalf("stats[0] = %+v, want %+v", got, want)
	}
	if store.checks != 2 {
		t.Fatalf("recorded %d checks, want 2", store.checks)
	}
}

// BenchmarkPool_Check reports a first-draft checks/sec number against a
// local httptest.Server — see .agents/roadmap/SKILL.md Phase 1's exit
// criteria and Phase 6, which replaces this with a real load test against a
// deployed instance. Run with:
//
//	go test ./internal/monitor/... -run '^$' -bench BenchmarkPool_Check -benchtime 2s
func BenchmarkPool_Check(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const batch = 200
	targets := make([]Target, batch)
	for i := range targets {
		targets[i] = Target{URL: srv.URL}
	}

	pool := New(nil, nil, nil, 64, time.Second, nil)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Check(ctx, targets)
	}
	b.StopTimer()

	b.ReportMetric(float64(batch)*float64(b.N)/b.Elapsed().Seconds(), "checks/sec")
}
