package main

// This is the entry point for the pulse-monitor application.
// It initializes the configuration, logger, and database connection,
// and starts the HTTP server.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmer-bell/pulse-monitor/internal/config"
	"github.com/programmer-bell/pulse-monitor/internal/handlers"
	"github.com/programmer-bell/pulse-monitor/internal/monitor"
	"github.com/programmer-bell/pulse-monitor/internal/ratelimit"
	"github.com/programmer-bell/pulse-monitor/internal/sse"
	"github.com/programmer-bell/pulse-monitor/internal/store"
	"github.com/programmer-bell/pulse-monitor/migrations"
)

// main is the application entry point. It calls run() and handles any top-level errors.
func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run orchestrates the startup and graceful shutdown of the server.
func run() error {
	// Initialize structured logging to stdout.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Load application configuration from environment variables or .env file.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Set up context that listens for interruption signals (SIGINT, SIGTERM) for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Establish connection pool to the PostgreSQL database.
	pool, err := connectDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := runMigrations(ctx, pool); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}

	dbStore := store.New(pool)
	limiter := ratelimit.NewManager(cfg.DomainRPS)
	hub := sse.New()

	// Handlers double as the monitor's Publisher (rendering SSE events to
	// out-of-band HTML), so they must exist before the engine starts ticking.
	h, err := handlers.New(dbStore, hub)
	if err != nil {
		return fmt.Errorf("init handlers: %w", err)
	}

	mon := monitor.New(nil, limiter, dbStore, cfg.MaxWorkers, cfg.CheckTimeout, h)

	// Start monitor loop
	go mon.Run(ctx, cfg.CheckInterval)

	// Initialize the HTTP request multiplexer (router).
	mux := http.NewServeMux()
	// Register the health check endpoint.
	mux.HandleFunc("GET /healthz", healthz)

	h.Register(mux)

	// Configure the HTTP server with port and timeouts.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Start the HTTP server in a separate goroutine to avoid blocking.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	// Wait for either a server error or a shutdown signal.
	select {
	case err := <-errCh:
		// If the server fails to start or stops unexpectedly.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Interruption signal received, proceed to gracefully shutdown.
		slog.Info("shutdown signal received, draining connections")
	}

	// Give the server up to 10 seconds to finish processing ongoing requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}

	// Check if the server had any errors during its shutdown process.
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	slog.Info("server stopped cleanly")
	return nil
}

// connectDatabase establishes and verifies a connection pool to the PostgreSQL database.
func connectDatabase(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	// Set a 10-second timeout for the connection attempt.
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Attempt to connect to the database.
	pool, err := pgxpool.New(connectCtx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}

	// Ping the database to ensure the connection is active and valid.
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Log the successful connection details.
	host, database := databaseLocation(databaseURL)
	slog.Info("db connect successfully", "host", host, "database", database)
	return pool, nil
}

// runMigrations applies the database schema if it doesn't already exist.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, migrations.InitSQL)
	return err
}

// databaseLocation parses the database URL to extract the host and database name for logging.
func databaseLocation(databaseURL string) (string, string) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", ""
	}
	// The database name is typically the path without the leading slash.
	return u.Hostname(), strings.TrimPrefix(u.Path, "/")
}

// healthz is a simple HTTP handler that responds with "ok" to indicate the server is running.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
