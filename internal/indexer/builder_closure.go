package indexer

import (
	"context"
	"io"
	"path"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// The affected closure of a change.
//
// A derived handle reads only what its own generation carries, with no fall
// through to the layer below. Anything the pass must see has to be in the
// generation, and "what must it see" is answered in both directions around the
// changed files:
//
//   - Backwards, the dependents: a file whose resolved references point INTO a
//     changed file holds edges and reference facts derived against the shape
//     the change is about to alter. It is the same frontier the incremental
//     pipeline re-resolves after a signature change
//     (semanticDependencyFrontierForDeletedFiles walks it for deletions), read
//     here from the base layer before anything is written.
//   - Forwards, the dependencies: a file a file in the closure resolves INTO
//     holds the definitions its own references bind to. Without them the pass
//     would re-derive that file against an empty world and park every
//     cross-file reference on an unresolved stub — a difference from a whole
//     index of the same tree, not a saving.
//
// Both directions are asked twice, of two different sources, because neither
// alone can answer them:
//
//   - Of the BASE layer, for a file whose content the change does not touch.
//     Its resolved edges and its durable reference facts already say what it
//     binds to and what binds to it, and they cannot have gone stale, because
//     the file is the same file. Both are read and unioned: the sidecar
//     survives evictions the live edges do not, and the live edges cover what
//     the sidecar has not been asked to record.
//   - Of the TARGET content, for a file the change writes. The base layer
//     describes the state the change replaces, so what the change INTRODUCES
//     has no base edge to walk — and an added file has no base nodes at all.
//     The changed and added files are therefore extracted from the target state
//     and read for both vocabularies. Their unresolved references are placed on
//     the base corpus by name and by import path, which is what carries a new
//     call's callee, a new parameter's type and the package behind a new import
//     into the generation. Their DEFINITIONS are placed the mirrored way: a
//     name the change defines is looked up as the placeholder identity that
//     unbound references park on, and the files holding those references join
//     the closure, because a whole index of the target binds them to the new
//     definition and a generation that never re-derives them would not.
//
// The walk iterates to a FIXED POINT. Every file it admits is asked the same
// forward question, so a closure file's own cross-file references bind in the
// generation exactly as they would in a whole index of the same tree, rather
// than parking on a stub one hop past the change. The reverse direction stays
// at ONE hop, and that is not a bound but a fact about the walk: a file the
// closure pulls in is re-derived from unchanged content, so it re-derives to
// the same identities, and its own dependents' edges into it still name
// something live.
//
// The one thing that IS bounded is the size of the result. Iterating a
// dependency graph to a fixed point is the whole repository in the limit, which
// is the thing a sparse generation exists to avoid, so the walk stops at
// ClosureCap files and says so: ClosureTruncated rides on the report and the
// build narrows the resolution and incoming-edge producer states. A truncated
// closure is a knowingly incomplete generation, published as one, rather than a
// silent divergence.
//
// Module manifests are not discovered — they join unconditionally when the
// target holds them. A manifest states the repository's own module identity and
// its dependency set, and a pass that cannot read one cannot tell a
// module-local import from an external module: it classifies the package
// stdlib or external and mints repo-scoped stubs, whose ids carry no path and
// which therefore no file mask can ever replace. Reading a handful of root
// files is cheaper than any rule that would have to undo that.

// builderClosureCap bounds the closure fan-out. It reuses the incremental
// pipeline's affected-by cap: both answer the same question — how many files
// may one change drag into a re-resolve before the bounded pass stops being
// bounded — and a repository that tuned one has tuned the other.
func (b *SparseGenerationBuilder) builderClosureCap() int {
	if n := b.Config.AffectedByReresolveMax; n > 0 {
		return n
	}
	return defaultAffectedByMax
}

func (b *SparseGenerationBuilder) affectedClosureContext(
	ctx context.Context,
	req BuildRequest,
	present, deleted map[string]struct{},
	report *BuildReport,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := b.builderClosureCap()
	report.ClosureCap = limit

	seeds := make([]string, 0, len(present)+len(deleted))
	for p := range present {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, p))
	}
	for p := range deleted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, p))
	}
	if len(seeds) == 0 {
		return nil, nil
	}
	sort.Strings(seeds)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	walk := &closureWalk{
		b:       b,
		req:     req,
		limit:   limit,
		deleted: deleted,
		seeds:   make(map[string]struct{}, len(seeds)),
		chosen:  make(map[string]struct{}),
	}
	for _, graphPath := range seeds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		walk.seeds[graphPath] = struct{}{}
	}

	// The manifests are offered on their own and first. They are the one part
	// of the closure that is not a discovery, and losing one to the cap would
	// cost the whole generation its module identity for the sake of whichever
	// source file happened to sort earlier.
	manifests := make(map[string]struct{})
	walk.collectManifests(manifests)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taken := walk.admitAll(manifests)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	frontier := make(map[string]struct{})
	targetEvidence := walk.collectIntroduced(present, frontier)
	seedNodeIDs, err := builderSemanticSeedNodeIDs(ctx, req, seeds, deleted, targetEvidence)
	if err != nil {
		return nil, err
	}
	if err := b.collectDependents(ctx, req, seedNodeIDs.reverse, frontier); err != nil {
		return nil, err
	}
	if err := b.collectDependencies(ctx, req, seeds, seedNodeIDs.all, frontier); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taken = append(taken, walk.admitAll(frontier)...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fixed point: every file the walk admits is asked what IT resolves into,
	// and the answer feeds the next round. The loop terminates because
	// admission is monotone and capped — a round that admits nothing new ends
	// the walk, and one that hits the cap ends it too.
	for len(taken) > 0 && !walk.truncated {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := make(map[string]struct{})
		nodeIDs, err := builderSeedNodeIDsContext(ctx, req.Base, taken)
		if err != nil {
			return nil, err
		}
		if err := b.collectDependencies(ctx, req, taken, nodeIDs, next); err != nil {
			return nil, err
		}
		taken = walk.admitAll(next)
	}

	closure := append([]string(nil), walk.order...)
	sort.Strings(closure)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if walk.truncated {
		report.ClosureTruncated = true
		b.Logger.Warn("indexer: sparse generation closure truncated",
			zap.String("repo", req.RepoPrefix),
			zap.Int("closure", len(closure)),
			zap.Int("cap", limit))
	}
	report.ClosureFiles = len(closure)
	report.ClosurePaths = closure
	return closure, nil
}

// closureWalk is one build's closure in progress: what it has admitted, whether
// the cap cut it short, and the per-build index the walk answers its import
// lookups from.
type closureWalk struct {
	b     *SparseGenerationBuilder
	req   BuildRequest
	limit int

	// seeds are the graph paths of the change set. They are never admitted —
	// the caller already carries them — but they are also never re-offered.
	seeds map[string]struct{}
	// deleted is the repo-relative set the change removed. A file the change
	// deleted cannot be re-derived, whatever still points at it.
	deleted map[string]struct{}

	chosen    map[string]struct{}
	order     []string
	truncated bool

	// dirIndex and lastDirIndex place an import path on the base corpus's own
	// files, by the same two-step rule the resolver's import cascade uses:
	// exact directory first, last path component second. Built once, on the
	// first import a changed file names, and only then — a change that
	// introduces no import never pays for it.
	dirIndex     map[string][]string
	lastDirIndex map[string][]string
}

// admit offers one graph path to the closure. It reports whether the path
// joined, which is what makes the caller's frontier the NEWLY admitted set
// rather than everything seen.
func (w *closureWalk) admit(graphPath string) bool {
	if graphPath == "" || w.truncated {
		return false
	}
	if _, seed := w.seeds[graphPath]; seed {
		return false
	}
	if _, already := w.chosen[graphPath]; already {
		return false
	}
	rel, owned := builderRelPath(w.req.RepoPrefix, graphPath)
	if !owned {
		return false
	}
	if _, gone := w.deleted[rel]; gone {
		return false
	}
	if _, err := w.req.Target.Stat(rel); err != nil {
		// The base layer knows a file the target state does not hold. The
		// caller's change set did not call it deleted, so this is a diff that
		// does not describe the content — skip it rather than plan a read that
		// would fail.
		return false
	}
	if len(w.chosen) >= w.limit {
		w.truncated = true
		return false
	}
	w.chosen[graphPath] = struct{}{}
	w.order = append(w.order, rel)
	return true
}

// admitAll offers a whole round's candidates in sorted order and returns the
// graph paths that joined. Sorting is what makes a truncated closure the same
// closure on every build of the same inputs.
func (w *closureWalk) admitAll(candidates map[string]struct{}) []string {
	if len(candidates) == 0 {
		return nil
	}
	offered := make([]string, 0, len(candidates))
	for graphPath := range candidates {
		offered = append(offered, graphPath)
	}
	sort.Strings(offered)
	var taken []string
	for _, graphPath := range offered {
		if w.admit(graphPath) {
			taken = append(taken, graphPath)
		}
	}
	return taken
}

// collectManifests offers every root manifest the target holds.
func (w *closureWalk) collectManifests(out map[string]struct{}) {
	for _, manifest := range rootManifests() {
		if _, err := w.req.Target.Stat(manifest.path); err != nil {
			continue
		}
		out[builderGraphPath(w.req.RepoPrefix, manifest.path)] = struct{}{}
	}
}

// collectIntroduced extracts the changed and added files from the target state
// and offers the base files the change newly binds to, in both directions.
//
// It is the half of the walk the base layer cannot answer on its own, because
// the base layer describes the state the change is replacing. An added file has
// no base nodes, and a modified file's base edges describe the references it
// USED to make, so a reference the change introduces — and a definition it
// introduces — is a link nothing in the base layer draws to the change.
func (w *closureWalk) collectIntroduced(
	present map[string]struct{},
	out map[string]struct{},
) builderSemanticTarget {
	semantic := newBuilderSemanticTarget(present)
	if len(present) == 0 {
		return semantic
	}
	rels := make([]string, 0, len(present))
	graphPaths := make([]string, 0, len(present))
	for rel := range present {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		graphPaths = append(graphPaths, builderGraphPath(w.req.RepoPrefix, rel))
	}
	accounted := w.baseAccountedNames(graphPaths)

	refs := closureRefs{
		names:    map[string]struct{}{},
		imports:  map[string]struct{}{},
		defines:  map[string]struct{}{},
		semantic: &semantic,
	}
	for _, rel := range rels {
		w.extractInto(rel, &refs)
	}
	w.collectPlaceholderReferrers(refs.defines, out)

	names := make([]string, 0, len(refs.names))
	for name := range refs.names {
		if _, known := accounted[name]; known {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, node := range w.req.Base.FindNodesByName(name) {
			if node == nil || node.FilePath == "" || !graph.IsReferenceableSymbol(node.Kind) {
				continue
			}
			if node.RepoPrefix != "" && node.RepoPrefix != w.req.RepoPrefix {
				continue
			}
			out[node.FilePath] = struct{}{}
		}
	}

	imports := make([]string, 0, len(refs.imports))
	for importPath := range refs.imports {
		imports = append(imports, importPath)
	}
	sort.Strings(imports)
	for _, importPath := range imports {
		for _, graphPath := range w.importedFiles(importPath) {
			out[graphPath] = struct{}{}
		}
	}
	return semantic
}

// collectPlaceholderReferrers offers the base files whose references park on a
// name the change now DEFINES.
//
// It is the other half of the reverse direction. collectDependents walks the
// in-edges of the seeds' BASE nodes, which finds every file already pointing AT
// the change — but a definition the change introduces has no base node for
// anything to point at, and an added file has no base nodes at all. What the
// referring files point at instead is the resolver's placeholder for the bare
// name, so the placeholder identity is the key the reverse lookup has to use.
// Without it a whole index of the target binds the reference while the
// generation keeps the stale placeholder edge showing through from below.
//
// Only a name the base defines NOWHERE is asked. A name the base corpus already
// carries a definition for was already the resolver's to bind or to leave
// parked, by the package proximity and import reachability the closure cannot
// replay; re-offering it would hand the closure every file in the repository
// holding an unbound call to a common method name.
func (w *closureWalk) collectPlaceholderReferrers(defines map[string]struct{}, out map[string]struct{}) {
	names := make([]string, 0, len(defines))
	for name := range defines {
		if name == "" || w.baseDefinesName(name) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)

	keys := make(map[string]struct{})
	for _, name := range names {
		for _, id := range closurePlaceholderIDs(w.req.RepoPrefix, name) {
			keys[id] = struct{}{}
		}
	}
	placeholders := make([]string, 0, len(keys))
	for id := range keys {
		placeholders = append(placeholders, id)
	}
	sort.Strings(placeholders)

	// Both directions of the placeholder's adjacency are read, because the
	// resolver writes it on both sides: a call parks on it as its TARGET, while
	// the value flow out of an unbound callee runs FROM it. Either way the edge
	// was recorded in the referring file, which is the file the closure wants,
	// so the recorded path answers directly and the far endpoint is only the
	// fallback for an edge base wrote without one.
	//
	// No edge kind is filtered out. A placeholder identity exists only because
	// a reference failed to bind, so every edge touching one IS that reference
	// or a flow derived from it — the identity is the evidence, and a kind test
	// could only lose a file the pass has to re-derive.
	endpoints := make(map[string]struct{})
	claim := func(edge *graph.Edge, far string) {
		if edge.FilePath != "" {
			out[edge.FilePath] = struct{}{}
			return
		}
		if far == "" || graph.IsUnresolvedTarget(far) {
			return
		}
		// IsUnresolvedTarget does not recognise the path-prefixed spelling, so
		// the ids this lookup was keyed on are excluded by name as well.
		if _, placeholder := keys[far]; placeholder {
			return
		}
		endpoints[far] = struct{}{}
	}
	for _, edges := range w.req.Base.GetInEdgesByNodeIDs(placeholders) {
		for _, edge := range edges {
			if edge != nil {
				claim(edge, edge.From)
			}
		}
	}
	for _, edges := range w.req.Base.GetOutEdgesByNodeIDs(placeholders) {
		for _, edge := range edges {
			if edge != nil {
				claim(edge, edge.To)
			}
		}
	}
	builderAddNodeFiles(w.req.Base, endpoints, out)

	if reader, ok := w.req.Base.(graph.RefFactsReader); ok {
		byFile, err := reader.LoadRefFactsByTargets(w.req.RepoPrefix, placeholders)
		if err != nil {
			w.b.Logger.Debug("indexer: closure placeholder fact lookup failed", zap.Error(err))
		}
		for graphPath := range byFile {
			if graphPath != "" {
				out[graphPath] = struct{}{}
			}
		}
	}
}

// baseDefinesName reports whether the base layer carries a definition of this
// name in a file of this repository.
func (w *closureWalk) baseDefinesName(name string) bool {
	for _, node := range w.req.Base.FindNodesByName(name) {
		if node == nil || node.FilePath == "" || !graph.IsReferenceableSymbol(node.Kind) {
			continue
		}
		if node.RepoPrefix != "" && node.RepoPrefix != w.req.RepoPrefix {
			continue
		}
		return true
	}
	return false
}

// closurePlaceholderIDs spells every id an unbound reference to name parks on.
//
// Two axes. A reference is either a free-standing call (`unresolved::Foo`) or a
// member call (`unresolved::*.foo`), and the repo-prefix stamp leaves an edge's
// TARGET bare while prefixing its SOURCE — so the same placeholder appears bare
// where it is pointed at and path-prefixed where it points. The multi-repo COPY
// rewrite spells a third form, which UnresolvedNameCandidateIDs supplies.
//
// Every id is a point query against the edge index, so offering a shape this
// store does not use costs a miss rather than a scan.
func closurePlaceholderIDs(repoPrefix, name string) []string {
	ids := graph.UnresolvedNameCandidateIDs(&graph.Node{Name: name, RepoPrefix: repoPrefix})
	if repoPrefix == "" {
		return ids
	}
	return append(ids,
		repoPrefix+"/"+graph.UnresolvedMarker+name,
		repoPrefix+"/"+graph.UnresolvedMarker+"*."+name,
	)
}

// baseAccountedNames is every symbol name the base layer already has an answer
// for from these files — the target of a resolved reference, and the name a
// reference it could NOT resolve parked on.
//
// It is what keeps the name lookup to what the change actually INTRODUCED, and
// that is not an optimisation. A bare name is not evidence on its own: the
// resolver picks one definition out of everything sharing it, by package
// proximity and import reachability the closure cannot replay, while a lookup
// by name alone offers every one of them. For a name the base already answers
// there is nothing to offer — a resolved one already had its file placed by
// collectDependencies, and an unresolved one is a reference a whole index of
// the same corpus does not bind either. Re-offering both is how a comment-only
// change to a file in a large package ends up dragging a third of the package
// in behind the word "Close".
func (w *closureWalk) baseAccountedNames(graphPaths []string) map[string]struct{} {
	ids := builderSeedNodeIDs(w.req.Base, graphPaths)
	if len(ids) == 0 {
		return nil
	}
	names := make(map[string]struct{})
	targets := make(map[string]struct{})
	for _, edges := range w.req.Base.GetOutEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			if edge == nil || edge.To == "" || !closureCarriesEdge(edge.Kind) {
				continue
			}
			if graph.IsUnresolvedTarget(edge.To) {
				// An import placeholder spells a module path, not a symbol
				// name; there is nothing about it for a name lookup to skip.
				parked := graph.UnresolvedName(edge.To)
				if name := closureBareName(parked); name != "" && !strings.HasPrefix(parked, "import::") {
					names[name] = struct{}{}
				}
				continue
			}
			targets[edge.To] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return names
	}
	list := make([]string, 0, len(targets))
	for id := range targets {
		list = append(list, id)
	}
	sort.Strings(list)
	for _, node := range w.req.Base.GetNodesByIDs(list) {
		if node != nil && node.Name != "" {
			names[node.Name] = struct{}{}
		}
	}
	return names
}

// extractInto parses one file of the target state and records the vocabulary
// its references and its definitions hand the closure.
//
// The extraction is thrown away as soon as that vocabulary has been read, which
// means the changed and added set — and only that set — is parsed twice per
// build: once here, once by the pass. The closure's other files are parsed once,
// by the pass alone. That second parse is paid deliberately, for two reasons:
//
//   - The two extractions are not the same extraction. This one runs the bare
//     dispatcher on the file's raw bytes under zero options, because all it
//     needs is a list of names. The pass runs the repository's configured
//     options over coordinate-stable prepared source, under parse admission and
//     crash isolation. Handing this result to the pass would write a payload
//     derived from unprepared bytes and the wrong options — a different graph,
//     not a saved parse.
//   - Retaining a result means retaining what produced it. The nodes and edges
//     are plain Go values, but they are cut from a live tree-sitter tree whose
//     memory sits behind CGo where the collector cannot reclaim it, so holding
//     one per changed file until the pass runs is an unbounded live set.
//
// A file the pass cannot extract — unknown language, over the size cap, a
// parser that fails — contributes nothing and is not an error here: the same
// file will be admitted, walked and reported by the pass itself, which is where
// an extraction failure belongs.
func (w *closureWalk) extractInto(rel string, refs *closureRefs) {
	meta, err := w.req.Target.Stat(rel)
	if err != nil {
		return
	}
	if max := w.b.Config.MaxFileSize; max > 0 && meta.Size > max {
		return
	}
	reader, _, err := w.req.Target.Open(rel)
	if err != nil {
		return
	}
	src, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return
	}
	lang, ok := w.b.Registry.DetectLanguageContent(rel, src)
	if !ok {
		return
	}
	extractor, ok := w.b.Registry.GetByLanguage(lang)
	if !ok || extractor == nil {
		return
	}
	result, err := safeExtractWithOptions(
		extractor, builderGraphPath(w.req.RepoPrefix, rel), src, parser.ExtractionOptions{})
	if result != nil && result.Tree != nil {
		defer result.Tree.Close()
	}
	if err != nil {
		w.b.Logger.Debug("indexer: closure extraction failed",
			zap.String("file", rel), zap.Error(err))
		return
	}
	if result == nil {
		return
	}
	refs.collect(result)
	refs.semantic.record(rel, result)
}

// closureRefs is the vocabulary one file's extraction hands the closure: the
// bare symbol names its unresolved references park on, the import paths it
// names, and the symbol names it DEFINES. The first two are what the resolver
// binds from and the third is what it binds to, so a file placed by any of them
// is a file the pass would have had to see.
type closureRefs struct {
	names    map[string]struct{}
	imports  map[string]struct{}
	defines  map[string]struct{}
	semantic *builderSemanticTarget
}

// collect reads one extraction's unresolved references and its definitions.
//
// Both endpoints of every edge are read, not only the target: a return value's
// value_flow edge runs FROM the callee, so an unresolved callee appears on the
// source side there and nowhere else.
func (r *closureRefs) collect(result *parser.ExtractionResult) {
	for _, node := range result.Nodes {
		if node == nil {
			continue
		}
		if node.Name != "" && graph.IsReferenceableSymbol(node.Kind) {
			r.defines[node.Name] = struct{}{}
		}
		if node.Kind != graph.KindImport {
			continue
		}
		if importPath, _ := node.Meta["path"].(string); importPath != "" {
			r.imports[importPath] = struct{}{}
		}
	}
	for _, edge := range result.Edges {
		if edge == nil {
			continue
		}
		r.addEndpoint(edge.From)
		r.addEndpoint(edge.To)
	}
}

// addEndpoint records one edge endpoint, if it is an unresolved placeholder.
//
// The placeholder namespace is the resolver's own, so the shapes are read the
// way the resolver reads them: `import::<path>` names a module, `extern::
// <path>::<symbol>` names a symbol reached THROUGH one, and anything else is a
// name — bare, member-qualified (`*.foo`), or scope-qualified.
func (r *closureRefs) addEndpoint(id string) {
	if !graph.IsUnresolvedTarget(id) {
		return
	}
	name := graph.UnresolvedName(id)
	switch {
	case name == "":
	case strings.HasPrefix(name, "import::"):
		r.addImportSpecifier(strings.TrimPrefix(name, "import::"))
	case strings.HasPrefix(name, "extern::"):
		rest := strings.TrimPrefix(name, "extern::")
		if i := strings.LastIndex(rest, "::"); i > 0 {
			r.imports[rest[:i]] = struct{}{}
			r.addName(rest[i+len("::"):])
			return
		}
		r.imports[rest] = struct{}{}
	default:
		r.addName(name)
	}
}

// addImportSpecifier records an import placeholder's module path. A re-export
// spells the re-exported binding after the path (`import::<path>::<original>`),
// so the leading segment is recorded too rather than only the whole string.
func (r *closureRefs) addImportSpecifier(specifier string) {
	if specifier == "" {
		return
	}
	r.imports[specifier] = struct{}{}
	if i := strings.Index(specifier, "::"); i > 0 {
		r.imports[specifier[:i]] = struct{}{}
	}
}

// addName records the bare symbol name a placeholder carries.
func (r *closureRefs) addName(raw string) {
	if name := closureBareName(raw); name != "" {
		r.names[name] = struct{}{}
	}
}

// closureBareName strips the scope and receiver qualifications a placeholder
// carries, the way the resolver strips them: `*.foo`, `Pkg::foo` and `obj.foo`
// all bind by the name `foo`.
func closureBareName(raw string) string {
	name := raw
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+len("::"):]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// importedFiles places an import path on the base corpus's files, by the same
// cascade the resolver's import binding uses: the exact directory the path
// names, then — only when that misses — every directory whose last component
// matches the path's. Being exactly as wide as the resolver is the point: a
// file the resolver would have bound this import to is a file the generation
// has to carry, and one it would not is a file the generation does not need.
func (w *closureWalk) importedFiles(importPath string) []string {
	if importPath == "" {
		return nil
	}
	w.buildDirIndexes()
	if files := w.dirIndex[importPath]; len(files) > 0 {
		return files
	}
	if files := w.dirIndex[builderGraphPath(w.req.RepoPrefix, importPath)]; len(files) > 0 {
		return files
	}
	return w.lastDirIndex[path.Base(importPath)]
}

// buildDirIndexes buckets the base corpus's own file nodes by directory. It is
// one bounded scan of the file nodes — not of the graph — taken at most once
// per build and only when an import has to be placed.
func (w *closureWalk) buildDirIndexes() {
	if w.dirIndex != nil {
		return
	}
	w.dirIndex = make(map[string][]string)
	w.lastDirIndex = make(map[string][]string)
	for node := range w.req.Base.NodesByKind(graph.KindFile) {
		if node == nil || node.FilePath == "" {
			continue
		}
		if _, owned := builderRelPath(w.req.RepoPrefix, node.FilePath); !owned {
			continue
		}
		dir := path.Dir(node.FilePath)
		w.dirIndex[dir] = append(w.dirIndex[dir], node.FilePath)
		if last := path.Base(dir); last != "" && last != dir {
			w.lastDirIndex[last] = append(w.lastDirIndex[last], node.FilePath)
		}
	}
}

// builderSeedNodeIDs reads every node the base layer carries at the given
// paths, in one batched read.
func builderSeedNodeIDs(base LayerBase, paths []string) []string {
	ids, _ := builderSeedNodeIDsContext(context.Background(), base, paths)
	return ids
}

func builderSeedNodeIDsContext(ctx context.Context, base LayerBase, paths []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodesByFile := base.GetFileNodesByPaths(paths)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, graphPath := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, node := range nodesByFile[graphPath] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			ids = append(ids, node.ID)
		}
	}
	return ids, nil
}

// collectDependents adds the files whose resolved references point at a seed
// node: one batched reverse-edge read plus the durable reverse lookup.
func (b *SparseGenerationBuilder) collectDependents(
	ctx context.Context,
	req BuildRequest,
	seedNodeIDs []string,
	out map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(seedNodeIDs) == 0 {
		return nil
	}
	sourceIDs := make(map[string]struct{})
	edgesByNode := req.Base.GetInEdgesByNodeIDs(seedNodeIDs)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, edges := range edgesByNode {
		for _, edge := range edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if edge == nil || !closureCarriesEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.From) {
				continue
			}
			sourceIDs[edge.From] = struct{}{}
		}
	}
	if err := builderAddNodeFilesContext(ctx, req.Base, sourceIDs, out); err != nil {
		return err
	}

	if reader, ok := req.Base.(graph.RefFactsReader); ok {
		byFile, err := reader.LoadRefFactsByTargets(req.RepoPrefix, seedNodeIDs)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			b.Logger.Debug("indexer: closure reverse fact lookup failed", zap.Error(err))
		}
		for graphPath := range byFile {
			if err := ctx.Err(); err != nil {
				return err
			}
			if graphPath != "" {
				out[graphPath] = struct{}{}
			}
		}
	}
	return nil
}

// collectDependencies adds the files the given files' resolved references
// point at: one batched forward-edge read plus the durable per-file facts.
func (b *SparseGenerationBuilder) collectDependencies(
	ctx context.Context,
	req BuildRequest,
	files []string,
	nodeIDs []string,
	out map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	targetIDs := make(map[string]struct{})
	if len(nodeIDs) > 0 {
		edgesByNode := req.Base.GetOutEdgesByNodeIDs(nodeIDs)
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, edges := range edgesByNode {
			for _, edge := range edges {
				if err := ctx.Err(); err != nil {
					return err
				}
				if edge == nil || !closureCarriesEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.To) {
					continue
				}
				targetIDs[edge.To] = struct{}{}
			}
		}
	}
	if reader, ok := req.Base.(graph.RefFactsReader); ok {
		facts, err := reader.LoadRefFactsByFiles(req.RepoPrefix, files)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			b.Logger.Debug("indexer: closure forward fact lookup failed", zap.Error(err))
		}
		for _, fact := range facts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if fact.ToID != "" && !graph.IsUnresolvedTarget(fact.ToID) {
				targetIDs[fact.ToID] = struct{}{}
			}
		}
	}
	return builderAddNodeFilesContext(ctx, req.Base, targetIDs, out)
}

// closureCarriesEdge reports whether an edge of this kind names content in
// another file that the closure has to carry.
//
// Three families, and each one is here because a file the generation claims
// cannot be re-derived correctly without the file on the other end:
//
//   - The resolver's name-bound reference set — calls, type positions, the
//     hierarchy — which is what IsResolvableRefEdge already answers.
//   - The import relation. It is not name-bound (no `unresolved::<Name>` stub
//     is ever rewritten into one) but it is exactly as cross-file as a call:
//     the imported file supplies the names the importer's own references bind
//     through.
//   - The similarity relation. A clone edge is not a reference at all — it is a
//     whole-corpus product — but it is RECORDED in the file, in both
//     directions, so a generation that claims one end and not the other
//     replaces a symmetric pair with half of one. The counterpart file joins
//     the closure for the same reason a callee does: the pass has to see it to
//     re-derive the row the mask is about to replace.
func closureCarriesEdge(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeImports, graph.EdgeReExports,
		graph.EdgeSimilarTo, graph.EdgeSemanticallyRelated:
		return true
	}
	return graph.IsResolvableRefEdge(kind)
}

// builderAddNodeFiles resolves node identities to the files they live at, in
// one batched read, and adds those files to out.
//
// The identity's own file component is used as a fallback when the base layer
// has no node under it: an edge may point at a symbol whose definition row was
// evicted, and the ID still names the file the reference was resolved into.
func builderAddNodeFiles(base LayerBase, ids map[string]struct{}, out map[string]struct{}) {
	_ = builderAddNodeFilesContext(context.Background(), base, ids, out)
}

func builderAddNodeFilesContext(
	ctx context.Context,
	base LayerBase,
	ids map[string]struct{},
	out map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		list = append(list, id)
	}
	sort.Strings(list)
	if err := ctx.Err(); err != nil {
		return err
	}
	nodes := base.GetNodesByIDs(list)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node := nodes[id]; node != nil && node.FilePath != "" {
			out[node.FilePath] = struct{}{}
			continue
		}
		if graph.IsStub(id) {
			continue
		}
		if file := graph.IDFile(id); file != "" {
			out[file] = struct{}{}
		}
	}
	return nil
}
