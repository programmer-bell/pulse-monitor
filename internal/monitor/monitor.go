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
	"log/slog"
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

// Stats are the per-target aggregates published once per tick.
type Stats struct {
	TargetID     string
	TotalChecks  int
	Failures     int
	P50LatencyMS int
	P99LatencyMS int
}

// Publisher receives a rendered event for each completed check and a stats
// event once per tick. Satisfied structurally by *handlers.Handlers.
type Publisher interface {
	PublishCheck(result Result)
	PublishStats(targetID string, totalChecks, failures, p50MS, p99MS int)
}

// MetricsRecorder receives a start/finish event for every check that
// actually executes (after it has acquired a worker slot), so a
// process-wide counter can track in-flight/total/failed for GET /metrics.
// Declared at the point of use, per this repo's interface convention;
// satisfied structurally by *metrics.Metrics.
type MetricsRecorder interface {
	CheckStarted()
	CheckFinished(failed bool)
}

// Target is a single URL to be checked.
type Target struct {
	ID  string
	URL string
}

// Result is the outcome of checking one Target.
type Result struct {
	Target     Target
	StatusCode int
	Duration   time.Duration
	Err        error
	CheckedAt  time.Time
}

// Failed reports whether this Result counts as a failure: a transport-level
// error (including a timeout or an interrupted rate-limit wait), or an HTTP
// response outside the 2xx/3xx range. This is the single definition of
// "down" that both the dashboard (checkStatusFromResult) and the /metrics
// failure counter use.
func (r Result) Failed() bool {
	return r.Err != nil || r.StatusCode < 200 || r.StatusCode >= 400
}

// dbOpTimeout bounds each individual store call made from tick(). tick()
// deliberately does not inherit Run's shutdown context (see Run/tick below),
// so without its own deadline a stuck DB round-trip could block shutdown
// forever; this is the "no unbounded blocking calls" rule from the
// instruction file, applied to the one place that was missing it.
const dbOpTimeout = 5 * time.Second

// Pool checks targets concurrently, bounded by MaxWorkers and by the
// supplied rate limiter.
type Pool struct {
	client       *http.Client
	limiter      *ratelimit.Manager
	store        Store
	publisher    Publisher
	metrics      MetricsRecorder
	checkTimeout time.Duration

	sem chan struct{}
}

// New builds a Pool. See the previous phases' doc comments for client,
// limiter, and publisher semantics — unchanged here.
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

// SetMetrics wires m into the pool so every check reports through it. This
// is a setter rather than a New parameter so existing callers/tests that
// build a Pool without metrics don't need to change — metrics is optional
// the same way publisher and limiter already are.
func (p *Pool) SetMetrics(m MetricsRecorder) {
	p.metrics = m
}

// Run starts a background loop that periodically fetches targets and checks
// them. It blocks until ctx is canceled.
//
// ctx only governs *this loop*: whether to start another tick. It is
// intentionally not threaded into tick() itself — see tick's doc comment.
// Because tick() is called synchronously from this loop, Run can only
// observe ctx.Done() between ticks, never in the middle of one: a tick
// already under way when ctx is canceled always runs to completion before
// Run returns. That's what lets main.go wait on this goroutine and get a
// real "in-flight checks finished" guarantee during shutdown.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.tick()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick runs one round of checks against every target. It deliberately uses
// context.Background() as its root, detached from Run's cancellable ctx: a
// tick already in progress when a shutdown signal arrives must not have its
// in-flight HTTP checks aborted mid-request just because the process is
// stopping. Each individual check is still bounded by p.checkTimeout inside
// checkOne, and every store call gets its own dbOpTimeout, so nothing here
// can actually block forever.
func (p *Pool) tick() {
	if p.store == nil {
		return
	}

	listCtx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	targets, err := p.store.ListTargets(listCtx)
	cancel()
	if err != nil {
		slog.Error("monitor: list targets failed", "err", err)
		return
	}

	results := p.Check(context.Background(), targets)
	for _, res := range results {
		if p.publisher != nil {
			p.publisher.PublishCheck(res)
		}
		var errMsg string
		if res.Err != nil {
			errMsg = res.Err.Error()
		}

		recCtx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		err := p.store.RecordCheck(recCtx, res.Target.ID, res.StatusCode, int(res.Duration.Milliseconds()), errMsg, res.CheckedAt)
		cancel()
		if err != nil {
			slog.Error("monitor: record check failed", "target_id", res.Target.ID, "err", err)
		}
	}

	if p.publisher != nil {
		for _, target := range targets {
			statsCtx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
			stats, err := p.store.RecentStats(statsCtx, target.ID)
			cancel()
			if err != nil {
				slog.Error("monitor: recent stats failed", "target_id", target.ID, "err", err)
				continue
			}
			p.publisher.PublishStats(stats.TargetID, stats.TotalChecks, stats.Failures, stats.P50LatencyMS, stats.P99LatencyMS)
		}
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Check runs one round of checks against every target, fanning out one
// goroutine per target but never letting more than MaxWorkers run their
// actual HTTP call at once.
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

			if p.metrics != nil {
				p.metrics.CheckStarted()
			}
			res := p.checkOne(ctx, target)
			if p.metrics != nil {
				p.metrics.CheckFinished(res.Failed())
			}
			if res.Err != nil {
				slog.Warn("check failed",
					"target_id", target.ID,
					"url", target.URL,
					"err", res.Err,
				)
			}
			results[i] = res
		}(i, target)
	}
	wg.Wait()

	return results
}

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
	_, _ = io.Copy(io.Discard, resp.Body)

	return Result{
		Target:     target,
		StatusCode: resp.StatusCode,
		Duration:   duration,
		CheckedAt:  now,
	}
}

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
