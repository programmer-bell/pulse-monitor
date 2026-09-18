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

	"github.com/jackc/pgx/v5"
	"github.com/programmer-bell/pulse-monitor/internal/config"
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

	// Establish connection to the PostgreSQL database.
	conn, err := connectDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Ensure the database connection is closed when the application exits.
	defer conn.Close(context.Background())

	// Initialize the HTTP request multiplexer (router).
	mux := http.NewServeMux()
	// Register the health check endpoint.
	mux.HandleFunc("GET /healthz", healthz)

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

// connectDatabase establishes and verifies a connection to the PostgreSQL database.
func connectDatabase(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	// Set a 10-second timeout for the connection attempt.
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	
	// Attempt to connect to the database.
	conn, err := pgx.Connect(connectCtx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	
	// Ping the database to ensure the connection is active and valid.
	if err := conn.Ping(connectCtx); err != nil {
		conn.Close(context.Background())
		return nil, fmt.Errorf("ping database: %w", err)
	}
	
	// Log the successful connection details.
	host, database := databaseLocation(databaseURL)
	slog.Info("db connect successfully", "host", host, "database", database)
	return conn, nil
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
