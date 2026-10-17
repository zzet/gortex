package graph

// A layer below the delta is immutable for the stack, so its adjacency by
// identity is the same for every delta over the stack: with a per-stack
// projection cache installed, a layer of the part the cache is kept for has
// it kept there, per layer (by its level in the stack) and direction, instead
// of per delta.

// stackLayerAdjacencyKey names one layer below the delta and a direction.
type stackLayerAdjacencyKey struct {
	level    int
	incoming bool
}

// stackLayerAdjacency answers a layer's adjacency batch from the stack's
// cache, reading and keeping the identities not kept yet. ok is false when no
// stack cache is installed or l is not a layer below the delta.
func (dw *DeltaWriter) stackLayerAdjacency(l OverlayLayerReader, p OverlayLayerProjectionReader, ids []string, incoming bool) (map[string][]*Edge, bool) {
	c := dw.baseCache
	if c == nil || l == OverlayLayerReader(dw.layer) {
		return nil, false
	}
	// Only a layer of the part of the stack the cache is kept for: the
	// layers above it (a dirty chain) differ between deltas over the same
	// cache.
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil, false
	}
	level := -1
	for i, layer := range s.layers[:k] {
		if layer == l {
			level = i
			break
		}
	}
	if level < 0 {
		return nil, false
	}
	key := stackLayerAdjacencyKey{level: level, incoming: incoming}
	seen := make(map[string]struct{}, len(ids))
	var missing []string
	c.mu.Lock()
	if c.stackLayerAdj == nil {
		c.stackLayerAdj = make(map[stackLayerAdjacencyKey]map[string][]*Edge)
	}
	known := c.stackLayerAdj[key]
	if known == nil {
		known = make(map[string][]*Edge)
		c.stackLayerAdj[key] = known
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := known[id]; !ok {
			missing = append(missing, id)
		}
	}
	c.stackLayerAdjHit += len(seen) - len(missing)
	c.stackLayerAdjMis += len(missing)
	c.mu.Unlock()
	if len(missing) > 0 {
		var fetched map[string][]*Edge
		if incoming {
			fetched = p.LayerInEdgesByNodeIDs(missing)
		} else {
			fetched = p.LayerOutEdgesByNodeIDs(missing)
		}
		n := 0
		c.mu.Lock()
		for _, id := range missing {
			edges := fetched[id]
			n += len(edges)
			if edges == nil {
				edges = []*Edge{}
			}
			known[id] = edges
		}
		c.mu.Unlock()
		if incoming {
			dw.noteLayerRowsFor("in_edges_by_ids", n)
		} else {
			dw.noteLayerRowsFor("out_edges_by_ids", n)
		}
	}
	out := make(map[string][]*Edge, len(seen))
	c.mu.Lock()
	for id := range seen {
		if edges := known[id]; len(edges) > 0 {
			out[id] = edges
		}
	}
	c.mu.Unlock()
	return out, true
}

// StackLayerAdjacencyStats reports the stack-level layer adjacency hits and
// misses (identities).
func (c *BaseProjectionCache) StackLayerAdjacencyStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackLayerAdjHit, c.stackLayerAdjMis
}
