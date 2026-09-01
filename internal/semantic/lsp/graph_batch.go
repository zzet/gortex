package lsp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// viewPathKey normalizes a graph file path for nodesByFile keying and
// comparisons. Store vintages carry both separator spellings — older
// Windows rows use `\` after the repo prefix while newer rows and every
// URI-derived path use `/` — so any join between a server answer and a
// node path must use the slash-normalized spelling. Mirrors the store's
// own file_dir separator normalization.
func viewPathKey(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// storePathSpellings returns the scoped-path spellings a store row may
// carry for a repo-relative path: the slash spelling plus, when the rel
// has separators, the backslash spelling older Windows rows use. Store
// lookups by file_path are exact string matches, so a URI-derived
// slash-relative path must query both.
func storePathSpellings(repoPrefix, rel string) []string {
	if rel == "" {
		return nil
	}
	spellings := []string{scopedPath(repoPrefix, rel)}
	if back := strings.ReplaceAll(rel, "/", `\`); back != rel {
		spellings = append(spellings, scopedPath(repoPrefix, back))
	}
	return spellings
}

type lspEdgeKey struct {
	from     string
	to       string
	kind     graph.EdgeKind
	filePath string
	line     int
}

// lspGraphView is a repo-scoped or explicitly bounded structural projection
// used by LSP enrichment. It turns per-result point lookups into map reads while
// retaining only nodes and edges the caller already selected from SQLite.
type lspGraphView struct {
	nodesByID   map[string]*graph.Node
	nodesByFile map[string][]*graph.Node
	outByID     map[string][]*graph.Edge
	inByID      map[string][]*graph.Edge
	fanInByID   map[string]int
	edgeKeys    map[lspEdgeKey]struct{}
}

func newLSPGraphView(nodes []*graph.Node, edges []*graph.Edge) *lspGraphView {
	v := &lspGraphView{
		nodesByID:   make(map[string]*graph.Node, len(nodes)),
		nodesByFile: make(map[string][]*graph.Node),
		outByID:     make(map[string][]*graph.Edge),
		inByID:      make(map[string][]*graph.Edge),
		fanInByID:   make(map[string]int),
		edgeKeys:    make(map[lspEdgeKey]struct{}, len(edges)),
	}
	v.addNodes(nodes)
	v.addEdges(edges)
	return v
}

func (v *lspGraphView) addNodes(nodes []*graph.Node) {
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if old, exists := v.nodesByID[n.ID]; exists {
			v.nodesByID[n.ID] = n
			oldKey, newKey := viewPathKey(old.FilePath), viewPathKey(n.FilePath)
			bucket := v.nodesByFile[oldKey]
			for i, candidate := range bucket {
				if candidate.ID != n.ID {
					continue
				}
				if oldKey == newKey {
					bucket[i] = n
					v.nodesByFile[oldKey] = bucket
				} else {
					v.nodesByFile[oldKey] = append(bucket[:i], bucket[i+1:]...)
					v.nodesByFile[newKey] = append(v.nodesByFile[newKey], n)
				}
				break
			}
			continue
		}
		v.nodesByID[n.ID] = n
		key := viewPathKey(n.FilePath)
		v.nodesByFile[key] = append(v.nodesByFile[key], n)
	}
}

func (v *lspGraphView) addEdges(edges []*graph.Edge) {
	for _, e := range edges {
		if e == nil {
			continue
		}
		key := lspEdgeIdentity(e)
		if _, exists := v.edgeKeys[key]; exists {
			continue
		}
		v.edgeKeys[key] = struct{}{}
		v.outByID[e.From] = append(v.outByID[e.From], e)
		v.inByID[e.To] = append(v.inByID[e.To], e)
	}
}

func (v *lspGraphView) matchNodeByFileLine(filePath string, line int) *graph.Node {
	filePath = viewPathKey(filePath)
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range v.nodesByFile[filePath] {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	if best != nil {
		return best
	}
	bestDist := int(^uint(0) >> 1)
	for _, n := range v.nodesByFile[filePath] {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		dist := lspAbs(n.StartLine - line)
		if dist < bestDist {
			best = n
			bestDist = dist
		}
	}
	if bestDist <= 2 {
		return best
	}
	return nil
}

func (v *lspGraphView) matchCallableByFileLine(filePath string, line int) *graph.Node {
	filePath = viewPathKey(filePath)
	callable := func(k graph.NodeKind) bool {
		return k == graph.KindFunction || k == graph.KindMethod || k == graph.KindClosure
	}
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range v.nodesByFile[filePath] {
		if !callable(n.Kind) {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	if best != nil {
		return best
	}
	bestDist := int(^uint(0) >> 1)
	for _, n := range v.nodesByFile[filePath] {
		if !callable(n.Kind) {
			continue
		}
		dist := lspAbs(n.StartLine - line)
		if dist < bestDist {
			best = n
			bestDist = dist
		}
	}
	if bestDist <= 2 {
		return best
	}
	return nil
}

func (v *lspGraphView) findDeclarationNode(filePath string, oneBasedLine int, name string) *graph.Node {
	filePath = viewPathKey(filePath)
	var near *graph.Node
	for _, n := range v.nodesByFile[filePath] {
		if n == nil || n.Name != name {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport || n.Kind == graph.KindParam {
			continue
		}
		if n.StartLine == oneBasedLine {
			return n
		}
		if near == nil && n.StartLine >= oneBasedLine-1 && n.StartLine <= oneBasedLine+1 {
			near = n
		}
	}
	return near
}

// findEnclosingTypeNamed returns the type / interface declaration named name
// whose span contains oneBasedLine in filePath. This is the ctor shape:
// definition on `new T(...)` answers the constructor's line — a member with
// its own name — but the line sits inside the declaration of the very type
// the site instantiates.
func (v *lspGraphView) findEnclosingTypeNamed(filePath string, oneBasedLine int, name string) *graph.Node {
	for _, n := range v.nodesByFile[viewPathKey(filePath)] {
		if n == nil || n.Name != name {
			continue
		}
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if n.StartLine <= oneBasedLine && oneBasedLine <= n.EndLine {
			return n
		}
	}
	return nil
}

// declaredDispatchMember reports whether n is a declared dispatch target — a
// member of an interface, or an abstract-marked member. A definition landing
// on one means the call site's static receiver is the declared surface, not
// any concrete impl.
func (v *lspGraphView) declaredDispatchMember(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if isAbstractMarked(n) {
		return true
	}
	parent := v.memberParentType(n)
	return parent != nil && parent.Kind == graph.KindInterface
}

// implementsDeclaredMember reports whether impl is a concrete implementation
// of the declared member decl: an explicit overrides edge, or membership in a
// type that implements / extends decl's declaring type.
func (v *lspGraphView) implementsDeclaredMember(impl, decl *graph.Node) bool {
	if impl == nil || decl == nil {
		return false
	}
	for _, e := range v.outByID[impl.ID] {
		if e.Kind == graph.EdgeOverrides && e.To == decl.ID {
			return true
		}
	}
	implParent := v.memberParentType(impl)
	declParent := v.memberParentType(decl)
	if implParent == nil || declParent == nil {
		return false
	}
	for _, e := range v.outByID[implParent.ID] {
		if (e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends) && e.To == declParent.ID {
			return true
		}
	}
	return false
}

// memberParentType resolves n's declaring type through its member_of edge.
func (v *lspGraphView) memberParentType(n *graph.Node) *graph.Node {
	for _, e := range v.outByID[n.ID] {
		if e.Kind == graph.EdgeMemberOf {
			return v.nodesByID[e.To]
		}
	}
	return nil
}

func (v *lspGraphView) findMatchingEdge(from, to string, kind graph.EdgeKind) *graph.Edge {
	for _, e := range v.outByID[from] {
		if e.To == to && e.Kind == kind {
			return e
		}
	}
	return nil
}

func (v *lspGraphView) edgeExistsAt(from, to string, kind graph.EdgeKind, line int) bool {
	for _, e := range v.outByID[from] {
		if e.To == to && e.Kind == kind && e.Line == line {
			return true
		}
	}
	return false
}

func (v *lspGraphView) setFanInCounts(counts map[string]int) {
	for id, count := range counts {
		v.fanInByID[id] = count
	}
}

func (v *lspGraphView) fanIn(id string) int {
	if count, projected := v.fanInByID[id]; projected {
		return count
	}
	return len(v.inByID[id])
}

func (v *lspGraphView) hasUnresolvedDemand(n *graph.Node) bool {
	if n == nil || n.Name == "" || (n.Kind != graph.KindMethod && n.Kind != graph.KindFunction) {
		return false
	}
	return len(v.inByID[graph.UnresolvedMarker+"*."+n.Name]) > 0
}

// typeIsDispatchRelevant reports whether a type declaration's super/subtype
// hierarchy is worth interrogating. An interface always is: it is the
// dispatch surface by definition, and its implementers' AST edges may be
// exactly what failed to resolve — the case where it looks adjacency-less is
// the case where the sweep is most needed. A class qualifies only through
// hierarchy involvement: an implements / extends edge in either direction.
// Edge KINDS survive even when the AST could not resolve the target, so a
// class with an unresolvable base list still qualifies — recovering those
// cross-file / dynamic hierarchy edges is the sweep's whole value for types.
// A bare data type with neither buys nothing from hover or hierarchy
// interrogation, and no longer keeps its file in the demand-gated sweep.
//
// That strict check presumes SOME lane other than the sweep can mint the
// qualifying edge. hierarchyEvidence says whether one has (see
// enrichLanguageHasHierarchyEvidence); when it has not, every class is
// treated as hierarchy-involved — the pre-gate permissive behaviour — because
// in such a language the sweep is the only producer of the very edge the
// strict check would require.
func (v *lspGraphView) typeIsDispatchRelevant(n *graph.Node, hierarchyEvidence bool) bool {
	if n == nil {
		return false
	}
	if n.Kind == graph.KindInterface {
		return true
	}
	if n.Kind != graph.KindType {
		return false
	}
	if !hierarchyEvidence {
		return true
	}
	for _, e := range v.outByID[n.ID] {
		if e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends {
			return true
		}
	}
	for _, e := range v.inByID[n.ID] {
		if e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends {
			return true
		}
	}
	return false
}

func (v *lspGraphView) callableIsDispatchRelevant(n *graph.Node) bool {
	if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
		return false
	}
	if isAbstractMarked(n) {
		return true
	}
	var parentType string
	for _, e := range v.outByID[n.ID] {
		switch e.Kind {
		case graph.EdgeOverrides:
			return true
		case graph.EdgeMemberOf:
			parentType = e.To
		}
	}
	for _, e := range v.inByID[n.ID] {
		if e.Kind == graph.EdgeOverrides {
			return true
		}
	}
	if parentType == "" {
		return false
	}
	for _, e := range v.outByID[parentType] {
		if e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends {
			return true
		}
	}
	for _, e := range v.inByID[parentType] {
		if e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends {
			return true
		}
	}
	return false
}

func (v *lspGraphView) stageAddedEdge(e *graph.Edge) bool {
	if e == nil {
		return false
	}
	key := lspEdgeIdentity(e)
	if _, exists := v.edgeKeys[key]; exists {
		return false
	}
	v.addEdges([]*graph.Edge{e})
	return true
}

func (v *lspGraphView) reindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || oldTo == e.To {
		return
	}
	oldKey := lspEdgeKey{from: e.From, to: oldTo, kind: e.Kind, filePath: e.FilePath, line: e.Line}
	delete(v.edgeKeys, oldKey)
	v.edgeKeys[lspEdgeIdentity(e)] = struct{}{}
	oldIn := v.inByID[oldTo]
	for i, candidate := range oldIn {
		if candidate == e {
			v.inByID[oldTo] = append(oldIn[:i], oldIn[i+1:]...)
			break
		}
	}
	v.inByID[e.To] = append(v.inByID[e.To], e)
}

// lspMutationBatch stages graph mutations so SQLite sees at most one
// ReindexEdges and one AddBatch call per enrichment unit, not one transaction
// per LSP result. The graph view is updated as edges are staged, preserving the
// original within-pass deduplication semantics.
type lspMutationBatch struct {
	adds        []*graph.Edge
	persists    []*graph.Edge
	reindexes   []graph.EdgeReindex
	addKeys     map[lspEdgeKey]struct{}
	persistKeys map[lspEdgeKey]struct{}
	reindexKeys map[lspEdgeKey]struct{}
}

func newLSPMutationBatch() *lspMutationBatch {
	return &lspMutationBatch{
		addKeys:     make(map[lspEdgeKey]struct{}),
		persistKeys: make(map[lspEdgeKey]struct{}),
		reindexKeys: make(map[lspEdgeKey]struct{}),
	}
}

func (b *lspMutationBatch) stagePersist(e *graph.Edge) {
	if e == nil {
		return
	}
	key := lspEdgeIdentity(e)
	if _, exists := b.persistKeys[key]; exists {
		return
	}
	b.persistKeys[key] = struct{}{}
	b.persists = append(b.persists, e)
}

func (b *lspMutationBatch) stageAdd(view *lspGraphView, e *graph.Edge) bool {
	if !view.stageAddedEdge(e) {
		return false
	}
	key := lspEdgeIdentity(e)
	if _, exists := b.addKeys[key]; exists {
		return false
	}
	b.addKeys[key] = struct{}{}
	b.adds = append(b.adds, e)
	return true
}

func (b *lspMutationBatch) stageReindex(view *lspGraphView, e *graph.Edge, oldTo string) {
	if e == nil {
		return
	}
	key := lspEdgeKey{from: e.From, to: oldTo, kind: e.Kind, filePath: e.FilePath, line: e.Line}
	if _, exists := b.reindexKeys[key]; exists {
		return
	}
	b.reindexKeys[key] = struct{}{}
	b.reindexes = append(b.reindexes, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	view.reindexEdge(e, oldTo)
}

func (b *lspMutationBatch) apply(g graph.Store, nodes []*graph.Node) {
	if len(b.reindexes) > 0 {
		g.ReindexEdges(b.reindexes)
	}
	if len(nodes) > 0 || len(b.adds) > 0 {
		g.AddBatch(nodes, b.adds)
	}
	if len(b.persists) == 0 {
		return
	}
	if batch, ok := g.(graph.EdgeMetaBatchPersister); ok {
		batch.PersistEdgeAttributesBatch(b.persists)
		return
	}
	// In-memory stores expose live edge pointers; their mutations are already
	// durable. AddBatch is the set-oriented fallback for any other backend and
	// avoids regressing to one PersistEdgeAttributes call per confirmation.
	g.AddBatch(nil, b.persists)
}

func lspEdgeIdentity(e *graph.Edge) lspEdgeKey {
	return lspEdgeKey{from: e.From, to: e.To, kind: e.Kind, filePath: e.FilePath, line: e.Line}
}

func lspAbs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func newLSPResolvedEdge(from, to string, kind graph.EdgeKind, filePath string, line int, provider, origin string) *graph.Edge {
	e := semantic.NewSemanticEdge(from, to, kind, filePath, line, provider)
	if origin != "" {
		e.Origin = origin
	}
	return e
}
