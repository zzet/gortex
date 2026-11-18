package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The workload is fixed and reclamation is disabled: the assertions concern
// actual admission counters and durable state, never elapsed-time thresholds.
func emptyRetirementWALFixture(t *testing.T) (*Store, int64, *Store) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s := openPayloadStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	seedPayloadControlPlane(t, s)
	req := payloadRequest()
	req.GenerationKind = "contract_analysis"
	id, payload, err := s.BeginPayloadGeneration(t.Context(), req)
	require.NoError(t, err)
	require.Greater(t, s.retirementLogBytes(), int64(0), "a real WAL is present")
	s.walReclaim.update(func(st *WALReclaimStats) { st.ThresholdBytes, st.CeilingBytes = 1, 1 })
	beforeMax, beforeEdit := retirementWALWaitMax, retirementWALWaitEditingMax
	retirementWALWaitMax, retirementWALWaitEditingMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { retirementWALWaitMax, retirementWALWaitEditingMax = beforeMax, beforeEdit })
	return s, id, payload
}

func TestEmptyPayloadRetirementDoesNotWaitForWAL(t *testing.T) {
	s, id, _ := emptyRetirementWALFixture(t)
	before := s.WALReclaimStats()
	require.NoError(t, s.RetirePayloadGeneration(t.Context(), id, nil))
	_, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, err)
	require.False(t, found)
	after := s.WALReclaimStats()
	require.Equal(t, before.RetirementWaits, after.RetirementWaits, "an empty reservation adds no payload WAL")
	require.Equal(t, before.RetirementWaitTimeouts, after.RetirementWaitTimeouts)
}

func TestNonemptyPayloadRetirementKeepsWALWait(t *testing.T) {
	for _, kind := range []string{"nodes", "file_metadata", "file_mask", "receipt", "analysis"} {
		t.Run(kind, func(t *testing.T) {
			s, id, payload := emptyRetirementWALFixture(t)
			switch kind {
			case "analysis":
				buildMinimalAnalysisGeneration(t, payload, "reservation", 2, true)
			case "nodes":
				require.NoError(t, payload.AddBatchChecked([]*graph.Node{{ID: "repo/a.go::A", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"}}, nil))
			case "file_metadata":
				require.NoError(t, payload.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/a.go", ContentHash: "source", Size: 1}}))
			case "file_mask":
				require.NoError(t, payload.SetFileMasks([]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipDelete}}))
			case "receipt":
				require.NoError(t, payload.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{boundaryStorageReceipt("a.go", "a")}))
			}
			before := s.WALReclaimStats()
			require.NoError(t, s.RetirePayloadGeneration(t.Context(), id, nil))
			require.Greater(t, s.WALReclaimStats().RetirementWaits, before.RetirementWaits, "real payload retains the existing reclamation policy")
			_, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}

func TestEmptyPayloadRetirementPreservesLeaseRefusal(t *testing.T) {
	s, id, _ := emptyRetirementWALFixture(t)
	require.ErrorIs(t, s.RetirePayloadGeneration(t.Context(), id, func(g int64) bool { return g == id }), ErrPayloadGenerationInUse)
	row, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ViewGenerationBuilding, row.State)
	require.NoError(t, s.RetirePayloadGeneration(t.Context(), id, nil))
}
