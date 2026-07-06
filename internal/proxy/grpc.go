package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	apierrors "apigateway/internal/errors"
	"apigateway/internal/middleware"
	"apigateway/proto/users"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCProxy manages the gRPC client connections and translates HTTP/JSON to gRPC.
type GRPCProxy struct {
	connections map[string]*grpc.ClientConn
	UsersClient users.UserServiceClient
}

// NewGRPCProxy creates a manager for gRPC upstream connections.
func NewGRPCProxy(addresses map[string]string) (*GRPCProxy, error) {
	connections := make(map[string]*grpc.ClientConn)

	// Keepalive settings for maintaining healthy, long-lived multiplexed connections
	kacp := keepalive.ClientParameters{
		Time:                10 * time.Second, // Send pings every 10s if active
		Timeout:             3 * time.Second,  // Wait 3s for ping response
		PermitWithoutStream: true,             // Send pings even without active streams
	}

	for name, addr := range addresses {
		// Establish connection with non-blocking, dial options
		conn, err := grpc.Dial(
			addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
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

	return &GRPCProxy{
		connections: connections,
		UsersClient: usersClient,
	}, nil
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
