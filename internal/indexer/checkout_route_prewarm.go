package indexer

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// RoutePrewarmer loads, before a route flip, what the first request on the
// new route would otherwise load: the view reader's layer masks for the
// generations the route will name (commit generation first, then the
// working-tree layer when there is one). It is best effort and must not fail
// the flip; the daemon installs the view materializer's WarmRoute.
type RoutePrewarmer func(ctx context.Context, generations []int64)

// prewarmRoute runs the installed prewarmer for the stack a flip of slot to
// generationID will route. It runs before the flip, on the flip's goroutine,
// so a request that reads the flipped route finds its masks loaded.
func (c *CheckoutCoordinator) prewarmRoute(ctx context.Context, route *store_sqlite.CheckoutRoute, slot store_sqlite.RouteSlot, generationID int64) {
	if c == nil || c.prewarm == nil || route == nil || generationID <= 0 {
		return
	}
	commit, dirty := route.CommitGenerationID, route.DirtyGenerationID
	switch slot {
	case store_sqlite.RouteSlotCommit:
		commit = generationID
	case store_sqlite.RouteSlotDirty:
		dirty = generationID
	default:
		return
	}
	if commit <= 0 {
		return
	}
	generations := []int64{commit}
	if dirty > 0 {
		generations = append(generations, dirty)
	}
	c.prewarm(ctx, generations)
}

// routePrewarmerSlot is the lifecycle's installed prewarmer, read at every
// flip so a coordinator started before the daemon installed one still uses it.
type routePrewarmerSlot struct {
	mu sync.RWMutex
	fn RoutePrewarmer
}

func (s *routePrewarmerSlot) set(fn RoutePrewarmer) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *routePrewarmerSlot) call(ctx context.Context, generations []int64) {
	s.mu.RLock()
	fn := s.fn
	s.mu.RUnlock()
	if fn != nil {
		fn(ctx, generations)
	}
}

// SetRoutePrewarmer installs the function every coordinator of this
// lifecycle calls before a route flip (RoutePrewarmer). nil removes it.
func (l *CheckoutLifecycle) SetRoutePrewarmer(fn RoutePrewarmer) {
	if l == nil {
		return
	}
	l.routePrewarm.set(fn)
}
