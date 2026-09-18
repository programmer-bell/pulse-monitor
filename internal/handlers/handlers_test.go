package handlers

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/programmer-bell/pulse-monitor/internal/monitor"
	"github.com/programmer-bell/pulse-monitor/internal/sse"
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

// TestNewParsesEmbeddedTemplates guards the prod-deploy regression: the
// distroless image contains only the compiled binary, so templates and static
// assets must be resolvable from the embedded FS (web/embed.go) with no web/
// directory present on disk. New() must succeed on a machine without the
// source tree.
func TestNewParsesEmbeddedTemplates(t *testing.T) {
	h, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, name := range []string{"index.html", "target_row", "stats"} {
		if h.tmpl.Lookup(name) == nil {
			t.Fatalf("template %q not parsed from embedded FS", name)
		}
	}
}

// TestStaticServedFromEmbeddedFS verifies /static/* is served from the
// embedded FS, not from disk — same regression as the template parse above.
func TestStaticServedFromEmbeddedFS(t *testing.T) {
	h, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/static/css/style.css", nil)
	rec := httptest.NewRecorder()
	mux := &http.ServeMux{}
	h.Register(mux)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, ":root") {
		t.Fatalf("expected embedded CSS content, got %q", body)
	}
}

// TestPublishCheckBroadcastsUpResult verifies a successful check is rendered
// as an out-of-band status/latency fragment and broadcast on the "check"
// SSE channel with hx-swap-oob targeting.
func TestPublishCheckBroadcastsUpResult(t *testing.T) {
	hub := sse.New()
	h, err := New(nil, hub)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ch := make(chan []byte, 4)
	hub.Subscribe(ch)
	defer hub.Unsubscribe(ch)

	h.PublishCheck(monitor.Result{
		Target:     monitor.Target{ID: "t-1", URL: "https://example.com"},
		StatusCode: http.StatusOK,
		Duration:   150 * time.Millisecond,
		CheckedAt:  time.Now(),
	})

	select {
	case frame := <-ch:
		body := string(frame)
		for _, want := range []string{
			"event: check\n",
			`id="status-t-1"`,
			"hx-swap-oob",
			"status-up",
			`id="latency-t-1"`,
			"150 ms",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("check frame missing %q:\n%s", want, body)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("PublishCheck never broadcast an event")
	}
}

// TestPublishCheckBroadcastsDownResult verifies a failed check renders with
// the down badge and exposes the error in a tooltip.
func TestPublishCheckBroadcastsDownResult(t *testing.T) {
	hub := sse.New()
	h, err := New(nil, hub)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ch := make(chan []byte, 4)
	hub.Subscribe(ch)
	defer hub.Unsubscribe(ch)

	h.PublishCheck(monitor.Result{
		Target: monitor.Target{ID: "t-1", URL: "https://example.com"},
		Err:    errors.New("connection refused"),
	})

	select {
	case frame := <-ch:
		body := string(frame)
		for _, want := range []string{
			"event: check\n",
			"status-down",
			`title="connection refused"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("down check frame missing %q:\n%s", want, body)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("PublishCheck never broadcast an event")
	}
}

// TestPublishStatsBroadcastsAggregates verifies the per-tick stats fragment.
func TestPublishStatsBroadcastsAggregates(t *testing.T) {
	hub := sse.New()
	h, err := New(nil, hub)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ch := make(chan []byte, 4)
	hub.Subscribe(ch)
	defer hub.Unsubscribe(ch)

	h.PublishStats("t-1", 10, 2, 120, 900)

	select {
	case frame := <-ch:
		body := string(frame)
		for _, want := range []string{
			"event: stats\n",
			`id="stats-t-1"`,
			"hx-swap-oob",
			">10<",
			">2<",
			"120 ms",
			"900 ms",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("stats frame missing %q:\n%s", want, body)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("PublishStats never broadcast an event")
	}
}

// TestHandleEventsStreamsPublishedEvents opens a real /events connection and
// verifies a published event reaches the wire. Uses a live listener (not a
// ResponseRecorder) because streaming is only meaningful over an actual
// flushed HTTP response.
//
// http.Server.Serve + Close() is used instead of httptest.Server.Close:
// Close() force-closes active connections and returns without waiting for
// handler goroutines. We can't rely on request-context cancellation to end
// the stream handler — Go's HTTP/1.1 server only notices a client disconnect
// on its next write, which is why HandleEvents sends a heartbeat — and
// waiting on a handler goroutine would make this test hang for the heartbeat
// interval.
func TestHandleEventsStreamsPublishedEvents(t *testing.T) {
	hub := sse.New()
	h, err := New(nil, hub)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	mux := http.NewServeMux()
	h.Register(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })
	url := "http://" + ln.Addr().String() + "/events"

	// DisableKeepAlives so resp.Body.Close() actually closes the TCP
	// connection rather than returning it to the transport's idle pool.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Drain the body in the background so the server can flush freely.
	frames := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		var pending string
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				pending += string(buf[:n])
				// Emit whole frames (boundary \n\n) as they arrive.
				for {
					i := indexByte(pending, '\n', '\n')
					if i < 0 {
						break
					}
					frames <- pending[:i+2]
					pending = pending[i+2:]
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Republish on a ticker: the handler subscribes only after WriteHeader,
	// which is a hair after the client's Do() returns, so a single early
	// publish could be dropped. Repeated publishing makes the test immune
	// to that ordering without a sleep.
	publish := time.NewTicker(50 * time.Millisecond)
	defer publish.Stop()

	timeout := time.After(3 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for a published event on /events")
		case <-publish.C:
			hub.Publish(sse.Event{Name: "check", Data: []byte("hello-stream")})
		case frame := <-frames:
			if !strings.Contains(frame, "event: check") || !strings.Contains(frame, "hello-stream") {
				continue
			}
			resp.Body.Close()
			return
		}
	}
}

// indexByte returns the index of the first occurrence of two consecutive
// bytes b1b2 in s, or -1 if absent.
func indexByte(s string, b1, b2 byte) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == b1 && s[i+1] == b2 {
			return i
		}
	}
	return -1
}
