package handlers

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/programmer-bell/pulse-monitor/internal/monitor"
)

type fakeStore struct {
	targets       []monitor.Target
	createErr     error
	listErr       error
	deleteErr     error
	createdURL    string
	deletedTarget string
}

func (s *fakeStore) CreateTarget(_ context.Context, rawURL string) (monitor.Target, error) {
	if s.createErr != nil {
		return monitor.Target{}, s.createErr
	}
	target := monitor.Target{ID: "target-1", URL: rawURL}
	s.createdURL = rawURL
	s.targets = append(s.targets, target)
	return target, nil
}

func (s *fakeStore) ListTargets(context.Context) ([]monitor.Target, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.targets, nil
}

func (s *fakeStore) DeleteTarget(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletedTarget = id
	return nil
}

func testHandlers(store Store) *Handlers {
	return &Handlers{
		store: store,
		tmpl: template.Must(template.New("test").Parse(`
			{{define "index.html"}}index{{end}}
			{{define "target_row"}}<tr id="{{.ID}}">{{.URL}}</tr>{{end}}
		`)),
	}
}

func TestValidateTargetURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "http", input: "http://example.com/health", want: "http://example.com/health"},
		{name: "https trims whitespace", input: " https://example.com ", want: "https://example.com"},
		{name: "missing URL", input: " ", wantErr: true},
		{name: "relative URL", input: "/health", wantErr: true},
		{name: "unsupported scheme", input: "ftp://example.com", wantErr: true},
		{name: "missing host", input: "https:///health", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateTargetURL(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTargetURL() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("validateTargetURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleCreateTarget(t *testing.T) {
	store := &fakeStore{}
	h := testHandlers(store)

	req := httptest.NewRequest(http.MethodPost, "/targets", nil)
	req.Form = make(map[string][]string)
	req.Form.Set("url", " https://example.com ")
	rec := httptest.NewRecorder()

	h.HandleCreateTarget(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if store.createdURL != "https://example.com" {
		t.Fatalf("created URL = %q, want trimmed URL", store.createdURL)
	}
	if rec.Body.String() == "" {
		t.Fatal("expected target row response")
	}
}

func TestHandleCreateTargetRejectsInvalidURL(t *testing.T) {
	store := &fakeStore{}
	h := testHandlers(store)
	req := httptest.NewRequest(http.MethodPost, "/targets", nil)
	req.Form = map[string][]string{"url": {"javascript:alert(1)"}}
	rec := httptest.NewRecorder()

	h.HandleCreateTarget(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if store.createdURL != "" {
		t.Fatal("store should not receive an invalid URL")
	}
}

func TestHandleDeleteTarget(t *testing.T) {
	store := &fakeStore{}
	h := testHandlers(store)
	req := httptest.NewRequest(http.MethodDelete, "/targets/target-1", nil)
	req.SetPathValue("id", "target-1")
	rec := httptest.NewRecorder()

	h.HandleDeleteTarget(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if store.deletedTarget != "target-1" {
		t.Fatalf("deleted target = %q, want target-1", store.deletedTarget)
	}
}

func TestHandleListTargetsReturnsServerError(t *testing.T) {
	h := testHandlers(&fakeStore{listErr: errors.New("database unavailable")})
	req := httptest.NewRequest(http.MethodGet, "/targets", nil)
	rec := httptest.NewRecorder()

	h.HandleListTargets(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
