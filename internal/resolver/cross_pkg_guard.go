package resolver

import (
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Cross-package name-match guard.
//
// The heuristic cascade in resolveFunctionCall / resolveMethodCall ends,
// for calls it can't pin precisely, in a name-only fallback: "the first
// function/method named X in the caller's repo". When the only candidate
// of that name lives in a package the caller never imports, that
// fallback manufactures a false `calls` edge — a JS/TS factory result
// `h.handle()` binding to an unrelated `handle`, or a `ns.foo()`
// namespace call binding to a free `foo` in some other module.
//
// This guard runs once after the main resolution pass. For every edge
// the pass resolved at one of the two weakest confidence tiers
// (text_matched / ast_inferred) it asks a single question: is the
// resolved target import-reachable from the call site? Reachable means
// the target sits in the caller's own directory (same package) or in a
// directory the caller's file imports. When it is not, the edge is
// reverted to its pre-resolution `unresolved::` target so a
// higher-evidence resolver (CrossRepoResolver, or a later LSP-backed
// pass) can have a clean attempt instead of inheriting a wrong binding.
//
// Genuine same-package and imported-target edges are never touched: the
// reachability set always contains the caller's own directory, and an
// imported package contributes its directory to the set. Edges resolved
// at ast_resolved or above are out of scope — those carry structural or
// compiler-grade evidence the name-only fallback never had.

// guardCrossPackageCallEdges inspects the edges mutated by the just-
// completed resolution pass and reverts any weak-tier call/reference
// edge whose resolved target is not import-reachable from the caller.
// jobs are the reindexJob records produced by ResolveAll's worker
// phase; each carries the edge's pre-resolution target in oldTo, so a
// reverted edge is restored exactly. closure is the import-reachability
// map from buildImportClosure. Returns the number of edges reverted.
func (r *Resolver) guardCrossPackageCallEdges(jobs []reindexJob, closure map[string]map[string]struct{}) int {
	if len(jobs) == 0 {
		return 0
	}
	var liveJobs resolveJobLiveness
	if r.validateLiveness {
		edges := make([]*graph.Edge, 0, len(jobs))
		for i := range jobs {
			edges = append(edges, jobs[i].edge)
		}
		liveJobs = loadEdgeLiveness(r.graph, edges)
	}
	// Collect both mutation lists across the whole pass and apply them
	// via the batched Store methods at the end. Per-edge
	// SetEdgeProvenance + ReindexEdge in the body would otherwise pay
	// two ACID round-trips per reverted edge against disk backends —
	// catastrophic on a 30k-job pass.
	var provBatch []graph.EdgeProvenanceUpdate
	var reindexBatch []graph.EdgeReindex
	var spoolReverts []lspSpoolRevert
	for i := range jobs {
		j := &jobs[i]
		// A concurrent edit during a chunked ResolveAll yield may have evicted
		// this edge since it resolved; reverting + reindexing it would
		// half-resurrect it. Skip — it is no longer in the graph.
		if r.validateLiveness && !liveJobs.containsEdge(j.edge) {
			continue
		}
		// The deferred LSP batch may have re-bound (or confirmed) this edge
		// after the heuristic job was recorded, stamping it OriginLSPResolved —
		// compiler-grade evidence the name-only fallback this guard polices
		// never had. j.origin still holds the stale heuristic tier, so trust
		// the live edge: never revert an LSP-owned binding. (The batch now
		// overrides confident heuristic binds, so a recorded job's target can
		// be LSP-owned; before, the batch only touched heuristic-unresolved
		// edges, disjoint from these jobs, and this never fired.)
		if j.edge.Origin == graph.OriginLSPResolved {
			continue
		}
		if !isCallLikeEdge(j.kind) {
			continue
		}
		// Only the two weakest tiers — a name-only guess — are in scope.
		// DefaultOriginFor backfills the tier for edges whose Origin the
		// resolver left unset (the heuristic fallbacks never stamp it).
		origin := j.origin
		if origin == "" {
			origin = graph.DefaultOriginFor(j.kind, j.confidence, "")
		}
		if origin != graph.OriginTextMatched && origin != graph.OriginASTInferred {
			continue
		}
		// The pre-resolution target must be a bare-name placeholder —
		// `unresolved::Foo` (function call) or `unresolved::*.foo`
		// (member call). Anything else carries evidence the name-only
		// fallback never had and is out of scope: `extern::` pins an
		// import path, `grpc::` / `pyrel::` / `import::` are owned by
		// dedicated passes, and a non-`unresolved::` target was never a
		// guess to begin with.
		if !isBareNameCallTarget(j.oldTo) {
			continue
		}
		callerFile := r.edgeCallerFile(j.edge)
		callerNode := r.cachedGetNode(j.edge.From)
		target := r.cachedGetNode(j.newTo)
		if callerFile == "" || target == nil {
			continue
		}
		if r.targetImportReachable(callerFile, callerNode, target, closure) {
			continue
		}
		// A member call whose only in-repo definition of the name is this
		// target is not a cross-package mis-guess — there is nowhere else the
		// call could bind. A method call carries its receiver, so it needs no
		// import of the method's package, and inherited / indirectly-typed
		// receivers (owner.foo() → BaseType.foo two packages up) never name the
		// declaring package, so the import closure structurally misses them.
		// Keep the resolution.
		if r.loneMemberDefnKeep(target, j.edge, j.oldTo, callerFile) {
			continue
		}
		// A C# extension-method bind is corroborated by namespace
		// visibility, not imports: the extension enters scope via the
		// caller's enclosing namespace or a project-scoped `global using`
		// declared in a sibling file (or `using static` of the declaring
		// class) — none of which leaves an import edge on the calling
		// file for the reachability closure to see. Re-check the same
		// visibility evidence the bind used and keep it when it holds.
		if r.csharpExtensionGuardKeep(j.edge, callerFile, target) {
			continue
		}
		// Same shape for a receiverless call bound through `using static`:
		// the directive is the scope evidence, never an import edge.
		if r.csharpUsingStaticGuardKeep(j.edge, callerFile, target) {
			continue
		}
		// Not reachable — revert to the unresolved placeholder and
		// re-index against the resolved target we are abandoning.
		// SetEdgeProvenance("") drops the resolution provenance so
		// the reverted edge's identity change is counted; the target
		// revert + re-bucket follows. Both go in their respective
		// batches so the whole pass commits in two chunks instead of
		// 2×N per-edge transactions.
		oldResolved := j.edge.To
		provBatch = append(provBatch, graph.EdgeProvenanceUpdate{Edge: j.edge, NewOrigin: ""})
		j.edge.To = j.oldTo
		j.edge.Confidence = 0
		// The label travels with the confidence — agents act on it, and an
		// unresolved edge advertising the abandoned bind's EXTRACTED label
		// would out-rank evidence that actually survived.
		j.edge.ConfidenceLabel = ""
		// Durable revert stamp: the guard's verdicts are otherwise invisible
		// after the provenance drop, which blocks any selective replay — a
		// future pass cannot learn which speculative shapes never survive
		// scrutiny. The stamp rides the reindex write it already pays for.
		if j.edge.Meta == nil {
			j.edge.Meta = map[string]any{}
		}
		j.edge.Meta["guard_reverted"] = true
		// The abandoned bind's provenance must not shadow a later
		// rebind — the receiver gate exempts resolution shapes like
		// extension_method, and a stale tag would carry that exemption
		// to whatever the edge binds to next.
		delete(j.edge.Meta, "resolution")
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: j.edge, OldTo: oldResolved})
		spoolReverts = append(spoolReverts, lspSpoolRevert{edge: j.edge, oldBoundTo: oldResolved})
	}
	if len(provBatch) > 0 {
		r.graph.SetEdgeProvenanceBatch(provBatch)
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
	// A reverted edge may still hold a deferred LSP verify-record spooled
	// while it was heuristically bound. Re-snapshot those records to the
	// post-revert edge state (after both batches, so the struct fields are
	// authoritative on either backend) — otherwise the next pass's exact
	// liveness matching declares them stale and deletes the queued retry.
	if len(spoolReverts) > 0 && r.lspDeferredSpool != nil {
		if err := r.lspDeferredSpool.refreshRevertedEdges(spoolReverts); err != nil {
			r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
		}
	}
	return len(reindexBatch)
}

// isBareNameCallTarget reports whether an unresolved edge target is a
// bare-name call placeholder — `unresolved::Foo` for a free-function
// call or `unresolved::*.foo` for a member call. These are the only
// shapes the name-only resolution fallback acts on. Targets that embed
// further structure (`unresolved::extern::path::sym`, `grpc::`,
// `pyrel::`, `import::`) carry evidence the fallback never had and are
// resolved by other code paths, so the guard leaves them alone.
func isBareNameCallTarget(target string) bool {
	rest, ok := strings.CutPrefix(target, unresolvedPrefix)
	if !ok || rest == "" {
		return false
	}
	rest = strings.TrimPrefix(rest, "*.")
	if rest == "" {
		return false
	}
	// A remaining `::` means the placeholder is one of the structured
	// forms (extern::, grpc::, pyrel::, import::), not a bare name.
	return !strings.Contains(rest, "::")
}

// isCallLikeEdge reports whether an edge kind is one the guard polices.
// EdgeCalls is the obvious case; EdgeReferences is included because the
// resolver promotes a call-shaped EdgeReads to EdgeReferences once it
// learns the target is a function/method, and that promotion runs
// through the very same name-only fallback.
func isCallLikeEdge(k graph.EdgeKind) bool {
	return k == graph.EdgeCalls || k == graph.EdgeReferences
}

// edgeCallerFile returns the file path of the node that owns the edge's
// From end. Empty when the caller node is unknown.
//
// Hot path: called once per cross-package-guarded edge. The pre-warmed
// per-pass cache populated in ResolveAll holds every From ID across the
// pending slice, so this call is a map lookup during a ResolveAll pass
// and a direct store call elsewhere.
func (r *Resolver) edgeCallerFile(e *graph.Edge) string {
	if n := r.cachedGetNode(e.From); n != nil && n.FilePath != "" {
		return n.FilePath
	}
	return e.FilePath
}

// targetImportReachable reports whether target sits in a package the
// caller's file can see: the caller's own directory (same package), or
// a directory present in the caller's import closure.
func (r *Resolver) targetImportReachable(callerFile string, callerNode, target *graph.Node, closure map[string]map[string]struct{}) bool {
	if target.FilePath == "" {
		// A target with no file (synthetic / external stub) can't be
		// shown unreachable — leave the edge alone.
		return true
	}
	callerDir := filePathDir(callerFile)
	targetDir := filePathDir(target.FilePath)
	if targetDir == callerDir {
		return true
	}
	// Same source package across different directories is reachable without
	// an import edge. Maven splits one package across src/main/java and
	// src/test/java, and JVM same-package callers import nothing — so a
	// directory-only closure reports a false "unreachable" for every
	// test→production same-package call. scope_pkg is stamped only on JVM
	// member nodes, so this never fires for directory-scoped ecosystems.
	if sameScopePackage(callerNode, target) {
		return true
	}
	dirs, ok := closure[callerFile]
	if !ok {
		// No closure entry for the caller (its file node or imports were
		// not indexed). Be conservative: without evidence of isolation
		// we keep the edge rather than risk dropping a real one.
		return true
	}
	_, reachable := dirs[targetDir]
	return reachable
}

// scopePkgOf returns a node's stamped source package (scope_pkg Meta),
// empty when absent. Only JVM extractors (Java / Kotlin) stamp it.
func scopePkgOf(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	if p, ok := n.Meta["scope_pkg"].(string); ok {
		return p
	}
	return ""
}

// sameScopePackage reports whether two nodes belong to the same source
// package of the same language. Empty package on either side is never a
// match, so directory-scoped ecosystems (no scope_pkg) never qualify.
func sameScopePackage(a, b *graph.Node) bool {
	if a == nil || b == nil {
		return false
	}
	pa := scopePkgOf(a)
	if pa == "" {
		return false
	}
	return pa == scopePkgOf(b) && a.Language == b.Language
}

// loneMemberDefnKeep reports whether a to-be-reverted member-call edge should
// survive the cross-package guard because its target is the sole in-repo
// definition of the method name. A name with exactly one candidate cannot be a
// cross-package mis-guess: a method call carries its receiver, so it needs no
// import of the method's package, and the import closure structurally misses
// inherited / indirectly-typed receivers (owner.foo() where owner came from a
// return value the caller's file never imports the type of). Restricted to the
// languages listed in loneMemberLang, and gated on the receiver, when known,
// naming an in-repo type so an external-typed receiver (a logging facade's
// `logger.info`) still reverts rather than latching onto an unrelated
// same-named local method. C# demands one more corroboration: the
// candidate's namespace must be visible from the calling file — see the
// BCL-shadowing note in the body.
func (r *Resolver) loneMemberDefnKeep(target *graph.Node, e *graph.Edge, oldTo, callerFile string) bool {
	if target == nil || !loneMemberLang(target.Language) {
		return false
	}
	bareName := graph.UnresolvedName(oldTo)
	memberCall := strings.HasPrefix(bareName, "*.")
	// Go has free functions that DO need their package imported, so only a
	// member call (`x.foo()` — the receiver carries the type, no import of the
	// method's package needed) is kept; a bare free-function call to an
	// un-imported package must still revert. Java has no free functions, so its
	// bare calls are static-member dispatch and keep too.
	if target.Language != "java" && !memberCall {
		return false
	}
	name := strings.TrimPrefix(bareName, "*.")
	if name == "" {
		return false
	}
	repo := r.callerRepoPrefix(e)
	if rt := edgeReceiverType(e); rt != "" && !r.hasInRepoType(rt, repo) {
		return false
	}
	// C#: a lone definition is no corroboration when the name shadows a
	// BCL member (`Where`, `Select`, `Parse`) — the true target is
	// external, so the sole indexed homonym is a decoy for every call the
	// tenv could not type. Demand the candidate's namespace be visible
	// from the calling file. Narrowing-only, like the extension-
	// visibility policy: a file with NO recorded usings is stale or
	// partial data, not evidence of absence — the keep is lost only when
	// using evidence exists and the namespace is not in it (nor an
	// enclosing one). For the member shapes that can bind cross-namespace
	// without the file naming the type — extensions and class-qualified
	// statics — visibility is the language rule, not a heuristic; for
	// var-typed instance receivers it trades a rare silent miss for an
	// honest unresolved.
	if target.Language == "csharp" {
		if ns, _ := target.Meta["scope_ns"].(string); ns != "" {
			visible := r.csharpFileNamespaceSet(callerFile)
			_, enc := visible.enclosing[ns]
			_, imp := visible.imported[ns]
			if len(visible.imported) > 0 && !enc && !imp {
				return false
			}
		}
	}
	n := 0
	for _, c := range r.cachedFindNodesByNameInRepo(name, repo) {
		if c.Language != target.Language {
			continue
		}
		// A member call can only bind to a method; count functions too only
		// for Java's static-member model (its bare calls).
		if c.Kind == graph.KindMethod || (target.Language == "java" && c.Kind == graph.KindFunction) {
			if n++; n > 1 {
				return false
			}
		}
	}
	return n == 1
}

// loneMemberLang reports whether a lone in-repo method definition is safe to
// keep against the cross-package guard for the given language.
//
// The bar is not static typing as such — loneMemberDefnKeep already excludes
// bare free-function calls and receivers typed as something the repo does not
// define, and only fires when the name has exactly ONE in-repo method
// definition, which is the case duck typing cannot confuse. The bar is whether
// a class-based member call is the language's dispatch model. PHP qualifies:
// its calls carry a receiver, and since PHP call sites stamp receiver_type the
// external-receiver gate above screens them more effectively than it does for
// most of this list. GDScript qualifies for the same reason and needs it more
// than any of them: a script IS a class, `class_name` registers it
// project-globally, and the language has no import statement at all — so the
// import closure this guard consults is structurally empty for every
// cross-directory call and would revert all of them. TS / Python / JS stay
// out — an object literal or a dynamically attached method makes a same-name
// coincidence likelier there, and their guard revert is load-bearing.
func loneMemberLang(lang string) bool {
	switch lang {
	case "java", "go", "rust", "csharp", "kotlin", "scala", "php", "gdscript":
		return true
	}
	return false
}

// hasInRepoType reports whether the repo defines a type/interface named
// typeName — the gate that keeps javaLoneMemberDefnKeep from latching a
// call on an external-typed receiver onto an unrelated in-repo method.
func (r *Resolver) hasInRepoType(typeName, repo string) bool {
	for _, c := range r.cachedFindNodesByNameInRepo(typeName, repo) {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			return true
		}
	}
	return false
}

// buildImportClosure maps each caller file path to the set of directories
// it can reach by import. The set is seeded with the file's own directory
// and extended with the directory of every node its resolved EdgeImports
// edges point at. It is built from the post-resolution graph — by the
// time the guard runs, import edges have been resolved to real file /
// package nodes, so this closure captures JS/TS relative-file imports
// that the pre-resolution reachability index (keyed on directory-shaped
// import paths) structurally misses.
func (r *Resolver) buildImportClosure() map[string]map[string]struct{} {
	return r.buildImportClosureFiltered(nil)
}

// buildImportClosureFiltered is buildImportClosure restricted to a set of repo
// prefixes: it seeds the closure only for files owned by those repos and only
// walks import edges whose caller sits in one of them. Each import edge
// contributes solely to its own caller's closure entry, so a caller in the set
// gets the same reachable-dir set it would in the whole-graph build — the guard
// queries the closure only for those callers, so its verdicts are unchanged.
// Re-export edges stay unfiltered: a caller in the set may import a barrel that
// re-exports from a repo outside it, and the transitive barrel walk must still
// reach it. A nil repos set builds the whole-graph closure.
func (r *Resolver) buildImportClosureFiltered(repos map[string]struct{}) map[string]map[string]struct{} {
	inScope := func(id string) bool {
		if repos == nil {
			return true
		}
		_, ok := repos[graph.RepoPrefixOfID(id)]
		return ok
	}
	closure := make(map[string]map[string]struct{})
	add := func(file, dir string) {
		if file == "" || dir == "" {
			return
		}
		set := closure[file]
		if set == nil {
			set = make(map[string]struct{})
			closure[file] = set
		}
		set[dir] = struct{}{}
	}
	var repoPrefixes []string
	if repos != nil {
		repoPrefixes = make([]string, 0, len(repos))
		for prefix := range repos {
			repoPrefixes = append(repoPrefixes, prefix)
		}
		sort.Strings(repoPrefixes)
	}
	for file := range graph.FileNodeIdentitiesSeq(r.graph, repoPrefixes) {
		if file.FilePath != "" && inScope(file.ID) {
			add(file.FilePath, filePathDir(file.FilePath))
		}
	}
	// Materialise metadata-free import projections and batch-load only the
	// endpoint placement fields needed by the closure. A per-edge point lookup
	// here would be a query round-trip per import on a disk backend.
	//
	// Re-export edges ride the same batch: an import that lands on a
	// barrel (`import { persist } from 'zustand/middleware'` resolving to
	// src/middleware.ts, which `export { persist } from
	// './middleware/persist.ts'`) must make the re-exported module's
	// directory reachable too — the consumer names the barrel, but the
	// symbol it calls lives behind the re-export hop. Without this, the
	// guard reverts every legitimate barrel-mediated call as
	// "not import-reachable".
	skipTarget := func(to string) bool {
		return strings.HasPrefix(to, unresolvedPrefix) ||
			strings.HasPrefix(to, "external::") ||
			graph.IsStdlibStub(to) ||
			strings.HasPrefix(to, "dep::")
	}
	var imports, reexports []*graph.Edge
	ids := make(map[string]struct{})
	collect := func(e *graph.Edge) {
		if e.From != "" {
			ids[e.From] = struct{}{}
		}
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeImports) {
		// Skip imports still pointing at an unresolved placeholder or an
		// out-of-repo stub — neither names an in-repo directory that a
		// name-only call candidate could legitimately live in.
		if skipTarget(e.To) {
			continue
		}
		// An import edge only extends its own caller's closure entry, so on a
		// scoped build we need just the edges whose caller is in scope.
		if !inScope(e.From) {
			continue
		}
		imports = append(imports, e)
		collect(e)
	}
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeReExports) {
		if skipTarget(e.To) {
			continue
		}
		reexports = append(reexports, e)
		collect(e)
	}
	if len(imports) == 0 {
		return closure
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	placements := graph.NodePlacementsByIDs(r.graph, idList)

	// Direct barrel-file → re-export-target-file map, then a memoised
	// transitive walk so chained barrels (src/index.ts → src/middleware.ts
	// → src/middleware/persist.ts) contribute every hop's directory.
	reexpTargets := make(map[string][]string)
	for _, e := range reexports {
		barrel := e.FilePath
		if from, ok := placements[e.From]; ok && from.FilePath != "" {
			barrel = from.FilePath
		}
		if to, ok := placements[e.To]; ok && to.FilePath != "" && barrel != "" {
			reexpTargets[barrel] = append(reexpTargets[barrel], to.FilePath)
		}
	}
	barrelDirCache := make(map[string][]string)
	var barrelDirs func(file string, seen map[string]bool) []string
	barrelDirs = func(file string, seen map[string]bool) []string {
		if dirs, ok := barrelDirCache[file]; ok {
			return dirs
		}
		if seen[file] {
			return nil
		}
		seen[file] = true
		var dirs []string
		for _, tf := range reexpTargets[file] {
			dirs = append(dirs, filePathDir(tf))
			dirs = append(dirs, barrelDirs(tf, seen)...)
		}
		barrelDirCache[file] = dirs
		return dirs
	}

	for _, e := range imports {
		callerFile := e.FilePath
		if from, ok := placements[e.From]; ok && from.FilePath != "" {
			callerFile = from.FilePath
		}
		if target, ok := placements[e.To]; ok && target.FilePath != "" {
			add(callerFile, filePathDir(target.FilePath))
			for _, d := range barrelDirs(target.FilePath, map[string]bool{}) {
				add(callerFile, d)
			}
		}
	}
	return closure
}
