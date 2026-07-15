package middleware

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"

	apierrors "apigateway/internal/errors"
	"github.com/golang-jwt/jwt/v5"
)

type AuthMiddleware struct {
	mu         sync.RWMutex
	publicKeys map[string]*rsa.PublicKey
	fallback   *rsa.PublicKey
	rawJWKS    []byte
	issuer     string
	audiences  []string
}

func NewAuthMiddleware(pubKey *rsa.PublicKey, issuer, audience string) *AuthMiddleware {
	return &AuthMiddleware{publicKeys: map[string]*rsa.PublicKey{}, fallback: pubKey, issuer: issuer, audiences: compactStrings([]string{audience})}
}

func NewAuthMiddlewareFromJWKS(raw []byte, issuer string, audiences []string) (*AuthMiddleware, error) {
	am := &AuthMiddleware{issuer: issuer, audiences: compactStrings(audiences)}
	if err := am.UpdateJWKS(raw); err != nil {
		return nil, err
	}
	return am, nil
}

type jwksDocument struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (am *AuthMiddleware) UpdateJWKS(raw []byte) error {
	var document jwksDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.Kty != "RSA" || key.Kid == "" || (key.Alg != "" && key.Alg != "RS256") {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return fmt.Errorf("decode JWKS modulus: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			return fmt.Errorf("decode JWKS exponent: %w", err)
		}
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() || e.Int64() <= 0 {
			return errors.New("invalid JWKS exponent")
		}
		keys[key.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no usable RS256 keys")
	}
	am.mu.Lock()
	am.publicKeys = keys
	am.rawJWKS = append(am.rawJWKS[:0], raw...)
	am.mu.Unlock()
	return nil
}

func (am *AuthMiddleware) JWKSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		am.mu.RLock()
		raw := append([]byte(nil), am.rawJWKS...)
		am.mu.RUnlock()
		if len(raw) == 0 {
			apierrors.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "JWKS is not available", "/.well-known/jwks.json")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(raw)
	}
}

// LoadOrGenerateKeys is kept for source compatibility, but is now fail-closed
// and never creates signing material in the gateway.
func LoadOrGenerateKeys(publicPath, _ string) (*rsa.PublicKey, error) {
	return LoadPublicKey(publicPath)
}

func LoadPublicKey(publicPath string) (*rsa.PublicKey, error) {
	pubData, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, fmt.Errorf("read JWT public key: %w", err)
	}
	block, _ := pem.Decode(pubData)
	if block == nil {
		return nil, errors.New("failed to decode PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("JWT public key is not RSA")
	}
	return key, nil
}

func (am *AuthMiddleware) Authenticate(next http.Handler) http.Handler {
	return am.authenticateFor(am.audiences)(next)
}

func (am *AuthMiddleware) AuthenticateLimited(next http.Handler) http.Handler {
	return am.authenticateFor([]string{"password-change"})(next)
}

func (am *AuthMiddleware) authenticateFor(allowedAudiences []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedAudiences))
	for _, audience := range allowedAudiences {
		allowed[audience] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenString := bearerFromHeader(r.Header.Get("Authorization"))
			if tokenString == "" {
				writeUnauthorized(w, "MISSING_TOKEN", "token requerido")
				return
			}
			token, err := jwt.Parse(tokenString, am.keyFunc, jwt.WithValidMethods([]string{"RS256"}), jwt.WithExpirationRequired())
			if err != nil || !token.Valid {
				writeUnauthorized(w, "TOKEN_INVALID", "token inválido o expirado")
				return
			}
			claims, ok := token.Claims.(jwt.MapClaims)
			if !ok {
				writeUnauthorized(w, "TOKEN_INVALID", "token inválido")
				return
			}
			issuer, err := claims.GetIssuer()
			if err != nil || issuer != am.issuer {
				writeUnauthorized(w, "TOKEN_INVALID", "token inválido")
				return
			}
			audiences, err := claims.GetAudience()
			if err != nil || !audienceMatches(audiences, allowed) {
				writeUnauthorized(w, "TOKEN_INVALID", "token inválido")
				return
			}
			subject, err := claims.GetSubject()
			if err != nil || subject == "" {
				writeUnauthorized(w, "TOKEN_INVALID", "token inválido")
				return
			}
			ctx := context.WithValue(r.Context(), "user_id", subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (am *AuthMiddleware) keyFunc(token *jwt.Token) (any, error) {
	if token.Method != jwt.SigningMethodRS256 {
		return nil, fmt.Errorf("unexpected signing method")
	}
	kid, _ := token.Header["kid"].(string)
	am.mu.RLock()
	defer am.mu.RUnlock()
	if kid != "" {
		if key := am.publicKeys[kid]; key != nil {
			return key, nil
		}
		return nil, fmt.Errorf("unknown JWT kid")
	}
	if am.fallback != nil {
		return am.fallback, nil
	}
	if len(am.publicKeys) == 1 {
		for _, key := range am.publicKeys {
			return key, nil
		}
	}
	return nil, fmt.Errorf("JWT kid is required")
}

func bearerFromHeader(value string) string {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func audienceMatches(values []string, allowed map[string]struct{}) bool {
	for _, value := range values {
		if _, ok := allowed[value]; ok {
			return true
		}
	}
	return false
}

func writeUnauthorized(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
