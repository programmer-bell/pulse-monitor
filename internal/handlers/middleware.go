package handlers

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover wraps next with panic recovery: a panic inside any handler is
// logged with its stack trace and turned into a 500, instead of crashing
// the process and dropping every other in-flight request.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"err", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
