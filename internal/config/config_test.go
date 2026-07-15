package config

import "testing"

func TestGatewayConfigLoadsAuthRoutes(t *testing.T) {
	cfg, err := Load("../../configs/gateway.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstreams["auth"].Protocol != "grpc" {
		t.Fatal("auth upstream must use gRPC")
	}
	authRoutes := 0
	for _, route := range cfg.Routes {
		if route.Upstream == "auth" {
			authRoutes++
		}
	}
	if authRoutes != 42 {
		t.Fatalf("expected 42 auth/admin routes, got %d", authRoutes)
	}
}

func TestProductionRequiresMTLSAndSecureBrowserConfig(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Addr: ":8080", Environment: "production"},
		Auth:      AuthConfig{JWTIssuer: "auth-service", JWTAudiences: []string{"mobile_app"}},
		Upstreams: map[string]UpstreamConfig{"auth": {Protocol: "grpc", Address: "auth:50051"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected fail-closed production validation")
	}
}
