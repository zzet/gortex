package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Once an edit's generation is published and routed, the first selection of
// the checkout's view loads no layer masks: the coordinator prewarmed the
// stack before it flipped the route. An edit's selection is then a
// composition over cached masks, whatever the size of the generation.
func TestFirstSelectionAfterAPublicationLoadsNoLayerMasks(t *testing.T) {
	fixture := newRealCheckoutMutationFixture(t)
	fixture.srv.wireRoutePrewarm()
	cwd := fixture.worktree
	for i, replacement := range []string{"func PrewarmedOne() {}", "func PrewarmedTwo() {}"} {
		old := "func New() {}"
		if i == 1 {
			old = "func PrewarmedOne() {}"
		}
		written := fixture.edit(t, cwd, map[string]any{"path": "repo/edit.go", "old_string": old, "new_string": replacement})
		require.False(t, written.IsError, viewResultText(t, written))
		fixture.awaitMutation(t, cwd, written)

		_, missesBefore, _ := fixture.srv.materializer.LayerCacheStats()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		view, err := fixture.srv.materializer.MaterializeCheckout(ctx, fixture.checkoutID)
		cancel()
		require.NoError(t, err)
		view.Close()
		_, missesAfter, _ := fixture.srv.materializer.LayerCacheStats()
		require.Zero(t, missesAfter-missesBefore,
			"edit %d: the first selection after its publication loaded %d mask set(s); the route was not prewarmed", i+1, missesAfter-missesBefore)
	}
}
