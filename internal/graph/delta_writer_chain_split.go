package graph

import "iter"

// A delta's per-stack caches over a dirty chain.
//
// A working-tree edit is built over the generation the previous edit
// published: the stack below the delta is the commit ancestry, then the
// working-tree generations of the dirty chain, one per consecutive edit. A
// cache keyed by every generation of that stack is a new cache on every
// consecutive edit. The caches are therefore kept for the commit ancestry
// only (the caller keys them by it and says how many chain layers stand above
// it, SetChainLayers), and every layer from the split up — the chain's, then
// the delta's own — is applied per read, by the same composition rule the
// uncached read applies to them.
//
// Every per-stack answer is loaded from the layers below the split alone, and
// never through a view that composes a layer above it: an answer kept for the
// commit ancestry is served to every chain over it.

// SetChainGenerations names the working-tree generations of the dirty chain
// the delta stands on, bottom first: the layers directly below the delta's
// own, above the stack the projection cache is kept for. None (the default)
// keeps the cache for every layer below the delta. A stack whose layers
// below the delta's own do not end in exactly these generations has no split,
// and the delta reads it without the cache.
func (dw *DeltaWriter) SetChainGenerations(generations []int64, epochs []uint64) {
	dw.chainGens = append([]int64(nil), generations...)
	dw.chainEpochs = append([]uint64(nil), epochs...)
	dw.chainLayers = len(dw.chainGens)
}

// ChainLayers is how many chain layers SetChainGenerations named.
func (dw *DeltaWriter) ChainLayers() int { return dw.chainLayers }

// GenerationLayerIdentity is a layer that names the published generation it
// reads.
type GenerationLayerIdentity interface {
	GenerationID() int64
}

// cacheSplit is the index, in the stack's layer list, of the lowest layer
// applied per read over the per-stack cache: the layers below it compose the
// cached part. ok is false when no cache is installed, or the stack does not
// end in the delta's own layer over the chain generations it was given.
func (dw *DeltaWriter) cacheSplit(s deltaStack) (int, bool) {
	last := len(s.layers) - 1
	if dw.baseCache == nil || last < 0 || s.layers[last] != OverlayLayerReader(dw.layer) {
		return 0, false
	}
	k := last - dw.chainLayers
	if k < 0 {
		return 0, false
	}
	for j, gen := range dw.chainGens {
		id, ok := s.layers[k+j].(GenerationLayerIdentity)
		if !ok || id.GenerationID() != gen {
			return 0, false
		}
	}
	return k, true
}

// overlaySplit is cacheSplit, or, with no cache installed and no chain named,
// the delta's own layer: the split the exported below-the-chain reads and
// their overlays use.
func (dw *DeltaWriter) overlaySplit(s deltaStack) (int, bool) {
	if k, ok := dw.cacheSplit(s); ok {
		return k, true
	}
	last := len(s.layers) - 1
	if dw.baseCache != nil || dw.chainLayers != 0 || last < 0 || s.layers[last] != OverlayLayerReader(dw.layer) {
		return 0, false
	}
	return last, true
}

// viewLevels lists the composed view's levels that carry a layer, bottom
// first: levels[i] composes the stack's layers[i] over the view below it.
func (dw *DeltaWriter) viewLevels() []*OverlaidView {
	var levels []*OverlaidView
	var r Reader = dw.view
	for r != nil {
		switch v := r.(type) {
		case *OverlaidView:
			if v.layer != nil {
				levels = append(levels, v)
			}
			if v.base == nil {
				r = nil
				continue
			}
			r = v.base
			continue
		case Unwrapper:
			if next := v.Unwrap(); next != nil {
				r = next
				continue
			}
		}
		break
	}
	for i, j := 0, len(levels)-1; i < j; i, j = i+1, j-1 {
		levels[i], levels[j] = levels[j], levels[i]
	}
	return levels
}

// viewBelowLevel is the composed view under the stack's layer k: every layer
// below k over the bottom store. nil when the view has no such level.
func (dw *DeltaWriter) viewBelowLevel(k int) Reader {
	levels := dw.viewLevels()
	if k < 0 || k >= len(levels) {
		return nil
	}
	return levels[k].base
}

// ChainSplitBase is the view the per-stack caches are loaded from: the stack
// below the dirty chain, with neither the chain's layers nor the delta's own
// composed. ok is false when no split applies (no cache installed, or the
// stack is not the shape SetChainLayers described); the view below the delta
// is then the answer the caches were always loaded from.
func (dw *DeltaWriter) ChainSplitBase() (Reader, bool) {
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil, false
	}
	if below := dw.viewBelowLevel(k); below != nil {
		return below, true
	}
	return nil, false
}

// chainSplit is the split with the chain's view levels and the reader below
// them, for the reads that replay the composed view's per-level rule. ok is
// false when there is no chain to apply (no split, or a chain of zero
// layers): callers then read as before.
func (dw *DeltaWriter) chainSplit() (levels []*OverlaidView, below Reader, ok bool) {
	if dw.chainLayers == 0 {
		return nil, nil, false
	}
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil, nil, false
	}
	all := dw.viewLevels()
	last := len(s.layers) - 1
	if len(all) != len(s.layers) || all[last] != dw.view {
		return nil, nil, false
	}
	return all[k:last], all[k].base, true
}

// coveredFrom reports whether a layer at index from or above covers a path.
func (s deltaStack) coveredFrom(path string, from int) bool {
	for i := from; i < len(s.layers); i++ {
		if s.layers[i].HasFile(path) {
			return true
		}
	}
	return false
}

// ChainFileNodesAt composes the chain's layers over the file nodes the view
// below the chain holds at a path (read at ChainSplitBase), level by level as
// the composed view's GetFileNodes does. It is the view below the delta's
// answer at the path.
func (dw *DeltaWriter) ChainFileNodesAt(path string, belowChain []*Node) []*Node {
	levels, _, ok := dw.chainSplit()
	if !ok {
		return belowChain
	}
	return dw.fileNodesUp(levels, path, belowChain)
}

// ChainRecordedEdgesAt composes the chain's layers over the edges the view
// below the chain records at paths, level by level as the composed view's
// RecordedEdgesAt does.
func (dw *DeltaWriter) ChainRecordedEdgesAt(paths []string, belowChain []*Edge) []*Edge {
	levels, _, ok := dw.chainSplit()
	if !ok {
		return belowChain
	}
	return dw.recordedEdgesUp(levels, paths, belowChain)
}

// fileNodesUp is OverlaidView.GetFileNodes applied level by level over rows
// read below the first level, each level's own rows read through the delta.
func (dw *DeltaWriter) fileNodesUp(levels []*OverlaidView, path string, nodes []*Node) []*Node {
	for _, v := range levels {
		if v.layer.HasFile(path) {
			src := dw.layerFileNodes(v.layer, path)
			nodes = make([]*Node, len(src))
			copy(nodes, src)
			continue
		}
		out := make([]*Node, 0, len(nodes))
		for _, n := range nodes {
			if v.baseNodeVisible(n) {
				out = append(out, n)
			}
		}
		out = append(out, dw.layerDetachedFileNodes(v.layer, path)...)
		nodes = out
	}
	return nodes
}

// recordedEdgesUp is overlaidRecordedEdges.RecordedEdgesAt applied level by
// level over rows read below the first level.
func (dw *DeltaWriter) recordedEdgesUp(levels []*OverlaidView, paths []string, edges []*Edge) []*Edge {
	uniq := UniqueRecordingPaths(paths)
	for _, v := range levels {
		var out []*Edge
		for _, e := range edges {
			if v.baseEdgeVisible(e) {
				out = append(out, e)
			}
		}
		edges = append(out, dw.layerRecordedEdgesAt(v.layer, uniq)...)
	}
	return edges
}

// EdgesBelowChainByNodeIDs is the composed adjacency of ids over the layers
// below the split alone (the cached part of the stack): what the delta's
// GetInEdgesByNodeIDs / GetOutEdgesByNodeIDs compose, without the chain and
// the delta's own layer. ok is false when there is no split or the store at
// the bottom has no batched adjacency.
func (dw *DeltaWriter) EdgesBelowChainByNodeIDs(ids []string, incoming bool) (map[string][]*Edge, bool) {
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		return nil, false
	}
	out, ok := dw.composeEdgesOver(deltaStack{base: s.base, layers: s.layers[:k]}, ids, incoming)
	if !ok {
		return nil, false
	}
	for id, edges := range out {
		out[id] = cloneDeltaEdges(edges)
	}
	return out, true
}

// OverlayEdgesAboveChain applies every layer from the split up (the chain's
// and the delta's own) to adjacency composed below it
// (EdgesBelowChainByNodeIDs): each row is kept only when no layer above hides
// it, and each layer's own rows are added, as the uncached composition does.
// The answer is the caller's copy.
func (dw *DeltaWriter) OverlayEdgesAboveChain(ids []string, belowChain map[string][]*Edge, incoming bool) map[string][]*Edge {
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		return belowChain
	}
	uniq := uniqueIDs(ids)
	out := make(map[string][]*Edge, len(uniq))
	for _, id := range uniq {
		for _, e := range belowChain[id] {
			if e != nil && s.edgeVisibleFrom(e, k) {
				out[id] = append(out[id], e)
			}
		}
	}
	for i := k; i < len(s.layers); i++ {
		for id, edges := range dw.layerAdjacency(s.layers[i], uniq, incoming) {
			for _, e := range edges {
				if e != nil && s.edgeVisibleFrom(e, i+1) {
					out[id] = append(out[id], e)
				}
			}
		}
	}
	for id, edges := range out {
		if len(edges) == 0 {
			delete(out, id)
			continue
		}
		out[id] = cloneDeltaEdges(edges)
	}
	return out
}

// EdgeVisibleAboveChain reports whether no layer from the split up (the
// chain's or the delta's own) hides a row read below it. Without a split it
// answers for the delta's own layer alone.
func (dw *DeltaWriter) EdgeVisibleAboveChain(e *Edge) bool {
	if e == nil {
		return false
	}
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		k = len(s.layers) - 1
		if k < 0 || s.layers[k] != OverlayLayerReader(dw.layer) {
			return true
		}
	}
	return s.edgeVisibleFrom(e, k)
}

// EdgesByKindBelowChain is the delta's composed kind scan (EdgesByKind) over
// the part of the stack the cache is kept for alone. ok is false when there
// is no split or a layer of that part has no generation-scoped projection.
func (dw *DeltaWriter) EdgesByKindBelowChain(kind EdgeKind) ([]*Edge, bool) {
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		return nil, false
	}
	return dw.composeEdgesByKindOver(deltaStack{base: s.base, layers: s.layers[:k]}, kind)
}

// EdgesByKindAboveChain lists the rows of the kind the layers above the kept
// part (the chain's and the delta's own) add to the scan, each kept only when
// no layer above it hides it: with the kept part's rows filtered by
// EdgeVisibleAboveChain, the delta's composed scan.
func (dw *DeltaWriter) EdgesByKindAboveChain(kind EdgeKind) []*Edge {
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		return nil
	}
	var out []*Edge
	for i := k; i < len(s.layers); i++ {
		for _, e := range dw.layerEdgesOfKinds(s.layers[i], []EdgeKind{kind}) {
			if e != nil && e.Kind == kind && s.edgeVisibleFrom(e, i+1) {
				out = append(out, e)
			}
		}
	}
	return out
}

// IdentityAboveChain reports whether a layer above the kept part (the chain's
// or the delta's own) speaks for an identity, so the kept part's answer for
// it is not the delta's.
func (dw *DeltaWriter) IdentityAboveChain(id string) bool {
	s := dw.stack()
	k, ok := dw.overlaySplit(s)
	if !ok {
		return true
	}
	return s.hiddenAbove(id, k)
}

// ChainTouchedPaths lists the graph paths whose nodes a layer of the dirty
// chain speaks for: the paths a chain layer covers, the paths of the
// identities it removed, and the paths of the nodes it carries outside its
// covered paths. Every other path's nodes are the stack's below the chain.
// Nil when there is no chain.
func (dw *DeltaWriter) ChainTouchedPaths() map[string]struct{} {
	if dw.chainLayers == 0 {
		return nil
	}
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil
	}
	out := make(map[string]struct{})
	for _, l := range s.layers[k : len(s.layers)-1] {
		for _, p := range l.FilePaths() {
			out[p] = struct{}{}
		}
		for id := range l.RemovedIDs() {
			out[deltaPathKey(id)] = struct{}{}
		}
		for n := range detachedNodesOf(l) {
			if n != nil {
				if n.FilePath != "" {
					out[n.FilePath] = struct{}{}
				}
				out[deltaPathKey(n.ID)] = struct{}{}
			}
		}
	}
	delete(out, "")
	return out
}

// ChainLayerEdgesByKinds lists the chain layers' own edges of the kinds.
func (dw *DeltaWriter) ChainLayerEdgesByKinds(kinds []EdgeKind) []*Edge {
	if dw.chainLayers == 0 {
		return nil
	}
	s := dw.stack()
	k, ok := dw.cacheSplit(s)
	if !ok {
		return nil
	}
	var out []*Edge
	for _, l := range s.layers[k : len(s.layers)-1] {
		out = append(out, dw.layerEdgesOfKinds(l, kinds)...)
	}
	return out
}

// OverlayDetachedSummaryReader is a layer that lists the nodes it carries
// outside its covered paths.
type OverlayDetachedSummaryReader interface {
	DetachedNodeSummaries() iter.Seq[*Node]
}

// detachedNodesOf iterates the nodes a layer carries outside the paths it
// covers.
func detachedNodesOf(l OverlayLayerReader) iter.Seq[*Node] {
	if d, ok := l.(OverlayDetachedSummaryReader); ok {
		return d.DetachedNodeSummaries()
	}
	return func(yield func(*Node) bool) {
		for n := range l.Nodes() {
			if n == nil || l.HasFile(n.FilePath) {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
