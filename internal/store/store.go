package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmer-bell/pulse-monitor/internal/monitor"
)

// Target represents a monitored URL.
type Target = monitor.Target

// Store wraps a Postgres connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a new Store from a pgxpool.Pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateTarget inserts a new target into the database.
func (s *Store) CreateTarget(ctx context.Context, url string) (Target, error) {
	query := `INSERT INTO targets (url) VALUES ($1) RETURNING id, url`
	var t Target
	err := s.pool.QueryRow(ctx, query, url).Scan(&t.ID, &t.URL)
	if err != nil {
		return Target{}, fmt.Errorf("create target: %w", err)
	}
	return t, nil
}

// ListTargets returns all monitored targets.
func (s *Store) ListTargets(ctx context.Context) ([]Target, error) {
	query := `SELECT id, url FROM targets ORDER BY created_at ASC`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	defer rows.Close()

	var targets []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ID, &t.URL); err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return targets, nil
}

// DeleteTarget removes a target and its associated checks (via CASCADE).
func (s *Store) DeleteTarget(ctx context.Context, id string) error {
	query := `DELETE FROM targets WHERE id = $1`
	_, err := s.pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete target: %w", err)
	}
	return nil
}

// RecordCheck saves the result of a single health check.
func (s *Store) RecordCheck(ctx context.Context, targetID string, statusCode int, durationMs int, errMsg string, checkedAt time.Time) error {
	query := `
		INSERT INTO checks (target_id, status_code, duration_ms, error_message, checked_at)
		VALUES ($1, $2, $3, $4, $5)
	`
	var errStr *string
	if errMsg != "" {
		errStr = &errMsg
	}
	_, err := s.pool.Exec(ctx, query, targetID, statusCode, durationMs, errStr, checkedAt)
	if err != nil {
		return fmt.Errorf("record check: %w", err)
	}
	return nil
}

// RecentStats calculates aggregate statistics for a given target. Returns the
// monitor.Stats view type so *Store satisfies monitor.Store (the monitor
// engine consumes these aggregates and publishes them to the dashboard).
func (s *Store) RecentStats(ctx context.Context, targetID string) (monitor.Stats, error) {
	query := `
		SELECT 
			COUNT(*),
			COUNT(*) FILTER (WHERE error_message IS NOT NULL OR status_code < 200 OR status_code >= 400),
			COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY duration_ms), 0),
			COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY duration_ms), 0)
		FROM checks 
		WHERE target_id = $1
	`
	stats := monitor.Stats{TargetID: targetID}
	var p50, p99 float64
	err := s.pool.QueryRow(ctx, query, targetID).Scan(
		&stats.TotalChecks,
		&stats.Failures,
		&p50,
		&p99,
	)
	if err != nil {
		return monitor.Stats{}, fmt.Errorf("recent stats: %w", err)
	}
	stats.P50LatencyMS = int(p50)
	stats.P99LatencyMS = int(p99)
	return stats, nil
}
