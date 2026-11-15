//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestExternalSourcePathConfinedUnderJunctionedRoots is the junction form of
// TestExternalSourcePathConfinedThroughLinks for the allowed roots. The roots
// reach confineExternalSource verbatim from GOROOT and GOMODCACHE, so a root
// spelled through a junction must resolve the same way its candidates do:
// ResolveRoot kept the junction in the root's spelling, and every declaration
// beneath such a root was rejected as outside it.
func TestExternalSourcePathConfinedUnderJunctionedRoots(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "modcache")
	cached := filepath.Join(realRoot, "golang.org", "x@v1.0.0")
	require.NoError(t, os.MkdirAll(cached, 0o755))
	realFile := filepath.Join(cached, "real.go")
	require.NoError(t, os.WriteFile(realFile, []byte("package x\n\nfunc Real() {}\n"), 0o600))
	mustJunction(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))
	mustJunction(t, realRoot, filepath.Join(base, "direct"))

	// A junction inside the root that stays inside it, and one that
	// leaves it for an outside directory.
	mustJunction(t, cached, filepath.Join(realRoot, "in"))
	secretDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secretDir, "secret.go"), []byte("package secret\n\nfunc Hidden() {}\n"), 0o600))
	mustJunction(t, secretDir, filepath.Join(realRoot, "out"))

	for _, tc := range []struct {
		name string
		root string
	}{
		{"a root that is a junction", filepath.Join(base, "direct")},
		{"a root beneath a junction", filepath.Join(base, "alias", "modcache")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, file := range []struct {
				name string
				path string
			}{
				{"a plain declaration", filepath.Join(tc.root, "golang.org", "x@v1.0.0", "real.go")},
				{"a declaration through a junction inside the root", filepath.Join(tc.root, "in", "real.go")},
			} {
				srv, _, _ := newIsolationServer(t)
				confineToTestRoot(t, tc.root)
				srv.graph.AddNode(&graph.Node{
					ID: "ext::go:probe::Real", Kind: graph.KindFunction, Name: "Probe",
					FilePath: "external::go:probe", Language: "go",
					Meta: map[string]any{
						"external":    true,
						"module_kind": "module_cache",
						"source_file": file.path,
						"source_line": 3,
					},
				})
				res, err := srv.handleGetSymbolSource(t.Context(),
					makeReq("get_symbol_source", map[string]any{"id": "ext::go:probe::Real", "format": "json"}))
				require.NoError(t, err)
				source, _ := extractTextResult(t, res)["source"].(string)
				require.Contains(t, source, "func Real()", "%s beneath %s must be served", file.name, tc.name)
			}

			srv, _, _ := newIsolationServer(t)
			confineToTestRoot(t, tc.root)
			srv.graph.AddNode(&graph.Node{
				ID: "ext::go:probe::Ancestor", Kind: graph.KindFunction, Name: "Probe",
				FilePath: "external::go:probe", Language: "go",
				Meta: map[string]any{
					"external":    true,
					"module_kind": "module_cache",
					"source_file": filepath.Join(tc.root, "out", "secret.go"),
					"source_line": 3,
				},
			})
			res, err := srv.handleGetSymbolSource(t.Context(),
				makeReq("get_symbol_source", map[string]any{"id": "ext::go:probe::Ancestor", "format": "json"}))
			require.NoError(t, err)
			out := extractTextResult(t, res)
			require.Nil(t, out["source"], "a junction out of %s must not be served", tc.name)
			require.NotEmpty(t, out["source_unavailable_reason"])
			require.NotContains(t, resultText(res), "Hidden")
		})
	}
}
