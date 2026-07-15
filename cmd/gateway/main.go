package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"apigateway/internal/config"
	"apigateway/internal/health"
	"apigateway/internal/middleware"
	"apigateway/internal/observability"
	"apigateway/internal/proxy"
	"apigateway/internal/router"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", "configs/gateway.yaml", "Path to configuration file")
	flag.Parse()

	// Human-friendly logs locally; structured JSON remains enabled in production.
	slog.SetDefault(observability.NewLogger("gateway", "development"))

	slog.Info("Starting API Gateway...")

	// 1. Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(observability.NewLogger("gateway", cfg.Server.Environment))
	slog.Info("Configuration loaded successfully", "addr", cfg.Server.Addr)

	// 2. Connect to gRPC upstreams, including auth.
	grpcProxy, err := proxy.NewGRPCProxyWithConfig(cfg.Upstreams)
	if err != nil {
		slog.Error("Failed to initialize gRPC upstreams", "error", err)
		os.Exit(1)
	}
	defer grpcProxy.Close()

	// 3. Bootstrap JWT validation from auth's JWKS. The gateway never creates
	// or receives auth's private signing key.
	jwksCtx, jwksCancel := context.WithTimeout(context.Background(), cfg.Upstreams["auth"].Timeout.Duration())
	jwks, err := grpcProxy.FetchJWKS(jwksCtx)
	jwksCancel()
	if err != nil {
		slog.Error("Failed to load JWKS from auth", "error", err)
		os.Exit(1)
	}
	audiences := cfg.Auth.JWTAudiences
	if len(audiences) == 0 && cfg.Auth.JWTAudience != "" {
		audiences = []string{cfg.Auth.JWTAudience}
	}
	authMW, err := middleware.NewAuthMiddlewareFromJWKS(jwks, cfg.Auth.JWTIssuer, audiences)
	if err != nil {
		slog.Error("Auth returned an invalid JWKS", "error", err)
		os.Exit(1)
	}
	jwksStop := startJWKSRefresh(grpcProxy, authMW, cfg.Auth.JWKSRefresh.Duration())
	defer close(jwksStop)

	// 4. Initialize HTTP reverse proxies
	httpProxies := make(map[string]*proxy.HTTPProxy)
	for name, uCfg := range cfg.Upstreams {
		if uCfg.Protocol == "http" {
			p, err := proxy.NewHTTPProxy(uCfg.Address, uCfg.Timeout.Duration())
			if err != nil {
				slog.Error("Failed to initialize HTTP upstream", "upstream", name, "error", err)
				os.Exit(1)
			}
			httpProxies[name] = p
		}
	}

	// 5. Initialize Health & Readiness
	healthHandler := health.NewHealthHandler()
	healthHandler.RegisterReadinessCheck("gateway_self", func(ctx context.Context) error {
		return nil
	})

	// Add dynamic readiness checks using the standard gRPC health protocol.
	for name, client := range grpcProxy.HealthClients {
		clientCopy := client
		nameCopy := name
		healthHandler.RegisterReadinessCheck("grpc_"+nameCopy, func(ctx context.Context) error {
			response, err := clientCopy.Check(ctx, &healthpb.HealthCheckRequest{})
			if err != nil {
				return err
			}
			if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				return errors.New("upstream is not serving")
			}
			return nil
		})
	}

	// 6. Build final Router Handler
	handler, err := router.New(cfg, healthHandler, grpcProxy, httpProxies, authMW)
	if err != nil {
		slog.Error("Failed to build routing table", "error", err)
		os.Exit(1)
	}

	// Wrap routing tree inside global middleware chain
	globalHandler := middleware.Chain(handler,
		middleware.Recovery,
		middleware.RequestID,
		middleware.Metrics,
		middleware.Logging,
		middleware.SecurityHeaders,
		middleware.CORSWithOrigins(cfg.CORS.AllowedOrigins),
	)

	// Configure HTTP server
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           globalHandler,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		WriteTimeout:      cfg.Server.WriteTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
	}

	// Optional secure TLS configuration
	var isTLS bool
	if cfg.Server.TLS != nil && cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
		isTLS = true
		srv.TLSConfig = &tls.Config{
			MinVersion:       tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{tls.CurveP256, tls.X25519},
		}
	}

	// Create and start the Metrics Server on a separate internal port (:8090)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:         ":8090",
		Handler:      metricsMux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	serverErrors := make(chan error, 1)

	// Start API Gateway Server
	go func() {
		if isTLS {
			slog.Info("API Gateway server listening securely (HTTPS)", "addr", srv.Addr)
			if err := srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- err
			}
		} else {
			slog.Info("API Gateway server listening (HTTP)", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- err
			}
		}
	}()

	// Start Metrics Server
	go func() {
		slog.Info("Metrics server listening internally (HTTP)", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Metrics server failed to start", "error", err)
		}
	}()

	// Listen for OS signals to coordinate graceful termination
	shutdownSig := make(chan os.Signal, 1)
	signal.Notify(shutdownSig, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		slog.Error("Critical server error during runtime", "error", err)
		os.Exit(1)

	case sig := <-shutdownSig:
		slog.Info("Shutdown signal intercepted", "signal", sig.String())

		// Allow grace period for active connections to finish processing
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		slog.Info("Gracefully shutting down HTTP server...")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("Graceful shutdown failed, forcing server shutdown", "error", err)
			_ = srv.Close()
		}

		slog.Info("Gracefully shutting down Metrics server...")
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("Metrics server shutdown failed, forcing close", "error", err)
			_ = metricsSrv.Close()
		}

		slog.Info("API Gateway shutdown complete")
	}
}

func startJWKSRefresh(grpcProxy *proxy.GRPCProxy, authMW *middleware.AuthMiddleware, interval time.Duration) chan struct{} {
	stop := make(chan struct{})
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				raw, err := grpcProxy.FetchJWKS(ctx)
				cancel()
				if err != nil {
					slog.Warn("JWKS refresh failed; keeping last valid key set", "error", err)
					continue
				}
				if err := authMW.UpdateJWKS(raw); err != nil {
					slog.Warn("Auth returned invalid JWKS; keeping last valid key set", "error", err)
				}
			case <-stop:
				return
			}
		}
	}()
	return stop
}
