package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// selectedBackendOverBuildingTarget is the production shape of a working-tree
// delta's contract journal: a selected backend over a building generation
// whose predecessor (physical0 here) holds an accepted receipt for path.
func selectedBackendOverBuildingTarget(t *testing.T, path string) (*store_sqlite.Store, *contractCoreStorageBackend) {
	t.Helper()
	ctx := context.Background()
	_, store := newSQLiteIndexer(t)
	primary, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	seed := retryReceiptChange(path, "v0", nil)
	require.NoError(t, primary.BeginBoundaryMutation(ctx, []contractCoreInputChange{seed}))
	require.NoError(t, primary.AcceptBoundaryMutation(ctx))
	generation, target, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-retry", LayerID: "retry", CheckoutID: "linked",
		GenerationKind: DirtyLayerGenerationKind, TreeOID: "tree-retry", CreatedAt: 10,
	})
	require.NoError(t, err)
	require.Positive(t, generation)
	backend, err := newSelectedContractCoreStorageBackend(ctx, target, store, "fixture", "linked")
	require.NoError(t, err)
	return target, backend
}

// retryReceiptChange is one accepted parse of path whose handler input is
// version: every version is a contract-relevant change against the last.
func retryReceiptChange(path, version string, prior *contractBoundaryReceipt) contractCoreInputChange {
	current := &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: path,
		Source: "source-" + version, Policy: "policy", HandlerInputs: map[string]string{"handler": version}}
	return contractCoreInputChange{FilePath: path, SourceFingerprint: current.Source, Prior: prior,
		Current: current, Delta: diffContractBoundaryReceipts(prior, current)}
}

// stageRetryReceipt is the journal's prepare (the prior read that registers
// the carry source) followed by its begin.
func stageRetryReceipt(t *testing.T, backend *contractCoreStorageBackend, path, version string) (contractCoreInputChange, error) {
	t.Helper()
	ctx := context.Background()
	prior, known, err := backend.PriorBoundaryReceipt(ctx, path)
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, prior, "the predecessor's accepted receipt is the prior of every parse in the build")
	change := retryReceiptChange(path, version, prior)
	return change, backend.BeginBoundaryMutation(ctx, []contractCoreInputChange{change})
}

// A file re-staged in the same building generation (the per-file pass's retry
// after a version race, or a re-derive pass) replaces its own receipt; the
// predecessor's receipt is carried into the target once, never onto the row
// the first stage wrote.
func TestContractCoreSelectedBackendRestagesAPathItAlreadyStaged(t *testing.T) {
	ctx := context.Background()
	const path = "fixture/retry.go"
	target, backend := selectedBackendOverBuildingTarget(t, path)
	installed := *backend.expected
	first, err := stageRetryReceipt(t, backend, path, "v1")
	require.NoError(t, err)
	second, err := stageRetryReceipt(t, backend, path, "v2")
	require.NoError(t, err, "a re-stage of a staged path must not be refused by the carry guard")
	require.NoError(t, backend.AcceptBoundaryMutation(ctx))

	want, err := contractCoreStoredReceipt("fixture", "linked", second)
	require.NoError(t, err)
	row, known, err := target.ContractBoundaryReceiptContext(ctx, "fixture", "linked", path)
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, row)
	require.Equal(t, want.Fingerprint, row.Fingerprint, "the target row is the second receipt")
	require.Equal(t, want.SourceFingerprint, row.SourceFingerprint)
	require.True(t, row.Accepted)
	require.Nil(t, row.Previous, "an accepted positive row retains no pending predecessor")

	afterFirst, changed, err := nextContractCoreInputState(&installed, "fixture", "linked", []contractCoreInputChange{first})
	require.NoError(t, err)
	require.True(t, changed)
	afterSecond, changed, err := nextContractCoreInputState(&afterFirst, "fixture", "linked", []contractCoreInputChange{second})
	require.NoError(t, err)
	require.True(t, changed)
	state, found, err := target.ContractInputStateContext(ctx, "fixture", "linked")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, afterSecond.InputFingerprint, state.InputFingerprint, "the second stage extends the input chain")
	require.NotEqual(t, afterFirst.InputFingerprint, state.InputFingerprint)
}

// The cold (full-namespace) branch buffers rows and carry sources per chunk.
// A re-stage inside the unflushed chunk, or after it flushed, carries the
// predecessor once and writes the newest receipt.
func TestContractCoreSelectedBackendColdRestageCarriesOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flushFirst bool
	}{{"unflushed", false}, {"flushed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const path = "fixture/retry.go"
			target, backend := selectedBackendOverBuildingTarget(t, path)
			require.NoError(t, backend.beginFullCoreNamespace(ctx))
			_, err := stageRetryReceipt(t, backend, path, "v1")
			require.NoError(t, err)
			if tc.flushFirst {
				backend.mu.Lock()
				err = backend.flushColdRows(ctx)
				backend.mu.Unlock()
				require.NoError(t, err)
			}
			second, err := stageRetryReceipt(t, backend, path, "v2")
			require.NoError(t, err)
			require.NoError(t, backend.AcceptBoundaryMutation(ctx), "the flush carries the predecessor once")

			want, err := contractCoreStoredReceipt("fixture", "linked", second)
			require.NoError(t, err)
			row, _, err := target.ContractBoundaryReceiptContext(ctx, "fixture", "linked", path)
			require.NoError(t, err)
			require.NotNil(t, row)
			require.Equal(t, want.Fingerprint, row.Fingerprint, "the target row is the second receipt")
			require.True(t, row.Accepted)
		})
	}
}
