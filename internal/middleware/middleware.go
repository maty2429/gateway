package middleware

import "net/http"

// Middleware defines a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain chains multiple middlewares around an http.Handler.
// The first middleware in the list is the outermost wrapper, meaning it executes first.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
