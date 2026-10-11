package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A pending pass that committed work re-polls without pausing; one that
// committed nothing keeps the base pause.
func TestDeferredRetirementLoopRepollsAtOnceAfterProgress(t *testing.T) {
	const basePause = 300 * time.Millisecond
	var committed atomic.Int64
	calls := make(chan time.Time, 8)
	var n atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		DeferredRetirementLoop{
			Sweep: func(context.Context) (int, bool, error) {
				calls <- time.Now()
				switch call := n.Add(1); {
				case call <= 3:
					committed.Add(1) // a productive pending pass
					return 0, true, nil
				case call == 4:
					return 0, true, nil // pending, nothing committed
				default:
					return 0, false, nil
				}
			},
			Logger:    zap.NewNop(),
			BasePause: basePause, MaxBackoff: time.Second,
			Progress: committed.Load,
		}.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	var at [5]time.Time
	for i := range at {
		select {
		case at[i] = <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("sweep call %d never came", i+1)
		}
	}
	// Twice the production budget for "at once": a goroutine hop, not a pause.
	for i := 1; i <= 3; i++ {
		require.Less(t, at[i].Sub(at[i-1]), 100*time.Millisecond, "pass %d after progress waited", i+1)
	}
	require.GreaterOrEqual(t, at[4].Sub(at[3]), basePause, "a pass that committed nothing must keep the base pause")
}

// An idle worker wakes for new debt (deferRetire) and for a released
// reference instead of sleeping out its idle pause.
func TestDeferredRetirementWorkerWakesOnNewDebtAndReleases(t *testing.T) {
	calls := make(chan time.Time, 8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		DeferredRetirementLoop{
			Sweep: func(context.Context) (int, bool, error) {
				calls <- time.Now()
				return 0, false, nil
			},
			Logger:    zap.NewNop(),
			BasePause: time.Second, MaxBackoff: time.Minute,
			IdlePause: time.Hour,
			Wake:      DeferredRetirementWake,
		}.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	await := func(what string) time.Time {
		t.Helper()
		select {
		case at := <-calls:
			return at
		case <-time.After(5 * time.Second):
			t.Fatalf("the idle worker did not wake for %s", what)
			return time.Time{}
		}
	}
	await("the first pass")
	// Let the worker reach its idle wait.
	time.Sleep(50 * time.Millisecond)
	c := &CheckoutCoordinator{checkoutID: "wake", backlog: map[int64]struct{}{}, logger: zap.NewNop()}
	owedAt := time.Now()
	c.deferRetire(42, "torn working-tree build")
	require.Less(t, await("new debt").Sub(owedAt), 100*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	releasedAt := time.Now()
	noteRetirementReferencesReleased(41)
	require.Less(t, await("a released reference").Sub(releasedAt), 100*time.Millisecond)
}
