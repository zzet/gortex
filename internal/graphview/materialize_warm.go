package graphview

import "context"

// WarmRoute loads, ahead of any request, what materializing a checkout route
// naming generations (commit first, then the working-tree layer) would load
// on a miss: the catalog ancestry of the stack and every generation's layer
// masks, into the same cache MaterializeCheckout opens them from. It is the
// publication-side half of an O(1) first selection: the coordinator calls it
// for a generation it is about to route, before the route flips, so the first
// request on the new route finds every mask set built and composes the stack
// from the cache.
//
// It takes no lease and holds nothing: a generation retired meanwhile fails
// its load, which is dropped (the cache keeps no failure), and a request then
// loads under its own lease as it always did. It reports how many mask sets it
// had to load.
func (m *Materializer) WarmRoute(ctx context.Context, generations ...int64) (loaded int, err error) {
	if err := m.validate(); err != nil {
		return 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ancestry, _, err := m.generationAncestry(ctx, generations)
	if err != nil {
		return 0, err
	}
	cache := m.layerCacheFor()
	for _, generationID := range ancestry {
		_, missesBefore, _ := cache.stats()
		if _, _, _, err := m.openGeneration(ctx, generationID); err != nil {
			return loaded, err
		}
		if _, missesAfter, _ := cache.stats(); missesAfter > missesBefore {
			loaded++
		}
	}
	return loaded, nil
}
