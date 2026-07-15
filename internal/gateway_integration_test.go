package internal_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apigateway/internal/config"
	"apigateway/internal/health"
	"apigateway/internal/middleware"
	"apigateway/internal/proxy"
	"apigateway/internal/resilience"
	"apigateway/internal/router"
	"apigateway/proto/users"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockUserServiceClient implements users.UserServiceClient interface.
type mockUserServiceClient struct{}

func (m *mockUserServiceClient) GetUser(ctx context.Context, in *users.GetUserRequest, opts ...grpc.CallOption) (*users.GetUserResponse, error) {
	if in.Id == "error_internal" {
		return nil, status.Error(codes.Internal, "simulated internal gRPC error")
	}
	if in.Id == "not_found" {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return &users.GetUserResponse{
		Id:    in.Id,
		Name:  "Matias King",
		Email: "matias@king.com",
	}, nil
}

func (m *mockUserServiceClient) CreateUser(ctx context.Context, in *users.CreateUserRequest, opts ...grpc.CallOption) (*users.CreateUserResponse, error) {
	return &users.CreateUserResponse{
		Id:    "usr_999",
		Name:  in.Name,
		Email: in.Email,
	}, nil
}

func TestGatewayIntegration(t *testing.T) {
	// 2. Setup mock HTTP server
	httpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure that the gateway cleaned the user-provided X-User-ID and forwarded the authenticated one
		if r.Header.Get("X-User-ID") != "user_123" {
			http.Error(w, "forbidden: wrong user id", http.StatusForbidden)
			return
		}

		// Ensure request ID propagation
		if r.Header.Get("X-Request-ID") == "" {
			http.Error(w, "bad request: missing request id", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"payment_id":"pay_456","status":"completed"}`))
	}))
	defer httpUpstream.Close()

	// 3. Generate an isolated test key. Runtime gateway code only consumes JWKS
	// and never creates or stores auth signing keys.
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}
	pubKey := &privateKey.PublicKey

	// 4. Build config programmatically pointing to mock servers
	cfg := &config.Config{
		Server: config.ServerConfig{
			Addr:        ":8080",
			ReadTimeout: config.Duration(10 * time.Second),
		},
		Auth: config.AuthConfig{
			JWTIssuer:   "https://auth.miapp.com",
			JWTAudience: "api.miapp.com",
		},
		RateLimit: config.RateLimitConfig{
			DefaultRPS:   100.0,
			DefaultBurst: 200,
		},
		Upstreams: map[string]config.UpstreamConfig{
			"users": {
				Protocol: "grpc",
				Address:  "localhost:50051",
				Timeout:  config.Duration(2 * time.Second),
			},
			"payments": {
				Protocol: "http",
				Address:  httpUpstream.URL,
				Timeout:  config.Duration(2 * time.Second),
			},
		},
		Routes: []config.RouteConfig{
			{
				Method:   "GET",
				Path:     "/api/v1/users/{id}",
				Upstream: "users",
				Auth:     "required",
			},
			{
				Method:   "POST",
				Path:     "/api/v1/auth/login",
				Upstream: "users",
				Auth:     "none",
			},
			{
				Method:   "GET",
				Path:     "/api/v1/payments/{id}",
				Upstream: "payments",
				Auth:     "required",
			},
		},
	}

	// 5. Initialize Gateway components
	healthHandler := health.NewHealthHandler()
	authMW := middleware.NewAuthMiddleware(pubKey, cfg.Auth.JWTIssuer, cfg.Auth.JWTAudience)

	grpcProxy, err := proxy.NewGRPCProxy(map[string]string{"users": "localhost:50051"})
	if err != nil {
		t.Fatalf("failed to create grpc proxy: %v", err)
	}
	defer grpcProxy.Close()

	// Inject the mock client
	grpcProxy.UsersClient = &mockUserServiceClient{}

	httpProxies := make(map[string]*proxy.HTTPProxy)
	p, err := proxy.NewHTTPProxy(httpUpstream.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to create http proxy: %v", err)
	}
	httpProxies["payments"] = p

	routerHandler, err := router.New(cfg, healthHandler, grpcProxy, httpProxies, authMW)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	// Chain global middlewares
	gatewayHandler := middleware.Chain(routerHandler,
		middleware.Recovery,
		middleware.RequestID,
		middleware.Logging,
		middleware.SecurityHeaders,
		middleware.CORS,
	)

	// Create test client server
	ts := httptest.NewServer(gatewayHandler)
	defer ts.Close()

	// 6. Generate valid JWT token signed with the isolated test key.
	validToken := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user_123",
		"iss": "https://auth.miapp.com",
		"aud": "api.miapp.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tokenString, err := validToken.SignedString(privateKey)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	// Test Case 1: Unauthenticated request to protected route
	t.Run("protected route auth missing", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/123", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
		}
	})

	// Test Case 2: Authenticated request to gRPC service
	t.Run("grpc user retrieval success", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/42", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		var body map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body["id"] != "42" || body["name"] != "Matias King" {
			t.Errorf("unexpected body structure: %+v", body)
		}

		// Ensure standard security headers are present
		if resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Error("missing security headers")
		}
	})

	// Test Case 3: gRPC Not Found mapping
	t.Run("grpc error mapped to 404", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/not_found", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 Not Found, got %d", resp.StatusCode)
		}

		var prob map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&prob)
		if prob["title"] != "NotFound" {
			t.Errorf("expected title to be NotFound, got %+v", prob)
		}
	})

	// Test Case 4: Authenticated HTTP proxy forwarding and header injection protection
	t.Run("http proxy header cleaning and forward success", func(t *testing.T) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/payments/999", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)
		// Malicious header injection attempt from client
		req.Header.Set("X-User-ID", "malicious_user")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		var body map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body["payment_id"] != "pay_456" {
			t.Errorf("unexpected body payload: %+v", body)
		}
	})

	// Test Case 5: Public Route Bypass Auth
	t.Run("public route bypassed auth success", func(t *testing.T) {
		reqBody := `{"name":"Matias Developer","email":"matias@dev.com"}`
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login", strings.NewReader(reqBody))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			t.Errorf("expected 201 Created, got %d", resp.StatusCode)
		}
	})
}

// TestCircuitBreaker verifies that circuit breaker transitions correctly
func TestCircuitBreaker(t *testing.T) {
	cb := resilience.NewCircuitBreaker(3, 2, 50*time.Millisecond)

	// CLOSED state initially
	if cb.State() != resilience.StateClosed {
		t.Error("should start in CLOSED state")
	}

	// Make 3 failures to trigger OPEN state
	for i := 0; i < 3; i++ {
		_ = cb.Execute(func() error {
			return errors.New("upstream connection timeout")
		})
	}

	if cb.State() != resilience.StateOpen {
		t.Error("should transition to OPEN after 3 failures")
	}

	// Immediate requests should return ErrCircuitOpen
	err := cb.Execute(func() error { return nil })
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got: %v", err)
	}

	// Wait for cooldown
	time.Sleep(60 * time.Millisecond)

	// Next request triggers HALF-OPEN
	err = cb.Execute(func() error { return nil }) // Success
	if err != nil {
		t.Errorf("should allow request, got: %v", err)
	}
	if cb.State() != resilience.StateHalfOpen {
		t.Error("should transition to HALF-OPEN")
	}

	// 2nd successful request triggers CLOSED
	_ = cb.Execute(func() error { return nil })
	if cb.State() != resilience.StateClosed {
		t.Error("should transition back to CLOSED after success threshold")
	}
}
