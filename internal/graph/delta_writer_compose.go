package graph

import (
	"fmt"
	"iter"
	"sort"
	"sync"
)

// Batched reads through the layer stack.
//
// OverlaidView answers a batched name or file read one key at a time on every
// level of a stack (a chain of working-tree generations over a commit layer
// over the base corpus), which turns one engine read of two thousand names
// into two thousand queries per level. A delta knows the stack it composes
// over, so it answers the same reads in the stack's own terms instead: one
// batched query on the store at the bottom, each layer's own rows from its
// in-memory indexes, and the composition's ownership rule applied row by row —
// a lower row survives exactly when no layer above it speaks for its identity
// (it covers the row's file, carries the identity, or removed it).

// deltaStack is the view below a delta taken apart: the store at the bottom
// and the overlay layers above it, lowest first, ending with the delta's own.
type deltaStack struct {
	base   Reader
	layers []OverlayLayerReader
}

// Unwrapper is implemented by reader wrappers that add capabilities to a
// composed view without changing what it answers.
type Unwrapper interface {
	Unwrap() Reader
}

// stack unwraps the delta's view into its store and layers. It is computed on
// every call: the layer list is short and the delta's own layer is mutable.
func (dw *DeltaWriter) stack() deltaStack {
	var layers []OverlayLayerReader
	var r Reader = dw.view
	for {
		switch v := r.(type) {
		case *OverlaidView:
			if v.layer != nil {
				layers = append(layers, v.layer)
			}
			if v.base == nil {
				return deltaStack{layers: reverseLayers(layers)}
			}
			r = v.base
			continue
		case Unwrapper:
			if next := v.Unwrap(); next != nil {
				r = next
				continue
			}
		}
		return deltaStack{base: r, layers: reverseLayers(layers)}
	}
}

func reverseLayers(in []OverlayLayerReader) []OverlayLayerReader {
	out := make([]OverlayLayerReader, len(in))
	for i, l := range in {
		out[len(in)-1-i] = l
	}
	return out
}

// hiddenAbove reports whether any layer from index `from` upwards speaks for
// a node identity, so a lower row under it is not served.
func (s deltaStack) hiddenAbove(id string, from int) bool {
	for i := from; i < len(s.layers); i++ {
		if s.layers[i].CoversNodeID(id) || s.layers[i].OwnsNodeIdentity(id) {
			return true
		}
	}
	return false
}

// coveredByAny reports whether any layer covers a path.
func (s deltaStack) coveredByAny(path string) bool {
	for _, l := range s.layers {
		if l.HasFile(path) {
			return true
		}
	}
	return false
}

// layerNameIndex is one immutable layer's short-name index, built once from
// its NamedNodes.
type layerNameIndex struct {
	once  sync.Once
	names map[string][]*Node
}

// nameIndexFor returns a layer's name index. The delta's own layer is mutable
// and is answered from the working graph instead; every layer below it is an
// immutable published generation, indexed once per delta.
func (dw *DeltaWriter) nameIndexFor(l OverlayLayerReader) map[string][]*Node {
	dw.nameIndexMu.Lock()
	if dw.nameIndexes == nil {
		dw.nameIndexes = make(map[OverlayLayerReader]*layerNameIndex)
	}
	idx := dw.nameIndexes[l]
	if idx == nil {
		idx = &layerNameIndex{}
		dw.nameIndexes[l] = idx
	}
	dw.nameIndexMu.Unlock()
	idx.once.Do(func() {
		idx.names = make(map[string][]*Node)
		for name, nodes := range l.NamedNodes() {
			idx.names[name] = append(idx.names[name], nodes...)
		}
	})
	return idx.names
}

// batchedNamesReader is the store capability the bottom of the stack serves
// batched name lookups with.
type batchedNamesReader interface {
	FindNodesByNames(names []string) map[string][]*Node
}

// composedFindNodesByNames answers FindNodesByNames through the stack: one
// batched query on the store at the bottom, every layer's own nodes, each row
// kept only when no layer above its level speaks for its identity. ok is false
// when the store at the bottom has no batched form.
func (dw *DeltaWriter) composedFindNodesByNames(names []string) (map[string][]*Node, bool) {
	s := dw.stack()
	base, ok := s.base.(batchedNamesReader)
	if !ok {
		return nil, false
	}
	out := make(map[string][]*Node, len(names))
	for name, nodes := range base.FindNodesByNames(names) {
		for _, n := range nodes {
			if n != nil && !s.hiddenAbove(n.ID, 0) {
				out[name] = append(out[name], n)
			}
		}
	}
	for i, l := range s.layers {
		if l == OverlayLayerReader(dw.layer) {
			for _, name := range names {
				for _, n := range dw.work.FindNodesByName(name) {
					if n != nil && !s.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
			continue
		}
		if p, ok := l.(OverlayLayerProjectionReader); ok {
			for name, nodes := range p.LayerNodesByNames(names) {
				dw.noteLayerRowsFor("nodes_by_names", len(nodes))
				for _, n := range nodes {
					if n != nil && !s.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
			continue
		}
		index := dw.nameIndexFor(l)
		for _, name := range names {
			for _, n := range index[name] {
				if n != nil && !s.hiddenAbove(n.ID, i+1) {
					out[name] = append(out[name], n)
				}
			}
		}
	}
	for name, nodes := range out {
		if len(nodes) == 0 {
			delete(out, name)
		}
	}
	return out, true
}

// batchedFileNodesReader is the store capability the bottom of the stack
// serves batched file reads with.
type batchedFileNodesReader interface {
	GetFileNodesByPaths(paths []string) map[string][]*Node
}

// composedFileNodesByPaths answers GetFileNodesByPaths through the stack:
// paths no layer covers come from one batched query on the store at the
// bottom (rows filtered by identity ownership, plus any layer's detached rows
// at the path), and covered paths are read through the view. ok is false when
// the store at the bottom has no batched form.
func (dw *DeltaWriter) composedFileNodesByPaths(paths []string) (map[string][]*Node, bool) {
	s := dw.stack()
	base, ok := s.base.(batchedFileNodesReader)
	if !ok {
		return nil, false
	}
	if k, ok := dw.cacheSplit(s); ok {
		if below := dw.viewBelowLevel(k); below != nil {
			return dw.stackComposedFileNodes(s, k, below, base, paths), true
		}
	}
	var clean, dirty []string
	for _, p := range UniqueRecordingPaths(paths) {
		if p == "" {
			continue
		}
		if s.coveredByAny(p) {
			dirty = append(dirty, p)
		} else {
			clean = append(clean, p)
		}
	}
	out := make(map[string][]*Node, len(paths))
	if len(clean) > 0 {
		for p, nodes := range base.GetFileNodesByPaths(clean) {
			for _, n := range nodes {
				if n != nil && !s.hiddenAbove(n.ID, 0) {
					out[p] = append(out[p], n)
				}
			}
		}
		for i, l := range s.layers {
			detached, ok := l.(OverlayDetachedNodeReader)
			if !ok {
				continue
			}
			for _, p := range clean {
				for _, n := range detached.DetachedFileNodes(p) {
					if n != nil && !s.hiddenAbove(n.ID, i+1) {
						out[p] = append(out[p], n)
					}
				}
			}
		}
	}
	for _, p := range dirty {
		if nodes := dw.view.GetFileNodes(p); len(nodes) > 0 {
			out[p] = nodes
		}
	}
	for p, nodes := range out {
		if len(nodes) == 0 {
			delete(out, p)
		}
	}
	return out, true
}

// FindNodesByNamesInRepoLanguages implements RepoLanguageNameFinder from the
// stack's batched name read, filtered to one repository and a language
// allow-list (empty: every language), each bucket ordered by identity.
func (dw *DeltaWriter) FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*Node {
	if names := uniqueNonEmptyNames(names); len(names) > 0 {
		if hits, ok := dw.stackCachedRepoNames(dw.stack(), names, repoPrefix, languages, true); ok {
			return hits
		}
	}
	hits := dw.FindNodesByNames(names)
	wantLang := make(map[string]struct{}, len(languages))
	for _, l := range languages {
		wantLang[l] = struct{}{}
	}
	out := make(map[string][]*Node, len(hits))
	for name, nodes := range hits {
		for _, n := range nodes {
			if n == nil || n.RepoPrefix != repoPrefix {
				continue
			}
			if len(wantLang) > 0 {
				if _, ok := wantLang[n.Language]; !ok {
					continue
				}
			}
			out[name] = append(out[name], n)
		}
		sortNodesByID(out[name])
	}
	for name, nodes := range out {
		if len(nodes) == 0 {
			delete(out, name)
		}
	}
	return out
}

func sortNodesByID(nodes []*Node) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

var _ RepoLanguageNameFinder = (*DeltaWriter)(nil)

// batchedAdjacencyReader is the store capability the bottom of the stack
// serves adjacency batches with.
type batchedAdjacencyReader interface {
	GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge
	GetInEdgesByNodeIDs(ids []string) map[string][]*Edge
}

// composedEdgesByNodeIDs answers an adjacency batch through the stack: one
// batched query on the store at the bottom, each immutable layer's rows from
// its cached row set and the delta's own from the working graph, every row
// kept only when no layer above its level hides it. withDelta false answers
// for the view below the delta. ok is false when the store at the bottom has
// no batched form. The rows are the ones the composed view serves; they are
// shared and must not be mutated.
func (dw *DeltaWriter) composedEdgesByNodeIDs(ids []string, incoming, withDelta bool) (map[string][]*Edge, bool) {
	s := dw.stack()
	if !withDelta && len(s.layers) > 0 && s.layers[len(s.layers)-1] == OverlayLayerReader(dw.layer) {
		s.layers = s.layers[:len(s.layers)-1]
	}
	return dw.composeEdgesOver(s, ids, incoming)
}

// composeEdgesOver is composedEdgesByNodeIDs over stack s.
func (dw *DeltaWriter) composeEdgesOver(s deltaStack, ids []string, incoming bool) (map[string][]*Edge, bool) {
	base, ok := s.base.(batchedAdjacencyReader)
	if !ok {
		return nil, false
	}
	uniq := uniqueIDs(ids)
	out := make(map[string][]*Edge, len(uniq))
	if len(uniq) == 0 {
		return out, true
	}
	load := func(ids []string) map[string][]*Edge {
		if incoming {
			return base.GetInEdgesByNodeIDs(ids)
		}
		return base.GetOutEdgesByNodeIDs(ids)
	}
	// The bottom store's rows are kept per stack (unfiltered: the loop
	// below filters them against the full stack, this delta's layer
	// included).
	rows, cached := dw.stackBaseAdjacency(uniq, incoming, load)
	if !cached {
		rows = load(uniq)
	}
	for _, id := range uniq {
		for _, e := range rows[id] {
			if e != nil && s.edgeVisibleFrom(e, 0) {
				out[id] = append(out[id], e)
			}
		}
	}
	for i, l := range s.layers {
		for id, edges := range dw.layerAdjacency(l, uniq, incoming) {
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
		}
	}
	return out, true
}

// composedEdgesByKind answers EdgesByKind through the stack: the store at the
// bottom's kind scan, each immutable layer's generation-scoped kind scan and
// the working graph's rows, each kept only when no layer above hides it. ok
// is false when a layer below has no generation-scoped projection.
func (dw *DeltaWriter) composedEdgesByKind(kind EdgeKind) ([]*Edge, bool) {
	return dw.composeEdgesByKindOver(dw.stack(), kind)
}

// composeEdgesByKindOver is composedEdgesByKind over stack s.
func (dw *DeltaWriter) composeEdgesByKindOver(s deltaStack, kind EdgeKind) ([]*Edge, bool) {
	if s.base == nil {
		return nil, false
	}
	for _, l := range s.layers {
		if _, ok := l.(OverlayLayerProjectionReader); !ok && l != OverlayLayerReader(dw.layer) {
			return nil, false
		}
	}
	var out []*Edge
	baseRows := s.base.EdgesByKind(kind)
	if kindFirst, ok := s.base.(interface {
		EdgesByKindKindFirst(EdgeKind) iter.Seq[*Edge]
	}); ok {
		// The bottom of the stack is a whole corpus (generation zero, or a
		// dedicated root generation): read it by kind, not generation-first.
		baseRows = kindFirst.EdgesByKindKindFirst(kind)
	}
	for e := range baseRows {
		if e != nil && s.edgeVisibleFrom(e, 0) {
			out = append(out, e)
		}
	}
	for i, l := range s.layers {
		for _, e := range dw.layerEdgesOfKinds(l, []EdgeKind{kind}) {
			if e != nil && e.Kind == kind && s.edgeVisibleFrom(e, i+1) {
				out = append(out, e)
			}
		}
	}
	return out, true
}

// composedNodesByKind answers NodesByKind through the stack, like
// composedEdgesByKind.
func (dw *DeltaWriter) composedNodesByKind(kind NodeKind) ([]*Node, bool) {
	s := dw.stack()
	if s.base == nil {
		return nil, false
	}
	for _, l := range s.layers {
		if _, ok := l.(interface {
			NodesByKind(NodeKind) iter.Seq[*Node]
		}); !ok {
			return nil, false
		}
	}
	var out []*Node
	for n := range s.base.NodesByKind(kind) {
		if n != nil && !s.hiddenAbove(n.ID, 0) {
			out = append(out, n)
		}
	}
	for i, l := range s.layers {
		byKind := l.(interface {
			NodesByKind(NodeKind) iter.Seq[*Node]
		})
		for n := range byKind.NodesByKind(kind) {
			if n != nil && !s.hiddenAbove(n.ID, i+1) {
				out = append(out, n)
			}
		}
	}
	return out, true
}

// RepoEdgesByKinds implements RepoEdgeKindReader through the stack: the store
// at the bottom's indexed repository projection plus each layer's own rows of
// the kinds whose source falls in the repositories, each kept only when no
// layer above hides it. It is the contract registry's load path, which must
// not scan the composed view.
func (dw *DeltaWriter) RepoEdgesByKinds(repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow {
	s := dw.stack()
	base, ok := s.base.(RepoEdgeKindReader)
	if !ok {
		return readRepoEdgesByKindsGeneric(dw, repoPrefixes, kinds)
	}
	for _, l := range s.layers {
		if _, ok := l.(OverlayLayerProjectionReader); !ok && l != OverlayLayerReader(dw.layer) {
			return readRepoEdgesByKindsGeneric(dw, repoPrefixes, kinds)
		}
	}
	var out []RepoEdgeRow
	for _, row := range base.RepoEdgesByKinds(repoPrefixes, kinds) {
		if row.Edge != nil && s.edgeVisibleFrom(row.Edge, 0) {
			out = append(out, row)
		}
	}
	wanted := stringKeySet(repoPrefixes)
	for i, l := range s.layers {
		if p, ok := l.(OverlayLayerProjectionReader); ok {
			layerRows := p.LayerRepoEdgesByKinds(repoPrefixes, kinds)
			dw.noteLayerRowsFor("repo_edges_by_kinds"+layerRowKinds(edgeKindNames(kinds)), len(layerRows))
			for _, row := range layerRows {
				if row.Edge != nil && s.edgeVisibleFrom(row.Edge, i+1) {
					out = append(out, row)
				}
			}
			continue
		}
		// The delta's own rows: the source's repository from the composition.
		edges := dw.layerEdgesOfKinds(l, kinds)
		ids := make([]string, 0, len(edges))
		for _, e := range edges {
			ids = append(ids, e.From)
		}
		sources := dw.view.GetNodesByIDs(ids)
		for _, e := range edges {
			src := sources[e.From]
			if src == nil || !s.edgeVisibleFrom(e, i+1) {
				continue
			}
			if _, want := wanted[src.RepoPrefix]; want {
				out = append(out, RepoEdgeRow{RepoPrefix: src.RepoPrefix, Edge: e})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.Edge.From != b.Edge.From {
			return a.Edge.From < b.Edge.From
		}
		if a.Edge.To != b.Edge.To {
			return a.Edge.To < b.Edge.To
		}
		if a.Edge.Kind != b.Edge.Kind {
			return a.Edge.Kind < b.Edge.Kind
		}
		if a.Edge.FilePath != b.Edge.FilePath {
			return a.Edge.FilePath < b.Edge.FilePath
		}
		return a.Edge.Line < b.Edge.Line
	})
	for i := range out {
		out[i].Edge = cloneDeltaEdge(out[i].Edge)
	}
	return out
}

var _ RepoEdgeKindReader = (*DeltaWriter)(nil)

// readRepoEdgesByKindsGeneric is ReadRepoEdgesByKinds over the delta's plain
// Store surface (its composed kind scans), for a stack whose layers cannot
// answer the repository projection themselves.
func readRepoEdgesByKindsGeneric(dw *DeltaWriter, repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow {
	return ReadRepoEdgesByKinds(struct{ Store }{dw}, repoPrefixes, kinds)
}

// StackShape names the store at the bottom of the delta's view and each layer
// above it, lowest first (Go types, with the generation-projection capability
// marked), for the delta's log.
func (dw *DeltaWriter) StackShape() []string {
	s := dw.stack()
	out := []string{fmt.Sprintf("base=%T", s.base)}
	for _, l := range s.layers {
		name := fmt.Sprintf("%T", l)
		if _, ok := l.(OverlayLayerProjectionReader); ok {
			name += "+projections"
		}
		out = append(out, name)
	}
	return out
}

// stackComposedFileNodes is composedFileNodesByPaths with the stack below
// layer k kept per stack: a path's nodes as that immutable part composes them
// are read once per stack (its per-path file-node read, the changed file's
// prior graph among them), and every layer from k up — the dirty chain's and
// the delta's own — is applied per read: its hiding and its detached rows, or,
// for a path one of them covers, the delta's composed view as before. view is
// the composed view below layer k.
func (dw *DeltaWriter) stackComposedFileNodes(s deltaStack, k int, view Reader, base batchedFileNodesReader, paths []string) map[string][]*Node {
	below := deltaStack{base: s.base, layers: s.layers[:k]}
	var clean, own []string
	for _, p := range UniqueRecordingPaths(paths) {
		if p == "" {
			continue
		}
		if s.coveredFrom(p, k) {
			own = append(own, p)
		} else {
			clean = append(clean, p)
		}
	}
	out := make(map[string][]*Node, len(paths))
	if len(clean) > 0 {
		kept := dw.baseCache.stackFileNodesByPath(clean, func(missing []string) map[string][]*Node {
			return dw.composeFileNodesOver(below, base, view, missing)
		})
		for _, p := range clean {
			for _, n := range kept[p] {
				if n != nil && !s.hiddenAbove(n.ID, k) {
					out[p] = append(out[p], n)
				}
			}
			for i := k; i < len(s.layers); i++ {
				for _, n := range dw.layerDetachedFileNodes(s.layers[i], p) {
					if n != nil && !s.hiddenAbove(n.ID, i+1) {
						out[p] = append(out[p], n)
					}
				}
			}
		}
	}
	for _, p := range own {
		if nodes := dw.view.GetFileNodes(p); len(nodes) > 0 {
			out[p] = nodes
		}
	}
	for p, nodes := range out {
		if len(nodes) == 0 {
			delete(out, p)
		}
	}
	return out
}

// composeFileNodesOver is the file-node composition over stack st whose
// composed view is view: the bottom store's rows at paths no layer covers,
// each layer's detached rows there, and the view's answer at covered paths.
func (dw *DeltaWriter) composeFileNodesOver(st deltaStack, base batchedFileNodesReader, view Reader, paths []string) map[string][]*Node {
	var clean, covered []string
	for _, p := range paths {
		if st.coveredByAny(p) {
			covered = append(covered, p)
		} else {
			clean = append(clean, p)
		}
	}
	out := make(map[string][]*Node, len(paths))
	if len(clean) > 0 {
		for p, nodes := range base.GetFileNodesByPaths(clean) {
			for _, n := range nodes {
				if n != nil && !st.hiddenAbove(n.ID, 0) {
					out[p] = append(out[p], n)
				}
			}
		}
		for i, l := range st.layers {
			detached, ok := l.(OverlayDetachedNodeReader)
			if !ok {
				continue
			}
			for _, p := range clean {
				for _, n := range detached.DetachedFileNodes(p) {
					if n != nil && !st.hiddenAbove(n.ID, i+1) {
						out[p] = append(out[p], n)
					}
				}
			}
		}
	}
	for _, p := range covered {
		if nodes := view.GetFileNodes(p); len(nodes) > 0 {
			out[p] = nodes
		}
	}
	return out
}
