package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"apigateway/internal/errors"
)

// Recovery catches panics and returns an RFC 7807 500 Internal Server Error response.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Request panic recovered",
					"error", err,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
				)
				errors.WriteProblem(
					w,
					http.StatusInternalServerError,
					"Internal Server Error",
					"An unexpected error occurred on the server.",
					r.URL.Path,
				)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
