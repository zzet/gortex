package main

import (
	"net/http"

	"github.com/zzet/gortex/internal/server"
)

// composeMCPServerHandler protects the optional mcp --server REST listener.
// Loopback clients may omit a token, but a browser must have an explicitly
// trusted origin. Keep the guard outermost so neither CORS preflights nor
// simple POSTs can bypass it. A wildcard CORS setting is not an origin grant.
func composeMCPServerHandler(api http.Handler, token, corsOrigin string) http.Handler {
	h := server.WithAuth(api, token)
	if corsOrigin != "" {
		h = server.WithCORS(h, server.CORSOptions{AllowOrigins: []string{corsOrigin}})
	}
	return browserOriginGuard(h, []string{corsOrigin})
}
