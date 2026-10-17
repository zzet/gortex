package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLifecycleForegroundActivityNamesInFlightWork: the view the compiler
// warm-ups yield to reports an edit's lane (a checkout cycle, even at
// background priority), a waiting refresh ticket and a demand wake as
// foreground work, not a background holder such as the committed-base
// publication; it reports the newest ticket as the last foreground instant,
// and a checkout as touched only once it admitted a ticket.
func TestLifecycleForegroundActivityNamesInFlightWork(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	gate := NewViewBuildGate()
	gate.Open()
	c := &CheckoutCoordinator{root: root, demand: make(chan struct{}, 1)}
	idle := &CheckoutCoordinator{root: other, demand: make(chan struct{}, 1)}
	l := &CheckoutLifecycle{
		gate:         gate,
		coordinators: map[string]*CheckoutCoordinator{"c": c, "idle": idle},
	}
	view := l.foregroundActivity()

	busy, last := view.ForegroundWork()
	require.Empty(t, busy)
	require.True(t, last.IsZero())
	require.False(t, view.CheckoutTouched(root))

	// A background holder (the committed-base publication declares nothing,
	// a chain compaction declares itself) is not foreground work.
	ctx := context.Background()
	release, err := gate.Acquire(ctx, ViewBuildBackground)
	require.NoError(t, err)
	busy, _ = view.ForegroundWork()
	require.Empty(t, busy, "an undeclared background build held warm-ups back")
	clearHolder := gate.NoteHolder(ViewBuildLaneHolder{Kind: "dirty_chain_compaction", CheckoutID: "c"})
	busy, _ = view.ForegroundWork()
	require.Empty(t, busy, "a chain compaction held warm-ups back")
	clearHolder()
	clearHolder = gate.NoteHolder(ViewBuildLaneHolder{Kind: "checkout_cycle", CheckoutID: "c", Priority: "background"})
	busy, _ = view.ForegroundWork()
	require.Equal(t, "checkout_cycle", busy, "a checkout's own cycle is an edit in flight")
	clearHolder()
	release()

	// A waiting refresh ticket.
	admitted := time.Now()
	c.ticketDemand.Store(admitted.UnixNano())
	c.refreshMu.Lock()
	c.refreshWaiters = map[uint64]*checkoutRefreshRequest{7: {}}
	c.refreshHighWater = 7
	c.refreshMu.Unlock()
	busy, last = view.ForegroundWork()
	require.Equal(t, "refresh_ticket", busy)
	require.True(t, last.Equal(time.Unix(0, admitted.UnixNano())), "last %v want %v", last, admitted)
	require.True(t, view.CheckoutTouched(root))
	require.True(t, view.CheckoutTouched(filepath.Join(root, ".")), "root spellings must compare equal")
	require.False(t, view.CheckoutTouched(other), "a checkout without tickets is not touched")

	c.refreshMu.Lock()
	c.refreshWaiters = nil
	c.refreshMu.Unlock()
	busy, _ = view.ForegroundWork()
	require.Empty(t, busy)

	// A demand wake not yet consumed by the loop.
	idle.demand <- struct{}{}
	busy, _ = view.ForegroundWork()
	require.Equal(t, "demand", busy)
	<-idle.demand

	// The end of a foreground cycle is the latest instant once it is newer
	// than the ticket.
	ended := admitted.Add(time.Second)
	idle.compaction.mu.Lock()
	idle.compaction.lastForeground = ended
	idle.compaction.mu.Unlock()
	_, last = view.ForegroundWork()
	require.True(t, last.Equal(ended))

	// An interactive build queued behind the lane.
	release, err = gate.Acquire(ctx, ViewBuildBackground)
	require.NoError(t, err)
	queued := make(chan func(), 1)
	go func() {
		r, err := gate.Acquire(ctx, ViewBuildInteractive)
		if err == nil {
			queued <- r
		}
	}()
	require.Eventually(t, func() bool {
		busy, _ := view.ForegroundWork()
		return busy == "interactive_build"
	}, 10*time.Second, 5*time.Millisecond)
	release()
	(<-queued)()
}
