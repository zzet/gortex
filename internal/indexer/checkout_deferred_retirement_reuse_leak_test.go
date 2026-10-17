package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A layer the working-tree reuse cache lets go of is owed a retirement unless
// the route names it. Filing a different generation under its key — a fold of
// the chain it topped, a layer re-filed under its logical key — used to drop
// it silently: nothing owed it, discovery never offers a live checkout's ready
// layers, and it pinned its whole chain for the life of the process.
func TestAWorkingTreeLayerDisplacedInTheReuseCacheIsOwed(t *testing.T) {
	const top, fold, routed, rerouted = 20, 21, 30, 31
	c := &CheckoutCoordinator{checkoutID: "displace", logger: zap.NewNop(), backlog: map[int64]struct{}{},
		retain: 4, routedDirty: routed,
		retainedDirty: []retainedDirtyLayer{{key: "top", generationID: top}, {key: "routed", generationID: routed}}}
	ctx := context.Background()
	c.retainDirty(ctx, "top", fold)
	require.Equal(t, map[int64]struct{}{top: {}}, c.backlog, "the displaced layer was not owed")
	// The route's own release owes the layer it names when it moves on.
	c.retainDirty(ctx, "routed", rerouted)
	require.Equal(t, map[int64]struct{}{top: {}}, c.backlog, "the routed layer was owed while routed")
	require.Equal(t, []retainedDirtyLayer{{key: "routed", generationID: rerouted}, {key: "top", generationID: fold}}, c.retainedDirty)
}

// A cached layer the cache drops because its row no longer renders the key it
// was filed under is owed too, once the route does not name it.
func TestAReuseCacheEntryDroppedForAForeignKeyIsOwed(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	first := coordinatorReconcile(t, c)
	require.NotZero(t, first.DirtyGenerationID)
	// The route has moved on (by hand here): the cache is the layer's only
	// holder.
	c.mu.Lock()
	c.routedDirty = 0
	c.mu.Unlock()
	c.retainDirty(ctx, "not-the-identity", first.DirtyGenerationID)
	_, ok := c.cachedDirty(ctx, "not-the-identity")
	require.False(t, ok)
	c.mu.Lock()
	_, owed := c.backlog[first.DirtyGenerationID]
	c.mu.Unlock()
	require.True(t, owed, "the dropped layer was not owed")
}
