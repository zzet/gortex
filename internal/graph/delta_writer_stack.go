package graph

import (
	"iter"
	"sort"
	"sync"
)

// Projections through the layer stack.
//
// The per-save engine reads repository- and file-scoped projections (the
// scoped node and edge cursors, the dataflow parameter and call batches) that
// a store serves from narrow indexed queries. A composed view has no such
// queries, so without them the engine falls back to reading whole repositories
// or every edge of a popular callee through the view. A delta answers them in
// the stack's own terms: the store at the bottom serves the projection, every
// row is kept only when no layer above speaks for it (the composition's own
// visibility rule, applied level by level), and each layer adds its own rows,
// kept only when no layer above it speaks for them. The answer is the rows the
// same projection of the composed graph would return.

// layerRows is one immutable layer's rows, read once per delta.
type layerRows struct {
	once   sync.Once
	nodes  []*Node
	edges  []*Edge
	byID   map[string]*Node
	byFrom map[string][]*Edge
	byTo   map[string][]*Edge
	// byName, byNodeFile and byEdgeFile index a kept chain layer's rows
	// (ChainLayerRows); nil otherwise.
	byName     map[string][]*Node
	byNodeFile map[string][]*Node
	byEdgeFile map[string][]*Edge
}

// rowsFor returns a layer's rows. The delta's own layer is answered live from
// the working graph.
func (dw *DeltaWriter) rowsFor(l OverlayLayerReader) *layerRows {
	if l == OverlayLayerReader(dw.layer) {
		rows := &layerRows{nodes: dw.work.AllNodes(), edges: dw.work.AllEdges()}
		rows.index()
		return rows
	}
	dw.nameIndexMu.Lock()
	if dw.layerRowCache == nil {
		dw.layerRowCache = make(map[OverlayLayerReader]*layerRows)
	}
	rows := dw.layerRowCache[l]
	if rows == nil {
		rows = &layerRows{}
		dw.layerRowCache[l] = rows
	}
	dw.nameIndexMu.Unlock()
	rows.once.Do(func() {
		defer func() {
			dw.noteWholeLayerLoad(len(rows.nodes) + len(rows.edges))
			dw.noteLayerRowsFor("whole_layer", len(rows.nodes)+len(rows.edges))
		}()
		for n := range l.Nodes() {
			if n != nil {
				rows.nodes = append(rows.nodes, n)
			}
		}
		for e := range l.Edges() {
			if e != nil {
				rows.edges = append(rows.edges, e)
			}
		}
		rows.index()
	})
	return rows
}

// layerNodesInScope is a layer's nodes a scoped read may keep: for the
// delta's own layer the working graph's nodes at the scope's files (all of
// them for a repository scope), read live; for an immutable layer its
// generation-scoped projection when it has one, its cached rows otherwise.
func (dw *DeltaWriter) layerNodesInScope(l OverlayLayerReader, repoPrefixes, filePaths []string, light bool, kinds ...NodeKind) []*Node {
	if l == OverlayLayerReader(dw.layer) {
		if len(filePaths) == 0 {
			return dw.work.AllNodes()
		}
		var out []*Node
		for _, p := range UniqueRecordingPaths(filePaths) {
			out = append(out, dw.work.GetFileNodes(p)...)
		}
		return out
	}
	if rows, ok := dw.chainRowsFor(l); ok {
		return rows.nodes
	}
	if p, ok := l.(OverlayLayerProjectionReader); ok {
		nodes := p.LayerNodesInScope(repoPrefixes, filePaths, light, kinds...)
		dw.noteLayerRowsFor("nodes_in_scope"+layerRowKinds(nodeKindNames(kinds)), len(nodes))
		return nodes
	}
	return dw.rowsFor(l).nodes
}

// layerAdjacency is a layer's own edges out of (or into) ids: the working
// graph for the delta's layer, the generation-scoped batch for an immutable
// layer that has one, its cached rows otherwise.
func (dw *DeltaWriter) layerAdjacency(l OverlayLayerReader, ids []string, incoming bool) map[string][]*Edge {
	if l == OverlayLayerReader(dw.layer) {
		out := make(map[string][]*Edge, len(ids))
		for _, id := range ids {
			var edges []*Edge
			if incoming {
				edges = dw.work.GetInEdges(id)
			} else {
				edges = dw.work.GetOutEdges(id)
			}
			if len(edges) > 0 {
				out[id] = edges
			}
		}
		return out
	}
	rows, kept := dw.chainRowsFor(l)
	if !kept {
		if p, ok := l.(OverlayLayerProjectionReader); ok {
			return dw.cachedLayerAdjacency(l, p, ids, incoming)
		}
		rows = dw.rowsFor(l)
	}
	index := rows.byFrom
	if incoming {
		index = rows.byTo
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if edges := index[id]; len(edges) > 0 {
			out[id] = edges
		}
	}
	return out
}

// layerEdgesOfKinds is a layer's edges of the kinds, for the delta's own
// layer read live from the working graph without indexing it, for an
// immutable layer from its generation-scoped projection when it has one.
func (dw *DeltaWriter) layerEdgesOfKinds(l OverlayLayerReader, kinds []EdgeKind) []*Edge {
	if l == OverlayLayerReader(dw.layer) {
		var out []*Edge
		for e := range dw.work.EdgesByKinds(kinds) {
			if e != nil {
				out = append(out, e)
			}
		}
		return out
	}
	if rows, ok := dw.chainRowsFor(l); ok {
		want := make(map[EdgeKind]struct{}, len(kinds))
		for _, k := range kinds {
			want[k] = struct{}{}
		}
		var out []*Edge
		for _, e := range rows.edges {
			if _, ok := want[e.Kind]; ok {
				out = append(out, e)
			}
		}
		return out
	}
	if p, ok := l.(OverlayLayerProjectionReader); ok {
		edges := p.LayerEdgesByKinds(kinds)
		dw.noteLayerRowsFor("edges_by_kinds"+layerRowKinds(edgeKindNames(kinds)), len(edges))
		return edges
	}
	return dw.rowsFor(l).edges
}

func (r *layerRows) index() {
	r.byID = make(map[string]*Node, len(r.nodes))
	for _, n := range r.nodes {
		r.byID[n.ID] = n
	}
	r.byFrom = make(map[string][]*Edge)
	r.byTo = make(map[string][]*Edge)
	for _, e := range r.edges {
		r.byFrom[e.From] = append(r.byFrom[e.From], e)
		r.byTo[e.To] = append(r.byTo[e.To], e)
	}
}

// edgeVisibleFrom applies the composition's per-layer edge rule for every
// layer from index `from` upwards: an edge is hidden by a layer that covers
// the path it is recorded at or replaces its source's adjacency, and by a
// layer that speaks for either endpoint without carrying it.
func (s deltaStack) edgeVisibleFrom(e *Edge, from int) bool {
	for i := from; i < len(s.layers); i++ {
		l := s.layers[i]
		if e.FilePath != "" {
			if l.HasFile(e.FilePath) || l.OwnsOutEdges(e.From) {
				return false
			}
		} else if l.CoversNodeID(e.From) || l.OwnsOutEdges(e.From) {
			return false
		}
		if layerClaimsEdge(l, e) {
			return false
		}
		for _, id := range [2]string{e.From, e.To} {
			if id == "" {
				continue
			}
			if (l.CoversNodeID(id) || l.OwnsNodeIdentity(id)) && l.NodeByID(id) == nil {
				return false
			}
		}
	}
	return true
}

// topNode returns the node the stack serves for an identity some layer speaks
// for: the topmost owning layer's row. owned is false when no layer speaks for
// it (the store at the bottom answers).
func (s deltaStack) topNode(id string) (node *Node, owned bool) {
	for i := len(s.layers) - 1; i >= 0; i-- {
		l := s.layers[i]
		if l.CoversNodeID(id) || l.OwnsNodeIdentity(id) {
			return l.NodeByID(id), true
		}
	}
	return nil, false
}

// inScope reports whether a node falls in a (repositories, files) scope with
// the stores' predicate: repository prefixes filter, a file list narrows.
func inScope(n *Node, repos, files map[string]struct{}) bool {
	if n == nil {
		return false
	}
	if len(repos) > 0 {
		if _, ok := repos[n.RepoPrefix]; !ok {
			return false
		}
	}
	if len(files) > 0 {
		if _, ok := files[n.FilePath]; !ok {
			return false
		}
	}
	return true
}

func stringKeySet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// NodesInScopeSeq implements ScopedProjectionSequencer through the stack.
func (dw *DeltaWriter) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...NodeKind) iter.Seq[*Node] {
	return dw.scopedNodes(repoPrefixes, filePaths, false, kinds...)
}

// NodesLightInScopeSeq implements ScopedProjectionSequencer through the stack.
// Layer rows are served whole (a superset of the light projection's fields).
func (dw *DeltaWriter) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*Node] {
	return dw.scopedNodes(repoPrefixes, filePaths, true)
}

func (dw *DeltaWriter) scopedNodes(repoPrefixes, filePaths []string, light bool, kinds ...NodeKind) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if (len(repoPrefixes) == 0 && len(filePaths) == 0) || (!light && len(kinds) == 0) {
			return
		}
		repos, files := stringKeySet(repoPrefixes), stringKeySet(filePaths)
		wantKind := make(map[NodeKind]struct{}, len(kinds))
		for _, k := range kinds {
			wantKind[k] = struct{}{}
		}
		kindOK := func(n *Node) bool {
			if light {
				return true
			}
			_, ok := wantKind[n.Kind]
			return ok
		}
		s := dw.stack()
		var out []*Node
		if seq, ok := s.base.(ScopedProjectionSequencer); ok {
			var base iter.Seq[*Node]
			if light {
				base = seq.NodesLightInScopeSeq(repoPrefixes, filePaths)
			} else {
				base = seq.NodesInScopeSeq(repoPrefixes, filePaths, kinds...)
			}
			for n := range base {
				if n != nil && !s.hiddenAbove(n.ID, 0) {
					out = append(out, n)
				}
			}
		} else {
			dw.noteSlowRead("NodesInScopeSeq")
			var src []*Node
			if len(filePaths) > 0 {
				for _, nodes := range dw.view.GetFileNodesByPathsFallback(filePaths) {
					src = append(src, nodes...)
				}
			} else {
				for _, repo := range repoPrefixes {
					src = append(src, dw.view.GetRepoNodes(repo)...)
				}
			}
			for _, n := range src {
				if inScope(n, repos, files) && kindOK(n) {
					out = append(out, n)
				}
			}
			dw.emitSortedNodes(out, yield)
			return
		}
		for i, l := range s.layers {
			for _, n := range dw.layerNodesInScope(l, repoPrefixes, filePaths, light, kinds...) {
				if inScope(n, repos, files) && kindOK(n) && !s.hiddenAbove(n.ID, i+1) {
					out = append(out, n)
				}
			}
		}
		dw.emitSortedNodes(out, yield)
	}
}

func (dw *DeltaWriter) emitSortedNodes(nodes []*Node, yield func(*Node) bool) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		if !yield(cloneDeltaNode(n)) {
			return
		}
	}
}

// EdgesInScopeSeq implements ScopedProjectionSequencer through the stack:
// the edges whose source node falls in the scope, with both endpoints as the
// composed graph serves them.
func (dw *DeltaWriter) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...EdgeKind) iter.Seq[ScopedEdgeRow] {
	return func(yield func(ScopedEdgeRow) bool) {
		if len(kinds) == 0 || (len(repoPrefixes) == 0 && len(filePaths) == 0) {
			return
		}
		repos, files := stringKeySet(repoPrefixes), stringKeySet(filePaths)
		wantKind := make(map[EdgeKind]struct{}, len(kinds))
		for _, k := range kinds {
			wantKind[k] = struct{}{}
		}
		s := dw.stack()
		seq, ok := s.base.(ScopedProjectionSequencer)
		if !ok {
			dw.noteSlowRead("EdgesInScopeSeq")
			dw.fallbackScopedEdges(repoPrefixes, filePaths, wantKind, yield)
			return
		}
		var rows []ScopedEdgeRow
		baseRows, cached := dw.stackBaseScopedEdges(repoPrefixes, filePaths, kinds, func() []ScopedEdgeRow {
			var all []ScopedEdgeRow
			for row := range seq.EdgesInScopeSeq(repoPrefixes, filePaths, kinds...) {
				all = append(all, row)
			}
			return all
		})
		if !cached {
			for row := range seq.EdgesInScopeSeq(repoPrefixes, filePaths, kinds...) {
				if row.Edge == nil || !s.edgeVisibleFrom(row.Edge, 0) {
					continue
				}
				rows = append(rows, row)
			}
		}
		for _, row := range baseRows {
			if row.Edge == nil || !s.edgeVisibleFrom(row.Edge, 0) {
				continue
			}
			rows = append(rows, row)
		}
		// Each layer's own edges whose source falls in the scope.
		type pending struct {
			edge  *Edge
			level int
		}
		var layered []pending
		sourceIDs := make(map[string]struct{})
		// A file scope names its sources: every node the composition serves
		// at the scope's files. Each layer's own edges out of them are read by
		// identity, whatever file they were recorded at; a repository scope has
		// no such bound and reads each layer's rows of the kinds.
		var scopeIDs []string
		if len(filePaths) > 0 {
			byFile, ok := dw.composedFileNodesByPaths(filePaths)
			if !ok {
				byFile = dw.view.GetFileNodesByPathsFallback(filePaths)
			}
			for _, nodes := range byFile {
				for _, n := range nodes {
					if n != nil && inScope(n, repos, files) {
						scopeIDs = append(scopeIDs, n.ID)
					}
				}
			}
		}
		for i, l := range s.layers {
			var edges []*Edge
			if len(filePaths) > 0 {
				for _, out := range dw.layerAdjacency(l, scopeIDs, false) {
					edges = append(edges, out...)
				}
			} else {
				edges = dw.layerEdgesOfKinds(l, kinds)
			}
			for _, e := range edges {
				if e == nil {
					continue
				}
				if _, ok := wantKind[e.Kind]; !ok {
					continue
				}
				if !s.edgeVisibleFrom(e, i+1) {
					continue
				}
				layered = append(layered, pending{edge: e, level: i})
				sourceIDs[e.From] = struct{}{}
			}
		}
		// Endpoint rows: the base rows carry the store's view of them; an
		// identity a layer speaks for is served from that layer instead.
		need := make([]string, 0, len(sourceIDs))
		for id := range sourceIDs {
			need = append(need, id)
		}
		endpoint := func(id string, fallback *Node) *Node {
			if id == "" {
				return fallback
			}
			if n, owned := s.topNode(id); owned {
				return n
			}
			return fallback
		}
		var baseNodes map[string]*Node
		if len(need) > 0 {
			baseNodes = dw.view.GetNodesByIDs(need)
		}
		out := make([]ScopedEdgeRow, 0, len(rows)+len(layered))
		for _, row := range rows {
			out = append(out, ScopedEdgeRow{
				Edge:   row.Edge,
				Source: endpoint(row.Edge.From, row.Source),
				Target: endpoint(row.Edge.To, row.Target),
			})
		}
		var targetIDs []string
		for _, p := range layered {
			src := baseNodes[p.edge.From]
			if !inScope(src, repos, files) {
				continue
			}
			out = append(out, ScopedEdgeRow{Edge: p.edge, Source: src})
			if p.edge.To != "" && !IsUnresolvedTarget(p.edge.To) {
				targetIDs = append(targetIDs, p.edge.To)
			}
		}
		var targets map[string]*Node
		if len(targetIDs) > 0 {
			targets = dw.view.GetNodesByIDs(targetIDs)
		}
		for i := len(rows); i < len(out); i++ {
			out[i].Target = targets[out[i].Edge.To]
		}
		for _, row := range out {
			if !yield(ScopedEdgeRow{
				Edge:   cloneDeltaEdge(row.Edge),
				Source: cloneDeltaNode(row.Source),
				Target: cloneDeltaNode(row.Target),
			}) {
				return
			}
		}
	}
}

func (dw *DeltaWriter) fallbackScopedEdges(repoPrefixes, filePaths []string, wantKind map[EdgeKind]struct{}, yield func(ScopedEdgeRow) bool) {
	repos, files := stringKeySet(repoPrefixes), stringKeySet(filePaths)
	var sources []*Node
	if len(filePaths) > 0 {
		for _, nodes := range dw.view.GetFileNodesByPathsFallback(filePaths) {
			sources = append(sources, nodes...)
		}
	} else {
		for _, repo := range repoPrefixes {
			sources = append(sources, dw.view.GetRepoNodes(repo)...)
		}
	}
	byID := make(map[string]*Node, len(sources))
	ids := make([]string, 0, len(sources))
	for _, n := range sources {
		if inScope(n, repos, files) {
			byID[n.ID] = n
			ids = append(ids, n.ID)
		}
	}
	var edges []*Edge
	var targetIDs []string
	for _, id := range ids {
		for _, e := range dw.view.GetOutEdgesByNodeIDs([]string{id})[id] {
			if _, ok := wantKind[e.Kind]; ok {
				edges = append(edges, e)
				targetIDs = append(targetIDs, e.To)
			}
		}
	}
	targets := dw.view.GetNodesByIDs(targetIDs)
	for _, e := range edges {
		if !yield(ScopedEdgeRow{Edge: cloneDeltaEdge(e), Source: cloneDeltaNode(byID[e.From]), Target: cloneDeltaNode(targets[e.To])}) {
			return
		}
	}
}

// GetFileNodesByPathsFallback reads a file batch one path at a time through
// the view, for stores with no batched form.
func (v *OverlaidView) GetFileNodesByPathsFallback(paths []string) map[string][]*Node {
	out := make(map[string][]*Node, len(paths))
	for _, p := range UniqueRecordingPaths(paths) {
		if nodes := v.GetFileNodes(p); len(nodes) > 0 {
			out[p] = nodes
		}
	}
	return out
}

// kindEdgesByNodeIDs answers a kind-filtered adjacency batch through the
// stack: the store at the bottom serves it (read), rows survive the
// composition's edge rule, and each layer adds its own rows of the kind.
func (dw *DeltaWriter) kindEdgesByNodeIDs(
	ids []string, kind EdgeKind, incoming bool,
	read func(base Reader) (map[string][]*Edge, bool),
) map[string][]*Edge {
	s := dw.stack()
	uniq := UniqueRecordingPaths(ids)
	out := make(map[string][]*Edge, len(uniq))
	base, ok := read(s.base)
	if !ok {
		var full map[string][]*Edge
		if incoming {
			full = dw.view.GetInEdgesByNodeIDs(uniq)
		} else {
			full = dw.view.GetOutEdgesByNodeIDs(uniq)
		}
		for id, edges := range full {
			for _, e := range edges {
				if e != nil && e.Kind == kind {
					out[id] = append(out[id], cloneDeltaEdge(e))
				}
			}
		}
		return out
	}
	for id, edges := range base {
		for _, e := range edges {
			if e != nil && e.Kind == kind && s.edgeVisibleFrom(e, 0) {
				out[id] = append(out[id], e)
			}
		}
	}
	for i, l := range s.layers {
		for id, edges := range dw.layerAdjacency(l, uniq, incoming) {
			for _, e := range edges {
				if e != nil && e.Kind == kind && s.edgeVisibleFrom(e, i+1) {
					out[id] = append(out[id], e)
				}
			}
		}
	}
	for id, edges := range out {
		out[id] = cloneDeltaEdges(edges)
	}
	return out
}

// GetDataflowParamEdgesByOwnerIDs is the dataflow pass's parameter batch: the
// param_of edges into each owner, through the stack.
func (dw *DeltaWriter) GetDataflowParamEdgesByOwnerIDs(ownerIDs []string) map[string][]*Edge {
	return dw.kindEdgesByNodeIDs(ownerIDs, EdgeParamOf, true, func(base Reader) (map[string][]*Edge, bool) {
		r, ok := base.(interface {
			GetDataflowParamEdgesByOwnerIDs([]string) map[string][]*Edge
		})
		if !ok {
			return nil, false
		}
		return r.GetDataflowParamEdgesByOwnerIDs(UniqueRecordingPaths(ownerIDs)), true
	})
}

// GetDataflowCallEdgesByCallerIDs is the dataflow pass's call batch: the calls
// edges out of each caller, through the stack.
func (dw *DeltaWriter) GetDataflowCallEdgesByCallerIDs(callerIDs []string) map[string][]*Edge {
	return dw.kindEdgesByNodeIDs(callerIDs, EdgeCalls, false, func(base Reader) (map[string][]*Edge, bool) {
		r, ok := base.(interface {
			GetDataflowCallEdgesByCallerIDs([]string) map[string][]*Edge
		})
		if !ok {
			return nil, false
		}
		return r.GetDataflowCallEdgesByCallerIDs(UniqueRecordingPaths(callerIDs)), true
	})
}

var _ ScopedProjectionSequencer = (*DeltaWriter)(nil)

// OverlayLayerProjectionReader is an optional capability of an immutable
// overlay layer: generation-scoped, indexed projections of its own rows, so a
// delta composing over it never loads the layer wholesale.
type OverlayLayerProjectionReader interface {
	LayerNodesInScope(repoPrefixes, filePaths []string, light bool, kinds ...NodeKind) []*Node
	LayerEdgesByKinds(kinds []EdgeKind) []*Edge
	LayerInEdgesByNodeIDs(ids []string) map[string][]*Edge
	LayerOutEdgesByNodeIDs(ids []string) map[string][]*Edge
	LayerNodesByNames(names []string) map[string][]*Node
	LayerRepoEdgesByKinds(repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow
}

// layerAdjacencyCache holds, per immutable layer and direction, the adjacency
// already read by identity during this delta (an absent entry was read and is
// empty), so an identity is read from a layer at most once per delta.
type layerAdjacencyCache struct {
	out map[string][]*Edge
	in  map[string][]*Edge
}

func (dw *DeltaWriter) cachedLayerAdjacency(l OverlayLayerReader, p OverlayLayerProjectionReader, ids []string, incoming bool) map[string][]*Edge {
	if out, ok := dw.stackLayerAdjacency(l, p, ids, incoming); ok {
		return out
	}
	dw.nameIndexMu.Lock()
	if dw.adjacencyCache == nil {
		dw.adjacencyCache = make(map[OverlayLayerReader]*layerAdjacencyCache)
	}
	cache := dw.adjacencyCache[l]
	if cache == nil {
		cache = &layerAdjacencyCache{out: make(map[string][]*Edge), in: make(map[string][]*Edge)}
		dw.adjacencyCache[l] = cache
	}
	known := cache.out
	if incoming {
		known = cache.in
	}
	var missing []string
	seen := make(map[string]struct{}, len(ids))
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
	dw.nameIndexMu.Unlock()
	if len(missing) > 0 {
		var fetched map[string][]*Edge
		if incoming {
			fetched = p.LayerInEdgesByNodeIDs(missing)
		} else {
			fetched = p.LayerOutEdgesByNodeIDs(missing)
		}
		n := 0
		dw.nameIndexMu.Lock()
		for _, id := range missing {
			edges := fetched[id]
			n += len(edges)
			if edges == nil {
				edges = []*Edge{}
			}
			known[id] = edges
		}
		dw.nameIndexMu.Unlock()
		if incoming {
			dw.noteLayerRowsFor("in_edges_by_ids", n)
		} else {
			dw.noteLayerRowsFor("out_edges_by_ids", n)
		}
	}
	out := make(map[string][]*Edge, len(seen))
	dw.nameIndexMu.Lock()
	for id := range seen {
		if edges := known[id]; len(edges) > 0 {
			out[id] = edges
		}
	}
	dw.nameIndexMu.Unlock()
	return out
}
