package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"apigateway/internal/config"
	apierrors "apigateway/internal/errors"
	"apigateway/internal/middleware"
	"apigateway/internal/resilience"
	"apigateway/proto/users"
	authv1 "github.com/maty2429/contracts/gen/go/auth/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCProxy manages the gRPC client connections and translates HTTP/JSON to gRPC.
type GRPCProxy struct {
	connections   map[string]*grpc.ClientConn
	UsersClient   users.UserServiceClient
	AuthClient    authv1.AuthServiceClient
	AdminClient   authv1.AdminServiceClient
	HealthClients map[string]healthpb.HealthClient
	authBreaker   *resilience.CircuitBreaker
}

// NewGRPCProxy creates a manager for gRPC upstream connections.
func NewGRPCProxy(addresses map[string]string) (*GRPCProxy, error) {
	upstreams := make(map[string]config.UpstreamConfig, len(addresses))
	for name, address := range addresses {
		upstreams[name] = config.UpstreamConfig{Protocol: "grpc", Address: address}
	}
	return NewGRPCProxyWithConfig(upstreams)
}

// NewGRPCProxyWithConfig configures plaintext or mutual-TLS connections per upstream.
func NewGRPCProxyWithConfig(upstreams map[string]config.UpstreamConfig) (*GRPCProxy, error) {
	connections := make(map[string]*grpc.ClientConn)

	// Keepalive settings for maintaining healthy, long-lived multiplexed connections
	kacp := keepalive.ClientParameters{
		Time:                5 * time.Minute,
		Timeout:             10 * time.Second,
		PermitWithoutStream: false,
	}

	for name, upstream := range upstreams {
		if upstream.Protocol != "grpc" {
			continue
		}
		transportCredentials, err := clientCredentials(upstream)
		if err != nil {
			return nil, fmt.Errorf("upstream %s credentials: %w", name, err)
		}
		// Establish connection with non-blocking, dial options
		conn, err := grpc.NewClient(
			upstream.Address,
			grpc.WithTransportCredentials(transportCredentials),
			grpc.WithKeepaliveParams(kacp),
		)
		if err != nil {
			// Close any successfully opened connections on error
			for _, c := range connections {
				_ = c.Close()
			}
			return nil, err
		}
		connections[name] = conn
	}

	var usersClient users.UserServiceClient
	if conn, exists := connections["users"]; exists {
		usersClient = users.NewUserServiceClient(conn)
	}

	proxy := &GRPCProxy{
		connections:   connections,
		UsersClient:   usersClient,
		HealthClients: make(map[string]healthpb.HealthClient, len(connections)),
		authBreaker:   resilience.NewCircuitBreaker(5, 2, 30*time.Second),
	}
	for name, conn := range connections {
		proxy.HealthClients[name] = healthpb.NewHealthClient(conn)
	}
	if conn, exists := connections["auth"]; exists {
		proxy.AuthClient = authv1.NewAuthServiceClient(conn)
		proxy.AdminClient = authv1.NewAdminServiceClient(conn)
	}
	return proxy, nil
}

func clientCredentials(upstream config.UpstreamConfig) (credentials.TransportCredentials, error) {
	if upstream.TLS == nil {
		return insecure.NewCredentials(), nil
	}
	caPEM, err := os.ReadFile(upstream.TLS.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file contains no certificates")
	}
	cert, err := tls.LoadX509KeyPair(upstream.TLS.CertFile, upstream.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: upstream.TLS.ServerName}), nil
}

// Connections returns the map of active gRPC connections.
func (p *GRPCProxy) Connections() map[string]*grpc.ClientConn {
	return p.connections
}

// Close closes all open gRPC client connections.
func (p *GRPCProxy) Close() {
	for name, conn := range p.connections {
		slog.Info("Closing gRPC connection", "upstream", name)
		_ = conn.Close()
	}
}

// PropagateMetadata builds and returns an outgoing context containing Request-ID and User-ID.
func PropagateMetadata(ctx context.Context) context.Context {
	md := metadata.New(nil)

	if reqID := middleware.GetRequestID(ctx); reqID != "" {
		md.Set("x-request-id", reqID)
	}

	if uVal := ctx.Value("user_id"); uVal != nil {
		if uStr, ok := uVal.(string); ok && uStr != "" {
			md.Set("x-user-id", uStr)
		}
	}

	return metadata.NewOutgoingContext(ctx, md)
}

// WriteGRPCError translates a gRPC status error into an RFC 7807 problem details response.
func WriteGRPCError(w http.ResponseWriter, r *http.Request, err error) {
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status error, return internal server error
		apierrors.WriteProblem(
			w,
			http.StatusInternalServerError,
			"Internal Server Error",
			err.Error(),
			r.URL.Path,
		)
		return
	}

	httpCode := MapGRPCCodeToHTTP(st.Code())
	apierrors.WriteProblem(
		w,
		httpCode,
		st.Code().String(),
		st.Message(),
		r.URL.Path,
	)
}

// MapGRPCCodeToHTTP maps gRPC codes to standard HTTP status codes.
func MapGRPCCodeToHTTP(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499 // Client Closed Request
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.Aborted:
		return http.StatusConflict
	case codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// HandleGetUser processes GET /api/v1/users/{id} and translates to UserService.GetUser.
func (p *GRPCProxy) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	if p.UsersClient == nil {
		apierrors.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "Users upstream is not configured", r.URL.Path)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		apierrors.WriteProblem(w, http.StatusBadRequest, "Invalid Request", "User ID is required", r.URL.Path)
		return
	}

	ctx := PropagateMetadata(r.Context())

	req := &users.GetUserRequest{Id: id}
	res, err := p.UsersClient.GetUser(ctx, req)
	if err != nil {
		WriteGRPCError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

// HandleCreateUser processes POST /api/v1/auth/login (mocked as routing to CreateUser/Login)
func (p *GRPCProxy) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	if p.UsersClient == nil {
		apierrors.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "Users upstream is not configured", r.URL.Path)
		return
	}

	var reqBody struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		apierrors.WriteProblem(w, http.StatusBadRequest, "Malformed JSON", err.Error(), r.URL.Path)
		return
	}

	ctx := PropagateMetadata(r.Context())

	req := &users.CreateUserRequest{
		Name:  reqBody.Name,
		Email: reqBody.Email,
	}
	res, err := p.UsersClient.CreateUser(ctx, req)
	if err != nil {
		WriteGRPCError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}
