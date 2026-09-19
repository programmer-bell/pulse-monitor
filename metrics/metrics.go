// Package metrics exposes a minimal set of process-wide counters behind
// GET /metrics, as plain text. At this scale — a handful of gauges/counters
// for one operator to eyeball — a hand-rolled atomic.Int64 struct is more
// honest than pulling in a Prometheus client for four numbers.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics holds process counters. Every field is an atomic, independently
// updated — no mutex needed, and none of the counters need to be read
// together atomically with each other.
type Metrics struct {
	checksTotal    atomic.Int64
	checksInFlight atomic.Int64
	checksFailed   atomic.Int64
	startedAt      time.Time
}

// New returns a Metrics with its uptime clock started now.
func New() *Metrics {
	return &Metrics{startedAt: time.Now()}
}

// CheckStarted records that a check has begun executing — i.e. it has
// acquired a worker slot and is about to make the request. Pair every call
// with exactly one CheckFinished.
func (m *Metrics) CheckStarted() {
	m.checksInFlight.Add(1)
}

// CheckFinished records a check's completion: decrements in-flight,
// increments the total, and increments the failure counter when failed is
// true. "failed" should use the same definition as monitor.Result.Failed().
func (m *Metrics) CheckFinished(failed bool) {
	m.checksInFlight.Add(-1)
	m.checksTotal.Add(1)
	if failed {
		m.checksFailed.Add(1)
	}
}

// Handler returns an http.Handler serving the current counters as plain
// text, one "name value" pair per line.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "checks_total %d\n", m.checksTotal.Load())
		fmt.Fprintf(w, "checks_in_flight %d\n", m.checksInFlight.Load())
		fmt.Fprintf(w, "checks_failures %d\n", m.checksFailed.Load())
		fmt.Fprintf(w, "uptime_seconds %d\n", int64(time.Since(m.startedAt).Seconds()))
	})
}
