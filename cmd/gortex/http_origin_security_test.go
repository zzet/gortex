package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/server"
	"go.uber.org/zap"
)

// Exercise the production REST dispatcher and write_file handler, not a mock
// filesystem sink. text/plain is deliberate: browsers can POST it without a
// CORS preflight, so removing CORS headers alone cannot prevent the mutation.
func TestHTTPBrowserOriginBlocksFileWrites(t *testing.T) {
	for _, surface := range []string{"mcp-server", "daemon"} {
		for _, corsOrigin := range []string{"", "*"} {
			t.Run(surface+"/cors="+corsOrigin, func(t *testing.T) {
				root := t.TempDir()
				g := graph.New()
				logger := zap.NewNop()
				idx := indexer.New(g, parser.NewRegistry(), config.IndexConfig{}, logger)
				idx.SetRootPath(root)
				srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, logger, nil)
				api := server.NewHandler(srv.MCPServer(), g, "test", logger)
				var h http.Handler
				if surface == "daemon" {
					h = composeDaemonHTTPHandler(http.NotFoundHandler(), api, func() string { return "" }, corsOrigin)
				} else {
					h = composeMCPServerHandler(api, "", corsOrigin)
				}

				target := filepath.Join(root, ".git", "hooks", "pre-commit")
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				const original = "#!/bin/sh\n# original test fixture\n"
				const replacement = "#!/bin/sh\n# replacement test fixture\n"
				require.NoError(t, os.WriteFile(target, []byte(original), 0o755))
				body, err := json.Marshal(map[string]any{"path": target, "content": replacement})
				require.NoError(t, err)
				call := func(origin string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, "/v1/tools/write_file", bytes.NewReader(body))
					req.Header.Set("Content-Type", "text/plain")
					if origin != "" {
						req.Header.Set("Origin", origin)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					return rec
				}

				blocked := call("https://evil.example")
				got, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, http.StatusForbidden, blocked.Code, "file changed=%t; response=%s", string(got) != original, blocked.Body.String())
				require.Equal(t, original, string(got))
				require.Empty(t, blocked.Header().Get("Access-Control-Allow-Origin"))

				// Prove the fixture really can write through this route, and that
				// non-browser loopback clients retain their existing access.
				local := call("")
				require.Equal(t, http.StatusOK, local.Code, local.Body.String())
				got, err = os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, replacement, string(got), local.Body.String())
			})
		}
	}
}

func TestHTTPBrowserOriginPolicy(t *testing.T) {
	const trusted = "https://ui.example"
	savedOrigins := daemonHTTPAllowedOrigins
	t.Cleanup(func() { daemonHTTPAllowedOrigins = savedOrigins })
	for _, surface := range []string{"mcp-server", "daemon"} {
		t.Run(surface, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				origin        string
				cors          string
				allowed       []string
				token         string
				authorization string
				want          int
			}{
				{name: "local client", want: http.StatusOK},
				{name: "untrusted", origin: "https://evil.example", want: http.StatusForbidden},
				{name: "opaque origin", origin: "null", want: http.StatusForbidden},
				{name: "wildcard does not grant access", origin: "https://evil.example", cors: "*", allowed: []string{"*"}, want: http.StatusForbidden},
				{name: "trusted", origin: trusted, cors: trusted, allowed: []string{trusted}, want: http.StatusOK},
				{name: "suffix mismatch", origin: trusted + ".evil.example", cors: trusted, allowed: []string{trusted}, want: http.StatusForbidden},
				{name: "scheme mismatch", origin: "http://ui.example", cors: trusted, allowed: []string{trusted}, want: http.StatusForbidden},
				{name: "port mismatch", origin: trusted + ":8443", cors: trusted, allowed: []string{trusted}, want: http.StatusForbidden},
				{name: "trusted still requires auth", origin: trusted, cors: trusted, allowed: []string{trusted}, token: "secret", want: http.StatusUnauthorized},
				{name: "trusted authenticated", origin: trusted, cors: trusted, allowed: []string{trusted}, token: "secret", authorization: "Bearer secret", want: http.StatusOK},
				{name: "auth does not bypass origin", origin: "https://evil.example", cors: "*", token: "secret", authorization: "Bearer secret", want: http.StatusForbidden},
			} {
				t.Run(tc.name, func(t *testing.T) {
					daemonHTTPAllowedOrigins = tc.allowed
					reached := false
					inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						reached = true
						w.WriteHeader(http.StatusOK)
					})
					var h http.Handler
					paths := []string{"/v1/tools/write_file", "/v1/graph", "/v1/conversations"}
					if surface == "daemon" {
						h = composeDaemonHTTPHandler(inner, inner, func() string { return tc.token }, tc.cors)
						// The streamable handler has its own auth; here the fake
						// only checks the outer Origin gate and CORS ordering.
						if tc.token == "" {
							paths = append(paths, "/mcp", "/healthz")
						}
					} else {
						h = composeMCPServerHandler(inner, tc.token, tc.cors)
					}
					for _, path := range paths {
						for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodOptions} {
							for _, contentType := range []string{"application/json", "text/plain"} {
								reached = false
								req := httptest.NewRequest(method, path, nil)
								req.Header.Set("Content-Type", contentType)
								req.Header.Set("Origin", tc.origin)
								req.Header.Set("Authorization", tc.authorization)
								if method == http.MethodOptions {
									req.Header.Set("Access-Control-Request-Method", http.MethodPost)
									req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
								}
								rec := httptest.NewRecorder()
								h.ServeHTTP(rec, req)
								want := tc.want
								if method == http.MethodOptions && tc.cors != "" && want != http.StatusForbidden {
									want = http.StatusNoContent
								}
								require.Equal(t, want, rec.Code, "%s %s (%s)", method, path, contentType)
								require.Equal(t, want == http.StatusOK, reached, "%s %s", method, path)
								if want == http.StatusForbidden {
									require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
								} else if tc.origin == trusted {
									require.Equal(t, trusted, rec.Header().Get("Access-Control-Allow-Origin"))
								}
							}
						}
					}
				})
			}
		})
	}
}

func TestHTTPBrowserDefaultsDenyOrigins(t *testing.T) {
	for _, cmd := range []*cobra.Command{mcpCmd, daemonStartCmd} {
		require.Empty(t, cmd.Flags().Lookup("cors-origin").DefValue, cmd.Name())
	}
}

func TestDaemonCORSDoesNotAuthorizeBrowserOrigin(t *testing.T) {
	savedOrigins := daemonHTTPAllowedOrigins
	daemonHTTPAllowedOrigins = nil
	t.Cleanup(func() { daemonHTTPAllowedOrigins = savedOrigins })
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("CORS configuration alone must not grant access")
	})
	h := composeDaemonHTTPHandler(inner, inner, func() string { return "" }, "https://ui.example")
	req := httptest.NewRequest(http.MethodOptions, "/v1/tools/write_file", nil)
	req.Header.Set("Origin", "https://ui.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}
