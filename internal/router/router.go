package router

import (
	"net/http"

	"apigateway/internal/config"
	"apigateway/internal/health"
	"apigateway/internal/middleware"
	"apigateway/internal/proxy"
)

// New creates a new http.Handler containing the API Gateway routing logic,
// wrapping handlers with appropriate route-specific middlewares (Auth, Rate Limiting, Timeout, etc.).
func New(
	cfg *config.Config,
	healthHandler *health.HealthHandler,
	grpcProxy *proxy.GRPCProxy,
	httpProxies map[string]*proxy.HTTPProxy,
	authMW *middleware.AuthMiddleware,
) (http.Handler, error) {
	mux := http.NewServeMux()

	// Register health endpoints
	mux.HandleFunc("GET /healthz", healthHandler.LivenessHandler())
	mux.HandleFunc("GET /readyz", healthHandler.ReadinessHandler())
	mux.HandleFunc("GET /.well-known/jwks.json", authMW.JWKSHandler())

	// Create global rate limiter (using default config parameters)
	globalRateLimiter := middleware.NewRateLimiter(cfg.RateLimit.DefaultRPS, cfg.RateLimit.DefaultBurst)

	// Register other routes
	for _, route := range cfg.Routes {
		var handler http.Handler

		upstreamCfg, exists := cfg.Upstreams[route.Upstream]
		if !exists {
			continue // Checked during config validation, safe to skip
		}

		// 1. Establish the base handler for the destination upstream
		if upstreamCfg.Protocol == "grpc" {
			if route.Upstream == "auth" {
				handler = grpcProxy.HandleAuthRoute(route.Path, route.Method, cfg.Auth.CookieSecure)
			} else {
				// Specific route mapping for gRPC service endpoints
				switch route.Path {
				case "/api/v1/users/{id}":
					handler = http.HandlerFunc(grpcProxy.HandleGetUser)
				case "/api/v1/auth/login":
					handler = http.HandlerFunc(grpcProxy.HandleCreateUser)
				default:
					routeCopy := route
					handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						http.Error(w, "gRPC endpoint not mapped for path: "+routeCopy.Path, http.StatusNotImplemented)
					})
				}
			}
		} else {
			// HTTP proxy forwarding
			if p, ok := httpProxies[route.Upstream]; ok {
				handler = p
			} else {
				routeCopy := route
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "HTTP proxy not initialized for upstream: "+routeCopy.Upstream, http.StatusServiceUnavailable)
				})
			}
		}

		// 2. Chain route-specific middlewares (applied from inside out)

		// Timeout (applied closest to the proxy handler execution)
		timeoutDur := upstreamCfg.Timeout.Duration()
		if timeoutDur > 0 {
			handler = middleware.Timeout(timeoutDur)(handler)
		}

		// Body Size Limits
		if cfg.Server.MaxBodyBytes > 0 {
			handler = middleware.BodyLimit(cfg.Server.MaxBodyBytes)(handler)
		}

		// Rate Limiting (applied before body reading to save resources)
		if route.RateLimit != nil {
			// Specific route rate limits
			routeLimiter := middleware.NewRateLimiter(route.RateLimit.RPS, route.RateLimit.Burst)
			handler = routeLimiter.Limit(handler)
		} else {
			// Global rate limiting
			handler = globalRateLimiter.Limit(handler)
		}

		// Authentication (outermost layer for the specific route handler)
		if route.Auth == "limited" {
			handler = authMW.AuthenticateLimited(handler)
		} else if route.Auth != "none" { // Fail-closed: default to auth required if unspecified or required
			handler = authMW.Authenticate(handler)
		}

		// Register route in Mux
		pattern := route.Method + " " + route.Path
		mux.Handle(pattern, handler)
	}

	return mux, nil
}
