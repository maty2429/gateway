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
	"apigateway/internal/proxy"
	"apigateway/internal/router"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/connectivity"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", "configs/gateway.yaml", "Path to configuration file")
	flag.Parse()

	// Configure structured JSON logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("Starting API Gateway...")

	// 1. Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}
	slog.Info("Configuration loaded successfully", "addr", cfg.Server.Addr)

	// 2. Initialize JWT auth keys and middleware
	jwtPubKey, err := middleware.LoadOrGenerateKeys("configs/jwt_public.pem", "configs/jwt_private_dev.pem")
	if err != nil {
		slog.Error("Failed to load or generate JWT keys", "error", err)
		os.Exit(1)
	}
	authMW := middleware.NewAuthMiddleware(jwtPubKey, cfg.Auth.JWTIssuer, cfg.Auth.JWTAudience)

	// 3. Connect to gRPC upstreams
	grpcUpstreams := make(map[string]string)
	for name, uCfg := range cfg.Upstreams {
		if uCfg.Protocol == "grpc" {
			grpcUpstreams[name] = uCfg.Address
		}
	}
	grpcProxy, err := proxy.NewGRPCProxy(grpcUpstreams)
	if err != nil {
		slog.Error("Failed to initialize gRPC upstreams", "error", err)
		os.Exit(1)
	}
	defer grpcProxy.Close()

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

	// Add dynamic readiness checks mapping state of gRPC upstreams
	for name, conn := range grpcProxy.Connections() {
		connCopy := conn
		nameCopy := name
		healthHandler.RegisterReadinessCheck("grpc_"+nameCopy, func(ctx context.Context) error {
			state := connCopy.GetState()
			if state == connectivity.TransientFailure || state == connectivity.Shutdown {
				return errors.New("upstream connection status: " + state.String())
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
		middleware.CORS,
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
