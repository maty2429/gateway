package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytesWritten int64
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		status:         http.StatusOK, // Default status is 200 OK
	}
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += int64(n)
	return n, err
}

// Logging logs HTTP requests structured as JSON via slog.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := newResponseWriter(w)

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		requestID := GetRequestID(r.Context())

		// Extracted user ID (will be populated by auth middleware later)
		var userID string
		if val := r.Context().Value("user_id"); val != nil {
			userID, _ = val.(string)
		}

		slog.Info("Request processed",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes_written", rw.bytesWritten,
			"latency_ms", float64(duration.Microseconds())/1000.0,
			"remote_ip", r.RemoteAddr,
			"user_id", userID,
		)
	})
}
