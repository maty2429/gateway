package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	apierrors "apigateway/internal/errors"
	"golang.org/x/time/rate"
)

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter implements a thread-safe, client-identified Token Bucket rate limiter.
type RateLimiter struct {
	mu           sync.Mutex
	clients      map[string]*clientLimiter
	defaultRPS   rate.Limit
	defaultBurst int
	cleanupDur   time.Duration
}

// NewRateLimiter creates a RateLimiter with specified RPS and burst settings.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		clients:      make(map[string]*clientLimiter),
		defaultRPS:   rate.Limit(rps),
		defaultBurst: burst,
		cleanupDur:   5 * time.Minute,
	}
	
	// Start background cleaning routine to prevent memory leaks from inactive clients
	go rl.cleanupLoop()

	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanupDur)
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for key, cl := range rl.clients {
			// Remove limiters for clients inactive for more than 10 minutes
			if now.Sub(cl.lastSeen) > 10*time.Minute {
				delete(rl.clients, key)
			}
		}
		rl.mu.Unlock()
	}
}

// Limit returns a middleware handler that executes rate limiting before routing.
func (rl *RateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Identify client by user ID (authenticated) or IP (anonymous)
		var key string
		if uVal := r.Context().Value("user_id"); uVal != nil {
			if uStr, ok := uVal.(string); ok && uStr != "" {
				key = "usr:" + uStr
			}
		}
		if key == "" {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			key = "ip:" + ip
		}

		rl.mu.Lock()
		cl, exists := rl.clients[key]
		if !exists {
			cl = &clientLimiter{
				limiter: rate.NewLimiter(rl.defaultRPS, rl.defaultBurst),
			}
			rl.clients[key] = cl
		}
		cl.lastSeen = time.Now()
		rl.mu.Unlock()

		if !cl.limiter.Allow() {
			// Set standard Retry-After header (seconds)
			w.Header().Set("Retry-After", "1")
			apierrors.WriteProblem(
				w,
				http.StatusTooManyRequests,
				"Too Many Requests",
				"API rate limit exceeded. Please try again shortly.",
				r.URL.Path,
			)
			return
		}

		next.ServeHTTP(w, r)
	})
}
