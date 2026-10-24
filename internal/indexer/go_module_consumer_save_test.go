package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A body-only consumer save reuses the cross-module call binding. The reuse
// must keep go_import, or the call is lost when its provider is untracked and
// not restored when it is tracked again.
func TestGoModuleConsumerSaveKeepsImportMarkerForRelink(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			for _, entry := range []int{1, 0} {
				_, err := mi.TrackRepoCtx(t.Context(), repos[entry])
				require.NoError(t, err)
			}
			check := func(stage string, parked bool) {
				t.Helper()
				var calls []*graph.Edge
				for _, edge := range g.GetOutEdges("a/main.go::Caller") {
					if edge.Kind == graph.EdgeCalls {
						calls = append(calls, edge)
					}
				}
				assert.Len(t, calls, 1, stage+": call must survive provider eviction")
				for _, edge := range calls {
					assert.Equal(t, "example.org/library::Hello", edge.Meta["go_import"], stage+": importer-owned marker must survive save/reuse")
					if parked {
						assert.NotEqual(t, "b/lib.go::Hello", edge.To, stage)
						assert.False(t, edge.CrossRepo, stage)
					} else {
						assert.Equal(t, "b/lib.go::Hello", edge.To, stage)
						assert.True(t, edge.CrossRepo, stage)
					}
				}
			}
			check("cold", false)
			callerPath := filepath.Join(repos[0].Path, "main.go")
			writeFile(t, callerPath, "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello(); _ = 1 }\n")
			saved, err := mi.IncrementalReindexRepo("a", []string{callerPath})
			require.NoError(t, err)
			require.Equal(t, 1, saved.StaleFileCount)
			check("consumer save", false)
			_, _, err = mi.UntrackRepoChecked(t.Context(), "b")
			require.NoError(t, err)
			check("provider evicted", true)
			_, err = mi.TrackRepoCtx(t.Context(), repos[1])
			require.NoError(t, err)
			check("provider retracked", false)
		})
	}
}
