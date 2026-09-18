package handlers

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/programmer-bell/pulse-monitor/internal/monitor"
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
}

// New constructs a new Handlers instance and parses templates.
func New(store Store) (*Handlers, error) {
	tmpl, err := template.ParseFiles(
		"web/templates/index.html",
		"web/templates/partials/target_row.html",
		"web/templates/partials/stats.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Handlers{
		store: store,
		tmpl:  tmpl,
	}, nil
}

// Register routes the handlers into the provided mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.HandleIndex)
	mux.HandleFunc("GET /targets", h.HandleListTargets)
	mux.HandleFunc("POST /targets", h.HandleCreateTarget)
	mux.HandleFunc("DELETE /targets/{id}", h.HandleDeleteTarget)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
}

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

	// Render the new target row
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "target_row", target); err != nil {
		slog.Error("render target_row", "err", err)
	}
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

	// Return empty response (200 OK) so htmx removes the element via outerHTML swap.
	w.WriteHeader(http.StatusOK)
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
