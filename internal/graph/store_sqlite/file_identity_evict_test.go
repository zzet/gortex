package store_sqlite

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileIdentityEvictionIsGenerationScoped(t *testing.T) {
	store, _ := openTempStore(t)
	path, legacy := "r/sub/a.py", `r/sub\a.py`
	for _, generation := range []int64{0, 7} {
		store.AtGeneration(generation).AddBatch([]*graph.Node{
			{ID: path, Kind: graph.KindFile, FilePath: path},
			{ID: legacy, Kind: graph.KindFile, FilePath: legacy},
			{ID: legacy + "::Helper", Kind: graph.KindFunction, FilePath: legacy},
		}, []*graph.Edge{{From: path, To: legacy + "::Helper", Kind: graph.EdgeCalls, FilePath: path}})
	}
	n, e := store.AtGeneration(7).EvictFilesWithNodeIDs([]string{path}, []string{legacy, legacy + "::Helper"})
	require.Equal(t, 3, n)
	require.Equal(t, 1, e)
	for _, id := range []string{path, legacy, legacy + "::Helper"} {
		require.Nil(t, store.AtGeneration(7).GetNode(id))
		require.NotNil(t, store.GetNode(id), "another payload generation must remain intact")
	}
	require.Len(t, store.GetOutEdges(path), 1)
}

func TestFileIdentityEvictionUsesBoundedIndexedFrontiers(t *testing.T) {
	store, _ := openTempStore(t)
	scoped, args := fileIdentityEvictionScope(evictFilesPredicate+` AND view_gen = ?`, []any{`["r/sub/a.py"]`, int64(0)}, `["r/sub\\a.py","r/sub\\a.py::Helper"]`, 0)
	plan := strings.Join(explainQueryPlanArgs(t, store, `SELECT id FROM nodes WHERE `+scoped, args...), "\n")
	require.NotContains(t, plan, "SCAN nodes", "neither file frontier nor exact legacy IDs may scan the graph")
	require.NotContains(t, plan, "UNION USING TEMP B-TREE")
	require.Contains(t, plan, "file_path=?")
	require.Contains(t, plan, "id=?")
}
