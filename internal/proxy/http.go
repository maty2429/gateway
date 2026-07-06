package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	apierrors "apigateway/internal/errors"
	"apigateway/internal/middleware"
)

// HTTPProxy wraps a httputil.ReverseProxy with custom transport, headers and error handling.
type HTTPProxy struct {
	proxy *httputil.ReverseProxy
}

// NewHTTPProxy creates an HTTP reverse proxy targeting targetURL.
func NewHTTPProxy(targetURL string, timeout time.Duration) (*HTTPProxy, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	// Tailor Transport for connection reuse and performance
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20, // Reuses idle connections per upstream host
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Set target server
			pr.SetURL(target)

			// Propagate Request ID
			reqID := middleware.GetRequestID(pr.In.Context())
			if reqID != "" {
				pr.Out.Header.Set("X-Request-ID", reqID)
			}

			// Clean internal headers coming from the client to prevent spoofing
			pr.Out.Header.Del("X-User-ID")

			// If user has been authenticated, inject their identity into the outbound header
			if uVal := pr.In.Context().Value("user_id"); uVal != nil {
				if uStr, ok := uVal.(string); ok && uStr != "" {
					pr.Out.Header.Set("X-User-ID", uStr)
				}
			}

			// Injects standard forward headers (X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto)
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("Reverse proxy upstream connection error",
				"error", err.Error(),
				"request_id", middleware.GetRequestID(r.Context()),
				"upstream", targetURL,
				"path", r.URL.Path,
			)

			// Client disconnected early or canceled the request
			if errors.Is(err, context.Canceled) {
				w.WriteHeader(499) // Client Closed Request status code
				return
			}

			// Context timeout expired
			if errors.Is(err, context.DeadlineExceeded) {
				apierrors.WriteProblem(
					w,
					http.StatusGatewayTimeout,
					"Gateway Timeout",
					"The upstream server timed out processing the request.",
					r.URL.Path,
				)
				return
			}

			// Unreachable upstream or general network failure
			apierrors.WriteProblem(
				w,
				http.StatusBadGateway,
				"Bad Gateway",
				"The gateway could not connect to the upstream server.",
				r.URL.Path,
			)
		},
	}

	return &HTTPProxy{proxy: rp}, nil
}

// ServeHTTP forwards the request to the proxy destination.
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}
