// internal/handlers/middleware_test.go
package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecover_TurnsPanicInto500(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	Recover(panicky).ServeHTTP(rec, httptest.NewRequest("GET", "/whatever", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

func TestRecover_PassesThroughNormalResponses(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	Recover(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/whatever", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("want 418, got %d", rec.Code)
	}
}
