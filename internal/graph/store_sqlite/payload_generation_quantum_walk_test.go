package store_sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sqlite "modernc.org/sqlite"
)

func emptyQuantumWalkFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _ := quantumPayloadFixture(t)
	id, _, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
	require.NoError(t, err)
	require.NoError(t, s.PublishPayloadGeneration(t.Context(), id, 0))
	return s, id
}

func TestPayloadQuantumEmptyProofsUseOneCommittedTransaction(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	base := baseSnapshot(t, s)
	now := time.Now()
	ctx := context.WithValue(t.Context(), payloadQuantumClockKey{}, func() time.Time { return now })
	progress, err := s.RetirePayloadGenerationQuantum(ctx, id, nil)
	require.NoError(t, err)
	require.True(t, progress.CatalogRemoved)
	require.EqualValues(t, 1, progress.ChunksCommitted)
	require.Zero(t, progress.RowsDeleted)
	require.Greater(t, progress.StepsAdvanced, 20)
	require.Equal(t, base, baseSnapshot(t, s))
}

func TestPayloadQuantumFirstPositiveDeleteStopsBeforeNextSource(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	// Direct physical rows intentionally omit FTS/sidecars so the walker reaches
	// core edges after proving all preceding empty sources in this transaction.
	_, err := s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'walk-a','Function','a','a.go'),(?,'walk-b','Function','b','b.go')`, id, id)
	require.NoError(t, err)
	_, err = s.writerDB.ExecContext(t.Context(), `INSERT INTO edges(view_gen,from_id,to_id,kind) VALUES (?,'walk-a','walk-b','Calls')`, id)
	require.NoError(t, err)
	progress, err := s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(t.Context()), id, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.EqualValues(t, 1, progress.ChunksCommitted)
	require.EqualValues(t, 1, progress.RowsDeleted)
	require.Zero(t, countAtGeneration(t, s, "edges", id))
	require.EqualValues(t, 2, countAtGeneration(t, s, "nodes", id))
	require.Greater(t, progress.StepsAdvanced, 20)
}

func TestPayloadQuantumErrorAfterEmptyProofsDoesNotPublishHints(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	_, err := s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'walk-fail','Function','fail','fail.go')`, id)
	require.NoError(t, err)
	_, err = s.writerDB.ExecContext(t.Context(), `CREATE TRIGGER reject_quantum_walk BEFORE DELETE ON nodes WHEN OLD.view_gen=`+sqlInt64List([]int64{id})+` BEGIN SELECT RAISE(ABORT,'quantum walk rejected'); END`)
	require.NoError(t, err)
	progress, err := s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(t.Context()), id, nil)
	require.ErrorContains(t, err, "quantum walk rejected")
	require.Equal(t, PayloadRetirementProgress{}, progress)
	state := s.payloadSweepStateFor(id)
	require.NotNil(t, state)
	require.Zero(t, state.cursor())
	require.False(t, state.analysisPointerDone)
	require.EqualValues(t, 1, countAtGeneration(t, s, "nodes", id))
	_, err = s.writerDB.ExecContext(t.Context(), `DROP TRIGGER reject_quantum_walk`)
	require.NoError(t, err)
	progress, err = s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(t.Context()), id, nil)
	require.True(t, errors.Is(err, ErrPayloadSweepBudgetExhausted))
	require.EqualValues(t, 1, progress.RowsDeleted)
	require.Greater(t, progress.StepsAdvanced, 20)
}

func TestPayloadQuantumFTSPairRollsBackWithoutHints(t *testing.T) {
	s, id := quantumPayloadFixture(t)
	// Interrupt the map deletion after virtual documents were deleted; both
	// members of the FTS pair and all preceding empty proofs must roll back.
	var before int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM symbol_fts_rowid WHERE view_gen=?`, id).Scan(&before))
	require.Positive(t, before)
	_, err := s.writerDB.ExecContext(t.Context(), `CREATE TRIGGER reject_quantum_map BEFORE DELETE ON symbol_fts_rowid WHEN OLD.view_gen=`+sqlInt64List([]int64{id})+` BEGIN SELECT RAISE(ABORT,'map rejected'); END`)
	require.NoError(t, err)
	progress, err := s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(t.Context()), id, nil)
	require.ErrorContains(t, err, "map rejected")
	require.Equal(t, PayloadRetirementProgress{}, progress)
	var after, documents int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM symbol_fts_rowid WHERE view_gen=?`, id).Scan(&after))
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM symbol_fts WHERE rowid IN (SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen=?)`, id).Scan(&documents))
	require.Equal(t, before, after)
	require.Equal(t, before, documents)
	require.Zero(t, s.payloadSweepStateFor(id).cursor())
}

// Existing held-writer, cancellation, nested stable-ID and mixed concurrent
// resume controls remain part of this atom's qualification selector.

func TestPayloadQuantumCommitFailureDoesNotPublishHints(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	_, err := s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'walk-commit','Function','commit','commit.go')`, id)
	require.NoError(t, err)
	_, err = s.writerDB.ExecContext(t.Context(), `CREATE TABLE walk_parent(id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = s.writerDB.ExecContext(t.Context(), `CREATE TABLE walk_child(parent INTEGER REFERENCES walk_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	require.NoError(t, err)
	_, err = s.writerDB.ExecContext(t.Context(), `CREATE TRIGGER reject_walk_commit BEFORE DELETE ON nodes WHEN OLD.view_gen=`+sqlInt64List([]int64{id})+` BEGIN INSERT INTO walk_child(parent) VALUES (99); END`)
	require.NoError(t, err)
	progress, err := s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(t.Context()), id, nil)
	require.Error(t, err)
	require.Equal(t, PayloadRetirementProgress{}, progress)
	require.Zero(t, s.payloadSweepStateFor(id).cursor())
	require.False(t, s.payloadSweepStateFor(id).analysisPointerDone)
	require.EqualValues(t, 1, countAtGeneration(t, s, "nodes", id))
	var children int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM walk_child`).Scan(&children))
	require.Zero(t, children)
}

type quantumWalkProbe struct{ fire func() }

var quantumWalkObserver atomic.Pointer[quantumWalkProbe]
var quantumWalkRegister sync.Once

func TestPayloadQuantumPreemptionAfterEmptyProofsRollsBackHints(t *testing.T) {
	quantumWalkRegister.Do(func() {
		require.NoError(t, sqlite.RegisterScalarFunction("gortex_test_quantum_walk", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			if p := quantumWalkObserver.Load(); p != nil {
				p.fire()
			}
			return int64(0), nil
		}))
	})
	for _, kind := range []string{"cancel", "writer"} {
		t.Run(kind, func(t *testing.T) {
			s, id := emptyQuantumWalkFixture(t)
			_, err := s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'walk-interrupt','Function','interrupt','interrupt.go')`, id)
			require.NoError(t, err)
			_, err = s.writerDB.ExecContext(t.Context(), `CREATE TRIGGER interrupt_quantum_walk BEFORE DELETE ON nodes WHEN OLD.view_gen=`+sqlInt64List([]int64{id})+` BEGIN SELECT gortex_test_quantum_walk(); END`)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered := make(chan struct{})
			allowSQL := make(chan struct{})
			quantumWalkObserver.Store(&quantumWalkProbe{fire: func() {
				close(entered)
				select {
				case <-allowSQL:
				case <-ctx.Done():
				}
			}})
			defer quantumWalkObserver.Store(nil)
			type result struct {
				progress PayloadRetirementProgress
				err      error
			}
			done := make(chan result, 1)
			go func() {
				p, e := s.RetirePayloadGenerationQuantum(quantumWalkFixedClock(ctx), id, nil)
				done <- result{p, e}
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("quantum did not join")
					}
				}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("walker did not reach nodes after empty proofs")
			}
			writerDone := make(chan error, 1)
			writerStarted, writerJoined := false, false
			defer func() {
				cancel()
				if writerStarted && !writerJoined {
					select {
					case <-writerDone:
					case <-time.After(5 * time.Second):
						t.Error("writer did not join")
					}
				}
			}()
			if kind == "writer" {
				writerStarted = true
				go func() {
					e := s.writeMu.LockContext(ctx)
					if e == nil {
						s.writeMu.Unlock()
					}
					writerDone <- e
				}()
				// The existing preemption goroutine cancels its child SQL context. Release
				// the test function once the actual writer has queued, then let SQLite
				// observe that cancellation; no artificial production refusal is injected.
				deadline := time.NewTimer(time.Second)
				defer deadline.Stop()
				for s.writeMu.waiting() == 0 {
					select {
					case <-deadline.C:
						t.Fatal("writer did not queue")
					default:
						time.Sleep(time.Millisecond)
					}
				}
			}
			if kind == "cancel" {
				cancel()
			} else {
				close(allowSQL)
			}
			var r result
			select {
			case r = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("quantum did not return")
			}
			if kind == "cancel" {
				require.ErrorIs(t, r.err, context.Canceled)
			} else {
				require.ErrorIs(t, r.err, ErrPayloadRetirementWriteWanted)
			}
			require.Equal(t, PayloadRetirementProgress{}, r.progress)
			require.Zero(t, s.payloadSweepStateFor(id).cursor())
			require.False(t, s.payloadSweepStateFor(id).analysisPointerDone)
			require.EqualValues(t, 1, countAtGeneration(t, s, "nodes", id))
			if kind == "writer" {
				select {
				case e := <-writerDone:
					writerJoined = true
					require.NoError(t, e)
				case <-time.After(5 * time.Second):
					t.Fatal("writer did not return")
				}
			}
		})
	}
}

func TestPayloadQuantumSlowEmptyProofCommitsPartialHints(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	// This test is about the budget itself: the modeled 200 ms proof exhausts
	// the production quantum's commit margin, so it runs on the real value.
	s.retirementQuantumOverride = 0
	_, err := s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'margin-live','Function','live','live.go')`, id)
	require.NoError(t, err)
	started := time.Now()
	calls := 0
	ctx := context.WithValue(t.Context(), payloadQuantumClockKey{}, func() time.Time {
		calls++
		if calls == 1 {
			return started
		}
		return started.Add(200 * time.Millisecond)
	})
	progress, err := s.RetirePayloadGenerationQuantum(ctx, id, nil)
	require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
	require.EqualValues(t, 1, progress.ChunksCommitted)
	require.Zero(t, progress.RowsDeleted)
	require.EqualValues(t, 1, progress.StepsAdvanced)
	require.True(t, s.payloadSweepStateFor(id).analysisPointerDone)
	require.EqualValues(t, 1, countAtGeneration(t, s, "nodes", id))
	// The modeled slow proof changes only commit scheduling; the original
	// context remains live and subsequent real quanta remove exact payload.
	resumeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for !progress.CatalogRemoved && resumeCtx.Err() == nil {
		progress, err = s.RetirePayloadGenerationQuantum(resumeCtx, id, nil)
		require.True(t, err == nil || errors.Is(err, ErrPayloadSweepBudgetExhausted), "%v", err)
		require.Positive(t, progress.ChunksCommitted)
	}
	require.True(t, progress.CatalogRemoved)
	require.Zero(t, countAtGeneration(t, s, "nodes", id))
}

func TestPayloadQuantumCommitMarginBoundaries(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithDeadline(t.Context(), started.Add(time.Second))
	defer cancel()
	for _, elapsed := range []time.Duration{499 * time.Millisecond, 500 * time.Millisecond, 501 * time.Millisecond} {
		c := context.WithValue(ctx, payloadQuantumClockKey{}, func() time.Time { return started.Add(elapsed) })
		require.Equal(t, elapsed >= 500*time.Millisecond, payloadQuantumCommitMargin(c, started))
	}
	require.False(t, payloadQuantumCommitMargin(t.Context(), started))
}

func TestPayloadQuantumCommitMarginCanceledAfterEmptyProofRollsBack(t *testing.T) {
	s, id := emptyQuantumWalkFixture(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	started := time.Now()
	calls := 0
	ctx = context.WithValue(ctx, payloadQuantumClockKey{}, func() time.Time {
		calls++
		if calls > 1 {
			cancel(context.DeadlineExceeded)
		}
		return started.Add(time.Duration(calls-1) * 200 * time.Millisecond)
	})
	progress, err := s.RetirePayloadGenerationQuantum(ctx, id, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, calls)
	require.Equal(t, PayloadRetirementProgress{}, progress)
	state := s.payloadSweepStateFor(id)
	require.NotNil(t, state)
	require.False(t, state.analysisPointerDone)
	require.Zero(t, state.cursor())
}

func quantumWalkFixedClock(ctx context.Context) context.Context {
	now := time.Now()
	return context.WithValue(ctx, payloadQuantumClockKey{}, func() time.Time { return now })
}
