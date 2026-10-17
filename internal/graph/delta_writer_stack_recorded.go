package graph

// The view below's recorded edges per path, kept per stack.
//
// The resolver reads the edges recorded at a path (RecordedEdgesAt) for the
// changed files' callers and dependents — paths the delta does not cover —
// several times per delta (the detached-pending scan, the outgoing
// frontier), and every read runs one statement per layer of the stack and
// one at the bottom store. The stack below a delta is immutable, so its
// answer at a path is too: it is read once per stack and path, and only the
// layers above the kept part (the dirty chain's and the delta's own: their
// hiding of lower rows and their own rows) are applied per read, by the same
// composition as before.

// stackRecordedEdgesMaxRows bounds the rows the cache retains; past it a
// read is answered but not kept.
const stackRecordedEdgesMaxRows = 400_000

// stackRecordedEdgesAt answers paths from the cache, loading the missing
// ones in one call. Rows are shared: callers must not modify them.
func (c *BaseProjectionCache) stackRecordedEdgesAt(paths []string, load func([]string) []*Edge) []*Edge {
	var out []*Edge
	var missing []string
	c.mu.Lock()
	for _, p := range paths {
		if rows, ok := c.stackRecorded[p]; ok {
			out = append(out, rows...)
			c.stackRecordedHit++
			continue
		}
		missing = append(missing, p)
	}
	c.stackRecordedMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	loaded := load(missing)
	byPath := make(map[string][]*Edge, len(missing))
	for _, p := range missing {
		byPath[p] = nil
	}
	for _, e := range loaded {
		if e == nil {
			continue
		}
		if _, asked := byPath[e.FilePath]; asked {
			byPath[e.FilePath] = append(byPath[e.FilePath], e)
		}
	}
	c.mu.Lock()
	if c.stackRecorded == nil {
		c.stackRecorded = make(map[string][]*Edge)
	}
	for p, rows := range byPath {
		if c.stackRecordedLen+len(rows) > stackRecordedEdgesMaxRows {
			break
		}
		c.stackRecorded[p] = rows
		c.stackRecordedLen += len(rows)
	}
	c.mu.Unlock()
	return append(out, loaded...)
}

// StackRecordedEdgeStats reports the per-path recorded-edge reads served
// from, and loaded into, the stack's cache.
func (c *BaseProjectionCache) StackRecordedEdgeStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackRecordedHit, c.stackRecordedMis
}

// stackRecordedBelow is the view below's recorded-edge reader with its
// answers kept per stack.
type stackRecordedBelow struct {
	cache *BaseProjectionCache
	inner RecordedEdgeReader
}

func (r stackRecordedBelow) RecordedEdgesAt(paths []string) []*Edge {
	uniq := UniqueRecordingPaths(paths)
	if len(uniq) == 0 {
		return nil
	}
	return r.cache.stackRecordedEdgesAt(uniq, r.inner.RecordedEdgesAt)
}

// stackRecordedEdges is the delta's recorded-edge reader over the cached
// part of the stack (below the dirty chain the delta stands on, or below the
// delta's own layer), or false when no stack cache is installed. Every level
// above the cached part is composed over it per read, as the view composes
// it.
func (dw *DeltaWriter) stackRecordedEdges() (RecordedEdgeReader, bool) {
	if dw.baseCache == nil || dw.view == nil || dw.view.layer == nil {
		return nil, false
	}
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil, false
	}
	levels := dw.viewLevels()
	if len(levels) != len(s.layers) || levels[len(levels)-1] != dw.view {
		return nil, false
	}
	below, ok := RecordedEdgesOf(levels[k].base)
	if !ok {
		return nil, false
	}
	var reader RecordedEdgeReader = stackRecordedBelow{cache: dw.baseCache, inner: below}
	for i, v := range levels[k:] {
		if k+i == len(levels)-1 {
			reader = overlaidRecordedEdges{view: v, base: reader}
			continue
		}
		reader = chainRecordedLevel{dw: dw, view: v, base: reader}
	}
	return reader, true
}
