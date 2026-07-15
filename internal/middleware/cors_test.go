package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSRejectsUnlistedCredentialedOrigin(t *testing.T) {
	handler := CORSWithOrigins([]string{"https://allowed.example"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/login", nil)
	req.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin was reflected")
	}
}
