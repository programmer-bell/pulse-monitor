// Command targetsrv is a throwaway deterministic upstream for the Phase 6
// load test. compose.load.yaml builds and runs it as a second container on
// the same network; the load-test topology points every monitored target at
// it (http://target:8099/u/<i>), so a load run exercises the engine against
// a fast, local, always-up server instead of hammering public sites for
// numbers. Not part of the product — it exists only so the load test is
// reproducible and polite.
package main

import (
	"flag"
	"log/slog"
	"net/http"
)

func main() {
	addr := flag.String("addr", ":8099", "listen address")
	flag.Parse()

	slog.Info("target server listening", "addr", *addr)
	srv := &http.Server{
		Addr: *addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("target server stopped", "err", err)
	}
}
