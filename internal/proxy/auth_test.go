package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apigateway/internal/resilience"
	authv1 "github.com/maty2429/contracts/gen/go/auth/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockAuthClient struct {
	authv1.AuthServiceClient
	login     func(context.Context, *authv1.GatewayRequest) (*authv1.GatewayResponse, error)
	getMe     func(context.Context, *authv1.GatewayRequest) (*authv1.GatewayResponse, error)
	authorize func(context.Context, *authv1.GatewayRequest) (*authv1.GatewayResponse, error)
}

func (m *mockAuthClient) BeginAuthorization(ctx context.Context, in *authv1.GatewayRequest, _ ...grpc.CallOption) (*authv1.GatewayResponse, error) {
	return m.authorize(ctx, in)
}

func (m *mockAuthClient) GetMe(ctx context.Context, in *authv1.GatewayRequest, _ ...grpc.CallOption) (*authv1.GatewayResponse, error) {
	return m.getMe(ctx, in)
}

func (m *mockAuthClient) Login(ctx context.Context, in *authv1.GatewayRequest, _ ...grpc.CallOption) (*authv1.GatewayResponse, error) {
	return m.login(ctx, in)
}

type mockAdminClient struct{ authv1.AdminServiceClient }

func TestAuthorizePreservesRedirectAndSSOCookie(t *testing.T) {
	client := &mockAuthClient{authorize: func(_ context.Context, in *authv1.GatewayRequest) (*authv1.GatewayResponse, error) {
		if in.GetSsoToken() != "sso-secret" {
			t.Fatalf("SSO cookie not propagated")
		}
		if in.GetQueryParams()["client_id"] != "farmacia_web" {
			t.Fatalf("query not propagated")
		}
		return &authv1.GatewayResponse{HttpStatus: 302, Location: "https://farmacia.test/callback?code=one"}, nil
	}}
	p := &GRPCProxy{AuthClient: client, AdminClient: &mockAdminClient{}, authBreaker: resilience.NewCircuitBreaker(5, 2, time.Second)}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/authorize?client_id=farmacia_web", nil)
	req.AddCookie(&http.Cookie{Name: "auth_sso", Value: "sso-secret"})
	rec := httptest.NewRecorder()
	p.HandleAuthRoute("/api/v1/auth/authorize", http.MethodGet, false).ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://farmacia.test/callback?code=one" {
		t.Fatalf("redirect lost: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthRouteWritesVersionedCookie(t *testing.T) {
	client := &mockAuthClient{login: func(_ context.Context, _ *authv1.GatewayRequest) (*authv1.GatewayResponse, error) {
		return &authv1.GatewayResponse{HttpStatus: 200, JsonBody: []byte(`{"status":"OK"}`), SetCookie: "refresh_token=secret; Path=/auth; HttpOnly; Secure; SameSite=Lax"}, nil
	}}
	p := &GRPCProxy{AuthClient: client, AdminClient: &mockAdminClient{}, authBreaker: resilience.NewCircuitBreaker(5, 2, time.Second)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	p.HandleAuthRoute("/api/v1/auth/login", http.MethodPost, false).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: %d", recorder.Code)
	}
	cookie := recorder.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "Path=/api/v1/auth") || strings.Contains(cookie, "; Secure") {
		t.Fatalf("cookie not rewritten: %s", cookie)
	}
}

func TestAuthRouteRestoresDomainError(t *testing.T) {
	client := &mockAuthClient{login: func(_ context.Context, _ *authv1.GatewayRequest) (*authv1.GatewayResponse, error) {
		st := status.New(codes.Unauthenticated, "usuario o contraseña incorrectos")
		withDetails, _ := st.WithDetails(&errdetails.ErrorInfo{Reason: "INVALID_CREDENTIALS", Domain: "auth-service"})
		return nil, withDetails.Err()
	}}
	p := &GRPCProxy{AuthClient: client, AdminClient: &mockAdminClient{}, authBreaker: resilience.NewCircuitBreaker(5, 2, time.Second)}
	recorder := httptest.NewRecorder()
	p.HandleAuthRoute("/api/v1/auth/login", http.MethodPost, false).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "INVALID_CREDENTIALS") {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRetriesOnlyReadOnlyUnavailableCalls(t *testing.T) {
	loginCalls := 0
	getMeCalls := 0
	client := &mockAuthClient{
		login: func(_ context.Context, _ *authv1.GatewayRequest) (*authv1.GatewayResponse, error) {
			loginCalls++
			return nil, status.Error(codes.Unavailable, "down")
		},
		getMe: func(_ context.Context, _ *authv1.GatewayRequest) (*authv1.GatewayResponse, error) {
			getMeCalls++
			return nil, status.Error(codes.Unavailable, "down")
		},
	}
	p := &GRPCProxy{AuthClient: client, AdminClient: &mockAdminClient{}, authBreaker: resilience.NewCircuitBreaker(10, 2, time.Second)}
	_, _ = p.callAuth(context.Background(), http.MethodPost, "/api/v1/auth/login", &authv1.GatewayRequest{}, false)
	_, _ = p.callAuth(context.Background(), http.MethodGet, "/api/v1/auth/me", &authv1.GatewayRequest{}, true)
	if loginCalls != 1 {
		t.Fatalf("mutation was retried %d times", loginCalls)
	}
	if getMeCalls != 2 {
		t.Fatalf("read-only call count = %d", getMeCalls)
	}
}
