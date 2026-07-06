package errors

import (
	"encoding/json"
	"net/http"
)

// ProblemDetails represents an RFC 7807 Problem Details object.
type ProblemDetails struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes a JSON problem details response.
func WriteProblem(w http.ResponseWriter, status int, title, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)

	problem := ProblemDetails{
		Type:     getProblemType(status),
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
	}

	_ = json.NewEncoder(w).Encode(problem)
}

func getProblemType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "https://api.gateway.local/errors/bad-request"
	case http.StatusUnauthorized:
		return "https://api.gateway.local/errors/unauthorized"
	case http.StatusForbidden:
		return "https://api.gateway.local/errors/forbidden"
	case http.StatusNotFound:
		return "https://api.gateway.local/errors/not-found"
	case http.StatusMethodNotAllowed:
		return "https://api.gateway.local/errors/method-not-allowed"
	case http.StatusRequestTimeout:
		return "https://api.gateway.local/errors/request-timeout"
	case http.StatusConflict:
		return "https://api.gateway.local/errors/conflict"
	case http.StatusTooManyRequests:
		return "https://api.gateway.local/errors/too-many-requests"
	case http.StatusBadGateway:
		return "https://api.gateway.local/errors/bad-gateway"
	case http.StatusServiceUnavailable:
		return "https://api.gateway.local/errors/service-unavailable"
	case http.StatusGatewayTimeout:
		return "https://api.gateway.local/errors/gateway-timeout"
	default:
		return "https://api.gateway.local/errors/internal-server-error"
	}
}
