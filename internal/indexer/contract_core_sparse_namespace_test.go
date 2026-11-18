package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Sparse builders use IndexCtx over a narrowed source. Entering that door must
// leave the selected per-file journal active, including additions and removals.
func TestContractCoreSparseNamespaceKeepsPerFileWork(t *testing.T) {
	for _, operation := range []string{"modified", "added", "deleted"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			const existing = "fixture/retry.go"
			target, backend := selectedBackendOverBuildingTarget(t, existing)
			before := *backend.expected
			require.NoError(t, backend.beginFullCoreNamespace(ctx))
			require.False(t, backend.cold, "a sparse walk must not enter full-census buffering")
			require.Equal(t, before, *backend.expected)
			work, err := target.PendingContractWorkForScopeContext(ctx, "fixture", "linked")
			require.NoError(t, err)
			require.Empty(t, work, "entering the walk adds no namespace debt")

			path := existing
			if operation == "added" {
				path = "fixture/new.go"
			}
			prior, _, err := backend.PriorBoundaryReceipt(ctx, path)
			require.NoError(t, err)
			if operation == "added" {
				require.Nil(t, prior)
			} else {
				require.NotNil(t, prior)
			}
			change := retryReceiptChange(path, "v1", prior)
			if operation == "deleted" {
				change.Current = nil
				change.Deleted = true
				change.Delta = diffContractBoundaryReceipts(prior, nil)
			}
			require.NoError(t, backend.BeginBoundaryMutation(ctx, []contractCoreInputChange{change}))
			require.NoError(t, backend.AcceptBoundaryMutation(ctx))

			state, found, err := target.ContractInputStateContext(ctx, "fixture", "linked")
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, state.Accepted)
			require.NotEqual(t, before.InputFingerprint, state.InputFingerprint)
			row, known, err := target.ContractBoundaryReceiptContext(ctx, "fixture", "linked", path)
			require.NoError(t, err)
			require.True(t, known)
			require.NotNil(t, row)
			require.True(t, row.Accepted)
			require.Equal(t, operation == "deleted", row.Deleted)
			want, err := contractCoreStoredReceipt("fixture", "linked", change)
			require.NoError(t, err)
			require.Equal(t, want.Fingerprint, row.Fingerprint)
			require.Equal(t, want.SourceFingerprint, row.SourceFingerprint)
			work, err = target.PendingContractWorkForScopeContext(ctx, "fixture", "linked")
			require.NoError(t, err)
			require.Len(t, work, 1)
			require.Equal(t, path, work[0].FilePath)
			require.Equal(t, target.ViewGeneration(), work[0].OriginGeneration)
			require.Equal(t, state.InputFingerprint, work[0].InputFingerprint)
			require.Equal(t, graph.ContractWorkPending, work[0].State)
			require.Equal(t, operation == "deleted", work[0].Scope.Deleted)
			require.Contains(t, work[0].Scope.Causes, "handler_inputs_changed")
			require.NotContains(t, work[0].Scope.Causes, "full_core_census_pending")
			baseline, err := target.ContractBoundaryReceiptBaselineContext(ctx, "fixture", "linked")
			require.NoError(t, err)
			require.Nil(t, baseline, "a sparse walk cannot certify a whole namespace")
		})
	}
}

func TestContractCoreFullNamespaceStillBlocksBeforeCoreWrites(t *testing.T) {
	for _, producer := range []string{"primary", "claimed_full"} {
		t.Run(producer, func(t *testing.T) {
			ctx := context.Background()
			var backend *contractCoreStorageBackend
			var err error
			if producer == "primary" {
				_, store := newSQLiteIndexer(t)
				backend, err = newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
				require.NoError(t, err)
			} else {
				_, backend = selectedBackendOverBuildingTarget(t, "fixture/retry.go")
				backend.fullNamespace = true
			}
			require.NoError(t, backend.beginFullCoreNamespace(ctx))
			require.True(t, backend.cold)
			work, err := backend.store.PendingContractWorkForScopeContext(ctx, "fixture", backend.checkout)
			require.NoError(t, err)
			require.Len(t, work, 1)
			require.Equal(t, "fixture/", work[0].FilePath)
			require.True(t, work[0].Scope.Unknown)
			require.Contains(t, work[0].Scope.Causes, "full_core_census_pending")
			require.NoError(t, backend.beginFullCoreNamespace(ctx))
			again, err := backend.store.PendingContractWorkForScopeContext(ctx, "fixture", backend.checkout)
			require.NoError(t, err)
			require.Equal(t, work, again, "the same full pass blocks once")
		})
	}
}
