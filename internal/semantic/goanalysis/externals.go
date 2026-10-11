package goanalysis

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// stdlibModuleID is the synthetic KindModule node ID used to attribute Go
// standard-library symbols. The standard library has no go.mod entry, so
// we materialise a single shared module node so dependency queries
// ("which modules does pkg X reach into?") return a stdlib bucket
// alongside real module-cache modules.
const stdlibModuleID = "module::go:stdlib"

// modulePathStdlib is the canonical "path" for the stdlib module node. Kept
// in sync with the existing internal/modules naming so any consumer that
// groups KindModule nodes by Meta["path"] can find stdlib alongside real
// go.mod entries.
const modulePathStdlib = "stdlib"

// externalsAttribution holds bookkeeping for a single Enrich() pass. It
// owns:
//
//   - pkgByPath: every transitively loaded *packages.Package, indexed by
//     import path. Top-level packages.Load returns only the repo's own
//     packages; we walk pkg.Imports recursively so external symbol
//     resolution can find the owning module for stdlib / module-cache
//     packages too.
//   - moduleByPath: import path → KindModule node ID. Cached so the
//     stdlib (and each dep module) materialises at most once per pass.
//   - knownNodeIDs: canonical external node IDs, avoiding retention of
//     go/types objects (and their package/type graphs) across the long apply.
//
// Statistics counters surface back through ExternalsResult so the caller
// can report nodes/edges added.
// externalUseIdentity is the string-only projection needed while applying one
// external compiler use. A pass interns it by canonical external node ID, so
// hundreds of thousands of Uses neither retain types.Object package graphs nor
// repeatedly allocate the same three resolver-stub targets.
type externalUseIdentity struct {
	name        string
	kind        graph.EdgeKind
	stubTargets [3]string
}

type externalsAttribution struct {
	g            graph.Store
	pkgByPath    map[string]*packages.Package
	moduleByPath map[string]string
	// extByObj is a projection-only accelerator. It is cleared together with
	// pkgByPath before graph apply, so types.Object never crosses that boundary.
	extByObj    map[types.Object]string
	useByNodeID map[string]*externalUseIdentity
	provider    string

	// repoPrefix is the owning repo's prefix, used to namespace stub
	// IDs (graph.StubID). Empty when the caller doesn't supply one
	// — in that case stub IDs are emitted in the legacy un-prefixed
	// form, which graph.IsStdlibStub / friends still recognise.
	repoPrefix string

	// edgeCandidates is replaced for each loaded package by EnrichRepo. The
	// SQLite backend fills it with predicate-shaped queries, so stub claiming
	// never issues GetOutEdges once per go/types use or retains a repository's
	// full adjacency in memory.
	edgeCandidates *graph.EdgeCandidateSet
	// knownNodeIDs is populated once, in one batched store lookup, before
	// processing Uses. It replaces the former GetNode call per unique external
	// symbol/module and is updated as this pass creates nodes.
	knownNodeIDs map[string]struct{}
	// Pending graph mutations are projected without store re-entry, then drained
	// in fixed pages after compiler state is released.
	pendingNodes           []*graph.Node
	pendingEdges           []*graph.Edge
	pendingDependencyEdges map[string]*graph.Edge
	pendingReindexes       []graph.EdgeReindex

	nodesAdded     int
	edgesAdded     int
	edgesUpgraded  int
	modulesLinked  int
	stdlibCreated  bool
	missingPkgInfo int
}

// newExternalsAttribution prepares externalsAttribution from the loaded
// roots. Walking pkg.Imports collects every dep — stdlib and module-cache
// alike — so resolveSymbol can find the owning *packages.Package for an
// arbitrary types.Object. depIndex (may be nil) seeds the map with the
// metadata-only closure (loadDepModuleIndex): under the export-data load the
// roots' Imports maps hold ID-only stubs with an empty PkgPath, so the walk
// alone would leave every dependency unclassifiable. Roots overlay the index
// entries — they carry the fuller (typed) package for their own path.
func newExternalsAttribution(g graph.Store, roots []*packages.Package, provider, repoPrefix string, depIndex map[string]*packages.Package) *externalsAttribution {
	pkgByPath := make(map[string]*packages.Package, len(depIndex))
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if p == nil || p.PkgPath == "" {
			return
		}
		if _, seen := pkgByPath[p.PkgPath]; seen {
			return
		}
		pkgByPath[p.PkgPath] = p
		for _, imp := range p.Imports {
			visit(imp)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	// Walked packages win (roots carry types; a NeedDeps closure carries the
	// full dep set); the metadata index only fills paths the walk never saw.
	for path, pkg := range depIndex {
		if _, seen := pkgByPath[path]; !seen {
			pkgByPath[path] = pkg
		}
	}
	return &externalsAttribution{
		g:            g,
		pkgByPath:    pkgByPath,
		moduleByPath: make(map[string]string),
		extByObj:     make(map[types.Object]string),
		useByNodeID:  make(map[string]*externalUseIdentity),
		provider:     provider,
		repoPrefix:   repoPrefix,
		knownNodeIDs: make(map[string]struct{}),
	}
}

// prefetchExistingNodes collects every external symbol/module ID this repo's
// Uses can touch and tests their existence in one batched store operation.
// Only IDs stay resident; decoded nodes are discarded immediately. keep, when
// non-nil, limits the walk to the uses it accepts (the ones the pass can
// project).
func (e *externalsAttribution) existingNodeIDs(pkgs []*packages.Package, objToNode map[types.Object]string, keep func(*ast.Ident) bool) []string {
	ids := make(map[string]struct{})
	for _, root := range pkgs {
		if root == nil || root.TypesInfo == nil {
			continue
		}
		for ident, obj := range root.TypesInfo.Uses {
			if obj == nil || obj.Pkg() == nil {
				continue
			}
			if keep != nil && !keep(ident) {
				continue
			}
			if _, internal := objToNode[obj]; internal {
				continue
			}
			pkg := e.pkgByPath[obj.Pkg().Path()]
			moduleID := externalModuleNodeID(pkg)
			if moduleID == "" || externalNodeKind(obj) == "" {
				continue
			}
			nodeID := externalNodeID(obj.Pkg().Path(), obj)
			if nodeID == "" {
				continue
			}
			ids[moduleID] = struct{}{}
			ids[nodeID] = struct{}{}
		}
	}
	batch := make([]string, 0, len(ids))
	for id := range ids {
		batch = append(batch, id)
	}
	return batch
}

func (e *externalsAttribution) prefetchExistingNodeIDs(batch []string) {
	if len(batch) == 0 {
		return
	}
	for id := range graph.LookupExistingNodeIDs(e.g, batch) {
		e.knownNodeIDs[id] = struct{}{}
	}
}

// resolveSymbol returns the graph node ID for an external go/types object,
// creating it (and the owning KindModule node, if not already present)
// on first sight. Returns "" when the object is unsuitable for
// externalisation: builtins / universe-scope (no Pkg), unsupported obj
// kinds, or a missing *packages.Package entry (rare — would mean a Use
// pointed at a package the loader didn't see).
//
// External symbols become first-class graph nodes so the call graph can
// reach into stdlib / module-cache without leaving stub-string targets
// behind. Each external symbol gains an EdgeDependsOnModule to its
// owning KindModule — i.e. stdlib/dep attribution.
func (e *externalsAttribution) resolveSymbol(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	if id, ok := e.extByObj[obj]; ok {
		return id
	}

	importPath := obj.Pkg().Path()
	if importPath == "" {
		return ""
	}
	pkg, ok := e.pkgByPath[importPath]
	if !ok {
		// Loader didn't see this package — happens when packages.Load
		// returns errors and the user-facing pkg list is partial. Bail
		// rather than synthesize a half-formed node.
		e.missingPkgInfo++
		return ""
	}
	moduleID := e.ensureModuleNode(pkg)
	if moduleID == "" {
		// pkg belongs to the indexed repo (Main module). Caller should
		// have routed obj through objToNode before falling back to
		// externals — return "" so the caller drops the use.
		return ""
	}

	kind := externalNodeKind(obj)
	if kind == "" {
		return ""
	}
	nodeID := externalNodeID(importPath, obj)
	if nodeID == "" {
		return ""
	}

	if _, exists := e.knownNodeIDs[nodeID]; !exists {
		e.pendingNodes = append(e.pendingNodes, buildExternalNode(nodeID, kind, importPath, moduleID, pkg, obj, e.provider))
		e.knownNodeIDs[nodeID] = struct{}{}
		e.nodesAdded++
	}

	// Schedule module attribution even when the canonical symbol already exists.
	// New symbols commit with this link in one AddBatch; a retry still repairs
	// legacy/partial state whose node exists without the exact dependency edge.
	// AddBatch is identity-idempotent, and drainPendingEdges filters edges already
	// present so result counters remain exact on ordinary warm runs.
	e.ensurePendingDependencyEdges()
	if _, scheduled := e.pendingDependencyEdges[nodeID]; !scheduled {
		edge := &graph.Edge{
			From:            nodeID,
			To:              moduleID,
			Kind:            graph.EdgeDependsOnModule,
			FilePath:        externalFilePath(importPath),
			Line:            0,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          graph.OriginLSPResolved,
			Meta: map[string]any{
				"semantic_source": e.provider,
			},
		}
		e.pendingEdges = append(e.pendingEdges, edge)
		e.pendingDependencyEdges[nodeID] = edge
		e.edgesAdded++
		e.modulesLinked++
	}
	e.extByObj[obj] = nodeID
	return nodeID
}

// releaseCompilerReferences ends the projection lifetime. The graph apply uses
// only interned string identities and pending graph mutations from this point.
func (e *externalsAttribution) releaseCompilerReferences() {
	e.extByObj = nil
	e.pkgByPath = nil
}

// projectedUse interns the compact, string-only facts needed by the graph
// apply. The cache key is the canonical external node ID, never types.Object,
// so it cannot pin dependency package scopes after compiler projection.
func (e *externalsAttribution) projectedUse(obj types.Object, nodeID string) *externalUseIdentity {
	if identity := e.useByNodeID[nodeID]; identity != nil {
		return identity
	}
	if obj == nil || obj.Pkg() == nil {
		return nil
	}
	identity := externalUseIdentityFor(e.repoPrefix, obj.Pkg().Path(), obj)
	if identity != nil {
		e.useByNodeID[nodeID] = identity
	}
	return identity
}

func (e *externalsAttribution) claimAndUpgradeProjectedStub(callerID string, identity *externalUseIdentity, newTarget string, line int) *graph.Edge {
	if identity == nil {
		return nil
	}
	if edge := e.claimByExactTargets(callerID, identity.stubTargets, identity.kind, newTarget); edge != nil {
		return edge
	}
	if edge := e.claimByLineAndNameKind(callerID, identity.name, identity.kind, newTarget, line); edge != nil {
		return edge
	}
	return nil
}

// claimByExactStub handles the canonical resolver-shaped targets. Pulled
// out so the fuzzy pass can layer on top.
func (e *externalsAttribution) claimByExactStub(callerID string, importPath string, obj types.Object, newTarget string) *graph.Edge {
	identity := externalUseIdentityFor(e.repoPrefix, importPath, obj)
	if identity == nil {
		return nil
	}
	return e.claimByExactTargets(callerID, identity.stubTargets, identity.kind, newTarget)
}

func (e *externalsAttribution) claimByExactTargets(callerID string, targets [3]string, kind graph.EdgeKind, newTarget string) *graph.Edge {
	if e.edgeCandidates == nil {
		return nil
	}
	for _, target := range targets {
		edge := e.edgeCandidates.EndpointKind(callerID, target, kind)
		if edge == nil {
			continue
		}
		oldTo := edge.To
		edge.To = newTarget
		semantic.ConfirmEdge(edge, e.provider)
		e.pendingReindexes = append(e.pendingReindexes, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
		e.edgeCandidates.Add(edge)
		e.edgesUpgraded++
		return edge
	}
	return nil
}

func (e *externalsAttribution) claimByLineAndNameKind(callerID, name string, expected graph.EdgeKind, newTarget string, line int) *graph.Edge {
	if line <= 0 || name == "" || e.edgeCandidates == nil {
		return nil
	}
	candidates := e.edgeCandidates.Site(callerID, line, expected)
	for _, edge := range candidates {
		if edge.Line != line {
			continue
		}
		if expected != "" && edge.Kind != expected {
			continue
		}
		if !isStubTarget(edge.To) {
			continue
		}
		if !stubTargetTrailingNameMatches(edge.To, name) {
			continue
		}
		oldTo := edge.To
		edge.To = newTarget
		semantic.ConfirmEdge(edge, e.provider)
		e.pendingReindexes = append(e.pendingReindexes, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
		e.edgeCandidates.Add(edge)
		e.edgesUpgraded++
		return edge
	}
	return nil
}

// wantedEdgeKind returns the EdgeKind goanalysis would emit for obj, used
// to scope the fuzzy claim so we don't accidentally rewrite an unrelated
// edge with a different semantic.
func wantedEdgeKind(obj types.Object) graph.EdgeKind {
	if obj == nil {
		return ""
	}
	switch obj.(type) {
	case *types.Func:
		return graph.EdgeCalls
	case *types.TypeName, *types.Var, *types.Const:
		return graph.EdgeReferences
	}
	return ""
}

// isStubTarget reports whether a target ID is one of the bookkeeping
// strings the resolver writes for unresolved or external lookups.
func isStubTarget(to string) bool {
	switch {
	case graph.IsUnresolvedTarget(to),
		strings.HasPrefix(to, "external::"),
		graph.IsStdlibStub(to),
		strings.HasPrefix(to, "dep::"):
		return true
	}
	return false
}

// stubTargetTrailingNameMatches reports whether the trailing name of a
// stub target (everything after the final `::` or after `*.`) equals
// `name`. The encodings include:
//
//	unresolved::FooBar              → trailing FooBar
//	unresolved::*.Method            → trailing Method
//	unresolved::extern::pkg::Sym    → trailing Sym
//	stdlib::pkg::Sym                → trailing Sym
//	dep::pkg::Sym                   → trailing Sym
//	external::pkg                   → trailing pkg (file-level imports)
func stubTargetTrailingNameMatches(to, name string) bool {
	trailing := to
	if idx := strings.LastIndex(trailing, "::"); idx >= 0 {
		trailing = trailing[idx+2:]
	}
	trailing = strings.TrimPrefix(trailing, "*.")
	return trailing == name
}

// ensureModuleNode finds (or creates) the KindModule node for pkg's owning
// module. Returns the module node ID, or "" when pkg belongs to the
// indexed repo's main module (no externalisation needed).
//
// The stdlib (pkg.Module == nil for stdlib) shares one synthetic
// "module::go:stdlib" node so callers can group stdlib references under
// a single edge target. Module-cache packages reuse any existing
// KindModule node materialised from go.mod by internal/modules and
// otherwise fall back to creating one — go/types is the source of
// truth when go.mod parsing missed the dep (e.g. tooling/test fixtures).
func (e *externalsAttribution) ensureModuleNode(pkg *packages.Package) string {
	if pkg == nil {
		return ""
	}
	importPath := pkg.PkgPath
	if id, ok := e.moduleByPath[importPath]; ok {
		return id
	}

	if pkg.Module == nil {
		// No module info → assume Go stdlib. The stdlib has no go.mod
		// entry; one shared module node covers every stdlib package.
		if !e.stdlibCreated {
			if _, exists := e.knownNodeIDs[stdlibModuleID]; !exists {
				e.pendingNodes = append(e.pendingNodes, &graph.Node{
					ID:       stdlibModuleID,
					Kind:     graph.KindModule,
					Name:     "stdlib",
					FilePath: externalFilePath("std"),
					Language: "go",
					Meta: map[string]any{
						"ecosystem":       "go",
						"path":            modulePathStdlib,
						"version":         "",
						"module_kind":     "stdlib",
						"semantic_source": e.provider,
					},
				})
				e.knownNodeIDs[stdlibModuleID] = struct{}{}
				e.nodesAdded++
			}
			e.stdlibCreated = true
		}
		e.moduleByPath[importPath] = stdlibModuleID
		return stdlibModuleID
	}

	if pkg.Module.Main {
		// The indexed repo itself — caller should treat this obj as
		// internal and look it up in objToNode instead.
		e.moduleByPath[importPath] = ""
		return ""
	}

	mod := pkg.Module
	if mod.Replace != nil {
		mod = mod.Replace
	}
	moduleID := goModuleNodeID(mod.Path, mod.Version)
	if _, exists := e.knownNodeIDs[moduleID]; !exists {
		meta := map[string]any{
			"ecosystem":       "go",
			"path":            mod.Path,
			"version":         mod.Version,
			"module_kind":     "module_cache",
			"indirect":        mod.Indirect,
			"semantic_source": e.provider,
		}
		if pkg.Module.Replace != nil {
			meta["replace"] = mod.Path + "@" + mod.Version
			meta["replaced_path"] = pkg.Module.Path
		}
		e.pendingNodes = append(e.pendingNodes, &graph.Node{
			ID:       moduleID,
			Kind:     graph.KindModule,
			Name:     shortModulePath(mod.Path),
			FilePath: externalFilePath(mod.Path),
			Language: "go",
			Meta:     meta,
		})
		e.knownNodeIDs[moduleID] = struct{}{}
		e.nodesAdded++
	}
	e.moduleByPath[importPath] = moduleID
	return moduleID
}

func externalModuleNodeID(pkg *packages.Package) string {
	if pkg == nil {
		return ""
	}
	if pkg.Module == nil {
		return stdlibModuleID
	}
	if pkg.Module.Main {
		return ""
	}
	mod := pkg.Module
	if mod.Replace != nil {
		mod = mod.Replace
	}
	return goModuleNodeID(mod.Path, mod.Version)
}

// drainPendingAdds performs the final missing-node recheck immediately before
// the caller's AddBatch. enrichRepoContext holds Store.ResolveMutex across this
// recheck and apply, so a canonical module/external node created after the
// initial prefetch cannot be rewritten by a stale synthetic identity. Edges are
// deliberately retained: the reused node may still need this pass's module or
// call/reference edges.
func (e *externalsAttribution) drainPendingAdds() (nodes []*graph.Node, edges []*graph.Edge) {
	nodes = e.drainPendingNodes(0)
	edges = e.drainPendingEdgesForNodes(nodes)
	edges = append(edges, e.drainPendingEdges(0)...)
	return nodes, edges
}

// drainPendingNodes returns at most limit nodes (all when limit <= 0), with the
// final store recheck applied to that page only. Copying and clearing the source
// slots prevents a suffix slice from keeping already-applied node pointers live.
func (e *externalsAttribution) drainPendingNodes(limit int) []*graph.Node {
	take := len(e.pendingNodes)
	if limit > 0 && take > limit {
		take = limit
	}
	if take == 0 {
		return nil
	}
	nodes := append([]*graph.Node(nil), e.pendingNodes[:take]...)
	clear(e.pendingNodes[:take])
	e.pendingNodes = e.pendingNodes[take:]
	if len(e.pendingNodes) == 0 {
		e.pendingNodes = nil
	}
	if e.g == nil {
		return nodes
	}

	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	if len(ids) == 0 {
		return nodes
	}
	existing := graph.LookupExistingNodeIDs(e.g, ids)
	if len(existing) == 0 {
		return nodes
	}

	kept := nodes[:0]
	reused := 0
	for _, node := range nodes {
		if node != nil {
			if _, ok := existing[node.ID]; ok {
				reused++
				continue
			}
		}
		kept = append(kept, node)
	}
	clear(nodes[len(kept):])
	e.nodesAdded -= reused
	return kept
}

// drainPendingEdgesForNodes pairs each genuinely new external symbol with its
// module edge in the same AddBatch transaction. Module nodes are projected
// before their symbols, so the target is already committed or in this page.
func (e *externalsAttribution) drainPendingEdgesForNodes(nodes []*graph.Node) []*graph.Edge {
	e.ensurePendingDependencyEdges()
	edges := make([]*graph.Edge, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if edge := e.pendingDependencyEdges[node.ID]; edge != nil {
			edges = append(edges, edge)
			delete(e.pendingDependencyEdges, node.ID)
		}
	}
	return edges
}

func (e *externalsAttribution) ensurePendingDependencyEdges() {
	if e.pendingDependencyEdges != nil {
		return
	}
	e.pendingDependencyEdges = make(map[string]*graph.Edge, len(e.pendingEdges))
	for _, edge := range e.pendingEdges {
		if edge != nil {
			e.pendingDependencyEdges[edge.From] = edge
		}
	}
}

// drainPendingEdges returns remaining retry/legacy repair links. Full logical
// identities are checked in one batch because SQLite uniqueness includes
// FilePath and Line in addition to endpoints and kind.
func (e *externalsAttribution) drainPendingEdges(limit int) []*graph.Edge {
	e.ensurePendingDependencyEdges()
	defer func() {
		if len(e.pendingEdges) == 0 && len(e.pendingDependencyEdges) == 0 {
			e.pendingDependencyEdges = nil
		}
	}()
	take := len(e.pendingEdges)
	if limit > 0 && take > limit {
		take = limit
	}
	if take == 0 {
		return nil
	}
	raw := append([]*graph.Edge(nil), e.pendingEdges[:take]...)
	clear(e.pendingEdges[:take])
	e.pendingEdges = e.pendingEdges[take:]
	if len(e.pendingEdges) == 0 {
		e.pendingEdges = nil
	}

	edges := raw[:0]
	for _, edge := range raw {
		if edge == nil || e.pendingDependencyEdges[edge.From] != edge {
			continue
		}
		delete(e.pendingDependencyEdges, edge.From)
		edges = append(edges, edge)
	}
	clear(raw[len(edges):])
	if len(edges) == 0 || e.g == nil {
		return edges
	}

	identities := make([]graph.EdgeIdentity, 0, len(edges))
	for _, edge := range edges {
		identities = append(identities, graph.EdgeIdentityFor(edge))
	}
	finder, ok := e.g.(graph.EdgeIdentityBatchFinder)
	if !ok {
		return edges
	}
	existing := finder.FindEdgesByIdentities(identities)
	kept := edges[:0]
	reused := 0
	for _, edge := range edges {
		if existing[graph.EdgeIdentityFor(edge)] != nil {
			reused++
			continue
		}
		kept = append(kept, edge)
	}
	clear(edges[len(kept):])
	e.edgesAdded -= reused
	e.modulesLinked -= reused
	return kept
}

func (e *externalsAttribution) drainPendingReindexes() []graph.EdgeReindex {
	reindexes := e.pendingReindexes
	e.pendingReindexes = nil
	return reindexes
}

// stubEdgeTargets enumerates every stub-string the resolver might have
// written for an external obj. Order matches resolver precedence:
// stdlib::/dep:: are produced post-resolve, unresolved::extern:: is the
// raw form when resolveExtern wasn't run.
//
// repoPrefix namespaces the stdlib stub form per-repo so two repos
// pinned to different Go SDK versions don't collide on a single
// `stdlib::fmt::Errorf` node. An empty repoPrefix yields the legacy
// un-prefixed form, which the resolver still emits today.
func stubEdgeTargets(repoPrefix, importPath string, obj types.Object) []string {
	identity := externalUseIdentityFor(repoPrefix, importPath, obj)
	if identity == nil {
		return nil
	}
	return identity.stubTargets[:]
}

func externalUseIdentityFor(repoPrefix, importPath string, obj types.Object) *externalUseIdentity {
	if obj == nil || importPath == "" || obj.Name() == "" {
		return nil
	}
	name := obj.Name()
	return &externalUseIdentity{
		name: name,
		kind: wantedEdgeKind(obj),
		stubTargets: [3]string{
			graph.StubID(repoPrefix, graph.StubKindStdlib, importPath, name),
			"dep::" + importPath + "::" + name,
			"unresolved::extern::" + importPath + "::" + name,
		},
	}
}

// buildExternalNode constructs a graph node for an external symbol. The
// FilePath is a synthetic "external::go:<importPath>" string so byFile
// lookups don't pollute real source-file buckets.
func buildExternalNode(nodeID string, kind graph.NodeKind, importPath, moduleID string, pkg *packages.Package, obj types.Object, provider string) *graph.Node {
	moduleKind, version := classifyPackage(pkg)
	meta := map[string]any{
		"external":        true,
		"import_path":     importPath,
		"module_path":     modulePathOf(pkg),
		"module_kind":     moduleKind,
		"module_id":       moduleID,
		"semantic_source": provider,
	}
	if version != "" {
		meta["version"] = version
	}
	if sig := types.ObjectString(obj, nil); sig != "" {
		meta["signature"] = sig
	}
	if typeStr := types.TypeString(obj.Type(), nil); typeStr != "" && typeStr != "invalid type" {
		meta["semantic_type"] = typeStr
	}
	if recv := receiverTypeName(obj); recv != "" {
		meta["receiver"] = recv
	}
	qualName := importPath + "." + obj.Name()
	if recv := receiverTypeName(obj); recv != "" {
		qualName = importPath + "." + recv + "." + obj.Name()
	}
	return &graph.Node{
		ID:       nodeID,
		Kind:     kind,
		Name:     obj.Name(),
		QualName: qualName,
		FilePath: externalFilePath(importPath),
		Language: "go",
		Meta:     meta,
	}
}

// classifyPackage labels a loaded package as stdlib / module_cache / main.
// Returns (kind, version) — version is empty for stdlib and main.
func classifyPackage(pkg *packages.Package) (string, string) {
	if pkg == nil {
		return "", ""
	}
	if pkg.Module == nil {
		return "stdlib", ""
	}
	if pkg.Module.Main {
		return "main", ""
	}
	mod := pkg.Module
	if mod.Replace != nil {
		mod = mod.Replace
	}
	return "module_cache", mod.Version
}

// modulePathOf returns the owning module's path for pkg. Stdlib packages
// fall back to the package import path so callers can group by package
// (the stdlib has no module path proper); module-cache packages return
// the module path which may be a prefix of the import path.
func modulePathOf(pkg *packages.Package) string {
	if pkg == nil {
		return ""
	}
	if pkg.Module == nil {
		return pkg.PkgPath
	}
	mod := pkg.Module
	if mod.Replace != nil {
		mod = mod.Replace
	}
	return mod.Path
}

// receiverTypeName returns the bare type name of obj's receiver when obj
// is a method. Strips pointer wrappers — Go forbids both `func (T) M()`
// and `func (*T) M()` on the same type, so the bare name uniquely
// identifies the method's owner. Returns "" for non-methods.
func receiverTypeName(obj types.Object) string {
	fn, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

// externalNodeID returns the canonical ID for an external symbol node.
// Methods are disambiguated by receiver type so `os.File.Write` and a
// hypothetical `os.Foo.Write` would land on distinct nodes; pure
// functions / vars / consts / types use the package-qualified short
// name. Returns "" when name is empty.
func externalNodeID(importPath string, obj types.Object) string {
	name := obj.Name()
	if name == "" {
		return ""
	}
	if recv := receiverTypeName(obj); recv != "" {
		return "ext::go:" + importPath + "::" + recv + "." + name
	}
	return "ext::go:" + importPath + "::" + name
}

// externalNodeKind classifies a types.Object into the matching graph
// NodeKind. Funcs with a receiver become methods; type names with an
// interface underlying type become interfaces. Unsupported obj kinds
// (PkgName, Label, Builtin, Nil) return "" so resolveSymbol drops them.
func externalNodeKind(obj types.Object) graph.NodeKind {
	switch t := obj.(type) {
	case *types.Func:
		if sig, ok := t.Type().(*types.Signature); ok && sig.Recv() != nil {
			return graph.KindMethod
		}
		return graph.KindFunction
	case *types.TypeName:
		if t.Type() == nil {
			return graph.KindType
		}
		if _, isIface := t.Type().Underlying().(*types.Interface); isIface {
			return graph.KindInterface
		}
		return graph.KindType
	case *types.Var:
		return graph.KindVariable
	case *types.Const:
		return graph.KindConstant
	default:
		return ""
	}
}

// externalFilePath returns the synthetic file-path used for external
// nodes. Kept consistent so byFile lookups don't pollute real file
// buckets.
func externalFilePath(importPath string) string {
	if importPath == "" {
		return "external::go"
	}
	return "external::go:" + importPath
}

// goModuleNodeID returns the canonical KindModule node ID for a Go
// module. Mirrors internal/modules/scanner.go::ModuleNodeID without
// importing scanner — that import would create graph→modules→graph
// cycle when scanner already imports graph.
func goModuleNodeID(path, version string) string {
	id := "module::go:" + path
	if version != "" {
		id += "@" + version
	}
	return id
}

// shortModulePath returns the last meaningful segment of a module path,
// stripping the /vN major-version suffix so `github.com/foo/bar/v2`
// surfaces as `bar` not `v2`. Mirrors internal/modules/scanner.go::shortName.
func shortModulePath(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if len(parts) >= 2 && isMajorVersion(last) {
		last = parts[len(parts)-2]
	}
	return last
}

// isMajorVersion matches "v2", "v3", "v10" etc. — the Go-modules SemVer
// major-version suffix that lives at the end of a module path.
func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
