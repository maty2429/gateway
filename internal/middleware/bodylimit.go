package middleware

import "net/http"

// BodyLimit returns a middleware that limits the maximum bytes read from the request body.
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil {
				// Wrap body with http.MaxBytesReader, which will reject reads exceeding the limit.
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
