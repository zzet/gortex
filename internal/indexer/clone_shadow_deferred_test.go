package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeferredShadowColdCloneRowsMatchStructuralReplay(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	// MultiIndexer cold indexing defers resolution and global passes until
	// every shadow has drained. The compact corpus must survive that handoff.
	f := newContractJobFixture(t, map[string]string{"main.go": cloneRepoSource})
	rows, err := f.store.CloneCorpusPage("fixture", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.True(t, row.Finalized)
		require.NotEmpty(t, row.Signature)
		require.NotEmpty(t, row.Shingles)
		require.Equal(t, row.Signature, f.store.GetNode(row.NodeID).Meta[cloneSigMetaKey])
	}
	before := cloneRowsByIdentity(f.store)
	require.Len(t, before, 2, "cold indexing must produce the symmetric clone pair")
	// Use the actual structural replay fixture door; this checks the same
	// unchanged bodies against the cold corpus rather than filtering a family.
	structuralRestatementSave(t, f, "main.go", cloneRepoSource+"\n// EOF comment\n", 0)
	after, err := f.store.CloneCorpusPage("fixture", "", 10)
	require.NoError(t, err)
	require.Equal(t, rows, after)
	require.Equal(t, before, cloneRowsByIdentity(f.store))
}

func cloneRowsByIdentity(store graph.Store) map[graph.EdgeIdentity]*graph.Edge {
	rows := make(map[graph.EdgeIdentity]*graph.Edge)
	for _, edge := range similarToEdges(store) {
		rows[graph.EdgeIdentityFor(edge)] = edge
	}
	return rows
}
