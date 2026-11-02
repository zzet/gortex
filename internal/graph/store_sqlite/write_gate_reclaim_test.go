package store_sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// parkWriter starts an edit-path writer on the store's gate and returns once
// it is parked behind the current holder; the returned channel closes when
// the writer has had its turn.
func parkWriter(t *testing.T, s *Store) <-chan struct{} {
	t.Helper()
	before := s.writeMu.waiting()
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := s.HoldWriteGate(context.Background())
		if err == nil {
			release()
		}
	}()
	require.Eventually(t, func() bool { return s.writeMu.waiting() > before },
		5*time.Second, time.Millisecond, "the writer never queued")
	return done
}

func TestWALReclaimWriterHoldAndWriterWait(t *testing.T) {
	const hold = 30 * time.Millisecond
	ctx := context.Background()

	t.Run("an ordinary hold records neither", func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		release, err := s.HoldWriteGate(ctx)
		require.NoError(t, err)
		done := parkWriter(t, s)
		time.Sleep(hold)
		release()
		<-done
		require.Zero(t, s.writeMu.reclaimHold.snapshot().Count())
		require.Zero(t, s.writeMu.reclaimWait.snapshot().Count())
	})

	t.Run("a reclaim hold with nobody queued records only the hold", func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		require.NoError(t, s.writeMu.lockReclaim(ctx))
		time.Sleep(hold)
		s.writeMu.Unlock()
		held := s.writeMu.reclaimHold.snapshot()
		require.Equal(t, int64(1), held.Count())
		require.GreaterOrEqual(t, held.Max, hold)
		require.Zero(t, s.writeMu.reclaimWait.snapshot().Count())
	})

	t.Run("a writer queued behind a reclaim records its wait", func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		require.NoError(t, s.writeMu.lockReclaim(ctx))
		done := parkWriter(t, s)
		// A second writer queued later does not add a sample: the wait is
		// the stall the first queued writer saw.
		second := parkWriter(t, s)
		time.Sleep(hold)
		s.writeMu.Unlock()
		<-done
		<-second
		held, waited := s.writeMu.reclaimHold.snapshot(), s.writeMu.reclaimWait.snapshot()
		require.Equal(t, int64(1), held.Count())
		require.Equal(t, int64(1), waited.Count())
		require.GreaterOrEqual(t, waited.Max, hold)
		require.LessOrEqual(t, waited.Max, held.Max, "a writer cannot wait longer than the hold it waited on")
		// The writers' own holds were ordinary ones.
		require.Equal(t, int64(1), s.writeMu.reclaimHold.snapshot().Count())
	})

	t.Run("a writer already parked when the reclaim takes the gate waits from then", func(t *testing.T) {
		s := &Store{storeCore: &storeCore{}}
		// The handoff order between parked callers is the runtime's, so the
		// already-parked writer is stood in for by the waiter count it keeps.
		s.writeMu.waiters.Add(1)
		require.NoError(t, s.writeMu.lockReclaim(ctx))
		time.Sleep(hold)
		s.writeMu.Unlock()
		s.writeMu.waiters.Add(-1)
		waited := s.writeMu.reclaimWait.snapshot()
		require.Equal(t, int64(1), waited.Count())
		require.GreaterOrEqual(t, waited.Max, hold)
	})
}

// TestWALReclaimGateClocksUnderContention drives reclaim holds and ordinary
// writers through one gate at once: every reclaim hold is recorded once,
// waits never outnumber holds, and no ordinary hold is mistaken for one.
func TestWALReclaimGateClocksUnderContention(t *testing.T) {
	s := &Store{storeCore: &storeCore{}}
	ctx := context.Background()
	const reclaims, writers, rounds = 50, 4, 200
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range rounds {
				release, err := s.HoldWriteGate(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				release()
			}
		})
	}
	wg.Go(func() {
		for range reclaims {
			if err := s.writeMu.lockReclaim(ctx); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(100 * time.Microsecond)
			s.writeMu.Unlock()
		}
	})
	wg.Wait()
	held, waited := s.writeMu.reclaimHold.snapshot(), s.writeMu.reclaimWait.snapshot()
	require.Equal(t, int64(reclaims), held.Count())
	require.LessOrEqual(t, waited.Count(), held.Count())
	require.Equal(t, int64(reclaims+writers*rounds), s.writeMu.holds.Load())
}
