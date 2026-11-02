//go:build !gortex_edit_gate_seed

package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWALBurstSeedPolicyAbsentWithoutTag(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "closed-read-gate")
	s, db := finalBackfillFixture(t)
	write := s.AnnounceWrite()
	read := s.AnnounceInteractiveRead()
	defer write()
	defer read()
	cfg := walReclaimConfig{thresholdBytes: -1, readerWait: 0, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}
	res := &walReclaimResult{urgent: true}
	require.ErrorIs(t, s.reclaimWALInLane(t.Context(), cfg, db, res), errWALReclaimWriterWaiting)
	require.False(t, res.pauseClosed)
	write()
	require.ErrorIs(t, s.reclaimWALInLane(t.Context(), cfg, db, res), errWALReclaimInteractiveRead)
	require.False(t, res.pauseClosed)
	require.Zero(t, s.WALReclaimStats().WriterHold.Count(), "a production build borrowed the tagged bad policy")
}
