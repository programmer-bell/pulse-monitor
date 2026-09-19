package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_CountersAndHandler(t *testing.T) {
	m := New()
	m.CheckStarted()
	m.CheckStarted()
	m.CheckFinished(false) // succeeds
	m.CheckFinished(true)  // fails

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{"checks_total 2", "checks_in_flight 0", "checks_failures 1", "uptime_seconds "} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q, got:\n%s", want, body)
		}
	}
}

func TestMetrics_InFlightGauge(t *testing.T) {
	m := New()
	m.CheckStarted()
	m.CheckStarted()
	m.CheckStarted()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "checks_in_flight 3") {
		t.Errorf("expected 3 in-flight, got:\n%s", rec.Body.String())
	}
}
