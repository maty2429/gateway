package health

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Checker defines a health check function.
type Checker func(ctx context.Context) error

type HealthHandler struct {
	mu        sync.RWMutex
	readiness map[string]Checker
}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{
		readiness: make(map[string]Checker),
	}
}

// RegisterReadinessCheck registers a readiness check.
func (h *HealthHandler) RegisterReadinessCheck(name string, check Checker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readiness[name] = check
}

// LivenessHandler handles liveness checks (/healthz).
func (h *HealthHandler) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	}
}

// ReadinessHandler handles readiness checks (/readyz).
func (h *HealthHandler) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		checks := make(map[string]Checker, len(h.readiness))
		for k, v := range h.readiness {
			checks[k] = v
		}
		h.mu.RUnlock()

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		status := http.StatusOK
		results := make(map[string]string)
		var wg sync.WaitGroup
		var mu sync.Mutex

		for name, check := range checks {
			wg.Add(1)
			go func(n string, c Checker) {
				defer wg.Done()
				err := c(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					status = http.StatusServiceUnavailable
					results[n] = "DOWN: " + err.Error()
				} else {
					results[n] = "UP"
				}
			}(name, check)
		}

		wg.Wait()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		
		// Simple custom serialization to avoid pulling in external map structure details
		resp := `{"status":"`
		if status == http.StatusOK {
			resp += "UP"
		} else {
			resp += "DOWN"
		}
		resp += `"`
		if len(results) > 0 {
			resp += `,"checks":{`
			first := true
			for k, v := range results {
				if !first {
					resp += ","
				}
				resp += `"` + k + `":"` + v + `"`
				first = false
			}
			resp += `}`
		}
		resp += `}`
		_, _ = w.Write([]byte(resp))
	}
}
