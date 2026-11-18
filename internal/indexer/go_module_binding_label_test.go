package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// Given a call bound into another tracked repository by module path, when it
// binds while the caller is indexed, while its provider is indexed, or again
// after the provider restores the symbol, then it carries the same confidence
// label every time, as do its parallel and the cold index.
func TestGoModuleBindingLabelMatchesAcrossPaths(t *testing.T) {
	want := graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6)
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{true, false} {
			name := backend + "/caller_first"
			if providerFirst {
				name = backend + "/provider_first"
			}
			t.Run(name, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				order := []config.RepoEntry{repos[0], repos[1]}
				if providerFirst {
					order[0], order[1] = order[1], order[0]
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				labels := func(step string) {
					t.Helper()
					seen := 0
					for _, e := range g.GetOutEdges("a/main.go::Caller") {
						if e.To == "b/lib.go::Hello" {
							seen++
							assert.Equal(t, want, e.ConfidenceLabel, "%s: %s", step, e.Kind)
						}
					}
					assert.Equal(t, 2, seen, "%s: call and parallel", step)
				}
				labels("tracked")
				lib := filepath.Join(repos[1].Path, "lib.go")
				writeFile(t, lib, "package library\nfunc Hello2() {}\n")
				require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
				writeFile(t, lib, "package library\nfunc Hello() {}\n")
				require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
				labels("rebound")
			})
		}
	}
}
