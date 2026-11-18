package contracts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPHandlerTrailStartsAtHandlerArgument(t *testing.T) {
	for _, tc := range []struct{ name, file, call string }{
		{"Go method path", "main.go", `mux.HandleFunc("GET /v1/t/id/files", Wrap(auth, h.Target))`},
		{"Go raw path", "main.go", "mux.HandleFunc(`GET /v1/t/id/files`, Wrap(auth, h.Target))"},
		{"Go comma path", "main.go", `mux.HandleFunc("GET /comma,quoted", Wrap(auth, h.Target))`},
		{"Go nested wrapper", "main.go", `r.GET("/v1/id", Wrap(auth, h.Target))`},
		{"JS single quote", "main.js", `app.get('/v1/t/id/files', Wrap(auth, h.Target))`},
		{"TS double quote", "main.ts", `router.get("/v1/t/id/files", Wrap(auth, h.Target))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := (&HTTPExtractor{}).Extract(tc.file, []byte(tc.call), nil, nil)
			require.Len(t, rows, 1)
			trail, ok := rows[0].Meta["handler_trail"].(string)
			require.True(t, ok)
			require.Equal(t, "Wrap(auth, h.Target)", trail)
			require.Equal(t, []string{"Wrap", "auth", "h.Target"}, HandlerCandidatesInTrail(trail))
			require.Equal(t, "Wrap", rows[0].Meta["handler_ident"], "wrapper fallback is retained")
		})
	}
}

func TestHTTPHandlerTrailCannotBindAFunctionNamedByTheRoute(t *testing.T) {
	src := []byte(`package main
func wire(mux *Mux) {
 mux.HandleFunc("GET /phantom", notInThisFile)
}
func phantom() {}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{{"wire", 2, 4}, {"phantom", 5, 5}})
	rows := (&HTTPExtractor{}).Extract("main.go", src, nodes, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "main.go::wire", rows[0].SymbolID, "an unresolved handler must not resolve from a path literal")
	require.Equal(t, []string{"notInThisFile"}, HandlerCandidatesInTrail(rows[0].Meta["handler_trail"].(string)))
}

func TestHTTPHandlerTrailRetainsLaterMiddlewareArguments(t *testing.T) {
	src := []byte(`package main
func wire(r *Router) {
 r.GET("/v1/id", Auth, Wrap(h.Target))
}
`)
	rows := (&HTTPExtractor{}).Extract("main.go", src, nil, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "Auth, Wrap(h.Target)", rows[0].Meta["handler_trail"])
	require.Equal(t, []string{"Auth", "Wrap", "h.Target"}, HandlerCandidatesInTrail(rows[0].Meta["handler_trail"].(string)))
}
