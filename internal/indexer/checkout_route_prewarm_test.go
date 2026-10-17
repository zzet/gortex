package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// A flip calls the prewarmer with the stack it is about to route — the commit
// generation, then the new working-tree layer — while the route still names
// the old layer, so the view reader loads the new masks before any request
// can select the new route.
func TestCheckoutRouteFlipPrewarmsTheNewStackBeforeItFlips(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	type call struct {
		generations []int64
		routedDirty int64
	}
	var mu sync.Mutex
	var calls []call
	c.prewarm = func(ctx context.Context, generations []int64) {
		route := f.route()
		mu.Lock()
		calls = append(calls, call{generations: append([]int64(nil), generations...), routedDirty: route.DirtyGenerationID})
		mu.Unlock()
	}
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		m.Close()
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	ticket, err := m.EnqueueRefresh(t.Context(), filepath.Join(f.worktree, "helper.go"))
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed {
		t.Fatalf("edit ticket: %+v", result)
	}
	after := f.route()
	mu.Lock()
	defer mu.Unlock()
	var warmed *call
	for i := range calls {
		g := calls[i].generations
		if len(g) == 2 && g[1] == after.DirtyGenerationID {
			warmed = &calls[i]
		}
	}
	if warmed == nil {
		t.Fatalf("no prewarm named the routed stack [%d %d]: %+v", after.CommitGenerationID, after.DirtyGenerationID, calls)
	}
	if warmed.generations[0] != after.CommitGenerationID {
		t.Fatalf("prewarmed commit generation %d, the route names %d", warmed.generations[0], after.CommitGenerationID)
	}
	if warmed.routedDirty == after.DirtyGenerationID {
		t.Fatalf("the prewarm ran after the route already named generation %d", after.DirtyGenerationID)
	}
}
