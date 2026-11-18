package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractCoreInstalledColdManifestReceiptsSurviveRestart(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "inline"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			expected := make(map[string]int64)
			for name, body := range map[string]string{
				"main.go": "package app\nfunc Main() {}\n",
				"go.mod":  "module example.test/app\n\ngo 1.24\nrequire example.test/dependency v1.0.0\n",
				"go.work": "go 1.24\nuse .\n",
			} {
				_, expected[name] = writeColdReceiptFile(t, root, name, body)
			}
			dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
			store, err := store_sqlite.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			cold := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			coldClosed := false
			t.Cleanup(func() {
				if !coldClosed {
					cold.Close()
				}
			})
			cold.SetRepoPrefix("repo")
			cold.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			cold.SetDeferResolve(deferred)
			cold.SetDeferGlobalPasses(deferred)
			_, err = cold.IndexCtx(ctx, root)
			require.NoError(t, err)
			if deferred {
				require.NotContains(t, store.LoadFileMtimes("repo"), "go.mod", "parser progress alone cannot certify deferred manifest work")
				cold.RunDeferredPasses(ctx)
			}
			require.Equal(t, expected, store.LoadFileMtimes("repo"))
			require.Nil(t, cold.pendingColdManifests)
			require.Nil(t, cold.contractRegistry, "source census completion must not re-enable synchronous contract analysis")
			require.Zero(t, cold.contractRegistryLoads)
			require.NotNil(t, store.GetNode("repo/go.mod"), "module core artifacts must be processed before certifying their source")
			pending, found, err := store.ContractInputStateContext(ctx, "repo", "")
			require.NoError(t, err)
			require.True(t, found)
			require.False(t, pending.Accepted, "fresh primary core receipts leave complete namespace certification to the baseline owner")
			mi := NewMultiIndexer(store, cold.registry, search.NewNull(), nil, zap.NewNop())
			mi.repos["repo"] = &RepoMetadata{RepoPrefix: "repo", RootPath: root}
			mi.indexers["repo"] = cold
			leases := graphview.NewLeaseManager()
			registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "repo", RootIdentity: root, Incarnation: t.Name()}, nil)
			require.NoError(t, err)
			_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-cold-manifest-source")
			require.NoError(t, err)
			mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
			options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}, MultiIndexer: mi, Registry: cold.registry, Config: cold.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
			require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, "repo"))
			before, found, err := store.ContractInputStateContext(ctx, "repo", "")
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, before.Accepted)
			rows := coldReceiptGraphRows(t, store)
			cold.Close()
			coldClosed = true
			require.NoError(t, store.Close())
			store, err = store_sqlite.Open(dbPath)
			require.NoError(t, err)
			warm := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			t.Cleanup(warm.Close)
			warm.SetRepoPrefix("repo")
			warm.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			warm.storeRootPath(root)
			warm.SetFileMtimes(store.LoadFileMtimes("repo"))
			changed, deleted, detected, err := warm.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			require.Empty(t, changed, "unchanged manifests must not manufacture a new accepted input on restart")
			require.Empty(t, deleted)
			require.Equal(t, len(expected), detected)
			result, err := warm.IncrementalReindexPaths(root, nil)
			require.NoError(t, err)
			require.Zero(t, result.StaleFileCount)
			after, found, err := store.ContractInputStateContext(ctx, "repo", "")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, before, after, "warm reconciliation must preserve the accepted input identity")
			require.Equal(t, rows, coldReceiptGraphRows(t, store))
		})
	}
}

func TestContractCoreInstalledDeferredManifestVersionChangeRemainsRetryable(t *testing.T) {
	for _, name := range []string{"go.mod", "go.work"} {
		t.Run(name, func(t *testing.T) {
			idx, store, root := coldReceiptFixture(t)
			writeColdReceiptFile(t, root, "main.go", "package app\nfunc Main() {}\n")
			writeColdReceiptFile(t, root, "go.mod", "module example.test/app\n\ngo 1.24\n")
			writeColdReceiptFile(t, root, "go.work", "go 1.24\nuse .\n")
			idx.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			idx.SetDeferResolve(true)
			idx.SetDeferGlobalPasses(true)
			_, err := idx.IndexCtx(context.Background(), root)
			require.NoError(t, err)
			captured := idx.pendingColdManifests.manifests[name].receipt.readVersion.mtime
			path := filepath.Join(root, name)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			require.NoError(t, err)
			_, err = file.WriteString("\n// changed after the cold read\n")
			require.NoError(t, file.Close())
			require.NoError(t, err)
			mtime := time.Unix(0, captured).Add(time.Second)
			require.NoError(t, os.Chtimes(path, mtime, mtime))
			idx.RunDeferredPasses(context.Background())
			require.NotContains(t, store.LoadFileMtimes("repo"), name)
			require.Contains(t, idx.fileIndexFailurePaths(), "repo/"+name)
			changed, _, _, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			require.Contains(t, changed, name)
			require.Nil(t, idx.pendingColdManifests)
			require.Nil(t, idx.contractRegistry)
		})
	}
}
