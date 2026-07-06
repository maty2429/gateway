package middleware

import "net/http"

// SecurityHeaders injects standard industry security headers into all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent framing of page
		w.Header().Set("X-Frame-Options", "DENY")
		// Prevent content-type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// XSS protection (for older browsers)
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		// HSTS (HTTP Strict Transport Security) - 1 year
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		// CSP (Content Security Policy) safe defaults for API
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		// Referrer policy
		w.Header().Set("Referrer-Policy", "no-referrer-when-downgrade")
		
		// Remove server metadata header to avoid revealing system details
		w.Header().Del("Server")

		next.ServeHTTP(w, r)
	})
}
