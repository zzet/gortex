package store_sqlite

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractReceiptAbsenceIsProvenBeforePayloadLookup(t *testing.T) {
	s := openCatalogStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	top := s.AtGeneration(11)
	row := boundaryStorageReceipt("large.go", "large")
	row.CheckoutID = "present"
	row.Payload = []byte(`{"records":[],"evidence":"` + strings.Repeat("v", 1<<20) + `"}`)
	require.NoError(t, top.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	var mu sync.Mutex
	var seen []string
	readStatementObserver = func(q string) {
		if strings.Contains(q, "generation_contract_boundary_receipt") && (strings.HasPrefix(strings.TrimSpace(q), "SELECT") || strings.HasPrefix(strings.TrimSpace(q), "WITH")) {
			mu.Lock()
			seen = append(seen, q)
			mu.Unlock()
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })
	absent, known, err := top.ContractBoundaryReceiptContext(t.Context(), "repo", "missing-owner", row.FilePath)
	require.NoError(t, err)
	require.Nil(t, absent)
	require.False(t, known, "another actor is not a certified baseline")
	mu.Lock()
	first := append([]string(nil), seen...)
	mu.Unlock()
	require.Len(t, first, 1, "one SQLite statement proves the key before loading any payload")
	plan := planOfUnbound(t, s, first[0])
	require.Contains(t, plan, "MATERIALIZE receipt_key")
	require.Contains(t, plan, "USING COVERING INDEX contract_boundary_receipt_stamps")
	absent, known, err = top.ContractBoundaryReceiptContext(t.Context(), "repo", "present", "repo/missing.go")
	require.NoError(t, err)
	require.Nil(t, absent)
	require.False(t, known)
	require.NoError(t, top.SetContractBoundaryReceiptBaselineContext(t.Context(), graph.ContractBoundaryReceiptBaseline{RepoPrefix: "repo", CheckoutID: row.CheckoutID, Version: row.Version, Fingerprint: "baseline"}))
	absent, known, err = top.ContractBoundaryReceiptContext(t.Context(), "repo", "present", "repo/missing.go")
	require.NoError(t, err)
	require.Nil(t, absent)
	require.True(t, known, "baseline certification remains distinct from key presence")
	present, known, err := top.ContractBoundaryReceiptContext(t.Context(), "repo", "present", row.FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, row, *present, "present evidence is still fully decoded and validated")
	none, known, err := s.AtGeneration(12).ContractBoundaryReceiptContext(t.Context(), "repo", "present", row.FilePath)
	require.NoError(t, err)
	require.Nil(t, none)
	require.False(t, known, "physical generations do not inherit receipt rows or baselines")
	_, known, err = top.ContractBoundaryReceiptForVersionContext(t.Context(), "repo", "present", "repo/missing.go", "different-version")
	require.NoError(t, err)
	require.False(t, known)
	none, known, err = top.ContractBoundaryReceiptForVersionContext(t.Context(), "repo", "present", row.FilePath, "different-version")
	require.NoError(t, err)
	require.Nil(t, none)
	require.False(t, known, "a present wrong-version receipt is not an accepted absence")
	require.NoError(t, top.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	accepted, known, err := top.ContractBoundaryReceiptContext(t.Context(), "repo", "present", row.FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, accepted.Accepted)
	require.Equal(t, row.Payload, accepted.Payload)
	deleted := row
	deleted.Fingerprint = "deleted"
	deleted.SourceFingerprint = "absent"
	deleted.Deleted = true
	deleted.Payload = nil
	require.NoError(t, top.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{deleted}))
	tombstone, known, err := top.ContractBoundaryReceiptContext(t.Context(), "repo", "present", row.FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, tombstone)
	require.True(t, tombstone.Deleted, "a present tombstone is never converted to key absence")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	none, known, err = top.ContractBoundaryReceiptContext(ctx, "repo", "present", row.FilePath)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, none)
	require.False(t, known)
}
