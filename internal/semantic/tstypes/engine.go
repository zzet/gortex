package tstypes

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/semantic"
)

// maxFileBytes guards the enrichment pass against pathological
// generated sources; files above the cap are skipped, same spirit as
// the indexer's own size gates.
const maxFileBytes = 4 << 20

// astConfidence is the confidence stamped on edges this engine
// confirms or adds. Deliberately below the 1.0 the compiler-grade
// ConfirmEdge uses: tree-sitter scope analysis is structurally
// grounded but not type-checked.
const astConfidence = 0.95

// inferredConfidence is the graded confidence stamped on edges this
// engine derives by a type heuristic rather than a direct structural
// match (e.g. a receiver type narrowed by inference). Honestly weaker
// than astConfidence, yet well above the name-only text-match floor.
// The edge still carries OriginASTResolved provenance — only the
// confidence and the resolution_strategy label distinguish it from the
// direct path.
const inferredConfidence = 0.7

// resolutionStrategy labels how the engine derived an edge it emits. It
// rides on Meta["resolution_strategy"] for graded (non-direct)
// emissions so consumers can see the inference path; the direct path
// carries no label. Extensible: later inference forms add their own
// constants.
type resolutionStrategy string

const (
	// strategyDirect is the default: a structurally grounded
	// tree-sitter resolution. Emitted at astConfidence with no
	// resolution_strategy label (its zero value is the empty string, so
	// the direct path stamps nothing extra).
	strategyDirect resolutionStrategy = ""
	// strategyInferred marks an edge derived by a type heuristic rather
	// than a direct scope match — emitted at inferredConfidence and
	// labelled so it stays honestly distinguishable from a direct
	// resolution.
	strategyInferred resolutionStrategy = "inferred"
)

// extendsWalkDepth bounds the inherited-method lookup walk up the
// resolved EdgeExtends chain.
const extendsWalkDepth = 3

// fileRef is one graph file node selected for analysis plus its
// on-disk location.
type fileRef struct {
	node    *graph.Node
	absPath string
}

// languageFiles selects the graph file nodes for the spec's languages
// that belong to the repo identified by repoPrefix and exist on disk
// under repoRoot.
//
// Disk existence alone is NOT a safe repo-membership test in multi-repo
// mode: the shared graph holds file nodes from every tracked repo, and
// two repos can share a relative path (both have `src/Svc.java`). Joining
// a foreign repo's node onto repoRoot would then stat-hit and read THIS
// repo's bytes for that repo's node, contaminating its graph. Selection is
// therefore gated on the node's own RepoPrefix matching the prefix of the
// repo being enriched. In single-repo mode every real node carries the
// empty prefix, so repoPrefix == "" selects them all.
func languageFiles(g graph.Store, spec *LangSpec, repoPrefix, repoRoot string) []fileRef {
	langs := make(map[string]bool, len(spec.Languages))
	for _, l := range spec.Languages {
		langs[l] = true
	}
	var out []fileRef
	for n := range g.NodesByKind(graph.KindFile) {
		if !langs[n.Language] || n.RepoPrefix != repoPrefix {
			continue
		}
		// Match the manager/LSP enrichment policy: vendored and generated
		// sources are intentionally outside semantic coverage. Without this
		// gate, one real source file can make the provider reparse an entire
		// dependency or generated tree that did not count as language-presence
		// evidence in the first place.
		if semantic.IsLowValueForEnrichment(n.FilePath, nil) {
			continue
		}
		ref, ok := fileRefFor(n, repoRoot)
		if !ok {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// fileRefFor maps a graph file node to its on-disk location under repoRoot
// (stripping the node's own RepoPrefix from the path) and reports whether
// it is an existing, in-cap regular file. The single point that turns a
// graph file key into bytes-on-disk for both the full and incremental
// passes.
func fileRefFor(n *graph.Node, repoRoot string) (fileRef, bool) {
	rel := n.FilePath
	if n.RepoPrefix != "" {
		rel = strings.TrimPrefix(rel, n.RepoPrefix+"/")
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(rel))
	if fi, err := os.Stat(abs); err != nil || fi.IsDir() || fi.Size() > maxFileBytes {
		return fileRef{}, false
	}
	return fileRef{node: n, absPath: abs}, true
}

// analyzeFile parses one file and runs the binder walk. Pure with
// respect to the graph — safe to fan out across workers.
func analyzeFile(spec *LangSpec, ref fileRef) (*fileFacts, error) {
	src, err := os.ReadFile(ref.absPath)
	if err != nil {
		return nil, err
	}
	grammar := spec.GrammarFor(ref.node.FilePath)
	if grammar == nil {
		return nil, nil
	}
	tree, err := parser.ParseFile(src, grammar)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	facts := &fileFacts{file: ref.node.FilePath, repoPrefix: ref.node.RepoPrefix}
	newBinder(spec, src, facts).run(tree.RootNode())
	return facts, nil
}

// resolvedAlias is a trait-use alias resolved against the graph: on the
// using type, `alias` routes to method `method`. When trait is non-nil
// the method is looked up on that specific trait; otherwise it is looked
// up on the using type's own inheritance closure.
type resolvedAlias struct {
	alias  string
	trait  *graph.Node
	method string
}

// applier owns every graph interaction of an enrichment pass. It runs
// single-goroutine so in-place edge mutations never race.
type applier struct {
	g            graph.Store
	spec         *LangSpec
	provider     string
	stampedNodes map[string]*graph.Node // collected for one AddBatch round-trip
	// hot, when non-nil, is the pass-scoped read-through cache shared by
	// every page applier of one streamed apply pass. It dedupes node, name
	// and adjacency store reads across pages; see applyHotCache for the
	// per-class safety model. Nil outside the streamed path.
	hot *applyHotCache
	// aliases maps a using type's node ID to its trait-use alias
	// adaptations, built in the alias phase and consulted when a call's
	// method name is not a direct or inherited member.
	aliases map[string][]resolvedAlias
	// languages is the immutable set served by spec. Keeping it once per
	// pass avoids rebuilding the same map for every receiver-type fact.
	languages map[string]bool
	// typeCandidatesCache memoizes the store's name lookup. The apply phase
	// never creates nodes, so the candidate set is immutable for its lifetime;
	// only edges and metadata change between its ordered phases.
	typeCandidatesCache map[typeCandidateKey][]*graph.Node
	// methodCache memoizes the inheritance/member walk after the supertype
	// phase has completed. The depth belongs in the key because the walk is
	// intentionally bounded and a lookup reached near the bound can differ
	// from the same lookup started at the root.
	methodCache map[methodLookupKey]*graph.Node
	// extensions indexes the language's extension functions by
	// (receiver-type-name, method-name). An extension `fun Foo.ext()` is
	// callable as `recv.ext()` on any Foo receiver but is declared at file
	// scope, so it is not a structural member of Foo (and cross-file its
	// synthetic member_of edge points at a same-file phantom of the
	// receiver type). The call phase consults this index as a FALLBACK,
	// only after a real member lookup misses, so a real member of the same
	// name always wins. nil until the first lookup lazily builds it; built
	// only for specs that set ExtensionFunctions.
	extensions   map[extKey][]*graph.Node
	extensionsOK bool
	// The production apply path preloads one repo/language node projection and
	// both adjacency directions in batches. These maps keep every ordered phase
	// query-free; the loaded sets preserve a one-shot fallback for direct unit
	// exercises that invoke an individual helper without applyAll.
	nodesByFile          map[string][]*graph.Node
	nodesByID            map[string]*graph.Node
	nodesByName          map[typeCandidateKey][]*graph.Node
	outByID              map[string][]*graph.Edge
	inByID               map[string][]*graph.Edge
	fileLoaded           map[string]bool
	nodeLoaded           map[string]bool
	nameLoaded           map[typeCandidateKey]bool
	repoProjectionLoaded map[string]bool
	outLoaded            map[string]bool
	inLoaded             map[string]bool
	allNodes             []*graph.Node
}

// extKey indexes an extension function by its receiver type name and its
// own method name — the pair a `recv.method()` call resolves against.
type extKey struct {
	receiver string
	method   string
}

type typeCandidateKey struct {
	repoPrefix string
	name       string
}

type methodLookupKey struct {
	typeID   string
	method   string
	argCount int
	depth    int
}

func newApplier(g graph.Store, spec *LangSpec, provider string) *applier {
	languages := make(map[string]bool)
	if spec != nil {
		for _, language := range spec.Languages {
			languages[language] = true
		}
	}
	return &applier{
		g:                    g,
		spec:                 spec,
		provider:             provider,
		stampedNodes:         make(map[string]*graph.Node),
		aliases:              make(map[string][]resolvedAlias),
		languages:            languages,
		typeCandidatesCache:  make(map[typeCandidateKey][]*graph.Node),
		methodCache:          make(map[methodLookupKey]*graph.Node),
		nodesByFile:          make(map[string][]*graph.Node),
		nodesByID:            make(map[string]*graph.Node),
		nodesByName:          make(map[typeCandidateKey][]*graph.Node),
		outByID:              make(map[string][]*graph.Edge),
		inByID:               make(map[string][]*graph.Edge),
		fileLoaded:           make(map[string]bool),
		nodeLoaded:           make(map[string]bool),
		nameLoaded:           make(map[typeCandidateKey]bool),
		repoProjectionLoaded: make(map[string]bool),
		outLoaded:            make(map[string]bool),
		inLoaded:             make(map[string]bool),
	}
}

var tstypesFileNodeKinds = []graph.NodeKind{
	graph.KindPackage, graph.KindFunction, graph.KindMethod, graph.KindType,
	graph.KindInterface, graph.KindVariable, graph.KindField, graph.KindParam,
	graph.KindClosure, graph.KindLocal, graph.KindConstant, graph.KindEnumMember,
	graph.KindGenericParam, graph.KindMacro,
}

func (a *applier) rememberNode(node *graph.Node) {
	if node == nil || a.nodeLoaded[node.ID] {
		return
	}
	a.nodeLoaded[node.ID] = true
	a.nodesByID[node.ID] = node
	a.allNodes = append(a.allNodes, node)
	if node.FilePath != "" {
		a.nodesByFile[node.FilePath] = append(a.nodesByFile[node.FilePath], node)
	}
	if node.Name != "" {
		key := typeCandidateKey{repoPrefix: node.RepoPrefix, name: node.Name}
		a.nodesByName[key] = append(a.nodesByName[key], node)
	}
}

func (a *applier) preload(all []*fileFacts) {
	a.preloadBounded(all)
}

// preloadApplicationFrontier loads exactly the adjacency the ordered apply
// phases can traverse. The seed projection contains every symbol in the
// analyzed repo/languages. From there the engine can only walk inheritance,
// member_of, and param_of links; call targets and unrelated graph neighbors
// are deliberately not expanded. The inheritance walk uses the same depth
// bound as methodOn, keeping both SQL work and retained adjacency bounded.
func (a *applier) preloadApplicationFrontier(seedIDs []string) {
	seedIDs = uniqueSortedIDs(seedIDs)
	a.loadAdjacency(seedIDs)

	inheritKinds := a.spec.inheritEdgeKinds()
	a.nodes(a.relevantEndpointIDs(seedIDs, inheritKinds))

	parentKinds := a.supertypeKinds()
	frontier := make([]string, 0, len(seedIDs))
	for _, id := range seedIDs {
		if node := a.nodesByID[id]; node != nil && parentKinds[node.Kind] {
			frontier = append(frontier, id)
		}
	}
	seenTypes := make(map[string]struct{}, len(frontier))
	for depth := 0; depth <= extendsWalkDepth && len(frontier) > 0; depth++ {
		for _, id := range frontier {
			seenTypes[id] = struct{}{}
		}

		members := make(map[string]struct{})
		parents := make(map[string]struct{})
		for _, id := range frontier {
			for _, edge := range a.inByID[id] {
				if edge != nil && edge.Kind == graph.EdgeMemberOf && edge.From != "" {
					members[edge.From] = struct{}{}
				}
			}
			if depth == extendsWalkDepth {
				continue
			}
			for _, edge := range a.outByID[id] {
				if edge == nil || edge.To == "" || graph.IsUnresolvedTarget(edge.To) ||
					!edgeKindIn(edge.Kind, inheritKinds) {
					continue
				}
				if _, seen := seenTypes[edge.To]; !seen {
					parents[edge.To] = struct{}{}
				}
			}
		}

		discoveredIDs := make([]string, 0, len(members)+len(parents))
		for id := range members {
			discoveredIDs = append(discoveredIDs, id)
		}
		for id := range parents {
			discoveredIDs = append(discoveredIDs, id)
		}
		discovered := a.nodes(discoveredIDs)

		adjacencyIDs := make([]string, 0, len(discoveredIDs))
		for id := range members {
			if node := discovered[id]; node != nil && node.Kind == graph.KindMethod {
				adjacencyIDs = append(adjacencyIDs, id)
			}
		}
		next := make([]string, 0, len(parents))
		for id := range parents {
			if node := discovered[id]; node != nil && parentKinds[node.Kind] {
				adjacencyIDs = append(adjacencyIDs, id)
				next = append(next, id)
			}
		}
		adjacencyIDs = uniqueSortedIDs(adjacencyIDs)
		a.loadAdjacency(adjacencyIDs)
		a.nodes(a.relevantEndpointIDs(adjacencyIDs, inheritKinds))
		frontier = uniqueSortedIDs(next)
	}
}

// loadAdjacency merges bounded bulk reads into the mutation-aware caches.
// Loaded IDs, including misses, are never queried again. Edge slices are
// retained in store order so overload and edge arbitration semantics stay
// identical to the underlying backend.
func (a *applier) loadAdjacency(ids []string) {
	ids = uniqueSortedIDs(ids)
	outMissing := make([]string, 0, len(ids))
	inMissing := make([]string, 0, len(ids))
	for _, id := range ids {
		if !a.outLoaded[id] {
			// Empty adjacency is cached too — repeated misses on leaf nodes
			// are exactly the reads the per-page re-hydration kept paying.
			if edges, ok := a.hot.getOut(id); ok {
				a.outByID[id] = edges
				a.outLoaded[id] = true
			} else {
				outMissing = append(outMissing, id)
			}
		}
		if !a.inLoaded[id] {
			if edges, ok := a.hot.getIn(id); ok {
				a.inByID[id] = edges
				a.inLoaded[id] = true
			} else {
				inMissing = append(inMissing, id)
			}
		}
	}
	if len(outMissing) > 0 {
		loaded := a.g.GetOutEdgesByNodeIDs(outMissing)
		for _, id := range outMissing {
			a.outByID[id] = loaded[id]
			a.outLoaded[id] = true
			a.hot.putOut(id, loaded[id])
		}
	}
	if len(inMissing) > 0 {
		loaded := a.g.GetInEdgesByNodeIDs(inMissing)
		for _, id := range inMissing {
			a.inByID[id] = loaded[id]
			a.inLoaded[id] = true
			a.hot.putIn(id, loaded[id])
		}
	}
}

func (a *applier) relevantEndpointIDs(ids []string, inheritKinds []graph.EdgeKind) []string {
	relevant := make(map[string]struct{})
	for _, id := range ids {
		for _, edge := range a.outByID[id] {
			if edge == nil || edge.To == "" || graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			if edge.Kind == graph.EdgeMemberOf || edgeKindIn(edge.Kind, inheritKinds) {
				relevant[edge.To] = struct{}{}
			}
		}
		for _, edge := range a.inByID[id] {
			if edge == nil || edge.From == "" {
				continue
			}
			if edge.Kind == graph.EdgeMemberOf || edge.Kind == graph.EdgeParamOf {
				relevant[edge.From] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(relevant))
	for id := range relevant {
		result = append(result, id)
	}
	return uniqueSortedIDs(result)
}

func uniqueSortedIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Strings(unique)
	return unique
}

func (a *applier) fileNodes(filePath string) []*graph.Node {
	if !a.fileLoaded[filePath] {
		for _, node := range a.g.GetFileNodes(filePath) {
			a.rememberNode(node)
		}
		a.fileLoaded[filePath] = true
	}
	return a.nodesByFile[filePath]
}

// withHotCache attaches the pass-scoped read-through cache. Chainable so the
// streamed driver can attach it at construction without widening newApplier's
// signature for the single-applier paths that need no cache.
func (a *applier) withHotCache(hot *applyHotCache) *applier {
	a.hot = hot
	return a
}

func (a *applier) node(id string) *graph.Node {
	return a.nodesByID[id]
}

func (a *applier) nodes(ids []string) map[string]*graph.Node {
	missing := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || a.nodeLoaded[id] {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		missing = append(missing, id)
	}
	if len(missing) > 0 {
		// Serve pass-cached nodes first; only the residue pays a store read.
		// Negative results stay per-applier — a missing ID is cheap to
		// re-ask and never worth a cache slot.
		residue := missing[:0]
		for _, id := range missing {
			if node, ok := a.hot.getNode(id); ok {
				a.rememberNode(node)
				a.nodeLoaded[id] = true
				continue
			}
			residue = append(residue, id)
		}
		if len(residue) > 0 {
			for _, node := range a.g.GetNodesByIDs(residue) {
				a.rememberNode(node)
				a.hot.putNode(node)
			}
			for _, id := range residue {
				a.nodeLoaded[id] = true
			}
		}
	}
	out := make(map[string]*graph.Node, len(ids))
	for _, id := range ids {
		if node := a.nodesByID[id]; node != nil {
			out[id] = node
		}
	}
	return out
}

func (a *applier) outEdges(id string) []*graph.Edge {
	return a.outByID[id]
}

func (a *applier) inEdges(id string) []*graph.Edge {
	return a.inByID[id]
}

func (a *applier) reindexEdge(edge *graph.Edge, oldTo string) {
	a.g.ReindexEdge(edge, oldTo)
	if a.inLoaded[oldTo] {
		filtered := a.inByID[oldTo][:0]
		for _, candidate := range a.inByID[oldTo] {
			if candidate != edge {
				filtered = append(filtered, candidate)
			}
		}
		a.inByID[oldTo] = filtered
	}
	if a.inLoaded[edge.To] {
		a.inByID[edge.To] = append(a.inByID[edge.To], edge)
	}
}

func (a *applier) removeEdge(edge *graph.Edge) {
	if edge == nil || !a.g.RemoveEdge(edge.From, edge.To, edge.Kind) {
		return
	}
	if a.outLoaded[edge.From] {
		filtered := a.outByID[edge.From][:0]
		for _, candidate := range a.outByID[edge.From] {
			if candidate != edge {
				filtered = append(filtered, candidate)
			}
		}
		a.outByID[edge.From] = filtered
	}
	if a.inLoaded[edge.To] {
		filtered := a.inByID[edge.To][:0]
		for _, candidate := range a.inByID[edge.To] {
			if candidate != edge {
				filtered = append(filtered, candidate)
			}
		}
		a.inByID[edge.To] = filtered
	}
}

// receiverTypeKinds is the node-kind set a call receiver's type may
// resolve to — methods only hang off types and interfaces.
var receiverTypeKinds = map[graph.NodeKind]bool{
	graph.KindType:      true,
	graph.KindInterface: true,
}

// supertypeKinds returns the node-kind set declared supertypes may
// resolve to: the receiver default unless the spec widens it.
func (a *applier) supertypeKinds() map[graph.NodeKind]bool {
	if a.spec.SupertypeKinds != nil {
		return a.spec.SupertypeKinds
	}
	return receiverTypeKinds
}

// fileIndex is the per-file view of the graph the apply phase joins
// facts against.
type fileIndex struct {
	facts   *fileFacts
	imports map[string]string // local name → path hint
	types   map[string]*graph.Node
	// superTypes additionally holds same-file nodes of the spec's
	// widened supertype kinds (Ruby modules); aliases types when the
	// spec doesn't widen.
	superTypes map[string]*graph.Node
	funcs      []*graph.Node // function/method nodes, for line containment
	// stubsByLine indexes the file's calls-edges by call line, snapshotted
	// before this file's calls phase applies. The extractor attributes a
	// call to its owner precisely (byte extents where the language has
	// them), so the stub's From is the ground truth a line-keyed caller
	// lookup can only approximate — and disagree with, when the owner
	// is a node kind outside idx.funcs (a C# property owning its
	// accessor-body calls) or shares its line with another member.
	// The snapshot stays sound in the paged driver too, where indexes are
	// rebuilt per phase after earlier pages mutated the graph: applyCall
	// only touches edges whose From is a node of the applying file, and
	// the fact spool keys one row per (class, file), so no other file's
	// apply can have disturbed this file's calls-edges first.
	stubsByLine map[int][]stubRef
	// stubOwners memoizes stubOwnersAt by (line, authored name). Every
	// call fact on a line asks the same question, so without this the
	// per-fact scan is quadratic in the sites sharing one physical line
	// (a generated single-line class body made Enrich 49x slower).
	stubOwners map[string][]*graph.Node
}

// stubRef is one snapshotted calls-edge under stubsByLine: the owning
// file node plus the edge's target id at snapshot time (the authored
// callee name survives there across the unresolved / resolved shapes).
type stubRef struct {
	owner *graph.Node
	to    string
}

// stubOwnersAt returns the distinct file nodes owning a snapshotted call
// of the given trailing name at line — the callers the extractor already
// attributed sites there to.
func (idx *fileIndex) stubOwnersAt(line int, method string) []*graph.Node {
	key := strconv.Itoa(line) + "\x00" + method
	if owners, ok := idx.stubOwners[key]; ok {
		return owners
	}
	var owners []*graph.Node
	seen := make(map[string]struct{})
	for _, s := range idx.stubsByLine[line] {
		if !trailingNameMatches(s.to, method) {
			continue
		}
		if _, dup := seen[s.owner.ID]; dup {
			continue
		}
		seen[s.owner.ID] = struct{}{}
		owners = append(owners, s.owner)
	}
	if idx.stubOwners == nil {
		idx.stubOwners = make(map[string][]*graph.Node)
	}
	idx.stubOwners[key] = owners
	return owners
}

func (a *applier) buildIndex(facts *fileFacts) *fileIndex {
	idx := &fileIndex{
		facts:       facts,
		imports:     make(map[string]string, len(facts.imports)),
		types:       make(map[string]*graph.Node),
		stubsByLine: make(map[int][]stubRef),
		stubOwners:  make(map[string][]*graph.Node),
	}
	idx.superTypes = idx.types
	superKinds := a.supertypeKinds()
	if a.spec.SupertypeKinds != nil {
		idx.superTypes = make(map[string]*graph.Node)
	}
	for _, imp := range facts.imports {
		if imp.Local != "" {
			idx.imports[imp.Local] = imp.Path
		}
	}
	// stubsByLine is read by applyCall alone, but buildIndex runs in
	// EVERY apply phase — supers, metas, aliases and calls all reach it
	// through preparePage, and applyAll builds it once per file whether
	// or not the file has call facts. Snapshot only when this file's
	// facts can ever ask: on a mixed corpus half the admitted stubs were
	// built for phases that never read them (issue #729 item 2).
	snapshotStubs := len(facts.calls) > 0
	for _, n := range a.fileNodes(facts.file) {
		if receiverTypeKinds[n.Kind] {
			if _, dup := idx.types[n.Name]; !dup {
				idx.types[n.Name] = n
			}
		}
		if a.spec.SupertypeKinds != nil && superKinds[n.Kind] {
			if _, dup := idx.superTypes[n.Name]; !dup {
				idx.superTypes[n.Name] = n
			}
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			idx.funcs = append(idx.funcs, n)
		}
		if !snapshotStubs || n.Kind == graph.KindFile {
			// Some languages park top-level calls on the file node; it is
			// never an adoptable caller (and the paged compatibility
			// branch loads file nodes a kind-filtered store would not).
			continue
		}
		for _, e := range a.outEdges(n.ID) {
			if e == nil || e.Kind != graph.EdgeCalls || e.Line == 0 {
				continue
			}
			if e.FilePath != "" && e.FilePath != facts.file {
				continue
			}
			if e.Line < n.StartLine || e.Line > n.EndLine {
				// Framework-dispatch synthesis (Rails callbacks, Laravel
				// middleware) parks an owner's edge on a line outside the
				// owner's own span — not site evidence at that line.
				continue
			}
			idx.stubsByLine[e.Line] = append(idx.stubsByLine[e.Line], stubRef{owner: n, to: e.To})
		}
	}
	return idx
}

// applyAll joins every analyzed file's facts against the graph in
// three phases: supertype edges and meta fills first, calls last —
// a call in one file may resolve through an extends edge (or a
// return_type stamp) another file's facts just synthesized.
func (a *applier) applyAll(all []*fileFacts, res *semantic.EnrichResult) {
	sort.Slice(all, func(i, j int) bool { return all[i].file < all[j].file })
	a.preload(all)
	idxs := make([]*fileIndex, len(all))
	for i, facts := range all {
		idxs[i] = a.buildIndex(facts)
	}
	for i, facts := range all {
		for _, sf := range facts.supers {
			a.applySuper(idxs[i], sf, res)
		}
	}
	for i, facts := range all {
		for _, mf := range facts.metas {
			a.applyMeta(idxs[i], mf, res)
		}
	}
	for i, facts := range all {
		for _, af := range facts.aliases {
			a.applyAlias(idxs[i], af)
		}
	}
	for i, facts := range all {
		for _, cf := range facts.calls {
			a.applyCall(idxs[i], cf, res)
		}
	}
}

// flush round-trips the stamped nodes through the store in one batch —
// on disk backends an in-place Meta mutation is otherwise discarded.
func (a *applier) flush() {
	if len(a.stampedNodes) == 0 {
		return
	}
	nodes := make([]*graph.Node, 0, len(a.stampedNodes))
	for _, n := range a.stampedNodes {
		nodes = append(nodes, n)
	}
	a.g.AddBatch(nodes, nil)
}

// --- Type / method resolution ----------------------------------------

// resolveTypeNode grounds a bare type name to a graph type node:
// same-file declaration first, then import-hinted cross-file match,
// then a repo-unique name match. Returns nil when the name stays
// ambiguous — the engine never guesses among candidates.
func (a *applier) resolveTypeNode(idx *fileIndex, name string) *graph.Node {
	return a.resolveNodeOfKinds(idx, name, idx.types, receiverTypeKinds)
}

// resolveSuperNode is resolveTypeNode over the spec's supertype kind
// set — identical strategy, wider target kinds where the language
// needs it (Ruby modules).
func (a *applier) resolveSuperNode(idx *fileIndex, name string) *graph.Node {
	return a.resolveNodeOfKinds(idx, name, idx.superTypes, a.supertypeKinds())
}

func (a *applier) resolveNodeOfKinds(idx *fileIndex, name string, sameFile map[string]*graph.Node, kinds map[graph.NodeKind]bool) *graph.Node {
	if name == "" {
		return nil
	}
	if n, ok := sameFile[name]; ok {
		return n
	}
	candidates := a.typeCandidates(idx, name, kinds)
	if len(candidates) == 0 {
		return nil
	}
	if hint, ok := idx.imports[name]; ok && hint != "" {
		var matched []*graph.Node
		for _, c := range candidates {
			if importMatches(c.FilePath, c.RepoPrefix, hint, idx.facts.file) {
				matched = append(matched, c)
			}
		}
		if len(matched) == 1 {
			return matched[0]
		}
		// The hint named a definition site; when it matches several
		// candidates the receiver stays ambiguous, and when it matches
		// none the real target is an external / stdlib dependency the
		// graph doesn't hold. Either way the engine must not fall back
		// to a repo-local same-named type — that would mint a false edge
		// shadowing the dependency. A missing edge beats a wrong one.
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

func (a *applier) typeCandidates(idx *fileIndex, name string, kinds map[graph.NodeKind]bool) []*graph.Node {
	key := typeCandidateKey{repoPrefix: idx.facts.repoPrefix, name: name}
	raw, ok := a.typeCandidatesCache[key]
	if !ok {
		raw = a.namedNodes(idx.facts.repoPrefix, name)
		a.typeCandidatesCache[key] = raw
	}
	lang := a.languageSet()
	var out []*graph.Node
	for _, c := range raw {
		if !kinds[c.Kind] {
			continue
		}
		if !lang[c.Language] {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (a *applier) namedNodes(repoPrefix, name string) []*graph.Node {
	key := typeCandidateKey{repoPrefix: repoPrefix, name: name}
	if !a.nameLoaded[key] {
		if !a.repoProjectionLoaded[repoPrefix] {
			var nodes []*graph.Node
			if repoPrefix != "" {
				nodes = a.g.FindNodesByNameInRepo(name, repoPrefix)
			} else {
				nodes = a.g.FindNodesByName(name)
			}
			for _, node := range nodes {
				a.rememberNode(node)
			}
		}
		a.nameLoaded[key] = true
	}
	return a.nodesByName[key]
}

func (a *applier) languageSet() map[string]bool {
	return a.languages
}

// importMatches reports whether a candidate definition file plausibly
// backs the import-path hint. Relative hints resolve against the
// importing file's directory; absolute (package-style) hints match as
// a path-segment suffix of the candidate's extension-less path.
func importMatches(candidateFile, candidatePrefix, hint, importerFile string) bool {
	cand := strings.TrimSuffix(candidateFile, filepath.Ext(candidateFile))
	if candidatePrefix != "" {
		cand = strings.TrimPrefix(cand, candidatePrefix+"/")
	}
	if strings.HasPrefix(hint, "./") || strings.HasPrefix(hint, "../") {
		base := importerFile
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[:i]
		} else {
			base = ""
		}
		resolved := filepath.ToSlash(filepath.Join(base, hint))
		return cand == resolved || cand == resolved+"/index"
	}
	hint = strings.Trim(hint, "/")
	if hint == "" {
		return false
	}
	// Package files: Python's __init__.py and Rust's mod.rs name the
	// directory, not the file.
	return pathSegSuffix(cand, hint) ||
		pathSegSuffix(cand, hint+"/__init__") ||
		pathSegSuffix(cand, hint+"/mod")
}

// pathSegSuffix reports whether want equals cand or a slash-aligned
// suffix of it.
func pathSegSuffix(cand, want string) bool {
	return cand == want || strings.HasSuffix(cand, "/"+want)
}

// methodOn resolves a method name against a type's member set,
// following resolved EdgeExtends links for inherited methods. Returns
// nil when the type (and its ancestry) declares zero same-named
// members. When it declares several (an overload set), the call site's
// argument count (argCount) may still pick a unique target by arity;
// argCount < 0 means the arity is unknown, in which case an overload
// set stays untouched rather than half-guessed.
func (a *applier) methodOn(typeNode *graph.Node, method string, argCount, depth int) (result *graph.Node) {
	if typeNode == nil || depth > extendsWalkDepth {
		return nil
	}
	key := methodLookupKey{typeID: typeNode.ID, method: method, argCount: argCount, depth: depth}
	if cached, ok := a.methodCache[key]; ok {
		return cached
	}
	defer func() { a.methodCache[key] = result }()

	var fromIDs []string
	for _, e := range a.inEdges(typeNode.ID) {
		if e.Kind == graph.EdgeMemberOf {
			fromIDs = append(fromIDs, e.From)
		}
	}
	var matches []*graph.Node
	if len(fromIDs) > 0 {
		members := a.nodes(fromIDs)
		for _, id := range fromIDs {
			n := members[id]
			if n == nil {
				continue
			}
			// An extension function carries a synthetic member_of edge to
			// its receiver type but is NOT a real member — a real member of
			// the same name must shadow it. Exclude extensions here so the
			// direct/inherited lookup sees only real members; the call phase
			// resolves extensions separately, as a fallback.
			if n.Kind == graph.KindMethod && n.Name == method && !nodeIsExtension(n) {
				matches = append(matches, n)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0]
	case 0:
		// Climb every inheritance edge the spec recognises — the
		// supertype chain plus, where the language widens it, the
		// mixin / include edges that pull a module's methods in. A
		// member contributed by exactly one ancestor resolves; one
		// contributed by several distinct ancestors (ambiguous across
		// mixins or supers) stays unresolved rather than half-guessed.
		// The depth bound above guards diamonds and mutual mixins from
		// looping.
		inheritKinds := a.spec.inheritEdgeKinds()
		parentKinds := a.supertypeKinds()
		parentIDs := make([]string, 0)
		for _, edge := range a.outEdges(typeNode.ID) {
			if !graph.IsUnresolvedTarget(edge.To) && edgeKindIn(edge.Kind, inheritKinds) {
				parentIDs = append(parentIDs, edge.To)
			}
		}
		parents := a.nodes(parentIDs)
		var found *graph.Node
		for _, e := range a.outEdges(typeNode.ID) {
			if graph.IsUnresolvedTarget(e.To) || !edgeKindIn(e.Kind, inheritKinds) {
				continue
			}
			parent := parents[e.To]
			if parent == nil || !parentKinds[parent.Kind] {
				continue
			}
			m := a.methodOn(parent, method, argCount, depth+1)
			if m == nil {
				continue
			}
			if found != nil && found.ID != m.ID {
				return nil
			}
			found = m
		}
		return found
	}
	// Several same-named members: an overload set. Today this is always
	// skipped. Narrow that skip with an arity filter — when the call
	// site's argument count uniquely selects ONE fixed-arity candidate,
	// resolve to it; in every other shape keep skipping (see
	// disambiguateByArity).
	return a.disambiguateByArity(matches, argCount)
}

// disambiguateByArity selects the unique member of an overload set whose
// declared parameter count equals the call site's argument count. It
// resolves ONLY among FIXED-arity candidates and only when exactly one
// of them matches: a variadic candidate that could also accept the call
// (its minimum arity is satisfied) makes the set ambiguous, because it
// would shadow the fixed match — in that case, and whenever the arity is
// unknown (argCount < 0), zero candidates match, or more than one fixed
// candidate matches, it returns nil so the caller keeps skipping.
func (a *applier) disambiguateByArity(matches []*graph.Node, argCount int) *graph.Node {
	if argCount < 0 {
		return nil
	}
	var fixedHit *graph.Node
	fixedHits := 0
	for _, m := range matches {
		count, variadic := a.paramArity(m)
		if variadic {
			// A variadic candidate accepts any arg count at or above its
			// minimum (non-variadic) arity. If the call could land here, the
			// set cannot be disambiguated safely.
			if argCount >= count-1 {
				return nil
			}
			continue
		}
		if count == argCount {
			fixedHit = m
			fixedHits++
		}
	}
	if fixedHits == 1 {
		return fixedHit
	}
	return nil
}

// paramArity returns a method's declared parameter count and whether its
// last parameter is variadic, derived from the KindParam nodes the
// extractor links to it via EdgeParamOf. A variadic parameter counts as
// one toward the total; its presence widens the method's acceptable
// arity to [count-1, +inf). A method whose extractor emits no parameter
// nodes reports count 0 — languages that opt into arity disambiguation
// via the CallArgCount hook must emit parameter nodes for the count to
// be meaningful.
func (a *applier) paramArity(m *graph.Node) (count int, variadic bool) {
	var paramIDs []string
	for _, e := range a.inEdges(m.ID) {
		if e.Kind != graph.EdgeParamOf {
			continue
		}
		count++
		paramIDs = append(paramIDs, e.From)
	}
	params := a.nodes(paramIDs)
	for _, id := range paramIDs {
		if p := params[id]; p != nil && p.Meta != nil {
			if v, _ := p.Meta["variadic"].(bool); v {
				variadic = true
			}
		}
	}
	return count, variadic
}

// edgeKindIn reports whether k is one of kinds.
func edgeKindIn(k graph.EdgeKind, kinds []graph.EdgeKind) bool {
	for _, want := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// callableReturnType resolves a bare callee name to its graph
// return_type: same-file declaration first, then a repo-unique
// function. The returned name is normalized to the bare type name. When
// no in-repo function grounds the callee, the stdlib seed table is
// consulted as a LAST RESORT — an in-repo symbol always wins. The bool
// reports whether the type came from the seed (a heuristic, graded at the
// inferred confidence band) rather than a grounded in-repo return type.
func (a *applier) callableReturnType(idx *fileIndex, callee string) (string, bool) {
	var match *graph.Node
	for _, n := range idx.funcs {
		if n.Name == callee {
			if match != nil {
				return "", false // same-file overloads: ambiguous
			}
			match = n
		}
	}
	if match == nil {
		raw := a.namedNodes(idx.facts.repoPrefix, callee)
		lang := a.languageSet()
		for _, c := range raw {
			if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
				continue
			}
			if !lang[c.Language] {
				continue
			}
			if match != nil {
				return "", false
			}
			match = c
		}
	}
	if match != nil {
		// An in-repo callee resolved the name — its declared return type
		// (possibly empty) wins; the seed is never consulted.
		if match.Meta == nil {
			return "", false
		}
		rt, _ := match.Meta["return_type"].(string)
		return a.spec.normalize(rt), false
	}
	// No in-repo function carries this name: fall back to the curated
	// stdlib seed table for a well-known free-function return type.
	if a.spec.StdlibReturnType != nil {
		if rt, ok := a.spec.StdlibReturnType(callee, ""); ok {
			return a.spec.normalize(rt), true
		}
	}
	return "", false
}

// enclosingCallable returns the innermost function/method node
// containing line.
func (idx *fileIndex) enclosingCallable(line int) *graph.Node {
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range idx.funcs {
		if n.StartLine <= line && line <= n.EndLine {
			if size := n.EndLine - n.StartLine; size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	return best
}

// --- Call application -------------------------------------------------

func (a *applier) applyCall(idx *fileIndex, cf callFact, res *semantic.EnrichResult) {
	typeNode, inferred := a.callReceiverType(idx, &cf)
	if typeNode == nil {
		return
	}
	target := a.methodOn(typeNode, cf.method, cf.arity(), 0)
	if target == nil {
		// A trait-use alias renames the member onto the using type; the
		// alias name is not a member of the type or its ancestry, so the
		// direct climb misses it. The alias map routes it through.
		target = a.resolveAlias(typeNode, cf.method)
	}
	if target == nil {
		// No real (own or inherited) member and no alias — try an extension
		// function declared on the receiver type. A real member would have
		// resolved above, so this honours members-shadow-extensions.
		target = a.extensionMethod(typeNode, cf.method)
	}
	if target == nil {
		return
	}
	// The extractor already attributed this site to its owner — adopt
	// that attribution when a stub of the authored name exists at the
	// call line, so the engine can never land its resolution on a
	// different node than extraction chose (a shared line would
	// otherwise mint a duplicate edge nothing dedupes, From being part
	// of every edge identity). The line-keyed containment lookup stays
	// as the fallback for sites the extractor recorded no stub for
	// (desugared operator calls carry no authored-name stub). On a
	// multi-owner tie, containment may still break it — but only WITHIN
	// the tied set: a callable that merely shares the line never
	// collects a call it did not author (it has no stub to claim, so it
	// would mint), and when no tied owner contains the line the site is
	// refused outright.
	//
	// Adoption couples this tier's precision to extraction's attribution
	// accuracy, and that trade is only sound where extraction is
	// byte-precise for the owner kind in question. An attribution defect
	// that used to surface as a harmless unresolved stub surfaces here as
	// a confident resolved edge instead: issue #728 caught an indexer's
	// body call parked on a same-line property, promoted to
	// ast_resolved/0.95 on a member whose whole body was `=> 1`.
	//
	// Two different things hold that end up, and they cover different
	// kinds. The accessor-bearing members (property, indexer, event with
	// add/remove) record byte extents, so they own their calls outright.
	// The kinds that still record NONE - operator, conversion operator,
	// destructor - are held only by the extractor REFUSING a call whose
	// line owner's recorded bytes provably exclude the offset. That
	// refusal is what turns their attribution defect into a dropped edge
	// rather than a confident wrong one. So: giving one of those kinds a
	// node without giving it extents in the same change re-opens #728,
	// because adoption would start trusting a line fallback again.
	var caller *graph.Node
	owners := idx.stubOwnersAt(cf.line, cf.method)
	switch len(owners) {
	case 0:
		caller = idx.enclosingCallable(cf.line)
	case 1:
		caller = owners[0]
	default:
		if enc := idx.enclosingCallable(cf.line); enc != nil {
			for _, o := range owners {
				if o.ID == enc.ID {
					caller = enc
					break
				}
			}
		}
	}
	if caller == nil {
		return
	}
	strategy, confidence := strategyDirect, astConfidence
	if inferred {
		// The receiver type was derived through a chained return-type
		// rewrite rather than a direct binding — grade the edge honestly.
		strategy, confidence = strategyInferred, inferredConfidence
	}
	if caller.ID == target.ID {
		// A SELF-typed receiver — a field of the enclosing class calling
		// the same method (linked list, chain of responsibility) — makes
		// the calling method its own genuine target. Claim the extracted
		// stub in place; minting a fresh self-edge stays forbidden, which
		// is what this guard has always been for.
		a.upgradeExistingCall(caller, target, cf, res, strategy, confidence)
		return
	}
	a.upgradeOrCreateCall(caller, target, cf, idx.facts.file, res, strategy, confidence)
}

// callReceiverType resolves a call's receiver to a graph type node. The
// bool reports whether the type was derived by inference — a chained
// return-type rewrite — rather than a direct binding; an inferred
// receiver lands its call edge at the graded confidence band.
func (a *applier) callReceiverType(idx *fileIndex, cf *callFact) (*graph.Node, bool) {
	if cf.recvChain != nil {
		return a.chainReturnType(idx, cf.recvChain), true
	}
	recvType := cf.recvType
	inferred := cf.inferred
	if recvType == "" && cf.recvPendingCallee != "" {
		rt, seeded := a.callableReturnType(idx, cf.recvPendingCallee)
		recvType = rt
		// A seed-derived receiver type is a heuristic, not a grounded
		// return type — grade any call through it at the inferred band.
		inferred = inferred || seeded
	}
	if recvType != "" {
		// cf.inferred is set when recvType came from a guard narrowing;
		// seeded marks a stdlib-table return type. A direct annotation /
		// constructor / propagation / in-repo return type leaves both
		// false, so the direct path is unchanged.
		return a.resolveTypeNode(idx, recvType), inferred
	}
	if cf.recvIdent != "" {
		// Static / type-qualified call: only when the identifier is a
		// real type in scope of this file's imports.
		return a.resolveTypeNode(idx, cf.recvIdent), false
	}
	return nil, false
}

// chainReturnType types the result of a method call standing in receiver
// position (`a.step().done()`): it resolves the inner receiver and method,
// reads the inner method's declared return type, and applies the fluent
// self / trait return rewrite so a trait method returning the trait type,
// called on a using class, types as that class. Returns nil when any link
// fails to ground — a missing edge beats a wrong one.
func (a *applier) chainReturnType(idx *fileIndex, inner *callFact) *graph.Node {
	// Stdlib element access on a typed collection builder:
	// `mutableListOf<Foo>().first()` types to Foo. Consulted before the
	// real-member path so the captured element type is preferred over the
	// (unresolvable, stdlib) container type.
	if elem := a.stdlibElementType(idx, inner); elem != nil {
		return elem
	}
	recv, _ := a.callReceiverType(idx, inner)
	if recv == nil {
		return nil
	}
	m := a.methodOn(recv, inner.method, inner.arity(), 0)
	if m == nil {
		m = a.resolveAlias(recv, inner.method)
	}
	if m == nil {
		// No in-repo member grounds the call. As a last resort, seed a
		// well-known stdlib transform's return type on the resolved
		// container (`Collection::map` -> Collection), so a fluent stdlib
		// chain keeps its type even where the container's members are not
		// declared in-repo.
		if a.spec.StdlibReturnType != nil {
			if rt, ok := a.spec.StdlibReturnType(inner.method, recv.Name); ok {
				return a.resolveTypeNode(idx, a.spec.normalize(rt))
			}
		}
		return nil
	}
	return a.effectiveReturnType(idx, m, recv)
}

// stdlibElementType types a stdlib collection element access whose receiver
// is a typed collection builder: `mutableListOf<Foo>().first()` resolves to
// the element type Foo. It fires only when the inner call is a bare builder
// call carrying an explicit type argument and the method is a seeded element
// accessor; otherwise it returns nil and the normal member path runs.
func (a *applier) stdlibElementType(idx *fileIndex, inner *callFact) *graph.Node {
	if a.spec.StdlibElementAccess == nil {
		return nil
	}
	if inner.recvPendingCallee == "" || inner.recvCallTypeArg == "" {
		return nil
	}
	if !a.spec.StdlibElementAccess(inner.recvPendingCallee, inner.method) {
		return nil
	}
	return a.resolveTypeNode(idx, a.spec.normalize(inner.recvCallTypeArg))
}

// effectiveReturnType resolves a method's declared return type to a graph
// type node, applying the fluent return rewrite: a method returning
// `self` / `static` types as the receiver, and a TRAIT method that
// returns its own trait name, reached through a using class, rebinds to
// that using class. Any other named return type resolves normally.
func (a *applier) effectiveReturnType(idx *fileIndex, m, receiver *graph.Node) *graph.Node {
	rt := a.spec.normalize(methodReturnTypeName(m))
	if rt == "" {
		return nil
	}
	if isSelfReturn(rt) {
		return receiver
	}
	// A trait method whose return type IS the trait itself, reached
	// through a class that uses the trait, fluently returns the using
	// class — rebind. Restricted to trait owners (Meta kind == "trait")
	// and to the case where the method was inherited (owner != receiver),
	// so a class method returning its own class is left to resolve
	// normally (it already lands on the right type).
	if owner := a.ownerType(m); owner != nil && owner.ID != receiver.ID &&
		isTraitNode(owner) && rt == owner.Name {
		return receiver
	}
	return a.resolveTypeNode(idx, rt)
}

// ownerType returns the type a method is a member of, following its
// EdgeMemberOf link; nil when the method has no resolved owner.
func (a *applier) ownerType(m *graph.Node) *graph.Node {
	for _, e := range a.outEdges(m.ID) {
		if e.Kind == graph.EdgeMemberOf {
			if owner := a.node(e.To); owner != nil {
				return owner
			}
		}
	}
	return nil
}

// applyAlias resolves one trait-use alias adaptation against the graph
// and records it under the using type's node ID for the call phase. A
// qualified alias whose trait cannot be resolved is dropped rather than
// guessed.
func (a *applier) applyAlias(idx *fileIndex, af aliasFact) {
	typeNode := idx.types[af.typeName]
	if typeNode == nil {
		return
	}
	var traitNode *graph.Node
	if af.trait != "" {
		if traitNode = a.resolveSuperNode(idx, af.trait); traitNode == nil {
			return
		}
	}
	a.aliases[typeNode.ID] = append(a.aliases[typeNode.ID], resolvedAlias{
		alias: af.alias, trait: traitNode, method: af.method,
	})
}

// resolveAlias routes a method name through a trait-use alias on the
// type: the aliased name resolves to the original trait member. Returns
// nil when no alias matches or the original member does not ground.
func (a *applier) resolveAlias(typeNode *graph.Node, method string) *graph.Node {
	for _, al := range a.aliases[typeNode.ID] {
		if al.alias != method {
			continue
		}
		owner := al.trait
		if owner == nil {
			owner = typeNode
		}
		// The alias path carries no call site, so its arity is unknown
		// (-1): an aliased overload set stays un-narrowed, as before.
		if m := a.methodOn(owner, al.method, -1, 0); m != nil {
			return m
		}
	}
	return nil
}

// nodeIsExtension reports whether a method node is an extension function —
// a top-level callable declared with a receiver type, stamped by the
// extractor with Meta["extension_receiver"]. Such a node is callable as a
// member of its receiver but is not a structural member, so it is excluded
// from the real-member lookup and resolved only as a fallback.
func nodeIsExtension(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	r, _ := n.Meta["extension_receiver"].(string)
	return r != ""
}

// extensionMethod resolves a method name against the extension functions
// declared on the receiver type. It is consulted ONLY after a real member
// lookup (direct, inherited, and alias) misses, so a real member of the
// same name always wins — even an ambiguous real overload set keeps the
// extension from resolving, because the type genuinely declares the method.
// Resolution is on the exact receiver type name within the receiver's repo;
// a name claimed by more than one extension on that receiver stays
// unresolved rather than guessed. Returns nil unless the spec opts in.
func (a *applier) extensionMethod(typeNode *graph.Node, method string) *graph.Node {
	if typeNode == nil || !a.spec.ExtensionFunctions {
		return nil
	}
	if a.typeHasRealMember(typeNode, method) {
		return nil
	}
	a.buildExtensionIndex()
	var hit *graph.Node
	for _, m := range a.extensions[extKey{receiver: typeNode.Name, method: method}] {
		if m.RepoPrefix != typeNode.RepoPrefix {
			continue
		}
		if hit != nil {
			return nil // two extensions claim this receiver+name — ambiguous.
		}
		hit = m
	}
	return hit
}

// typeHasRealMember reports whether the type declares a real (non-extension)
// method of this name directly. It guards the extension fallback so an
// ambiguous real overload set — which makes methodOn return nil without
// meaning "no member" — never resolves to an extension instead.
func (a *applier) typeHasRealMember(typeNode *graph.Node, method string) bool {
	var memberIDs []string
	for _, e := range a.inEdges(typeNode.ID) {
		if e.Kind == graph.EdgeMemberOf {
			memberIDs = append(memberIDs, e.From)
		}
	}
	members := a.nodes(memberIDs)
	for _, id := range memberIDs {
		n := members[id]
		if n != nil && n.Kind == graph.KindMethod && n.Name == method && !nodeIsExtension(n) {
			return true
		}
	}
	return false
}

// buildExtensionIndex lazily indexes every extension function in the graph
// (KindMethod nodes carrying Meta["extension_receiver"], in the spec's
// languages) by (normalized receiver type name, method name). Built once
// per applier, on first use; a no-op for specs that never call it.
func (a *applier) buildExtensionIndex() {
	if a.extensionsOK {
		return
	}
	a.extensionsOK = true
	a.extensions = make(map[extKey][]*graph.Node)
	lang := a.languageSet()
	for _, n := range a.allNodes {
		if n.Meta == nil || !lang[n.Language] || n.Name == "" {
			continue
		}
		recv, _ := n.Meta["extension_receiver"].(string)
		if recv == "" {
			continue
		}
		k := extKey{receiver: a.spec.normalize(recv), method: n.Name}
		a.extensions[k] = append(a.extensions[k], n)
	}
}

// upgradeOrCreateCall lands a grounded call resolution on the graph:
// confirm the edge when it already points at the target, claim a
// weaker-tier or still-unresolved edge at the same line, otherwise add
// a fresh edge. Edges that already carry compiler/AST-grade provenance
// pointing elsewhere are never overridden.
func (a *applier) upgradeOrCreateCall(caller, target *graph.Node, cf callFact, file string, res *semantic.EnrichResult, strategy resolutionStrategy, confidence float64) {
	if a.upgradeExistingCall(caller, target, cf, res, strategy, confidence) {
		return
	}
	a.addASTEdge(caller.ID, target.ID, graph.EdgeCalls, file, cf.line, strategy, confidence)
	res.EdgesAdded++
}

// upgradeExistingCall confirms or retargets an edge already present at
// the call site and reports whether the site is handled — false means
// no edge matched and the caller may mint a fresh one. Split out so the
// self-target path (a SELF-typed receiver field, where target == caller)
// can claim the extracted stub without ever being allowed to create.
func (a *applier) upgradeExistingCall(caller, target *graph.Node, cf callFact, res *semantic.EnrichResult, strategy resolutionStrategy, confidence float64) bool {
	outs := a.outEdges(caller.ID)
	for _, e := range outs {
		if e.Kind == graph.EdgeCalls && e.To == target.ID {
			if a.confirmCall(e, strategy, confidence) {
				res.EdgesConfirmed++
			}
			return true
		}
	}
	for _, e := range outs {
		if e.Kind != graph.EdgeCalls || e.Line != cf.line {
			continue
		}
		if !trailingNameMatches(e.To, cf.method) {
			continue
		}
		if !a.claimable(e) {
			// A same-line edge for this name already carries
			// equal-or-stronger evidence for a different target —
			// leave it alone and don't double the call site.
			return true
		}
		oldTo := e.To
		e.To = target.ID
		a.reindexEdge(e, oldTo)
		a.confirmCall(e, strategy, confidence)
		res.EdgesConfirmed++
		return true
	}
	return false
}

// confirmCall lands the provenance of a resolved call edge at the band
// the resolution earned: the direct path raises it to the AST ceiling
// (confirmAST), the inferred path stamps the graded band and the
// resolution_strategy label without ever downgrading a stronger edge.
func (a *applier) confirmCall(e *graph.Edge, strategy resolutionStrategy, confidence float64) bool {
	if strategy == strategyDirect {
		return a.confirmAST(e)
	}
	return a.confirmInferred(e, confidence)
}

// confirmInferred stamps the graded inferred band on an edge the engine
// resolved by a return-type rewrite: OriginASTResolved provenance at the
// honest inferred confidence, the inferred resolution_strategy label,
// and the provider — never downgrading an edge that already carries
// stronger provenance or a higher confidence.
func (a *applier) confirmInferred(e *graph.Edge, confidence float64) bool {
	if graph.OriginRank(effectiveOrigin(e)) > graph.OriginRank(graph.OriginASTResolved) {
		return false
	}
	changed := false
	if effectiveOrigin(e) != graph.OriginASTResolved {
		a.g.SetEdgeProvenance(e, graph.OriginASTResolved)
		changed = true
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	if s, _ := e.Meta["semantic_source"].(string); s == "" {
		e.Meta["semantic_source"] = a.provider
		changed = true
	}
	if rs, _ := e.Meta["resolution_strategy"].(string); rs == "" {
		e.Meta["resolution_strategy"] = string(strategyInferred)
		changed = true
	}
	if e.Confidence < confidence {
		e.Confidence = confidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, confidence)
		changed = true
	}
	if changed {
		a.persistEdgeRow(e)
	}
	return changed
}

// methodReturnTypeName returns a method node's declared return type as
// recorded in Meta["return_type"], "" when absent.
func methodReturnTypeName(m *graph.Node) string {
	if m == nil || m.Meta == nil {
		return ""
	}
	rt, _ := m.Meta["return_type"].(string)
	return rt
}

// isTraitNode reports whether a type node was extracted as a trait
// (Meta kind == "trait"), the marker the PHP extractor stamps.
func isTraitNode(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	k, _ := n.Meta["kind"].(string)
	return k == "trait"
}

// isSelfReturn reports whether a normalized return type names the
// receiver's own type — the fluent self / late-static-binding forms.
func isSelfReturn(t string) bool {
	switch t {
	case "self", "static", "$this", "this":
		return true
	}
	return false
}

// --- Supertype application --------------------------------------------

func (a *applier) applySuper(idx *fileIndex, sf superFact, res *semantic.EnrichResult) {
	typeNode, ok := idx.superTypes[sf.typeName]
	if !ok {
		return
	}
	superNode := a.resolveSuperNode(idx, sf.superName)
	if superNode == nil || superNode.ID == typeNode.ID {
		return
	}
	kind := sf.kind
	if kind == "" {
		// Syntax didn't discriminate (C# base list): the resolved
		// target's node kind decides.
		if superNode.Kind == graph.KindInterface {
			kind = graph.EdgeImplements
		} else {
			kind = graph.EdgeExtends
		}
	}
	outs := a.outEdges(typeNode.ID)
	for _, e := range outs {
		if e.Kind == kind && e.To == superNode.ID {
			if a.confirmAST(e) {
				res.EdgesConfirmed++
			}
			return
		}
	}
	for _, e := range outs {
		if e.Kind != graph.EdgeExtends && e.Kind != graph.EdgeImplements {
			continue
		}
		if !a.claimable(e) || !trailingNameMatches(e.To, sf.superName) {
			continue
		}
		if e.Kind == kind {
			// Same relation kind, only the target changes — an in-place
			// retarget + ReindexEdge is safe because the edge's logical
			// key (which folds Kind) keeps the same Kind on both sides.
			oldTo := e.To
			e.To = superNode.ID
			a.reindexEdge(e, oldTo)
			a.confirmAST(e)
			res.EdgesConfirmed++
			return
		}
		// The relation kind itself changes (a C#-style base list whose
		// member turned out to be an interface, not a base class).
		// Mutating Kind in place corrupts the adjacency index: ReindexEdge
		// reconstructs the old logical key from the already-mutated Kind,
		// so the original entry is never removed — the in-memory store
		// leaks a stale index slot and the sqlite store ends up with two
		// contradictory rows. Drop the old edge and add a fresh one of the
		// correct kind instead, mirroring how the compiler-grade providers
		// only ever add new edges rather than flip an existing one's kind.
		a.removeEdge(e)
		a.addASTEdge(typeNode.ID, superNode.ID, kind, idx.facts.file, sf.line, strategyDirect, astConfidence)
		res.EdgesAdded++
		return
	}
	a.addASTEdge(typeNode.ID, superNode.ID, kind, idx.facts.file, sf.line, strategyDirect, astConfidence)
	res.EdgesAdded++
}

// --- Node meta application --------------------------------------------

func (a *applier) applyMeta(idx *fileIndex, mf metaFact, res *semantic.EnrichResult) {
	var node *graph.Node
	if mf.owner != "" {
		node = a.findMember(idx, mf.owner, mf.name)
	} else if mf.line > 0 {
		node = idx.enclosingCallable(mf.line)
		if node != nil && node.StartLine != mf.line {
			node = nil
		}
	}
	if node == nil {
		return
	}
	if node.Meta != nil {
		if existing, ok := node.Meta[mf.key].(string); ok && existing != "" {
			return // never overwrite an existing (possibly stronger) stamp
		}
	}
	semantic.EnrichNodeMeta(node, mf.key, mf.value, a.provider)
	a.stampedNodes[node.ID] = node
	res.NodesEnriched++
}

// findMember locates the field/variable node for owner.name in the
// file (extractor convention: Meta["receiver"] carries the owner).
func (a *applier) findMember(idx *fileIndex, owner, name string) *graph.Node {
	for _, n := range a.fileNodes(idx.facts.file) {
		if n.Name != name {
			continue
		}
		if n.Kind != graph.KindField && n.Kind != graph.KindVariable {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv == owner {
			return n
		}
	}
	return nil
}

// --- Edge provenance helpers -------------------------------------------

// confirmAST stamps tree-sitter-grade provenance on an edge the engine
// grounded: OriginASTResolved (deliberately NOT the lsp_* tiers —
// these resolutions are scope-grounded but not compiler-verified),
// confidence raised to the AST ceiling, and the provider recorded as
// semantic_source. Never downgrades an edge that already carries
// AST-or-better provenance; returns whether anything changed.
func (a *applier) confirmAST(e *graph.Edge) bool {
	// Never downgrade. The comparison runs against the EFFECTIVE origin,
	// which backfills legacy edges that carry their compiler-grade
	// provenance only in Meta["semantic_source"] (Origin unset). Requiring
	// a non-empty Origin here would wrongly let those edges through and
	// clobber both their tier and their semantic_source — so the only
	// gate is the effective-rank comparison.
	origin := effectiveOrigin(e)
	if graph.OriginRank(origin) >= graph.OriginRank(graph.OriginASTResolved) {
		// Origin is already AST-or-better — never downgrade it. But an edge the
		// extractor emitted may carry that provenance only implicitly (for
		// example, a structural extends edge with no Origin field). Materialize
		// the effective tier while crediting the provider and raising confidence.
		changed := false
		if e.Origin == "" && origin == graph.OriginASTResolved {
			e.Origin = origin
			changed = true
		}
		if e.Meta == nil {
			e.Meta = make(map[string]any)
		}
		if s, _ := e.Meta["semantic_source"].(string); s == "" {
			e.Meta["semantic_source"] = a.provider
			changed = true
		}
		if e.Confidence < astConfidence {
			e.Confidence = astConfidence
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
			changed = true
		}
		if changed {
			a.persistEdgeRow(e)
		}
		return changed
	}
	a.persistConfirmedAST(e)
	return true
}

// persistConfirmedAST stamps the AST-grade provenance bundle (origin,
// confidence, label, semantic_source) on e and makes it durable on every
// backend. SetEdgeProvenance only writes origin+tier; on a disk backend e
// is a detached row copy, so the confidence / label / Meta mutations would
// be lost unless the full edge is round-tripped — persistEdgeRow does that
// through the backend's edge-attribute write path.
func (a *applier) persistConfirmedAST(e *graph.Edge) {
	a.g.SetEdgeProvenance(e, graph.OriginASTResolved)
	if e.Confidence < astConfidence {
		e.Confidence = astConfidence
	}
	e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta["semantic_source"] = a.provider
	a.persistEdgeRow(e)
}

// persistEdgeRow makes a confirmed edge's full attribute bundle durable.
// On the in-memory backend GetOutEdges returns the live *Edge pointer, so
// the field mutations are already persisted and this is a no-op. A disk
// backend returns a detached row copy; SetEdgeProvenance only wrote
// origin+tier, so the confidence / label / Meta mutations need an explicit
// round-trip through the backend's edge-attribute write path.
func (a *applier) persistEdgeRow(e *graph.Edge) {
	if w, ok := a.g.(graph.EdgePersister); ok {
		w.PersistEdgeAttributes(e)
	}
}

// addASTEdge mints an AST-grade resolution edge. The default direct
// path (strategyDirect, astConfidence) keeps the structurally-grounded
// confidence and carries no resolution_strategy label — its callers
// have already arbitrated the edge state before reaching here, so it
// adds unconditionally exactly as before. A graded path (e.g.
// strategyInferred, inferredConfidence) emits the same OriginASTResolved
// provenance at a lower, honest confidence and stamps
// Meta["resolution_strategy"] with its label. A graded emission never
// clobbers or downgrades a pre-existing equal-or-stronger edge on the
// same (from,to,kind): on contention the stronger edge is returned
// untouched.
func (a *applier) addASTEdge(from, to string, kind graph.EdgeKind, file string, line int, strategy resolutionStrategy, confidence float64) *graph.Edge {
	if strategy != strategyDirect {
		if existing := a.strongerEdge(from, to, kind, confidence); existing != nil {
			return existing
		}
	}
	e := &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        file,
		Line:            line,
		Confidence:      confidence,
		ConfidenceLabel: graph.ConfidenceLabelFor(kind, confidence),
		Origin:          graph.OriginASTResolved,
		Meta: map[string]any{
			"semantic_source": a.provider,
		},
	}
	if strategy != strategyDirect {
		e.Meta["resolution_strategy"] = string(strategy)
	}
	a.g.AddEdge(e)
	if a.outLoaded[from] {
		a.outByID[from] = append(a.outByID[from], e)
	}
	if a.inLoaded[to] {
		a.inByID[to] = append(a.inByID[to], e)
	}
	return e
}

// strongerEdge returns an existing (from->to, kind) edge whose
// provenance outranks the AST-grade origin a graded emission would
// stamp — or whose confidence is equal-or-higher at the same rank — so
// a lower-confidence inferred edge yields to it instead of downgrading
// it. Returns nil when no such edge exists. Graded emissions stay at
// OriginASTResolved, so the rank floor is that tier: a pre-existing LSP
// edge (higher rank) or a direct AST edge (same rank, higher
// confidence) both win.
func (a *applier) strongerEdge(from, to string, kind graph.EdgeKind, confidence float64) *graph.Edge {
	gradedRank := graph.OriginRank(graph.OriginASTResolved)
	for _, e := range a.outEdges(from) {
		if e.Kind != kind || e.To != to {
			continue
		}
		rank := graph.OriginRank(effectiveOrigin(e))
		if rank > gradedRank {
			return e
		}
		if rank == gradedRank && e.Confidence >= confidence {
			return e
		}
	}
	return nil
}

// claimable reports whether the engine may rewire this edge's target:
// still-unresolved / external stub targets always are; resolved
// targets only when their effective provenance ranks below AST-grade
// (a name-locality guess this engine's type evidence outranks).
func (a *applier) claimable(e *graph.Edge) bool {
	if isStubTarget(e.To) {
		return true
	}
	// A member-call bind that never got an Origin stamp came from the
	// resolver's caller-receiver fallback (which stamps no Origin) or a
	// pre-stamping vintage — name evidence no matter its confidence.
	// Without this, the DefaultOriginFor backfill grades a 0.9 guess at
	// the AST ceiling and blocks the retarget: the facade shape (a
	// service wrapping a same-named repository method) stays bound to
	// the calling method itself forever. Explicitly stamped tiers, DI
	// binds (resolution marker), and provider edges (semantic_source)
	// keep their rank.
	if e.Origin == "" && e.Meta != nil {
		mc, _ := e.Meta["member_call"].(bool)
		_, hasResolution := e.Meta["resolution"]
		_, hasSemantic := e.Meta["semantic_source"]
		if mc && !hasResolution && !hasSemantic {
			return true
		}
	}
	return graph.OriginRank(effectiveOrigin(e)) < graph.OriginRank(graph.OriginASTResolved)
}

// effectiveOrigin returns the edge's provenance tier, backfilling the
// legacy default for edges minted before Origin stamping.
func effectiveOrigin(e *graph.Edge) string {
	if e.Origin != "" {
		return e.Origin
	}
	sem := ""
	if e.Meta != nil {
		sem, _ = e.Meta["semantic_source"].(string)
	}
	return graph.DefaultOriginFor(e.Kind, e.Confidence, sem)
}

func isStubTarget(to string) bool {
	if graph.IsUnresolvedTarget(to) {
		return true
	}
	for _, p := range []string{"external::", "stdlib::", "dep::"} {
		if strings.HasPrefix(to, p) || strings.Contains(to, "::"+p) {
			return true
		}
	}
	return false
}

// trailingNameMatches reports whether a target id's final name segment
// equals name — across the unresolved / stub / resolved id shapes
// (`unresolved::*.m`, `unresolved::m`, `a/b.go::T.m`).
func trailingNameMatches(to, name string) bool {
	if name == "" {
		return false
	}
	s := to
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s == name
}
