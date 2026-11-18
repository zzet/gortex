package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// A bounded retirement that commits a quantum and yields with work left is
// progress, not a refusal: a generation drained over many quanta leaves the
// error label untouched, counts each yield as yielded, and its throughput
// lands on the quanta and rows counters.
func TestRetirementQuantumYieldsAreNotCountedAsErrors(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	buildMinimalAnalysisGeneration(t, s.AtGeneration(generationID), "labels", 12, true)
	before := viewmetrics.Read()
	var quanta, rows, yields int64
	removed := false
	for attempt := 0; attempt < 512 && !removed; attempt++ {
		progress, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
		quanta += progress.ChunksCommitted
		rows += progress.RowsDeleted
		if progress.CatalogRemoved {
			require.NoError(t, err)
			removed = true
			break
		}
		require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
		yields++
	}
	require.True(t, removed, "the generation did not retire")
	require.Greater(t, yields, int64(2), "the fixture must take several quanta")
	after := viewmetrics.Read()
	require.Zero(t, retireRefusalDelta(before, after, viewmetrics.RefusedError), "a yield was counted as an error")
	require.Equal(t, yields, retireRefusalDelta(before, after, viewmetrics.RetireYielded))
	require.EqualValues(t, 1, retiredDelta(before, after, viewmetrics.OwnerCheckout))
	require.Equal(t, quanta, after.Counters[viewmetrics.RetirementQuantaTotal]-before.Counters[viewmetrics.RetirementQuantaTotal])
	require.Equal(t, rows, after.Counters[viewmetrics.RetirementRowsDeletedTotal]-before.Counters[viewmetrics.RetirementRowsDeletedTotal])
}

// A quantum a waiting writer or an expired deadline cuts (its transaction
// rolled back, automatically or not) is a preemption; only a real failure is
// an error.
func TestRetirementStopReasonSeparatesPreemptionFromError(t *testing.T) {
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	require.Equal(t, viewmetrics.RetirePreempted, retireStopReason(expired, expired.Err()))
	require.Equal(t, viewmetrics.RetirePreempted, retireStopReason(expired, fmt.Errorf("quantum commit: %w", sql.ErrTxDone)))
	require.Equal(t, viewmetrics.RetirePreempted, retireStopReason(t.Context(), ErrPayloadRetirementWriteWanted))
	require.Equal(t, viewmetrics.RetireYielded, retireStopReason(t.Context(), fmt.Errorf("%w: retirement WAL pause", ErrPayloadSweepBudgetExhausted)))
	require.Equal(t, viewmetrics.RefusedError, retireStopReason(t.Context(), sql.ErrTxDone), "a live transaction failure stays an error")
	require.Equal(t, viewmetrics.RefusedError, retireStopReason(t.Context(), errors.New("disk I/O error")))
}
