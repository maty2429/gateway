package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"apigateway/internal/middleware"
	"apigateway/internal/resilience"
	authv1 "github.com/maty2429/contracts/gen/go/auth/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type authErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// HandleAuthRoute adapts one public REST route to its explicit auth.v1 RPC.
func (p *GRPCProxy) HandleAuthRoute(routePath, method string, cookieSecure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.AuthClient == nil || p.AdminClient == nil {
			writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "servicio de autenticación no disponible")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAuthError(w, http.StatusBadRequest, "INVALID_BODY", "body JSON inválido")
			return
		}
		request := &authv1.GatewayRequest{
			JsonBody:    body,
			PathParams:  extractPathParams(routePath, r),
			QueryParams: firstQueryValues(r),
		}
		if cookie, cookieErr := r.Cookie("refresh_token"); cookieErr == nil {
			request.RefreshToken = cookie.Value
		}
		if cookie, cookieErr := r.Cookie("auth_sso"); cookieErr == nil {
			request.SsoToken = cookie.Value
		}

		ctx := outgoingAuthContext(r)
		readonly := method == http.MethodGet && routePath != "/api/v1/auth/authorize"
		response, callErr := p.callAuth(ctx, method, routePath, request, readonly)
		if callErr != nil {
			p.writeAuthGRPCError(w, callErr)
			return
		}
		if response.GetSetCookie() != "" {
			cookie := strings.Replace(response.GetSetCookie(), "Path=/auth", "Path=/api/v1/auth", 1)
			if !cookieSecure {
				cookie = strings.Replace(cookie, "; Secure", "", 1)
			}
			w.Header().Add("Set-Cookie", cookie)
		}
		if response.GetLocation() != "" {
			w.Header().Set("Location", response.GetLocation())
		}
		w.Header().Set("Content-Type", "application/json")
		statusCode := int(response.GetHttpStatus())
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write(response.GetJsonBody())
	})
}

type authCall func(context.Context, *authv1.GatewayRequest, ...grpc.CallOption) (*authv1.GatewayResponse, error)

func (p *GRPCProxy) callAuth(ctx context.Context, method, path string, request *authv1.GatewayRequest, readonly bool) (*authv1.GatewayResponse, error) {
	call := p.authCallFor(method, path)
	if call == nil {
		return nil, status.Error(codes.Unimplemented, "auth route is not mapped")
	}
	var response *authv1.GatewayResponse
	var callErr error
	breakerErr := p.authBreaker.Execute(func() error {
		response, callErr = call(ctx, request)
		if readonly && status.Code(callErr) == codes.Unavailable {
			response, callErr = call(ctx, request)
		}
		if isTransportFailure(callErr) {
			return callErr
		}
		return nil
	})
	if errors.Is(breakerErr, resilience.ErrCircuitOpen) {
		return nil, status.Error(codes.Unavailable, "auth circuit breaker is open")
	}
	if breakerErr != nil {
		return nil, callErr
	}
	return response, callErr
}

func isTransportFailure(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal:
		return err != nil
	default:
		return false
	}
}

func (p *GRPCProxy) authCallFor(method, path string) authCall {
	switch method + " " + path {
	case "POST /api/v1/auth/login":
		return p.AuthClient.Login
	case "POST /api/v1/auth/mobile/login":
		return p.AuthClient.MobileLogin
	case "GET /api/v1/auth/authorize":
		return p.AuthClient.BeginAuthorization
	case "GET /api/v1/auth/authorization-requests/{request_id}":
		return p.AuthClient.GetAuthorizationRequest
	case "POST /api/v1/auth/authorization/login":
		return p.AuthClient.LoginForAuthorization
	case "POST /api/v1/auth/token":
		return p.AuthClient.ExchangeAuthorizationCode
	case "POST /api/v1/auth/sso/logout":
		return p.AuthClient.LogoutSSO
	case "POST /api/v1/auth/refresh":
		return p.AuthClient.Refresh
	case "POST /api/v1/auth/logout":
		return p.AuthClient.Logout
	case "POST /api/v1/auth/logout-all":
		return p.AuthClient.LogoutAll
	case "GET /api/v1/auth/me":
		return p.AuthClient.GetMe
	case "GET /api/v1/auth/sessions":
		return p.AuthClient.ListSessions
	case "DELETE /api/v1/auth/sessions/{session_id}":
		return p.AuthClient.RevokeSession
	case "POST /api/v1/auth/password/change":
		return p.AuthClient.ChangePassword
	case "POST /api/v1/auth/password/change-required":
		return p.AuthClient.ChangeRequiredPassword
	case "POST /api/v1/auth/password/recovery/start":
		return p.AuthClient.StartRecovery
	case "POST /api/v1/auth/password/recovery/confirm":
		return p.AuthClient.ConfirmRecovery
	case "POST /api/v1/auth/service-token":
		return p.AuthClient.IssueServiceToken
	case "POST /api/v1/admin/users":
		return p.AdminClient.CreateUser
	case "GET /api/v1/admin/users":
		return p.AdminClient.ListUsers
	case "PATCH /api/v1/admin/users/{user_id}":
		return p.AdminClient.UpdateUser
	case "POST /api/v1/admin/users/{user_id}/disable":
		return p.AdminClient.DisableUser
	case "POST /api/v1/admin/users/{user_id}/enable":
		return p.AdminClient.EnableUser
	case "POST /api/v1/admin/users/{user_id}/roles":
		return p.AdminClient.AssignRole
	case "DELETE /api/v1/admin/users/{user_id}/roles/{project_id}/{role_id}":
		return p.AdminClient.RemoveRole
	case "GET /api/v1/admin/security-events":
		return p.AdminClient.ListSecurityEvents
	case "POST /api/v1/admin/service-accounts":
		return p.AdminClient.CreateServiceAccount
	case "GET /api/v1/admin/service-accounts":
		return p.AdminClient.ListServiceAccounts
	case "PATCH /api/v1/admin/service-accounts/{id}":
		return p.AdminClient.UpdateServiceAccount
	case "POST /api/v1/admin/projects":
		return p.AdminClient.CreateProject
	case "GET /api/v1/admin/projects":
		return p.AdminClient.ListProjects
	case "PATCH /api/v1/admin/projects/{project_id}":
		return p.AdminClient.UpdateProject
	case "POST /api/v1/admin/projects/{project_id}/enable":
		return p.AdminClient.EnableProject
	case "POST /api/v1/admin/projects/{project_id}/disable":
		return p.AdminClient.DisableProject
	case "POST /api/v1/admin/clients":
		return p.AdminClient.CreateClient
	case "GET /api/v1/admin/clients":
		return p.AdminClient.ListClients
	case "PATCH /api/v1/admin/clients/{client_id}":
		return p.AdminClient.UpdateClient
	case "POST /api/v1/admin/clients/{client_id}/enable":
		return p.AdminClient.EnableClient
	case "POST /api/v1/admin/clients/{client_id}/disable":
		return p.AdminClient.DisableClient
	case "POST /api/v1/admin/clients/{client_id}/rotate-secret":
		return p.AdminClient.RotateClientSecret
	case "PUT /api/v1/admin/users/{user_id}/global-roles/{role_id}":
		return p.AdminClient.AssignGlobalRole
	case "DELETE /api/v1/admin/users/{user_id}/global-roles/{role_id}":
		return p.AdminClient.RemoveGlobalRole
	default:
		return nil
	}
}

func (p *GRPCProxy) FetchJWKS(ctx context.Context) ([]byte, error) {
	if p.AuthClient == nil {
		return nil, errors.New("auth upstream is not configured")
	}
	response, err := p.AuthClient.GetJWKS(ctx, &authv1.GatewayRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetJsonBody(), nil
}

func outgoingAuthContext(r *http.Request) context.Context {
	md := metadata.New(nil)
	if value := r.Header.Get("Authorization"); value != "" {
		md.Set("authorization", value)
	}
	if value := middleware.GetRequestID(r.Context()); value != "" {
		md.Set("x-request-id", value)
	}
	md.Set("x-client-ip-hash", hashEdgeValue(clientIP(r)))
	md.Set("x-user-agent-hash", hashEdgeValue(r.UserAgent()))
	return metadata.NewOutgoingContext(r.Context(), md)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func hashEdgeValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func extractPathParams(pattern string, r *http.Request) map[string]string {
	params := make(map[string]string)
	for {
		start := strings.IndexByte(pattern, '{')
		if start < 0 {
			break
		}
		end := strings.IndexByte(pattern[start:], '}')
		if end < 0 {
			break
		}
		name := pattern[start+1 : start+end]
		params[name] = r.PathValue(name)
		pattern = pattern[start+end+1:]
	}
	return params
}

func firstQueryValues(r *http.Request) map[string]string {
	result := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			result[key] = values[0]
		}
	}
	return result
}

func (p *GRPCProxy) writeAuthGRPCError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "ocurrió un error interno")
		return
	}
	reason := "INTERNAL_ERROR"
	message := "ocurrió un error interno"
	switch st.Code() {
	case codes.Unavailable:
		reason, message = "SERVICE_UNAVAILABLE", "servicio de autenticación no disponible"
	case codes.DeadlineExceeded:
		reason, message = "GATEWAY_TIMEOUT", "el servicio de autenticación excedió el tiempo de respuesta"
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == "auth-service" {
			reason = info.GetReason()
			message = st.Message()
			break
		}
	}
	writeAuthError(w, MapGRPCCodeToHTTP(st.Code()), reason, message)
}

func writeAuthError(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(authErrorBody{Error: code, Message: message})
}
