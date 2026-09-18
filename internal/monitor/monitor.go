// Package monitor is the concurrency core of pulse-monitor: a bounded
// worker pool that checks a batch of URLs in parallel. Two independent
// mechanisms bound the work in flight at any moment:
//
//   - a semaphore (buffered channel), capping total simultaneous checks at
//     Pool.maxWorkers, regardless of how many targets are in the domain;
//   - a ratelimit.Manager, capping requests per second *per domain*, so one
//     slow or high-volume host can't starve every other host's share of the
//     worker pool.
//
// Persistence and pub/sub are injected, not owned: Store records results,
// and Publisher broadcasts a rendered event per check and per tick so the
// dashboard updates in real time over Server-Sent Events (Phase 4). Both are
// satisfied structurally by *store.Store and *handlers.Handlers, which keeps
// this package testable with nothing but an httptest.Server, no real
// network or database involved.
package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/programmer-bell/pulse-monitor/internal/ratelimit"
)

// Store represents the persistence layer for targets and checks.
type Store interface {
	ListTargets(ctx context.Context) ([]Target, error)
	RecordCheck(ctx context.Context, targetID string, statusCode int, durationMs int, errMsg string, checkedAt time.Time) error
	RecentStats(ctx context.Context, targetID string) (Stats, error)
}

// Stats are the per-target aggregates published once per tick. Owned here so
// the engine (not the store package) defines the Publisher contract;
// store.RecentStats returns this type.
type Stats struct {
	TargetID     string
	TotalChecks  int
	Failures     int
	P50LatencyMS int
	P99LatencyMS int
}

// Publisher receives a rendered event for each completed check and a stats
// event once per tick. It is declared at the point of use and satisfied
// structurally by *handlers.Handlers, which renders the events as
// out-of-band HTML fragments and broadcasts them over the SSE hub.
type Publisher interface {
	PublishCheck(result Result)
	PublishStats(targetID string, totalChecks, failures, p50MS, p99MS int)
}

// Target is a single URL to be checked.
type Target struct {
	ID  string
	URL string
}

// Result is the outcome of checking one Target. Err is non-nil for any
// failure to complete the check — a malformed URL, a rate-limit wait that
// was interrupted, a timeout, a connection failure, and so on — and
// StatusCode/Duration are only meaningful when Err is nil.
//
// This intentionally mirrors what Phase 2's store.CheckResult is expected to
// persist; keeping the shape close now means wiring RecordCheck through
// later should be closer to a rename than a redesign.
type Result struct {
	Target     Target
	StatusCode int
	Duration   time.Duration
	Err        error
	CheckedAt  time.Time
}

// Pool checks targets concurrently, bounded by MaxWorkers and by the
// supplied rate limiter. A Pool is safe for concurrent use — in particular,
// Check may be called from multiple goroutines (e.g. a real tick loop and a
// manual "check now" request) simultaneously; MaxWorkers still bounds the
// total in-flight checks across all of them, since the semaphore is created
// once in New and shared by every call to Check.
type Pool struct {
	client       *http.Client
	limiter      *ratelimit.Manager
	store        Store
	publisher    Publisher
	checkTimeout time.Duration

	sem chan struct{}
}

// New builds a Pool. If client is nil, New constructs a default one whose
// Transport raises MaxIdleConnsPerHost — left at net/http's default of 2,
// that setting alone would throttle throughput to any single domain no
// matter how large maxWorkers is, making the rate limiter (not connection
// starvation) the thing actually governing per-domain throughput.
//
// limiter may be nil, which disables per-domain rate limiting entirely
// (every check proceeds as soon as a worker slot is free); this is mainly
// useful for tests and benchmarks that want to isolate the semaphore's
// behavior from the limiter's.
//
// publisher may be nil to disable live events (used by tests that only
// exercise Check directly); when set, the tick loop publishes a result event
// per completed check and a stats event per target per tick.
func New(client *http.Client, limiter *ratelimit.Manager, s Store, maxWorkers int, checkTimeout time.Duration, pub Publisher) *Pool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if client == nil {
		client = defaultHTTPClient()
	}
	return &Pool{
		client:       client,
		limiter:      limiter,
		store:        s,
		publisher:    pub,
		checkTimeout: checkTimeout,
		sem:          make(chan struct{}, maxWorkers),
	}
}

// Run starts a background loop that periodically fetches targets and checks them.
// It blocks until the context is canceled.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Perform an initial check immediately.
	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Pool) tick(ctx context.Context) {
	if p.store == nil {
		return
	}
	targets, err := p.store.ListTargets(ctx)
	if err != nil {
		// Just log or drop error in this context since we don't have a logger injected,
		// but standard practice here is to let it fail or log it.
		// For now we just return.
		return
	}

	results := p.Check(ctx, targets)
	for _, res := range results {
		// Live event first so the dashboard reflects the check as it
		// happens; the recorded history below is what makes it durable.
		if p.publisher != nil {
			p.publisher.PublishCheck(res)
		}
		var errMsg string
		if res.Err != nil {
			errMsg = res.Err.Error()
		}
		_ = p.store.RecordCheck(ctx, res.Target.ID, res.StatusCode, int(res.Duration.Milliseconds()), errMsg, res.CheckedAt)
	}

	// Once the tick's checks are recorded, publish per-target aggregates so
	// the dashboard's stats update once per tick rather than once per check.
	if p.publisher != nil {
		for _, target := range targets {
			stats, err := p.store.RecentStats(ctx, target.ID)
			if err != nil {
				continue
			}
			p.publisher.PublishStats(stats.TargetID, stats.TotalChecks, stats.Failures, stats.P50LatencyMS, stats.P99LatencyMS)
		}
	}
}

// defaultHTTPClient returns an http.Client tuned for many concurrent
// requests against a modest number of distinct hosts. See the New doc
// comment and the README's "Why these choices" section for why
// MaxIdleConnsPerHost specifically is raised here.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		// No client-level Timeout: each request's deadline is set
		// per-check via context in checkOne, derived from the ctx the
		// caller passed to Check so a process shutdown still cancels
		// in-flight requests promptly.
	}
}

// Check runs one round of checks against every target, fanning out one
// goroutine per target but never letting more than MaxWorkers run their
// actual HTTP call at once. It blocks until every target has a Result — a
// canceled ctx does not abort in-flight checks early, it makes any
// goroutine still waiting on a worker slot or a rate-limit token fail fast
// with ctx.Err() instead.
//
// Results are returned in the same order as targets, one per target,
// regardless of completion order.
func (p *Pool) Check(ctx context.Context, targets []Target) []Result {
	results := make([]Result, len(targets))

	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, target := range targets {
		go func(i int, target Target) {
			defer wg.Done()

			select {
			case p.sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{Target: target, Err: ctx.Err(), CheckedAt: time.Now()}
				return
			}
			defer func() { <-p.sem }()

			results[i] = p.checkOne(ctx, target)
		}(i, target)
	}
	wg.Wait()

	return results
}

// checkOne performs a single check: wait for the target's domain to have a
// rate-limit token, then issue a GET request bounded by p.checkTimeout.
func (p *Pool) checkOne(ctx context.Context, target Target) Result {
	now := time.Now()

	domain, err := domainOf(target.URL)
	if err != nil {
		return Result{Target: target, Err: fmt.Errorf("monitor: parse target url: %w", err), CheckedAt: now}
	}

	if p.limiter != nil {
		if err := p.limiter.Wait(ctx, domain); err != nil {
			return Result{Target: target, Err: fmt.Errorf("monitor: rate limit wait: %w", err), CheckedAt: now}
		}
	}

	checkCtx, cancel := context.WithTimeout(ctx, p.checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target.URL, nil)
	if err != nil {
		return Result{Target: target, Err: fmt.Errorf("monitor: build request: %w", err), CheckedAt: now}
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	duration := time.Since(start)
	if err != nil {
		return Result{Target: target, Duration: duration, Err: fmt.Errorf("monitor: request failed: %w", err), CheckedAt: now}
	}
	defer resp.Body.Close()
	// Drain the body so the underlying connection can be reused (matters a
	// lot here: this is exactly the throughput MaxIdleConnsPerHost exists to
	// protect).
	_, _ = io.Copy(io.Discard, resp.Body)

	return Result{
		Target:     target,
		StatusCode: resp.StatusCode,
		Duration:   duration,
		CheckedAt:  now,
	}
}

// domainOf extracts the host (no port) from a target URL, used as the
// ratelimit.Manager key. Two targets on the same host but different ports
// intentionally share a rate-limit bucket — the limit exists to be polite to
// a host, and a host is identified by its name, not the port a particular
// service happens to listen on.
func domainOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("no host in url %q", rawURL)
	}
	return u.Hostname(), nil
}
