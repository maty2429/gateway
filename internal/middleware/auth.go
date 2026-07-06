package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apierrors "apigateway/internal/errors"
	"github.com/golang-jwt/jwt/v5"
)

type AuthMiddleware struct {
	publicKey *rsa.PublicKey
	issuer    string
	audience  string
}

// NewAuthMiddleware creates a new JWT validation middleware.
func NewAuthMiddleware(pubKey *rsa.PublicKey, issuer, audience string) *AuthMiddleware {
	return &AuthMiddleware{
		publicKey: pubKey,
		issuer:    issuer,
		audience:  audience,
	}
}

// LoadOrGenerateKeys attempts to load RSA public key from configPath,
// or generates a new key pair and saves them to disk if none is found.
func LoadOrGenerateKeys(publicPath, privatePath string) (*rsa.PublicKey, error) {
	// Attempt to load existing public key
	pubData, err := os.ReadFile(publicPath)
	if err == nil {
		block, _ := pem.Decode(pubData)
		if block == nil {
			return nil, errors.New("failed to decode PEM block for public key")
		}
		pubKey, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse public key: %w", err)
		}
		rsaPubKey, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("key is not of type RSA public key")
		}
		slog.Info("Loaded JWT RSA Public Key from file", "path", publicPath)
		return rsaPubKey, nil
	}

	// Generate key pair if public key is not found
	slog.Info("JWT RSA Public Key not found. Generating a new key pair for local development...", "path", publicPath)

	// Ensure config directory exists
	dir := filepath.Dir(publicPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}

	// Save private key
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})
	if err := os.WriteFile(privatePath, privPEM, 0600); err != nil {
		return nil, fmt.Errorf("failed to write private key to file: %w", err)
	}

	// Save public key
	pubBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})
	if err := os.WriteFile(publicPath, pubPEM, 0644); err != nil {
		return nil, fmt.Errorf("failed to write public key to file: %w", err)
	}

	slog.Info("Generated development JWT keys", "public_key_path", publicPath, "private_key_path", privatePath)
	return &privateKey.PublicKey, nil
}

// Authenticate is the middleware function to validate JWT tokens.
func (am *AuthMiddleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			apierrors.WriteProblem(
				w,
				http.StatusUnauthorized,
				"Unauthorized",
				"Missing Authorization header",
				r.URL.Path,
			)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			apierrors.WriteProblem(
				w,
				http.StatusUnauthorized,
				"Unauthorized",
				"Authorization header format must be: Bearer <token>",
				r.URL.Path,
			)
			return
		}

		tokenStr := parts[1]
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			// Validate signing method
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return am.publicKey, nil
		})

		if err != nil || !token.Valid {
			apierrors.WriteProblem(
				w,
				http.StatusUnauthorized,
				"Unauthorized",
				"Invalid or expired access token",
				r.URL.Path,
			)
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			apierrors.WriteProblem(
				w,
				http.StatusUnauthorized,
				"Unauthorized",
				"Failed to parse token claims",
				r.URL.Path,
			)
			return
		}

		// Verify Issuer (iss)
		if am.issuer != "" {
			iss, err := claims.GetIssuer()
			if err != nil || iss != am.issuer {
				apierrors.WriteProblem(
					w,
					http.StatusUnauthorized,
					"Unauthorized",
					"Token issuer mismatch",
					r.URL.Path,
				)
				return
			}
		}

		// Verify Audience (aud)
		if am.audience != "" {
			aud, err := claims.GetAudience()
			if err != nil {
				apierrors.WriteProblem(
					w,
					http.StatusUnauthorized,
					"Unauthorized",
					"Token audience is missing or malformed",
					r.URL.Path,
				)
				return
			}
			audMatched := false
			for _, a := range aud {
				if a == am.audience {
					audMatched = true
					break
				}
			}
			if !audMatched {
				apierrors.WriteProblem(
					w,
					http.StatusUnauthorized,
					"Unauthorized",
					"Token audience mismatch",
					r.URL.Path,
				)
				return
			}
		}

		// Extract subject (sub) as the user identifier
		sub, err := claims.GetSubject()
		if err != nil || sub == "" {
			apierrors.WriteProblem(
				w,
				http.StatusUnauthorized,
				"Unauthorized",
				"Token subject (sub) is missing",
				r.URL.Path,
			)
			return
		}

		// Set the identity in request context
		ctx := context.WithValue(r.Context(), "user_id", sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
