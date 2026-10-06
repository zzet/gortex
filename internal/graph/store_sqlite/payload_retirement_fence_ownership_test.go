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

type retirementFenceProbe struct{ enter func() }

var retirementFenceObserver atomic.Pointer[retirementFenceProbe]
var retirementFenceRegister sync.Once

func TestRetirementSuccessorWaitsOutsideWriterGate(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	retirementFenceRegister.Do(func() {
		require.NoError(t, sqlite.RegisterScalarFunction("gortex_test_retirement_fence", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			if p := retirementFenceObserver.Load(); p != nil {
				p.enter()
			}
			return int64(0), nil
		}))
	})
	for _, mode := range []string{"quantum", "slice", "full", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := openPayloadStore(t)
			seedPayloadBase(t, s)
			seedPayloadControlPlane(t, s)
			id, _, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
			require.NoError(t, err)
			_, err = s.writerDB.ExecContext(t.Context(), `INSERT INTO nodes(view_gen,id,kind,name,file_path) VALUES (?,'fence-node','Function','fence','fence.go')`, id)
			require.NoError(t, err)
			require.NoError(t, s.PublishPayloadGeneration(t.Context(), id, 1))
			// All preceding sources are physically empty. Start their existing hint at
			// the last step solely to rendezvous in a real payload DELETE immediately.
			for _, table := range payloadSweepTables() {
				require.Zero(t, countAtGeneration(t, s, table, id), table)
			}
			require.Zero(t, countAtGeneration(t, s, "edges", id))
			require.Zero(t, len(ftsRowidsAtGeneration(t, s, "symbol_fts_rowid", id)))
			require.Zero(t, len(ftsRowidsAtGeneration(t, s, "content_fts_rowid", id)))
			require.NoError(t, s.Catalog().BeginViewGenerationRetirement(t.Context(), id))
			seal := s.payloadSealFor(id)
			seal.state.Store(payloadSealRetired)
			seal.sweep.advance(len(s.payloadSweepSteps(id)) - 1)
			_, err = s.writerDB.ExecContext(t.Context(), `CREATE TRIGGER hold_retirement_fence BEFORE DELETE ON nodes WHEN OLD.view_gen=`+sqlInt64List([]int64{id})+` BEGIN SELECT gortex_test_retirement_fence(); END`)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, releaseSQL := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			retirementFenceObserver.Store(&retirementFenceProbe{enter: func() {
				close(entered)
				select {
				case <-releaseSQL:
				case <-ctx.Done():
				}
			}})
			defer retirementFenceObserver.Store(nil)
			type result struct {
				progress PayloadRetirementProgress
				err      error
			}
			firstDone, secondDone := make(chan result, 1), make(chan result, 1)
			firstJoined, secondJoined, secondStarted := false, false, false
			defer func() {
				cancel()
				unblock.Do(func() { close(releaseSQL) })
				if !firstJoined {
					select {
					case <-firstDone:
					case <-time.After(5 * time.Second):
						t.Error("first retiree did not join")
					}
				}
				if secondStarted && !secondJoined {
					select {
					case <-secondDone:
					case <-time.After(5 * time.Second):
						t.Error("successor retiree did not join")
					}
				}
			}()
			go func() { p, e := s.RetirePayloadGenerationQuantum(ctx, id, nil); firstDone <- result{p, e} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first quantum did not enter payload SQL")
			}
			secondCtx, secondCancel := context.WithCancel(ctx)
			defer secondCancel()
			secondStarted = true
			go func() {
				var p PayloadRetirementProgress
				var e error
				switch mode {
				case "slice":
					e = s.RetirePayloadGenerationSlice(secondCtx, id, nil, time.Second)
				case "full":
					e = s.RetirePayloadGeneration(secondCtx, id, nil)
				default:
					p, e = s.RetirePayloadGenerationQuantum(secondCtx, id, nil)
				}
				secondDone <- result{p, e}
			}()
			// Distinguish which existing gate owns the successor, not elapsed sleeps.
			for seal.sweep.retirementGate.waiting() == 0 && s.writeMu.waiting() == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("successor did not reach an ownership gate")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			require.Positive(t, seal.sweep.retirementGate.waiting(), "same-generation fence queued as a foreground writer")
			require.Zero(t, s.writeMu.waiting(), "successor can falsely preempt the current quantum")
			if mode == "cancel" {
				secondCancel()
			}
			unblock.Do(func() { close(releaseSQL) })
			var first, second result
			select {
			case first = <-firstDone:
				firstJoined = true
			case <-ctx.Done():
				t.Fatal("first quantum did not finish")
			}
			select {
			case second = <-secondDone:
				secondJoined = true
			case <-ctx.Done():
				t.Fatal("successor did not finish")
			}
			require.ErrorIs(t, first.err, ErrPayloadSweepBudgetExhausted)
			require.EqualValues(t, 1, first.progress.RowsDeleted)
			require.EqualValues(t, 1, first.progress.ChunksCommitted)
			require.False(t, errors.Is(first.err, ErrPayloadRetirementWriteWanted))
			if mode == "cancel" {
				require.ErrorIs(t, second.err, context.Canceled)
				p, e := s.RetirePayloadGenerationQuantum(ctx, id, nil)
				require.NoError(t, e)
				require.True(t, p.CatalogRemoved)
			} else {
				require.NoError(t, second.err)
				if mode == "quantum" {
					require.True(t, second.progress.CatalogRemoved)
				}
			}
			_, found := generationState(t, s, id)
			require.False(t, found)
			require.Zero(t, countAtGeneration(t, s, "nodes", id))
		})
	}
}
