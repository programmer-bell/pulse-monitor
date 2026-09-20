// Command loadtest drives a Phase 6 load run against the running instance.
//
// Compose.load.yaml runs two containers: the prod app (pointed at the real
// DATABASE_URL, .env -> Neon) and a throwaway internal "target" server.
// This tool, run from the host, seeds N monitored targets whose URLs hit
// that internal server, samples GET /metrics through the whole run to
// compute sustained checks/sec, then pulls the p50/p99 check latencies
// straight from the checks table the engine just filled. It deletes its own
// marked rows afterwards (prefixed URLs only) — the dashboard's real targets
// are untouched.
//
// Usage (with the stack up):
//
//	go run ./cmd/loadtest -app http://localhost:8080 -targets 500 -duration 180s
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmer-bell/pulse-monitor/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("loadtest", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// config.Load reads DATABASE_URL from the environment or .env (via
	// internal/config, the only package allowed to read env vars); it is
	// never logged below.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	appURL := flag.String("app", "http://localhost:8080", "base URL of the running app (--app=http://localhost:8080)")
	targetsN := flag.Int("targets", 500, "number of monitored targets to seed")
	duration := flag.Duration("duration", 180*time.Second, "how long to sample the engine")
	sampleEvery := flag.Duration("sample", 5*time.Second, "how often to read /metrics")
	targetHost := flag.String("target-host", "target", "hostname of the internal target server, as the app resolves it (compose service name)")
	targetPort := flag.String("target-port", "8099", "port of the internal target server")
	flag.Parse()

	prefix := fmt.Sprintf("http://%s:%s/u/loadtest-", *targetHost, *targetPort)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database (check DATABASE_URL): %w", err)
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping database (check DATABASE_URL): %w", err)
	}

	// Idempotent start: clear rows a previous (possibly interrupted) load
	// run left behind, so totals reflect this run alone.
	if _, err := pool.Exec(ctx, `DELETE FROM targets WHERE url LIKE $1`, prefix+"%"); err != nil {
		return fmt.Errorf("clear stale loadtest rows: %w", err)
	}

	urls := make([]string, *targetsN)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO targets (url)
		SELECT unnest($1::text[])`, urls); err != nil {
		return fmt.Errorf("seed %d targets: %w", *targetsN, err)
	}
	slog.Info("seeded targets",
		"count", *targetsN,
		"target_host", *targetHost,
		"sample_duration", duration.String())

	// Wait for the first recorded check so the window we sample is a steady
	// one, then start from that baseline.
	baseline, err := waitForChecks(ctx, http.DefaultClient, *appURL, *sampleEvery)
	if err != nil {
		return err
	}

	samples := sampleMetrics(http.DefaultClient, *appURL, *duration, *sampleEvery)
	if len(samples) == 0 {
		return fmt.Errorf("no /metrics samples collected from %s", *appURL)
	}

	totalDelta := samples[len(samples)-1].total - baseline
	if totalDelta <= 0 {
		return fmt.Errorf("checks_total did not advance across the run (engine not ticking? check_interval too large for %s?)", duration)
	}
	window := samples[len(samples)-1].at.Sub(samples[0].at)
	sustained := float64(totalDelta) / window.Seconds()

	var burstMin, burstMax, burstSum float64
	burstMin = math.MaxFloat64
	for _, s := range samples {
		if s.rate < burstMin {
			burstMin = s.rate
		}
		if s.rate > burstMax {
			burstMax = s.rate
		}
		burstSum += s.rate
	}

	// Latency percentiles from the checks the engine just recorded for our
	// marked targets — the same query shape store.RecentStats uses, but
	// widened to the whole run.
	var totalChecks, failures int64
	var p50, p99 float64
	err = pool.QueryRow(ctx, `
		SELECT
			COUNT(*)::bigint,
			COUNT(*) FILTER (WHERE error_message IS NOT NULL OR status_code < 200 OR status_code >= 400)::bigint,
			COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY duration_ms), 0),
			COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY duration_ms), 0)
		FROM checks c
		JOIN targets t ON t.id = c.target_id
		WHERE t.url LIKE $1 AND c.checked_at >= now() - interval '2 hours'`,
		prefix+"%").Scan(&totalChecks, &failures, &p50, &p99)
	if err != nil {
		return fmt.Errorf("query check latencies: %w", err)
	}

	fmt.Println()
	fmt.Println("pulse-monitor load report")
	fmt.Println("-------------------------")
	fmt.Printf("targets monitored      %d\n", *targetsN)
	fmt.Printf("sampling window        %s\n", window.Round(time.Second))
	fmt.Printf("checks exercised       %d\n", totalDelta)
	fmt.Printf("sustained checks/sec   %.1f\n", sustained)
	fmt.Printf("burst checks/sec       %.0f (min %.0f, avg %.1f)\n", burstMax, burstMin, burstSum/float64(len(samples)))
	fmt.Printf("p50 check latency      %.1f ms\n", p50)
	fmt.Printf("p99 check latency      %.1f ms\n", p99)
	fmt.Printf("failed checks          %d (%.1f%%)\n", failures, pct(failures, totalChecks))
	fmt.Println()
	fmt.Println("memory footprint: sample alongside e.g.")
	fmt.Println("  docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' \\")
	fmt.Println("    $(docker compose -f compose.load.yaml ps -q app)")
	fmt.Println("(the app container's MemUsage line at this target count is the number to record)")
	fmt.Println()

	// Leave the dataset clean: only our prefixed rows are removed, the
	// dashboard's own targets survive.
	if _, err := pool.Exec(ctx, `DELETE FROM targets WHERE url LIKE $1`, prefix+"%"); err != nil {
		return fmt.Errorf("cleanup loadtest rows: %w", err)
	}
	slog.Info("cleaned up loadtest targets")
	return nil
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

func waitForChecks(ctx context.Context, client *http.Client, appURL string, sampleEvery time.Duration) (int64, error) {
	deadline := time.Now().Add(60 * time.Second)
	ticker := time.NewTicker(sampleEvery / 2)
	defer ticker.Stop()
	for {
		total, err := readMetric(client, appURL, "checks_total")
		if err == nil && total > 0 {
			return total, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no checks recorded within 60s (is the app up at %s and ticking?)", appURL)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

type sample struct {
	at    time.Time
	total int64
	rate  float64
}

// sampleMetrics polls /metrics every sampleEvery across the duration and
// returns one sample per tick plus the per-interval checks/sec rate.
func sampleMetrics(client *http.Client, appURL string, duration, every time.Duration) []sample {
	var samples []sample
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	deadline := time.Now().Add(duration)
	var prev int64
	var prevAt time.Time
	for {
		total, err := readMetric(client, appURL, "checks_total")
		if err != nil {
			slog.Warn("metrics read failed", "err", err)
		} else {
			now := time.Now()
			s := sample{at: now, total: total}
			if !prevAt.IsZero() {
				s.rate = float64(total-prev) / now.Sub(prevAt).Seconds()
				if s.rate < 0 {
					s.rate = 0 // counter reset would mean the app restarted
				}
			}
			samples = append(samples, s)
			prev, prevAt = total, now
		}
		if !time.Now().Before(deadline) {
			return samples
		}
		<-ticker.C
	}
}

// readMetric returns one named numeric line from GET /metrics.
func readMetric(client *http.Client, appURL, name string) (int64, error) {
	resp, err := client.Get(appURL + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET /metrics: status %d", resp.StatusCode)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%q not found in /metrics", name)
}
