package store_sqlite

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// widePayloadFixture is a failed generation carrying many node rows.
func widePayloadFixture(t *testing.T, nodes int) (*Store, int64) {
	t.Helper()
	s, _ := quantumPayloadFixture(t)
	request := payloadRequest()
	request.LayerID, request.ConfigHash = "wide", "wide"
	generationID, handle, err := s.BeginPayloadGeneration(t.Context(), request)
	require.NoError(t, err)
	batch := make([]*graph.Node, 0, nodes)
	for i := 0; i < nodes; i++ {
		batch = append(batch, &graph.Node{ID: fmt.Sprintf("wide/f%d.go::F%d", i%50, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("F%d", i), FilePath: fmt.Sprintf("wide/f%d.go", i%50)})
	}
	require.NoError(t, handle.AddBatchChecked(batch, nil))
	require.NoError(t, s.Catalog().SetViewGenerationState(t.Context(), generationID, ViewGenerationFailed, ViewGenerationBuilding))
	return s, generationID
}

// With nobody waiting, a wide quantum deletes learned-size chunks and keeps
// going within one transaction; with a writer queued at its start it is the
// ordinary 16-row quantum.
func TestWideRetirementQuantumDeletesMoreWhenNobodyWaits(t *testing.T) {
	s, generationID := widePayloadFixture(t, 4000)
	// Fence with an ordinary quantum.
	_, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	var wideRows int64
	for attempt := 0; attempt < 64 && wideRows == 0; attempt++ {
		progress, err := s.RetirePayloadGenerationQuantumFenced(WithWideRetirementQuantum(t.Context()), generationID, nil)
		require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
		require.EqualValues(t, 1, progress.ChunksCommitted, "a wide quantum is still one transaction")
		wideRows = progress.RowsDeleted
	}
	require.Greater(t, wideRows, int64(payloadSweepMinBatch), "a wide quantum deleted no more than a narrow one")
	require.LessOrEqual(t, wideRows, int64(payloadGenerationSweepBatch)*int64(payloadWideQuantumBudget/time.Millisecond+1))

	// A writer queued before the quantum keeps it narrow (and refuses it).
	s.writeMu.waiters.Add(1)
	progress, err := s.RetirePayloadGenerationQuantumFenced(WithWideRetirementQuantum(t.Context()), generationID, nil)
	s.writeMu.waiters.Add(-1)
	require.ErrorIs(t, err, ErrPayloadRetirementWriteWanted)
	require.Zero(t, progress.RowsDeleted)
	progress, err = s.RetirePayloadGenerationQuantumFenced(context.WithoutCancel(t.Context()), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.LessOrEqual(t, progress.RowsDeleted, int64(payloadSweepMinBatch), "a quantum without the wide mark must stay narrow")
}

// fencedWideFixture is a wide payload fixture fenced by one ordinary quantum.
func fencedWideFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, generationID := widePayloadFixture(t, 4000)
	_, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	return s, generationID
}

// wideQuantumUntilRows runs wide quanta under ctx until the walk has deleted
// a row (the observer reports it), and returns that quantum's outcome.
func wideQuantumUntilRows(t *testing.T, s *Store, ctx context.Context, generationID int64, deleted *atomic.Int64) (PayloadRetirementProgress, error) {
	t.Helper()
	var progress PayloadRetirementProgress
	var err error
	for attempt := 0; attempt < 64 && deleted.Load() == 0; attempt++ {
		progress, err = s.RetirePayloadGenerationQuantumFenced(ctx, generationID, nil)
	}
	require.Positive(t, deleted.Load(), "no wide quantum deleted a row")
	return progress, err
}

// The WAL pause crossed in the middle of a wide quantum ends it, and what it
// deleted commits: nobody waits on a wide quantum, so rolling its deletes back
// bought nothing, and the next quantum is refused by the pause either way.
func TestWideRetirementQuantumCommitsItsDeletesWhenTheWALPauseIsCrossed(t *testing.T) {
	s, generationID := fencedWideFixture(t)
	var wal, deleted atomic.Int64
	ctx := context.WithValue(WithWideRetirementQuantum(t.Context()), payloadQuantumLogBytesKey{}, wal.Load)
	ctx = context.WithValue(ctx, payloadQuantumChunkObserverKey{}, func(n int64) {
		if n > 0 {
			deleted.Add(n)
			wal.Store(payloadRetirementQuantumWALPause + 1)
		}
	})
	progress, err := wideQuantumUntilRows(t, s, ctx, generationID, &deleted)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.EqualValues(t, 1, progress.ChunksCommitted, "the deletes made before the WAL pause were rolled back")
	require.Equal(t, deleted.Load(), progress.RowsDeleted)

	progress, err = s.RetirePayloadGenerationQuantumFenced(ctx, generationID, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.Zero(t, progress.ChunksCommitted, "the WAL pause admitted the next quantum")
}

// A writer that queues in the middle of a wide quantum gets the writer back
// through an interrupt and a rollback, never by waiting for the rest of the
// quantum or its commit — the WAL pause crossed beside it included: nothing
// the quantum deleted commits, and a later quantum deletes it again.
func TestWideRetirementQuantumRollsBackForAWriterThatQueuesMidway(t *testing.T) {
	for _, crossWAL := range []bool{false, true} {
		t.Run(fmt.Sprintf("wal_pause_crossed=%v", crossWAL), func(t *testing.T) {
			s, generationID := fencedWideFixture(t)
			var wal, deleted atomic.Int64
			var queued atomic.Bool
			ctx := context.WithValue(WithWideRetirementQuantum(t.Context()), payloadQuantumLogBytesKey{}, wal.Load)
			ctx = context.WithValue(ctx, payloadQuantumChunkObserverKey{}, func(n int64) {
				if n > 0 && queued.CompareAndSwap(false, true) {
					deleted.Add(n)
					if crossWAL {
						wal.Store(payloadRetirementQuantumWALPause + 1)
					}
					s.writeMu.waiters.Add(1)
				}
			})
			defer func() {
				if queued.Load() {
					s.writeMu.waiters.Add(-1)
				}
			}()
			progress, err := wideQuantumUntilRows(t, s, ctx, generationID, &deleted)
			require.Error(t, err)
			require.Zero(t, progress.ChunksCommitted, "a quantum committed while a writer waited")
			require.Zero(t, progress.RowsDeleted)
			if !crossWAL {
				require.ErrorIs(t, err, ErrPayloadRetirementWriteWanted)
			}
		})
	}
}
