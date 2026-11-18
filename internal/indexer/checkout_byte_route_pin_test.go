package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Only the completed legacy ancestry lease holds generation zero. Use a
// noncancelable parent so earlier SQL calls cannot leave row-cancel watchers;
// the final catalog read must reach this barrier with all pins held.
type byteAnchorRereadContext struct {
	context.Context
	leases  *graphview.LeaseManager
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *byteAnchorRereadContext) Done() <-chan struct{} {
	if c.leases.InUse(graphview.BaseCorpusGeneration) {
		c.once.Do(func() {
			close(c.entered)
			<-c.release
		})
	}
	return c.Context.Done()
}

func TestByteAnchorPinMotionRetainsTypedCauseAndReleasesCustody(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
		return
	}
	c.closeByteAnchor()
	require.Zero(t, f.leases.Held(), "the reread barrier requires an unheld legacy base")
	before := f.route()
	barrier := &byteAnchorRereadContext{Context: context.Background(), leases: f.leases, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	done, exited := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(exited)
		done <- c.captureByteAnchor(barrier, before)
	}()
	defer func() {
		release()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("anchor capture did not drain")
		}
	}()
	select {
	case <-barrier.entered:
	case err := <-done:
		t.Fatalf("anchor capture returned before ancestry reread: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("anchor capture did not reach full ancestry reread")
	}
	for _, id := range []int64{graphview.BaseCorpusGeneration, before.CommitGenerationID, before.DirtyGenerationID} {
		require.True(t, f.leases.InUse(id))
	}
	// Even an ABA publication of the identical payload is a different route.
	require.NoError(t, f.catalog.FlipCheckoutRouteSlot(t.Context(), store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID: f.checkoutID, Slot: store_sqlite.RouteSlotDirty,
		GenerationID: before.DirtyGenerationID, ExpectedRouteEpoch: before.RouteEpoch, State: store_sqlite.RouteActive,
	}))
	release()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("anchor capture did not return after route motion")
	}
	require.ErrorIs(t, err, ErrCheckoutMutationRouteMoved)
	require.ErrorIs(t, err, graphview.ErrViewBuilding)
	var refusal *graphview.ViewError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "checkout route moved before ancestry pin", refusal.Message)
	c.byteAnchorMu.Lock()
	anchor := c.byteAnchor
	c.byteAnchorMu.Unlock()
	require.Nil(t, anchor, "a refused pin must not become an authenticated anchor")
	require.Zero(t, f.leases.Held())
	c.byteMu.Lock()
	demand := c.byteDemand
	c.byteMu.Unlock()
	require.Zero(t, demand)
	// Original whole-view mutation admission can use the actual replacement
	// route, without an old pin or byte demand preventing it from proceeding.
	after := f.route()
	require.NotEqual(t, before.RouteEpoch, after.RouteEpoch)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, after.RouteEpoch)
	require.NoError(t, err)
	require.NotNil(t, m)
	m.Close()
}
