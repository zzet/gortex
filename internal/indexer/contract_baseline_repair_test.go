package indexer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractBaselineRepairsStaleReceiptsFromAcceptedCore(t *testing.T) {
	for _, mode := range []string{"unknown", "unaccepted", "policy_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			idx, store := newSQLiteIndexer(t)
			idx.SetRepoPrefix("fixture")
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nconst route = \"/constant-route\"\nfunc register(router *Router) { router.GET(route, serve) }\nfunc serve() {}\n")
			backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
			require.NoError(t, err)
			idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
			require.NoError(t, err)
			result, err := idx.IndexCtx(ctx, root)
			require.NoError(t, err)
			require.Empty(t, result.FailedFiles)
			original, _, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
			require.NoError(t, err)
			require.NotNil(t, original)
			require.True(t, original.Accepted)
			mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
			mi.repos["fixture"] = &RepoMetadata{RepoPrefix: "fixture", RootPath: root}
			mi.indexers["fixture"] = idx
			leases := graphview.NewLeaseManager()
			registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: root, Incarnation: mode}, nil)
			require.NoError(t, err)
			_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-core-source")
			require.NoError(t, err)
			materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}
			options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
			prior := contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "fixture/routes.go", Source: "old-source", Policy: "old-policy"}
			if mode == "unknown" {
				prior.Version = "unknown-legacy-version"
			}
			payload, err := json.Marshal(prior)
			require.NoError(t, err)
			bad := graph.ContractBoundaryReceipt{RepoPrefix: "fixture", FilePath: prior.FilePath, Version: contractBoundaryReceiptVersion, Fingerprint: "old-receipt", SourceFingerprint: prior.Source, Payload: payload, Scope: graph.ContractWorkScope{Unknown: true}}
			require.NoError(t, store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}))
			if mode != "unaccepted" {
				require.NoError(t, store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}))
			}
			require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, "fixture"))
			state, found, err := store.ContractInputStateContext(ctx, "fixture", "")
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, state.Accepted)
			baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, "fixture", "")
			require.NoError(t, err)
			require.NotNil(t, baseline)
			require.Equal(t, contractBoundaryReceiptVersion, baseline.Version)
			repaired, _, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", prior.FilePath)
			require.NoError(t, err)
			require.True(t, repaired.Accepted)
			require.NotEqual(t, bad.SourceFingerprint, repaired.SourceFingerprint)
			require.JSONEq(t, string(original.Payload), string(repaired.Payload), "baseline must retain real accepted-parser constant route and produced lookup facts")
			// The same invalid proof remains forbidden on the strict path.
			require.NoError(t, store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}))
			if mode != "unaccepted" {
				require.NoError(t, store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}))
			}
			selected, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
			require.NoError(t, err)
			_, _, err = NewContractFollowupCapture(options)(ctx, nil, selected, "fixture", "")
			require.Error(t, err)
		})
	}
}
