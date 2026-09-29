package resolver

import (
	"iter"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	frameworkScopeRetainedRowCap  = 4096
	frameworkScopeRetainedByteCap = 16 << 20
	frameworkScopeTokenCap        = 2048
)

// frameworkExecutionScope is intentionally richer than the public legacy
// map: watcher/incremental callers can provide exact changed files, while a
// warm-start batch that only knows changed repositories still gets a
// cursor-backed repository predicate rather than a workspace scan.
type frameworkExecutionScope struct {
	repos        map[string]bool
	repoPrefixes []string
	filePaths    []string
	repoSet      map[string]struct{}
	fileSet      map[string]struct{}
}

func newFrameworkExecutionScope(repos map[string]bool, filePaths []string) frameworkExecutionScope {
	prefixes := frameworkScopePrefixes(repos)
	files := append([]string(nil), filePaths...)
	sort.Strings(files)
	files = compactFrameworkStrings(files)
	return frameworkExecutionScope{
		repos:        repos,
		repoPrefixes: prefixes,
		filePaths:    files,
		repoSet:      frameworkStringSet(prefixes),
		fileSet:      frameworkStringSet(files),
	}
}

func compactFrameworkStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value == "" || (len(out) > 0 && out[len(out)-1] == value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func frameworkStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

type frameworkScopedStoreStats struct {
	RetainedRows  int
	RetainedBytes int
	RowCap        int
	ByteCap       int
}

// frameworkScopedStore is a read-through candidate view. Predicate scans are
// supplied by SQLite's keyset-paged scope projections; writes still land on
// the underlying graph. Only a small exact dependency cache is retained.
//
// The embedded Store preserves the broad backend contract, while the methods
// framework synthesizers actually use are overridden below. Whole-corpus
// snapshots fail closed so adding one to a synthesizer cannot silently turn a
// one-file edit into a workspace scan.
// frameworkScopedSeed is the immutable changed-file frontier shared by every
// synthesizer in one settle. Only the initial exact-file nodes, their incident
// edges/endpoints, and exact-name dependencies live here. Reads discovered by
// one synthesizer belong to its pass-local overlay and must not become input to
// a later, unrelated synthesizer.
type frameworkScopedSeed struct {
	store graph.Store
	scope frameworkExecutionScope

	nodes          map[string]*graph.Node
	incidentByKind map[graph.EdgeKind][]*graph.Edge
	incidentSeen   map[graph.EdgeIdentity]struct{}

	// outputs is deliberately separate from the immutable read seed. It records
	// only durable mutations produced by earlier synthesizers, which later
	// passes must observe; pass-local read expansion never enters this overlay.
	outputs *frameworkScopedOutputs

	retainedRows  int
	retainedBytes int
}

type frameworkScopedOutputs struct {
	nodes   map[string]*graph.Node
	edges   map[graph.EdgeIdentity]*graph.Edge
	order   []graph.EdgeIdentity
	removed map[graph.EdgeIdentity]struct{}
}

type frameworkScopedStore struct {
	graph.Store
	scope frameworkExecutionScope
	seed  *frameworkScopedSeed

	// These maps are a pass-local read-through overlay. A fresh view is created
	// for every synthesizer so dynamic name and adjacency expansion remains
	// available within the pass without widening later passes.
	nodes          map[string]*graph.Node
	incidentByKind map[graph.EdgeKind][]*graph.Edge
	incidentSeen   map[graph.EdgeIdentity]struct{}
	inEdges        map[string][]*graph.Edge
	outEdges       map[string][]*graph.Edge
	inReady        map[string]struct{}
	outReady       map[string]struct{}

	retainedRows  int
	retainedBytes int
	lastNode      *graph.Node
}

func newFrameworkScopedSeed(
	store graph.Store,
	repos map[string]bool,
	filePaths []string,
) *frameworkScopedSeed {
	seedView := newFrameworkScopedPassStore(
		store,
		newFrameworkExecutionScope(repos, filePaths),
		nil,
	)
	seedView.seedChangedFileFrontier()
	return &frameworkScopedSeed{
		store:          store,
		scope:          seedView.scope,
		nodes:          seedView.nodes,
		incidentByKind: seedView.incidentByKind,
		incidentSeen:   seedView.incidentSeen,
		outputs: &frameworkScopedOutputs{
			nodes:   make(map[string]*graph.Node),
			edges:   make(map[graph.EdgeIdentity]*graph.Edge),
			removed: make(map[graph.EdgeIdentity]struct{}),
		},
		retainedRows:  seedView.retainedRows,
		retainedBytes: seedView.retainedBytes,
	}
}

func (s *frameworkScopedSeed) newPassStore() *frameworkScopedStore {
	if s == nil {
		return nil
	}
	return newFrameworkScopedPassStore(s.store, s.scope, s)
}

func newFrameworkScopedPassStore(
	store graph.Store,
	scope frameworkExecutionScope,
	seed *frameworkScopedSeed,
) *frameworkScopedStore {
	return &frameworkScopedStore{
		Store:          store,
		scope:          scope,
		seed:           seed,
		nodes:          make(map[string]*graph.Node),
		incidentByKind: make(map[graph.EdgeKind][]*graph.Edge),
		incidentSeen:   make(map[graph.EdgeIdentity]struct{}),
		inEdges:        make(map[string][]*graph.Edge),
		outEdges:       make(map[string][]*graph.Edge),
		inReady:        make(map[string]struct{}),
		outReady:       make(map[string]struct{}),
	}
}

func newFrameworkScopedStore(
	store graph.Store,
	repos map[string]bool,
	filePaths []string,
) *frameworkScopedStore {
	return newFrameworkScopedSeed(store, repos, filePaths).newPassStore()
}

func (v *frameworkScopedStore) stats() frameworkScopedStoreStats {
	rows, bytes := v.retainedRows, v.retainedBytes
	if v.seed != nil {
		rows += v.seed.retainedRows
		bytes += v.seed.retainedBytes
	}
	return frameworkScopedStoreStats{
		RetainedRows:  rows,
		RetainedBytes: bytes,
		RowCap:        frameworkScopeRetainedRowCap,
		ByteCap:       frameworkScopeRetainedByteCap,
	}
}

func (v *frameworkScopedStore) AddNode(node *graph.Node) {
	v.Store.AddNode(node)
	v.recordOutputNode(node)
}

func (v *frameworkScopedStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	v.Store.AddBatch(nodes, edges)
	for _, node := range nodes {
		v.recordOutputNode(node)
	}
	for _, edge := range edges {
		v.recordOutputEdge(edge)
	}
}

func (v *frameworkScopedStore) AddEdge(edge *graph.Edge) {
	v.Store.AddEdge(edge)
	v.recordOutputEdge(edge)
}

func (v *frameworkScopedStore) ReindexEdge(edge *graph.Edge, oldTo string) {
	v.Store.ReindexEdge(edge, oldTo)
	old := graph.EdgeIdentityFor(edge)
	old.To = oldTo
	v.removeOutputIdentity(old)
	v.recordOutputEdge(edge)
}

func (v *frameworkScopedStore) ReindexEdges(batch []graph.EdgeReindex) {
	v.Store.ReindexEdges(batch)
	for _, mutation := range batch {
		if mutation.Edge == nil {
			continue
		}
		old := graph.EdgeIdentityFor(mutation.Edge)
		if mutation.OldFrom != "" {
			old.From = mutation.OldFrom
		}
		if mutation.OldTo != "" {
			old.To = mutation.OldTo
		}
		if mutation.OldKind != "" {
			old.Kind = mutation.OldKind
		}
		if mutation.RefreshIdentity {
			old.FilePath = mutation.OldFilePath
			old.Line = mutation.OldLine
		}
		v.removeOutputIdentity(old)
		v.recordOutputEdge(mutation.Edge)
	}
}

func (v *frameworkScopedStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	removed := v.Store.RemoveEdge(from, to, kind)
	if removed && v.seed != nil && v.seed.outputs != nil {
		v.seed.outputs.removeMatching(from, to, kind, v.seed.incidentByKind[kind])
	}
	return removed
}

func (v *frameworkScopedStore) recordOutputNode(node *graph.Node) {
	if v.seed == nil || v.seed.outputs == nil || node == nil || node.ID == "" {
		return
	}
	copyNode := *node
	copyNode.Meta = cloneFrameworkMeta(node.Meta)
	v.seed.outputs.nodes[node.ID] = &copyNode
}

func (v *frameworkScopedStore) recordOutputEdge(edge *graph.Edge) {
	if v.seed == nil || v.seed.outputs == nil || edge == nil {
		return
	}
	key := graph.EdgeIdentityFor(edge)
	if _, exists := v.seed.outputs.edges[key]; !exists {
		v.seed.outputs.order = append(v.seed.outputs.order, key)
	}
	delete(v.seed.outputs.removed, key)
	v.seed.outputs.edges[key] = cloneFrameworkEdge(edge)
}

func (v *frameworkScopedStore) removeOutputIdentity(identity graph.EdgeIdentity) {
	if v.seed == nil || v.seed.outputs == nil {
		return
	}
	v.seed.outputs.removed[identity] = struct{}{}
	delete(v.seed.outputs.edges, identity)
}

func (o *frameworkScopedOutputs) removeMatching(
	from, to string,
	kind graph.EdgeKind,
	seedEdges []*graph.Edge,
) {
	if o == nil {
		return
	}
	for _, edge := range seedEdges {
		if edge != nil && edge.From == from && edge.To == to && edge.Kind == kind {
			o.removed[graph.EdgeIdentityFor(edge)] = struct{}{}
		}
	}
	for identity := range o.edges {
		if identity.From == from && identity.To == to && identity.Kind == kind {
			o.removed[identity] = struct{}{}
			delete(o.edges, identity)
		}
	}
}

func (v *frameworkScopedStore) AllNodes() []*graph.Node {
	panic("framework partial scope attempted AllNodes")
}

func (v *frameworkScopedStore) AllEdges() []*graph.Edge {
	panic("framework partial scope attempted AllEdges")
}

func (v *frameworkScopedStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	var base iter.Seq[*graph.Node]
	if len(v.scope.filePaths) > 0 {
		// Same rows and order as the scoped projection, read by file (see
		// frameworkFileFrontierNodes): the projection walks the generation.
		base = frameworkFileFrontierNodes(v.Store, v.scope.repoPrefixes, v.scope.filePaths, kind)
	} else {
		base = graph.NodesInScopeSeq(v.Store, v.scope.repoPrefixes, v.scope.filePaths, kind)
	}
	return func(yield func(*graph.Node) bool) {
		for node := range base {
			v.lastNode = node
			v.rememberNode(node)
			if !yield(node) {
				return
			}
		}
		ids := make(map[string]struct{}, len(v.nodes)+v.seedNodeCount())
		v.collectOffBaseNodeIDs(ids, kind, v.seedNodes())
		v.collectOffBaseNodeIDs(ids, kind, v.outputNodes())
		v.collectOffBaseNodeIDs(ids, kind, v.nodes)
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		for _, id := range ordered {
			node := v.rememberedNode(id)
			v.lastNode = node
			if !yield(node) {
				return
			}
		}
	}
}

func (v *frameworkScopedStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	base := graph.EdgesInScopeSeq(v.Store, v.scope.repoPrefixes, v.scope.filePaths, kind)
	return func(yield func(*graph.Edge) bool) {
		yielded := make(map[graph.EdgeIdentity]struct{})
		for row := range base {
			v.lastNode = row.Source
			v.rememberNode(row.Source)
			v.rememberNode(row.Target)
			if row.Edge == nil {
				continue
			}
			identity := graph.EdgeIdentityFor(row.Edge)
			yielded[identity] = struct{}{}
			if !yield(row.Edge) {
				return
			}
		}
		for _, edges := range [][]*graph.Edge{
			v.seedIncidentEdges(kind),
			v.outputEdges(kind),
			v.incidentByKind[kind],
		} {
			for _, edge := range edges {
				if edge == nil {
					continue
				}
				identity := graph.EdgeIdentityFor(edge)
				if v.outputRemoved(identity) {
					continue
				}
				if _, duplicate := yielded[identity]; duplicate {
					continue
				}
				source := v.rememberedNode(edge.From)
				if source != nil && v.inBaseScope(source) {
					continue // already yielded by the source-owned projection
				}
				yielded[identity] = struct{}{}
				v.lastNode = source
				if !yield(edge) {
					return
				}
			}
		}
	}
}

func (v *frameworkScopedStore) seedNodes() map[string]*graph.Node {
	if v.seed == nil {
		return nil
	}
	return v.seed.nodes
}

func (v *frameworkScopedStore) seedNodeCount() int {
	if v.seed == nil {
		return 0
	}
	return len(v.seed.nodes)
}

func (v *frameworkScopedStore) seedIncidentEdges(kind graph.EdgeKind) []*graph.Edge {
	if v.seed == nil {
		return nil
	}
	return v.seed.incidentByKind[kind]
}

func (v *frameworkScopedStore) outputNodes() map[string]*graph.Node {
	if v.seed == nil || v.seed.outputs == nil {
		return nil
	}
	return v.seed.outputs.nodes
}

func (v *frameworkScopedStore) outputEdges(kind graph.EdgeKind) []*graph.Edge {
	if v.seed == nil || v.seed.outputs == nil {
		return nil
	}
	out := make([]*graph.Edge, 0)
	for _, identity := range v.seed.outputs.order {
		edge := v.seed.outputs.edges[identity]
		if edge != nil && edge.Kind == kind {
			out = append(out, edge)
		}
	}
	return out
}

func (v *frameworkScopedStore) outputRemoved(identity graph.EdgeIdentity) bool {
	if v.seed == nil || v.seed.outputs == nil {
		return false
	}
	_, removed := v.seed.outputs.removed[identity]
	return removed
}

func (v *frameworkScopedStore) collectOffBaseNodeIDs(
	ids map[string]struct{},
	kind graph.NodeKind,
	nodes map[string]*graph.Node,
) {
	for id, node := range nodes {
		if node != nil && node.Kind == kind && !v.inBaseScope(node) {
			ids[id] = struct{}{}
		}
	}
}

func (v *frameworkScopedStore) rememberedNode(id string) *graph.Node {
	if v.lastNode != nil && v.lastNode.ID == id {
		return v.lastNode
	}
	if node := v.nodes[id]; node != nil {
		return node
	}
	if v.seed != nil {
		if v.seed.outputs != nil {
			if node := v.seed.outputs.nodes[id]; node != nil {
				return node
			}
		}
		return v.seed.nodes[id]
	}
	return nil
}

func (v *frameworkScopedStore) GetNode(id string) *graph.Node {
	if node := v.rememberedNode(id); node != nil {
		return node
	}
	node := v.Store.GetNode(id)
	v.lastNode = node
	v.rememberNode(node)
	return node
}

func (v *frameworkScopedStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out := make(map[string]*graph.Node, len(ids))
	missing := make([]string, 0, len(ids))
	seenMissing := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if node := v.rememberedNode(id); node != nil {
			out[id] = node
			continue
		}
		if _, seen := seenMissing[id]; !seen && id != "" {
			seenMissing[id] = struct{}{}
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		for id, node := range v.Store.GetNodesByIDs(missing) {
			out[id] = node
			v.rememberNode(node)
		}
	}
	return out
}

func (v *frameworkScopedStore) FindNodesByName(name string) []*graph.Node {
	nodes := v.Store.FindNodesByName(name)
	for _, node := range nodes {
		v.rememberNode(node)
	}
	return nodes
}

func (v *frameworkScopedStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	rows := v.Store.FindNodesByNames(names)
	for _, nodes := range rows {
		for _, node := range nodes {
			v.rememberNode(node)
		}
	}
	return rows
}

func (v *frameworkScopedStore) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	nodes := v.Store.FindNodesByNameInRepo(name, repoPrefix)
	for _, node := range nodes {
		v.rememberNode(node)
	}
	return nodes
}

func (v *frameworkScopedStore) GetInEdges(id string) []*graph.Edge {
	if _, ready := v.inReady[id]; ready {
		return v.inEdges[id]
	}
	return v.Store.GetInEdges(id)
}

func (v *frameworkScopedStore) GetOutEdges(id string) []*graph.Edge {
	if _, ready := v.outReady[id]; ready {
		return v.outEdges[id]
	}
	return v.Store.GetOutEdges(id)
}

func (v *frameworkScopedStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows := v.Store.GetInEdgesByNodeIDs(ids)
	v.rememberAdjacency(ids, rows, true)
	return rows
}

func (v *frameworkScopedStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows := v.Store.GetOutEdgesByNodeIDs(ids)
	v.rememberAdjacency(ids, rows, false)
	return rows
}

// FindEdgesByIdentities preserves exact candidate refetch through the scoped
// view without retaining broad adjacency or widening the execution scope.
func (v *frameworkScopedStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	return findFrameworkEdgesByIdentities(v.Store, identities)
}

func (v *frameworkScopedStore) inBaseScope(node *graph.Node) bool {
	if node == nil {
		return false
	}
	if len(v.scope.repoSet) > 0 {
		if _, ok := v.scope.repoSet[node.RepoPrefix]; !ok {
			return false
		}
	}
	if len(v.scope.fileSet) > 0 {
		_, ok := v.scope.fileSet[node.FilePath]
		return ok
	}
	_, ok := v.scope.repoSet[node.RepoPrefix]
	return ok
}

func (v *frameworkScopedStore) rememberNode(node *graph.Node) bool {
	if node == nil || node.ID == "" {
		return false
	}
	if _, exists := v.nodes[node.ID]; exists {
		return true
	}
	if v.seed != nil {
		if v.seed.outputs != nil {
			if _, exists := v.seed.outputs.nodes[node.ID]; exists {
				return true
			}
		}
		if _, exists := v.seed.nodes[node.ID]; exists {
			return true
		}
	}
	size := frameworkNodeBytes(node)
	if !v.canRetain(size) {
		return false
	}
	v.nodes[node.ID] = node
	v.retainedRows++
	v.retainedBytes += size
	return true
}

func (v *frameworkScopedStore) rememberEdge(edge *graph.Edge) bool {
	if edge == nil {
		return false
	}
	key := graph.EdgeIdentityFor(edge)
	if _, exists := v.incidentSeen[key]; exists {
		return true
	}
	if v.seed != nil {
		if v.seed.outputs != nil {
			if _, exists := v.seed.outputs.edges[key]; exists {
				return true
			}
		}
		if _, exists := v.seed.incidentSeen[key]; exists {
			return true
		}
	}
	size := frameworkEdgeBytes(edge)
	if !v.canRetain(size) {
		return false
	}
	v.incidentSeen[key] = struct{}{}
	v.incidentByKind[edge.Kind] = append(v.incidentByKind[edge.Kind], edge)
	v.retainedRows++
	v.retainedBytes += size
	return true
}

func (v *frameworkScopedStore) canRetain(size int) bool {
	rows, bytes := v.retainedRows, v.retainedBytes
	if v.seed != nil {
		rows += v.seed.retainedRows
		bytes += v.seed.retainedBytes
	}
	return rows < frameworkScopeRetainedRowCap &&
		bytes+size <= frameworkScopeRetainedByteCap
}

func frameworkNodeBytes(node *graph.Node) int {
	if node == nil {
		return 0
	}
	size := 160 + len(node.ID) + len(node.Name) + len(node.QualName) +
		len(node.FilePath) + len(node.Language) + len(node.RepoPrefix) +
		len(node.WorkspaceID) + len(node.ProjectID)
	for key, value := range node.Meta {
		size += len(key) + frameworkValueBytes(value)
	}
	return size
}

func frameworkEdgeBytes(edge *graph.Edge) int {
	if edge == nil {
		return 0
	}
	size := 128 + len(edge.From) + len(edge.To) + len(edge.FilePath) +
		len(edge.Origin) + len(edge.ConfidenceLabel)
	for key, value := range edge.Meta {
		size += len(key) + frameworkValueBytes(value)
	}
	return size
}

func frameworkValueBytes(value any) int {
	switch typed := value.(type) {
	case string:
		return len(typed)
	case []string:
		size := 0
		for _, item := range typed {
			size += len(item)
		}
		return size
	case []any:
		size := 0
		for _, item := range typed {
			size += frameworkValueBytes(item)
		}
		return size
	default:
		return 16
	}
}

// seedChangedFileFrontier admits only exact changed-file nodes, their incident
// edges/endpoints, and exact name dependencies extracted from those rows. The
// maps are hard-capped; repository-only warm scopes skip this preload and use
// cursor projections directly.
func (v *frameworkScopedStore) seedChangedFileFrontier() {
	if v.Store == nil || len(v.scope.filePaths) == 0 {
		return
	}
	byFile := v.GetFileNodesByPaths(v.scope.filePaths)
	ids := make([]string, 0)
	tokens := make(map[string]struct{})
	for _, filePath := range v.scope.filePaths {
		for _, node := range byFile[filePath] {
			if node == nil || !v.inBaseScope(node) {
				continue
			}
			ids = append(ids, node.ID)
			v.rememberNode(node)
			addFrameworkNodeTokens(tokens, node)
		}
	}
	if len(ids) == 0 {
		return
	}
	incoming := v.Store.GetInEdgesByNodeIDs(ids)
	outgoing := v.Store.GetOutEdgesByNodeIDs(ids)
	v.rememberAdjacency(ids, incoming, true)
	v.rememberAdjacency(ids, outgoing, false)
	endpointIDs := make([]string, 0)
	seenEndpoint := make(map[string]struct{})
	for _, rows := range []map[string][]*graph.Edge{incoming, outgoing} {
		for _, edges := range rows {
			for _, edge := range edges {
				if edge == nil {
					continue
				}
				v.rememberEdge(edge)
				addFrameworkEdgeTokens(tokens, edge)
				for _, id := range []string{edge.From, edge.To} {
					if id == "" || graph.IsUnresolvedTarget(id) {
						continue
					}
					if _, seen := seenEndpoint[id]; !seen {
						seenEndpoint[id] = struct{}{}
						endpointIDs = append(endpointIDs, id)
					}
				}
			}
		}
	}
	if len(endpointIDs) > 0 {
		for _, node := range v.Store.GetNodesByIDs(endpointIDs) {
			v.rememberNode(node)
			addFrameworkNodeTokens(tokens, node)
		}
	}
	names := make([]string, 0, len(tokens))
	for token := range tokens {
		names = append(names, token)
	}
	sort.Strings(names)
	if len(names) > frameworkScopeTokenCap {
		names = names[:frameworkScopeTokenCap]
	}
	if len(names) > 0 {
		for _, matches := range v.Store.FindNodesByNames(names) {
			for _, node := range matches {
				v.rememberNode(node)
			}
		}
	}
}

func (v *frameworkScopedStore) rememberAdjacency(ids []string, rows map[string][]*graph.Edge, incoming bool) {
	for _, id := range ids {
		edges := rows[id]
		kept := make([]*graph.Edge, 0, len(edges))
		complete := true
		for _, edge := range edges {
			if edge == nil {
				continue
			}
			if !v.rememberEdge(edge) {
				complete = false
				break
			}
			kept = append(kept, edge)
		}
		if !complete {
			// A partial adjacency answer would be semantically unsafe. Keep the
			// admitted incident rows for candidate scans, but make point reads
			// fall through to the backend instead of returning a truncated set.
			continue
		}
		if incoming {
			v.inReady[id] = struct{}{}
			v.inEdges[id] = kept
		} else {
			v.outReady[id] = struct{}{}
			v.outEdges[id] = kept
		}
	}
}

func addFrameworkNodeTokens(tokens map[string]struct{}, node *graph.Node) {
	if node == nil || len(tokens) >= frameworkScopeTokenCap {
		return
	}
	// A changed node's own bare name is not a dependency key. Admitting every
	// same-named node would turn source candidates in unrelated repositories
	// into candidates too (for example every Gin `setup`/`Next` method). Edge
	// targets and explicit framework Meta values carry the actual join keys.
	for _, value := range node.Meta {
		addFrameworkToken(tokens, value)
		if len(tokens) >= frameworkScopeTokenCap {
			return
		}
	}
}

func addFrameworkEdgeTokens(tokens map[string]struct{}, edge *graph.Edge) {
	if edge == nil || len(tokens) >= frameworkScopeTokenCap {
		return
	}
	if graph.IsUnresolvedTarget(edge.To) {
		addFrameworkToken(tokens, graph.UnresolvedName(edge.To))
	}
	for _, value := range edge.Meta {
		addFrameworkToken(tokens, value)
		if len(tokens) >= frameworkScopeTokenCap {
			return
		}
	}
}

func addFrameworkToken(tokens map[string]struct{}, value any) {
	if len(tokens) >= frameworkScopeTokenCap {
		return
	}
	switch typed := value.(type) {
	case string:
		token := strings.TrimSpace(typed)
		if token == "" || len(token) > 256 {
			return
		}
		tokens[token] = struct{}{}
		if i := strings.LastIndexAny(token, ".:/#"); i >= 0 && i+1 < len(token) {
			tokens[token[i+1:]] = struct{}{}
		}
	case []string:
		for _, item := range typed {
			addFrameworkToken(tokens, item)
		}
	case []any:
		for _, item := range typed {
			addFrameworkToken(tokens, item)
		}
	}
}

var (
	_ graph.Store                   = (*frameworkScopedStore)(nil)
	_ graph.EdgeIdentityBatchFinder = (*frameworkScopedStore)(nil)
)
