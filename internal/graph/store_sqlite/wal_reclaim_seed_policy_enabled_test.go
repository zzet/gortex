//go:build gortex_edit_gate_seed

package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The existing edit-gate seed deliberately selects the bad closed-admission
// policy. It must retain real interactive/write demand and produce a real
// pause, rather than clearing markers or manufacturing a report sample.
func TestWALBurstTaggedSeedRetainsRealReadGateFault(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "closed-read-gate")
	s, db := finalBackfillFixture(t)
	write := s.AnnounceWrite()
	defer write()
	read := s.AnnounceInteractiveRead()
	defer read()
	require.True(t, s.editIntentActive())
	cfg := walReclaimConfig{thresholdBytes: -1, readerWait: 0, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}
	res := &walReclaimResult{urgent: true, bytesBefore: s.observedWALFileSize(s.dbPath + "-wal")}
	require.NoError(t, s.reclaimWALInLane(t.Context(), cfg, db, res))
	require.True(t, res.pauseClosed, "tagged policy did not actually close the real read gate")
	require.Positive(t, res.pause)
	require.Positive(t, s.WALReclaimStats().WriterHold.Count())
	require.True(t, s.editIntentActive(), "the seed must never clear real write ownership")
	require.EqualValues(t, 1, s.walReclaim.interactive.active.Load(), "the seed must never clear interactive ownership")
	require.Zero(t, s.WALReclaimStats().BulkExceptionHolds)
	require.Zero(t, s.WALReclaimStats().HardCapExceptionHolds)
}
