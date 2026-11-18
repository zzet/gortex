package store_sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Receipt presence suppresses lower keys, including a deletion with no keys.
// The existence proof needs no payload: keep it on the existing narrow index.
// Compare all mask/receipt bytes against an independently written selected
// reference, for both paged and one-shot callers of the shared operation.
func TestChainFoldReceiptKeySuppressionUsesCoveringStamps(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	lower := reservedGeneration(t, s, "receipt-key-lower")
	upper := reservedGeneration(t, s, "receipt-key-upper")
	live := boundaryStorageReceipt("é.go", "live")
	live.CheckoutID = "owner"
	live.Payload = []byte(`{"records":[],"evidence":"` + strings.Repeat("x", 1<<20) + `"}`)
	for i := 0; i < 130; i++ {
		live.LookupKeys = append(live.LookupKeys, fmt.Sprintf("*::type::N%03d", i))
	}
	gone := boundaryStorageReceipt("gone.go", "gone-old")
	gone.CheckoutID = "owner"
	changed := boundaryStorageReceipt("changed.go", "changed-old")
	changed.CheckoutID = "owner"
	changed.Accepted = true
	other := live
	other.CheckoutID = "other"
	other.Fingerprint = "other-owner"
	require.NoError(t, s.AtGeneration(lower).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{live, gone, changed, other}))
	deleted := gone
	deleted.Deleted = true
	deleted.Payload = nil
	deleted.LookupKeys = nil
	deleted.ProducedKeys = nil
	deleted.Fingerprint = "gone-now"
	replacement := changed
	replacement.Fingerprint = "changed-now"
	replacement.SourceFingerprint = "new-source"
	replacement.Accepted = false
	replacement.LookupKeys = []string{"*::type::New"}
	replacement.Previous = &changed
	require.NoError(t, s.AtGeneration(upper).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{deleted, replacement}))
	for _, id := range []int64{lower, upper} {
		publishContractCoreForTest(t, s, id)
	}
	reference := reservedGeneration(t, s, "receipt-key-selected-reference")
	require.NoError(t, s.AtGeneration(reference).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{live, deleted, replacement, other}))
	expected := renderMaskOperationRows(t, s, reference)
	require.NotEmpty(t, expected)
	// Plan the actual shared production statement; this also covers a migrated
	// store where stale planner statistics would otherwise favor BLOB records.
	require.Contains(t, planOfUnbound(t, s, foldMaskOperations[1].query), "USING COVERING INDEX contract_boundary_receipt_stamps")
	oneShot := reservedGeneration(t, s, "receipt-key-one-shot")
	_, err := s.FlattenGenerationChain(ctx, []int64{lower, upper}, oneShot)
	require.NoError(t, err)
	require.Equal(t, expected, renderMaskOperationRows(t, s, oneShot))
	maskQuantumBudget(t)
	paged := reservedGeneration(t, s, "receipt-key-paged")
	fold, err := s.BeginChainFold(ctx, ChainFoldRequest{Chain: []int64{lower, upper}, To: paged, Owner: "receipt-key-test"})
	require.NoError(t, err)
	defer func() { require.NoError(t, fold.Release(context.WithoutCancel(ctx))) }()
	runFold(t, fold, nil)
	require.Equal(t, expected, renderMaskOperationRows(t, s, paged), "Unicode, owner identity, deleted rows, pending predecessor and all paged keys survive exactly")
}
