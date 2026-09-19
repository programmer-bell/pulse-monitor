package handlers

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/programmer-bell/pulse-monitor/internal/monitor"
	"github.com/programmer-bell/pulse-monitor/internal/sse"
	appweb "github.com/programmer-bell/pulse-monitor/web"
)

// Store defines the data access methods required by handlers.
// Following the repo's pattern, this interface is defined at the point of use.
type Store interface {
	CreateTarget(ctx context.Context, url string) (monitor.Target, error)
	ListTargets(ctx context.Context) ([]monitor.Target, error)
	DeleteTarget(ctx context.Context, id string) error
}

// Handlers encapsulates dependencies for HTTP handlers.
type Handlers struct {
	store Store
	tmpl  *template.Template
	hub   *sse.Hub
}

// New constructs a new Handlers instance and parses templates from the
// embedded filesystem (web/embed.go), so the prod binary is fully
// self-contained — no runtime dependency on a web/ directory.
func New(store Store, hub *sse.Hub) (*Handlers, error) {
	tmpl, err := template.ParseFS(
		appweb.FS,
		"templates/index.html",
		"templates/partials/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Handlers{
		store: store,
		tmpl:  tmpl,
		hub:   hub,
	}, nil
}

// Register routes the handlers into the provided mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.HandleIndex)
	mux.HandleFunc("GET /events", h.HandleEvents)
	mux.HandleFunc("GET /targets", h.HandleListTargets)
	mux.HandleFunc("POST /targets", h.HandleCreateTarget)
	mux.HandleFunc("DELETE /targets/{id}", h.HandleDeleteTarget)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}

// staticFS exposes the embedded web/static directory to the file server.
var staticFS = func() fs.FS {
	sub, err := fs.Sub(appweb.FS, "static")
	if err != nil {
		// The embed directive guarantees static exists; a missing subdir here
		// is a programmer error, so panicking at startup is acceptable.
		panic(fmt.Errorf("embed static subdir: %w", err))
	}
	return sub
}()

// HandleIndex serves the main index.html page.
func (h *Handlers) HandleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		slog.Error("render index", "err", err)
	}
}

// HandleListTargets renders the list of targets as HTML rows (partials).
func (h *Handlers) HandleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.store.ListTargets(r.Context())
	if err != nil {
		slog.Error("list targets", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, target := range targets {
		if err := h.tmpl.ExecuteTemplate(w, "target_row", target); err != nil {
			slog.Error("render target_row", "err", err)
			return
		}
	}
}

// HandleCreateTarget handles the form submission to add a new target.
func (h *Handlers) HandleCreateTarget(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	rawURL, err := validateTargetURL(r.FormValue("url"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	target, err := h.store.CreateTarget(r.Context(), rawURL)
	if err != nil {
		slog.Error("create target", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Every open tab appends the new row from the target_added SSE event, so
	// the POST response carries no row HTML — a response body would insert
	// the row a second time in the submitting tab, once via the htmx swap
	// and once via the SSE out-of-band fragment.
	h.PublishTargetAdded(target)
	w.WriteHeader(http.StatusOK)
}

// HandleDeleteTarget removes a target.
func (h *Handlers) HandleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing ID", http.StatusBadRequest)
		return
	}

	if err := h.store.DeleteTarget(r.Context(), id); err != nil {
		slog.Error("delete target", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// The submitting tab already removes its row via the outerHTML swap on
	// the empty 200 response; every other tab removes it from the
	// target_removed SSE event. The submitter may also receive that event —
	// htmx's "delete" out-of-band swap is a no-op when the row is gone.
	h.PublishTargetRemoved(id)

	// Return empty response (200 OK) so htmx removes the element via outerHTML swap.
	w.WriteHeader(http.StatusOK)
}

// CheckStatusView is the view-model for the out-of-band status/latency cells
// rendered from one completed check.
type CheckStatusView struct {
	ID      string
	Status  string // "up" | "down"
	Label   string // "Up" | "Down"
	Latency string
	Error   string
}

// StatsView is the view-model for the out-of-band per-target stats cell.
type StatsView struct {
	ID       string
	Total    int
	Failures int
	P50MS    int
	P99MS    int
}

// handleEvents streams every published SSE event to a browser tab. The
// dashboard page subscribes here (sse-connect="/events") and htmx's SSE
// extension routes each frame — "check" and "stats" — to the out-of-band
// targets they belong to.
func (h *Handlers) HandleEvents(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Push the header block to the wire so the client's EventSource handshake
	// completes immediately instead of waiting for the first frame.
	flusher.Flush()

	ch := make(chan []byte, 32)
	h.hub.Subscribe(ch)
	defer h.hub.Unsubscribe(ch)

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client went away (or is reconnecting): unsubscribe and exit.
			// Unsubscribe removes us from the hub before the channel is
			// garbage collected, so the goroutine has a bounded lifetime.
			return
		case <-heartbeat.C:
			// Comment-only frame keeps the stream alive past idle proxies
			// and surfaces a dead connection at most one heartbeat later.
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// PublishCheck renders one completed check as out-of-band status/latency
// cells and broadcasts them to every open tab on the "check" channel.
// Satisfies monitor.Publisher.
func (h *Handlers) PublishCheck(r monitor.Result) {
	if h.hub == nil {
		return
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "check_status", checkStatusFromResult(r)); err != nil {
		slog.Error("render check_status", "err", err)
		return
	}
	h.hub.Publish(sse.Event{Name: "check", Data: buf.Bytes()})
}

// PublishStats renders one target's per-tick aggregates as an out-of-band
// stats cell and broadcasts it on the "stats" channel. Satisfies
// monitor.Publisher.
func (h *Handlers) PublishStats(targetID string, totalChecks, failures, p50MS, p99MS int) {
	if h.hub == nil {
		return
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "stats", StatsView{
		ID:       targetID,
		Total:    totalChecks,
		Failures: failures,
		P50MS:    p50MS,
		P99MS:    p99MS,
	}); err != nil {
		slog.Error("render stats", "err", err)
		return
	}
	h.hub.Publish(sse.Event{Name: "stats", Data: buf.Bytes()})
}

// PublishTargetAdded wraps a created target's row in a <tbody> carrying an
// out-of-band "beforeend" fragment and broadcasts it on the "target_added"
// channel. Every open tab appends the row to #targets-tbody — not just the
// tab that submitted the create form.
func (h *Handlers) PublishTargetAdded(target monitor.Target) {
	if h.hub == nil {
		return
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "target_added", target); err != nil {
		slog.Error("render target_added", "err", err)
		return
	}
	h.hub.Publish(sse.Event{Name: "target_added", Data: buf.Bytes()})
}

// PublishTargetRemoved broadcasts an out-of-band "delete" fragment for one
// target's row on the "target_removed" channel, so every open tab removes
// the row, not just the tab that clicked Delete.
func (h *Handlers) PublishTargetRemoved(id string) {
	if h.hub == nil {
		return
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "target_removed", struct{ ID string }{ID: id}); err != nil {
		slog.Error("render target_removed", "err", err)
		return
	}
	h.hub.Publish(sse.Event{Name: "target_removed", Data: buf.Bytes()})
}

// checkStatusFromResult maps a raw engine result to the renderable
// view-model: up on a 2xx/3xx, down on anything else (including transport
// errors). Display values are computed here so the template stays free of
// business logic.
func checkStatusFromResult(r monitor.Result) CheckStatusView {
	ok := r.Err == nil && r.StatusCode >= 200 && r.StatusCode < 400
	v := CheckStatusView{ID: r.Target.ID}
	if ok {
		v.Status = "up"
		v.Label = "Up"
	} else {
		v.Status = "down"
		v.Label = "Down"
	}
	if r.Duration > 0 {
		v.Latency = fmt.Sprintf("%d ms", r.Duration.Milliseconds())
	} else {
		v.Latency = "—"
	}
	if r.Err != nil {
		v.Error = r.Err.Error()
	}
	return v
}

func validateTargetURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("URL is required")
	}

	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("URL must include a valid host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("URL scheme must be http or https")
	}
	return rawURL, nil
}
