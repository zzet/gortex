package graph

// The bottom store's adjacency, kept per stack.
//
// A composed adjacency read (composedEdgesByNodeIDs) takes the bottom store's
// rows for the identities, then each layer's (kept per stack by
// stackLayerAdjacency), and filters them by the stack's visibility. Under a
// per-stack projection cache the bottom store is the stack's immutable base
// generation, so its rows for an identity are the same for every delta over
// the stack: they are kept here, unfiltered, and filtered against the full
// stack (the delta's own layer included) at every read. A per-save pass that
// evaluates the same receiver-call closure on every save of a file (the
// capability pass) then reads the bottom store once per identity per stack.

// maxStackBaseAdjacencyRows bounds the kept rows; past it, missing identities
// are read and served without being kept.
const maxStackBaseAdjacencyRows = 500_000

// stackBaseAdjacency answers the bottom store's adjacency for ids from the
// per-stack cache, reading and keeping the missing ones through load. ok is
// false when no per-stack cache is installed (the caller reads directly).
func (dw *DeltaWriter) stackBaseAdjacency(ids []string, incoming bool, load func([]string) map[string][]*Edge) (map[string][]*Edge, bool) {
	c := dw.baseCache
	if c == nil {
		return nil, false
	}
	dir := 0
	if incoming {
		dir = 1
	}
	var missing []string
	out := make(map[string][]*Edge, len(ids))
	c.mu.Lock()
	if c.stackBaseAdj[dir] == nil {
		c.stackBaseAdj[dir] = make(map[string][]*Edge)
	}
	known := c.stackBaseAdj[dir]
	for _, id := range ids {
		if edges, ok := known[id]; ok {
			if len(edges) > 0 {
				out[id] = edges
			}
			continue
		}
		missing = append(missing, id)
	}
	c.stackBaseAdjHit += len(ids) - len(missing)
	c.stackBaseAdjMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, true
	}
	fetched := load(missing)
	c.mu.Lock()
	for _, id := range missing {
		edges := fetched[id]
		if len(edges) > 0 {
			out[id] = edges
		}
		if c.stackBaseAdjLen+len(edges) > maxStackBaseAdjacencyRows {
			continue
		}
		if edges == nil {
			edges = []*Edge{}
		}
		known[id] = edges
		c.stackBaseAdjLen += len(edges)
	}
	c.mu.Unlock()
	return out, true
}

// StackBaseAdjacencyStats reports the per-stack bottom-store adjacency hits
// and misses (identities).
func (c *BaseProjectionCache) StackBaseAdjacencyStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackBaseAdjHit, c.stackBaseAdjMis
}
