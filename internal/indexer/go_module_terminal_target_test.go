package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// Given two calls into one module on one line, one bound and one on the
// package terminal, when the provider's module is renamed, then the single
// terminal row records the call a cold index keeps there: the smallest target,
// whichever call sat on the terminal first.
func TestGoModuleRenameKeepsColdTerminalTarget(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, bound := range []string{"Hello", "World"} {
			t.Run(backend+"/bound_"+bound, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello(library.World()) }\n")
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc "+bound+"(...int) int { return 1 }\n")
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				manifest := filepath.Join(repos[1].Path, "go.mod")
				writeFile(t, manifest, "module example.org/renamed\n")
				_, err := mi.IncrementalReindexRepo("b", []string{manifest})
				require.NoError(t, err)
				cold := revisionColdGraph(t, backend, repos[:2])
				target := func(s graph.Store) []string {
					var out []string
					for _, e := range s.GetOutEdges("a/main.go::Caller") {
						if e.Kind == graph.EdgeCalls {
							v, _ := e.Meta["go_extern_target"].(string)
							out = append(out, e.To+" "+v)
						}
					}
					return out
				}
				want := []string{"external-call::dep::example.org/library dep::example.org/library::Hello"}
				require.Equal(t, want, target(cold))
				assert.Equal(t, want, target(g))
			})
		}
	}
}
