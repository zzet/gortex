package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// Exclude policy refreshes compare ConfigManager's shared snapshot by
// identity, so both admission paths must hand the indexer that snapshot.
func TestTrackedIndexerUsesTheSharedExcludeSnapshot(t *testing.T) {
	for _, path := range []string{"index_all", "track"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root+"/keep.go", "package app\n\nfunc Kept() {}\n")
			cm := newTestConfigManager(t)
			mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			entry := config.RepoEntry{Path: root, Name: "app"}
			if path == "index_all" {
				cm.Global().Repos = []config.RepoEntry{entry}
				_, err := mi.IndexAll()
				require.NoError(t, err)
			} else {
				_, err := mi.TrackRepoCtx(context.Background(), entry)
				require.NoError(t, err)
			}
			idx := mi.GetIndexer("app")
			require.NotNil(t, idx)
			shared := cm.EffectiveExclude("app")
			got := idx.config.Exclude
			require.NotEmpty(t, shared)
			require.Len(t, got, len(shared))
			require.True(t, &got[0] == &shared[0], "indexer exclude list is a private copy, not the shared snapshot")
		})
	}
}
