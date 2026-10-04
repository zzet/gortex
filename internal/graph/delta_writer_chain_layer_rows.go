package graph

import "sync"

// A dirty chain's layers, kept across deltas.
//
// The per-stack caches are kept for the stack below a dirty chain, and every
// delta over the chain composes the chain's layers per read (SetChainGenerations).
// A chain layer is a published generation, immutable until a derived-row
// correction moves its correction epoch: its own rows are the same for every
// delta that stands on it, which a chain of consecutive edits is — each edit's
// delta stands on every layer the edits before it published. Without a kept
// copy each delta re-read each chain layer's adjacency, names and file nodes
// through the generation's projections, about 5 µs per identity per layer.
//
// ChainLayerRows keeps each chain layer's whole rows (the rows a layer with no
// projection is answered from, rowsFor) per generation and correction epoch,
// indexed for the composition's reads. A layer past the per-layer bound is not
// kept: it is read through its projections as before. The caller drops a
// generation that stops being servable (Forget).

// ChainLayerRowsDefaultEntries and ChainLayerRowsDefaultRows bound a
// ChainLayerRows: the layers kept, and the node and edge rows they hold
// together. A chain is at most 16 layers while a fold runs; the rows bound is
// about 150 MB at the 500 bytes a decoded row takes.
const (
	ChainLayerRowsDefaultEntries = 64
	ChainLayerRowsDefaultRows    = 300_000
	// chainLayerRowsMaxPerLayer is the most rows one kept layer may hold.
	chainLayerRowsMaxPerLayer = 60_000
)

type chainLayerKey struct {
	generation int64
	epoch      uint64
}

// ChainLayerRows keeps immutable chain layers' rows across deltas.
type ChainLayerRows struct {
	mu         sync.Mutex
	maxEntries int
	maxRows    int
	entries    map[chainLayerKey]*layerRows
	order      []chainLayerKey // least recently used first
	rows       int
	hits       int
	loads      int
	declined   int
	tooLarge   map[chainLayerKey]struct{}
}

// NewChainLayerRows returns an empty keeper bounded to maxEntries layers and
// maxRows rows (defaults for non-positive bounds).
func NewChainLayerRows(maxEntries, maxRows int) *ChainLayerRows {
	if maxEntries <= 0 {
		maxEntries = ChainLayerRowsDefaultEntries
	}
	if maxRows <= 0 {
		maxRows = ChainLayerRowsDefaultRows
	}
	return &ChainLayerRows{
		maxEntries: maxEntries, maxRows: maxRows,
		entries:  make(map[chainLayerKey]*layerRows),
		tooLarge: make(map[chainLayerKey]struct{}),
	}
}

// Forget drops every kept epoch of generation.
func (c *ChainLayerRows) Forget(generation int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.order[:0]
	for _, key := range c.order {
		if key.generation == generation {
			c.rows -= len(c.entries[key].nodes) + len(c.entries[key].edges)
			delete(c.entries, key)
			continue
		}
		kept = append(kept, key)
	}
	c.order = kept
	for key := range c.tooLarge {
		if key.generation == generation {
			delete(c.tooLarge, key)
		}
	}
}

// Stats reports the kept layers, their rows, and the reads served from and
// loaded into the keeper.
func (c *ChainLayerRows) Stats() (layers, rows, hits, loads int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.rows, c.hits, c.loads
}

// Counters reports the kept layers and rows, and the reads served (hits),
// loaded (loads) and declined over the per-layer bound, cumulative.
func (c *ChainLayerRows) Counters() (layers, rows, hits, loads, declined int) {
	if c == nil {
		return 0, 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.rows, c.hits, c.loads, c.declined
}

// Holds reports whether a generation's rows are kept at epoch.
func (c *ChainLayerRows) Holds(generation int64, epoch uint64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[chainLayerKey{generation, epoch}]
	return ok
}

// get returns the kept rows of key, loading them from l on a miss; false when
// the layer is past the per-layer bound.
func (c *ChainLayerRows) get(key chainLayerKey, l OverlayLayerReader) (*layerRows, bool) {
	c.mu.Lock()
	if rows, ok := c.entries[key]; ok {
		c.hits++
		c.touchLocked(key)
		c.mu.Unlock()
		return rows, true
	}
	if _, big := c.tooLarge[key]; big {
		c.declined++
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Unlock()
	rows := &layerRows{}
	for n := range l.Nodes() {
		if n != nil {
			rows.nodes = append(rows.nodes, n)
		}
		if len(rows.nodes) > chainLayerRowsMaxPerLayer {
			break
		}
	}
	if len(rows.nodes) <= chainLayerRowsMaxPerLayer {
		for e := range l.Edges() {
			if e != nil {
				rows.edges = append(rows.edges, e)
			}
			if len(rows.nodes)+len(rows.edges) > chainLayerRowsMaxPerLayer {
				break
			}
		}
	}
	size := len(rows.nodes) + len(rows.edges)
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > chainLayerRowsMaxPerLayer || size > c.maxRows {
		c.tooLarge[key] = struct{}{}
		c.declined++
		return nil, false
	}
	if kept, ok := c.entries[key]; ok {
		c.touchLocked(key)
		return kept, true
	}
	rows.index()
	rows.indexForChain()
	c.loads++
	c.entries[key] = rows
	c.order = append(c.order, key)
	c.rows += size
	for len(c.order) > 0 && (len(c.order) > c.maxEntries || c.rows > c.maxRows) {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.rows -= len(c.entries[oldest].nodes) + len(c.entries[oldest].edges)
		delete(c.entries, oldest)
	}
	return rows, true
}

func (c *ChainLayerRows) touchLocked(key chainLayerKey) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

// indexForChain adds the indexes the chain's reads use: nodes by name and by
// file, edges by the file they are recorded at.
func (r *layerRows) indexForChain() {
	r.byName = make(map[string][]*Node)
	r.byNodeFile = make(map[string][]*Node)
	for _, n := range r.nodes {
		if n.Name != "" {
			r.byName[n.Name] = append(r.byName[n.Name], n)
		}
		r.byNodeFile[n.FilePath] = append(r.byNodeFile[n.FilePath], n)
	}
	r.byEdgeFile = make(map[string][]*Edge)
	for _, e := range r.edges {
		r.byEdgeFile[e.FilePath] = append(r.byEdgeFile[e.FilePath], e)
	}
}

// ChainLayerRowsKeeper is the keeper SetChainLayerRows installed, nil when
// none.
func (dw *DeltaWriter) ChainLayerRowsKeeper() *ChainLayerRows { return dw.chainRows }

// SetChainLayerRows installs the keeper chain layers are read through.
func (dw *DeltaWriter) SetChainLayerRows(c *ChainLayerRows) { dw.chainRows = c }

// chainRowsFor returns a chain layer's kept rows: false for a layer that is
// not one of the chain's (SetChainGenerations), with no keeper installed, or
// past the per-layer bound.
func (dw *DeltaWriter) chainRowsFor(l OverlayLayerReader) (*layerRows, bool) {
	if dw.chainRows == nil || len(dw.chainGens) == 0 || l == OverlayLayerReader(dw.layer) {
		return nil, false
	}
	id, ok := l.(GenerationLayerIdentity)
	if !ok {
		return nil, false
	}
	gen := id.GenerationID()
	for j, g := range dw.chainGens {
		if g != gen {
			continue
		}
		var epoch uint64
		if j < len(dw.chainEpochs) {
			epoch = dw.chainEpochs[j]
		}
		return dw.chainRows.get(chainLayerKey{generation: gen, epoch: epoch}, l)
	}
	return nil, false
}

// layerDetachedFileNodes is a layer's nodes at a path it does not cover.
// A layer that serves no detached rows has none composed, kept or not.
func (dw *DeltaWriter) layerDetachedFileNodes(l OverlayLayerReader, path string) []*Node {
	detached, ok := l.(OverlayDetachedNodeReader)
	if !ok {
		return nil
	}
	if rows, ok := dw.chainRowsFor(l); ok {
		if l.HasFile(path) {
			return nil
		}
		return rows.byNodeFile[path]
	}
	return detached.DetachedFileNodes(path)
}

// layerFileNodes is a layer's nodes at a path it covers.
func (dw *DeltaWriter) layerFileNodes(l OverlayLayerReader, path string) []*Node {
	if rows, ok := dw.chainRowsFor(l); ok {
		return rows.byNodeFile[path]
	}
	return l.FileNodes(path)
}

// layerRecordedEdgesAt is a layer's own edges recorded at paths.
func (dw *DeltaWriter) layerRecordedEdgesAt(l OverlayLayerReader, paths []string) []*Edge {
	uniq := UniqueRecordingPaths(paths)
	if rows, ok := dw.chainRowsFor(l); ok {
		var out []*Edge
		for _, p := range uniq {
			out = append(out, rows.byEdgeFile[p]...)
		}
		return out
	}
	if reader, ok := l.(OverlayLayerRecordedEdgeReader); ok {
		return reader.LayerRecordedEdgesAt(uniq)
	}
	want := make(map[string]struct{}, len(uniq))
	for _, p := range uniq {
		want[p] = struct{}{}
	}
	var out []*Edge
	for e := range l.Edges() {
		if e == nil {
			continue
		}
		if _, here := want[e.FilePath]; here {
			out = append(out, e)
		}
	}
	return out
}

// chainRecordedLevel is one chain view level's recorded-edge composition
// (overlaidRecordedEdges) with the layer's own rows read through the delta.
type chainRecordedLevel struct {
	dw   *DeltaWriter
	view *OverlaidView
	base RecordedEdgeReader
}

func (p chainRecordedLevel) RecordedEdgesAt(paths []string) []*Edge {
	uniq := UniqueRecordingPaths(paths)
	if len(uniq) == 0 {
		return nil
	}
	var out []*Edge
	for _, e := range p.base.RecordedEdgesAt(uniq) {
		if p.view.baseEdgeVisible(e) {
			out = append(out, e)
		}
	}
	return append(out, p.dw.layerRecordedEdgesAt(p.view.layer, uniq)...)
}
