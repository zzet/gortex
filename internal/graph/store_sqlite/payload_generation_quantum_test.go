package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quantumPayloadFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s := openPayloadStore(t)
	// Twice the production quantum. The tests drive the commit margin with
	// an injected clock, but the quantum itself is a real-time deadline; a
	// busy CI runner gets headroom, and missing even double the budget means
	// an unacceptable slowdown.
	s.retirementQuantumOverride = 2 * payloadRetirementQuantumDuration
	seedPayloadBase(t, s)
	seedPayloadControlPlane(t, s)
	return s, publishedPayloadGeneration(t, s)
}

func TestPayloadRetirementQuantumResumesNestedAnalysisAndPreservesBase(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	base := baseSnapshot(t, s)
	baseAnalysis := buildMinimalAnalysisGeneration(t, s, "base", 3, true)
	handle := s.AtGeneration(generationID)
	first := buildMinimalAnalysisGeneration(t, handle, "first", 37, false)
	second := buildMinimalAnalysisGeneration(t, handle, "second", 35, true)
	started, walBefore := time.Now(), s.WALWriteMark()
	var rows, chunks int64
	var advanced int
	finished := false
	for attempt := 0; attempt < 512; attempt++ {
		progress, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
		require.LessOrEqual(t, progress.RowsDeleted, int64(16))
		require.LessOrEqual(t, progress.ChunksCommitted, int64(1))
		rows += progress.RowsDeleted
		chunks += progress.ChunksCommitted
		advanced += progress.StepsAdvanced
		if progress.CatalogRemoved {
			require.NoError(t, err)
			finished = true
			break
		}
		require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
		require.EqualValues(t, 1, progress.ChunksCommitted)
		state, found := generationState(t, s, generationID)
		require.True(t, found)
		require.Equal(t, ViewGenerationRetiring, state)
		if attempt == 20 {
			// A lost process-local hint costs re-walking empty sources, not
			// an orphaned child or a mutable-list ordinal skipping a manifest.
			resume := s.payloadSweepStateFor(generationID)
			resume.mu.Lock()
			resume.step = 0
			resume.mu.Unlock()
			resume.analysisPointerDone, resume.analysisID, resume.analysisPhase = false, 0, 0
		}
	}
	require.True(t, finished, "single-transaction quanta did not converge")
	require.Greater(t, rows, int64(72))
	require.Positive(t, chunks)
	require.Positive(t, advanced)
	for _, table := range payloadGenerationTables() {
		require.Zero(t, countAtGeneration(t, s, table, generationID), table)
	}
	for _, id := range []int64{first, second} {
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM analysis_generations WHERE generation_id=?`, id).Scan(&count))
		require.Zero(t, count)
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM analysis_nodes WHERE generation_id=?`, id).Scan(&count))
		require.Zero(t, count)
	}
	var retained int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM analysis_generations WHERE generation_id=?`, baseAnalysis).Scan(&retained))
	require.Equal(t, 1, retained)
	require.Equal(t, base, baseSnapshot(t, s))
	wal := WALWrittenBetween(walBefore, s.WALWriteMark())
	t.Logf("committed_rows=%d committed_payload_transactions=%d advanced_steps=%d elapsed=%s wal_delta_valid=%t wal_bytes=%d", rows, chunks, advanced, time.Since(started), wal.Valid, wal.Bytes)
}

func TestPayloadRetirementQuantumRefusalAndWriterDeadlineHaveNoProgress(t *testing.T) {
	for _, kind := range []string{"lease", "route", "expired", "held_writer"} {
		t.Run(kind, func(t *testing.T) {
			s, generationID := quantumPayloadFixture(t)
			before := countAtGeneration(t, s, "nodes", generationID)
			ctx := t.Context()
			var inUse func(int64) bool
			var wanted error
			switch kind {
			case "lease":
				inUse = func(int64) bool { return true }
				wanted = ErrPayloadGenerationInUse
			case "route":
				require.NoError(t, s.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
					CheckoutID: payloadCheckoutID, Slot: RouteSlotDirty,
					ExpectedRouteEpoch: 0, GenerationID: generationID, State: RouteActive,
				}))
				wanted = ErrCatalogGenerationReferenced
			case "expired", "held_writer":
				deadline := time.Now().Add(-time.Second)
				if kind == "held_writer" {
					s.writeMu.Lock()
					defer s.writeMu.Unlock()
					deadline = time.Now().Add(25 * time.Millisecond)
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
				wanted = context.DeadlineExceeded
			}
			progress, err := s.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
			require.ErrorIs(t, err, wanted)
			require.Equal(t, PayloadRetirementProgress{}, progress)
			require.Equal(t, before, countAtGeneration(t, s, "nodes", generationID))
			state, found := generationState(t, s, generationID)
			require.True(t, found)
			require.Equal(t, ViewGenerationReady, state)
		})
	}
}

func TestPayloadRetirementQuantumRollsBackForARealQueuedWriter(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	require.NoError(t, s.Catalog().BeginViewGenerationRetirement(t.Context(), generationID))
	s.setPayloadSeal(generationID, payloadSealRetired)
	before := countAtGeneration(t, s, "nodes", generationID)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	writerDone := make(chan error, 1)
	writerJoined := false
	defer func() {
		cancel()
		if !writerJoined {
			select {
			case <-writerDone:
			case <-time.After(5 * time.Second):
				t.Error("queued producer did not join during cleanup")
			}
		}
	}()
	go func() {
		select {
		case <-entered:
		case <-ctx.Done():
			writerDone <- ctx.Err()
			return
		}
		if err := s.writeMu.LockContext(ctx); err != nil {
			writerDone <- err
			return
		}
		_, err := s.writerDB.ExecContext(ctx, `UPDATE nodes SET name='writer-ran' WHERE view_gen=0 AND id=?`, payloadKept)
		s.writeMu.Unlock()
		writerDone <- err
	}()
	chunk := func(chunkCtx context.Context, tx *sql.Tx) (int64, error) {
		_, err := tx.ExecContext(chunkCtx, `DELETE FROM nodes WHERE view_gen=?`, generationID)
		if err != nil {
			return 0, err
		}
		close(entered)
		// Actual SQL has changed rows while writeMu is held. The queued
		// producer must cause cancellation and rollback before it acquires.
		<-chunkCtx.Done()
		return 0, chunkCtx.Err()
	}
	removed, _, err := s.deletePayloadChunkMode(ctx, generationID, chunk, true)
	require.ErrorIs(t, err, ErrPayloadRetirementWriteWanted)
	require.Zero(t, removed)
	select {
	case err := <-writerDone:
		writerJoined = true
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("queued producer did not finish after quantum rollback")
	}
	require.Equal(t, before, countAtGeneration(t, s, "nodes", generationID))
	require.Equal(t, "writer-ran", s.GetNode(payloadKept).Name)
	require.False(t, errors.Is(err, context.DeadlineExceeded))
}

func TestPayloadRetirementQuantumWALPauseDoesNotDeleteOrFence(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	before := countAtGeneration(t, s, "nodes", generationID)
	// The production wrapper supplies its actual since-reset WAL measurement.
	// Test the admission boundary without generating 256 MiB of fixture WAL.
	require.NoError(t, s.retirementQuantumAdmissionAt(t.Context(), 256<<20))
	require.ErrorIs(t, s.retirementQuantumAdmissionAt(t.Context(), (256<<20)+1), ErrPayloadSweepBudgetExhausted)
	require.True(t, s.walReclaimNudged.Load())
	require.Equal(t, before, countAtGeneration(t, s, "nodes", generationID))
	state, found := generationState(t, s, generationID)
	require.True(t, found)
	require.Equal(t, ViewGenerationReady, state)
}

func TestPayloadRetirementQuantumSerializesConcurrentResumeHints(t *testing.T) {
	s, generationID := quantumPayloadFixture(t)
	buildMinimalAnalysisGeneration(t, s.AtGeneration(generationID), "concurrent", 37, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		removed int
		err     error
	}
	done := make(chan result, 4)
	joined := 0
	defer func() {
		cancel()
		for joined < 4 {
			select {
			case <-done:
				joined++
			case <-time.After(5 * time.Second):
				t.Error("concurrent retirement did not join")
				return
			}
		}
	}()
	for worker := 0; worker < 4; worker++ {
		go func() {
			for attempt := 0; attempt < 512; attempt++ {
				progress, err := s.RetirePayloadGenerationQuantum(ctx, generationID, nil)
				if progress.CatalogRemoved {
					done <- result{removed: 1}
					return
				}
				if errors.Is(err, ErrCatalogNotFound) {
					done <- result{}
					return
				}
				// The quantum owns a shorter internal deadline than this bounded
				// concurrent driver. Resume that yield only while its parent is live.
				if !resumeConcurrentRetirementQuantum(ctx, err) {
					done <- result{err: err}
					return
				}
			}
			done <- result{err: errors.New("concurrent quantum did not converge")}
		}()
	}
	removed := 0
	for joined < 4 {
		select {
		case result := <-done:
			joined++
			require.NoError(t, result.err)
			removed += result.removed
		case <-ctx.Done():
			t.Fatal("concurrent retirement exceeded its bounded control")
		}
	}
	require.Equal(t, 1, removed)
	_, found := generationState(t, s, generationID)
	require.False(t, found)
}

func TestPayloadRetirementQuantumRejectsInvalidAndMaintenanceHandles(t *testing.T) {
	for _, s := range []*Store{nil, &Store{}} {
		progress, err := s.RetirePayloadGenerationQuantum(t.Context(), 1, nil)
		require.ErrorIs(t, err, ErrCatalogInvalidValue)
		require.Equal(t, PayloadRetirementProgress{}, progress)
	}
	s, _ := quantumPayloadFixture(t)
	progress, err := s.RetirePayloadGenerationQuantum(t.Context(), 0, nil)
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	require.Equal(t, PayloadRetirementProgress{}, progress)
	_, minted := s.payloadSeals.Load(int64(0))
	require.False(t, minted)
}

func TestPayloadRetirementQuantumSharesLifetimeWithLegacyRetirement(t *testing.T) {
	for _, mode := range []string{"full", "slice"} {
		t.Run(mode, func(t *testing.T) {
			s, generationID := quantumPayloadFixture(t)
			seal := s.payloadSealFor(generationID)
			require.NoError(t, seal.sweep.retirementGate.LockContext(t.Context()))
			var release sync.Once
			unpark := func() { release.Do(seal.sweep.retirementGate.Unlock) }
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			joined := false
			defer func() {
				cancel()
				unpark()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("legacy retirement did not join")
					}
				}
			}()
			go func() {
				if mode == "full" {
					done <- s.RetirePayloadGeneration(ctx, generationID, nil)
				} else {
					done <- s.RetirePayloadGenerationSlice(ctx, generationID, nil, 2*time.Second)
				}
			}()
			require.Eventually(t, func() bool { return seal.sweep.retirementGate.waiting() == 1 }, 5*time.Second, time.Millisecond)
			// Both modes reached the same exact state gate. The quantum's hard
			// context must end without payload work or late gate acquisition.
			progress, err := s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, PayloadRetirementProgress{}, progress)
			require.EqualValues(t, 1, seal.sweep.retirementGate.waiting())
			unpark()
			select {
			case err := <-done:
				joined = true
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("legacy retirement did not complete")
			}
			progress, err = s.RetirePayloadGenerationQuantum(t.Context(), generationID, nil)
			require.ErrorIs(t, err, ErrCatalogNotFound)
			require.Equal(t, PayloadRetirementProgress{}, progress)
			_, orphaned := s.payloadSeals.Load(generationID)
			require.False(t, orphaned)
		})
	}
}
