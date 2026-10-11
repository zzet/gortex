package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractRuntimeWarmRestoreCandidateAdmission(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\n")
			entry := config.RepoEntry{Path: root, Name: "repo"}
			cm := newTestConfigManager(t)
			cm.Global().Repos = []config.RepoEntry{entry}
			path := filepath.Join(t.TempDir(), "restart.sqlite")
			store, err := store_sqlite.Open(path)
			require.NoError(t, err)
			seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			if enabled {
				seed.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			}
			_, err = seed.IndexAll()
			require.NoError(t, err)
			prior := seed.GetIndexer("repo").FileMtimes()
			before := store.Stats()
			require.NoError(t, store.Close())
			store, err = store_sqlite.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			if enabled {
				restarted.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			}
			require.Nil(t, restarted.GetIndexer("repo"), "restore candidate is not a live executor yet")
			result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, restarted.GetIndexer("repo"))
			require.Zero(t, result.StaleFileCount)
			require.Zero(t, result.DeletedFileCount)
			require.False(t, result.FullRetrack)
			require.Empty(t, result.FailedFiles)
			require.Equal(t, before.TotalNodes, store.Stats().TotalNodes)
			require.Equal(t, before.TotalEdges, store.Stats().TotalEdges)
			// Removing the live executor must still refuse a point/reconcile
			// door, rather than granting topology-candidate authority to it.
			if enabled {
				idx := restarted.GetIndexer("repo")
				restarted.mu.Lock()
				delete(restarted.indexers, "repo")
				restarted.mu.Unlock()
				ran := false
				err = idx.withOutputGenerationSource(context.Background(), OutputEntryRepositoryReconcileLane, func(func(*OutputSourceContent)) error { ran = true; return nil })
				require.ErrorContains(t, err, "no live indexer")
				require.False(t, ran)
			}
		})
	}
}
