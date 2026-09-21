package indexer

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
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

// Committed-base closure sizing. A dedicated delta's closure is the corpus it
// READS to resolve its change; since the generation carries payload for the
// change set alone (see withholdContextPayload), the size of that corpus is a
// bound on parse and resolve work, not on what is written. Sizing it off the
// change the build exists for is therefore the right shape: a one-file commit
// has no business dragging a two-hundred-file re-resolve behind it, and a
// hundred-file commit needs more room than one.
const (
	// builderCommittedBaseClosurePerChange is how many context files one
	// changed file may pull into a committed-base delta's resolution corpus.
	builderCommittedBaseClosurePerChange = 32
	// builderCommittedBaseClosureFloor is the smallest corpus any committed
	// base gets, so a single-file commit still reaches its dependents and its
	// manifests. It is the historical default this path used to inherit.
	builderCommittedBaseClosureFloor = defaultAffectedByMax
	// builderCommittedBaseClosureCeiling keeps the walk bounded whatever the
	// change set's size. Past it the build is a reseed, not a delta.
	builderCommittedBaseClosureCeiling = 4096
)

// Which bound produced a build's ClosureCap. It rides on the BuildReport
// beside ClosureTruncated, because "the generation is knowingly incomplete" is
// only half a completeness fact: an operator who lowered a cap and an operator
// who never touched one need different answers to "why", and a truncation
// attributed to the wrong bound sends the second one looking for a knob that
// was not in force.
const (
	// ClosureCapFromOperator says index.affected_by_reresolve_max is the
	// bound in force. On a committed base it is named whenever the configured
	// value is at or below the change-sized cap — it is then the constraint
	// the operator can move.
	ClosureCapFromOperator = "operator"
	// ClosureCapFromChangeSized says the committed-base cap computed from the
	// change set is the bound in force (see builderCommittedBaseClosureCap).
	ClosureCapFromChangeSized = "change_sized"
	// ClosureCapFromDefault says no knob was configured and the built-in
	// defaultAffectedByMax applied.
	ClosureCapFromDefault = "default"
)

// builderClosureCap bounds the closure fan-out, and names the bound it used.
//
// A committed-base delta gets its OWN computed cap, sized off its change set,
// because index.affected_by_reresolve_max was chosen against a different
// question: it tunes the incremental pipeline's re-resolve frontier over the
// live working copy — a different pass, over a different corpus, with a
// different failure mode — and the two only ever shared a number. Inheriting
// it meant a repository that raised the incremental frontier silently widened
// the resolution corpus of every committed-base build.
//
// The knob is still HONOURED, in the one direction an operator sets it for.
// index.affected_by_reresolve_max is a ceiling on resolve fan-out, so a
// committed base takes the MINIMUM of the configured value and its own
// change-sized cap: a repository that lowered the knob to bound resolve cost
// keeps that bound on this path, and no committed base is ever walked wider
// than the operator allowed. When the knob is unset the computed cap applies
// alone. The cap is never RAISED by this path above what the operator asked
// for, and which of the two fired is recorded on the report.
//
// Every other sparse build — commit layers, dirty layers, ref views — reads
// the configured value directly, because for those the closure IS the write
// set bound that knob was chosen against.
func (b *SparseGenerationBuilder) builderClosureCap(req BuildRequest) (int, string) {
	operator := b.Config.AffectedByReresolveMax
	if req.Identity.GenerationKind == DedicatedBaseGenerationKind && req.Identity.BaseGenerationID > 0 {
		sized := builderCommittedBaseClosureCap(len(req.Changes))
		// At a tie the operator bound is named: it is the value explicitly
		// set, and lowering it moves the cap.
		if operator > 0 && operator <= sized {
			return operator, ClosureCapFromOperator
		}
		return sized, ClosureCapFromChangeSized
	}
	if operator > 0 {
		return operator, ClosureCapFromOperator
	}
	return defaultAffectedByMax, ClosureCapFromDefault
}

// builderClosureCapSourceLabel spells a ClosureCap source for a reader of the
// published completeness fact. An unrecorded source is said to be unrecorded
// rather than guessed at.
func builderClosureCapSourceLabel(source string) string {
	switch source {
	case ClosureCapFromOperator:
		return "index.affected_by_reresolve_max"
	case ClosureCapFromChangeSized:
		return "the change-sized committed-base cap"
	case ClosureCapFromDefault:
		return "the built-in default cap"
	default:
		return "an unrecorded bound"
	}
}

// builderCommittedBaseClosureCap sizes one committed-base delta's resolution
// corpus off the change it is built for, clamped to a floor and a ceiling so
// the walk stays bounded at both ends.
func builderCommittedBaseClosureCap(changed int) int {
	sized := changed * builderCommittedBaseClosurePerChange
	if sized < builderCommittedBaseClosureFloor {
		sized = builderCommittedBaseClosureFloor
	}
	if sized > builderCommittedBaseClosureCeiling {
		sized = builderCommittedBaseClosureCeiling
	}
	return sized
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
	limit, capSource := b.builderClosureCap(req)
	report.ClosureCap = limit
	report.ClosureCapSource = capSource

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
			zap.Int("cap", limit),
			zap.String("cap_source", capSource))
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
	// fileIndex is the same scan's membership set: what a joined relative
	// specifier's stem is probed against.
	fileIndex map[string]struct{}

	// qualNameFiles is the qualified-name arm's per-specifier placement,
	// filled by prepareQualNames in ONE batched lookup per build. A present
	// key with a nil value is an ANSWERED specifier that no node carries —
	// distinguishing it from an unasked one is what keeps the lazy path from
	// re-issuing a lookup per call.
	qualNameFiles map[string][]string
	// qualNamesBatched is the multi-valued lookup the base could offer, and
	// qualNamesComplete records whether it offered one at all. See
	// closureBatchedQualNames.
	qualNamesBatched  closureQualNameLookup
	qualNamesComplete bool
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
		imports:  map[string]map[string]struct{}{},
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
	// ONE batched qualified-name lookup for the whole specifier set, before any
	// placement runs. Every specifier goes in, relative ones included: the cost
	// of a name the placement never reads is a few bytes of one statement's
	// bind payload, while asking per specifier is a round trip each. One call
	// per build is the invariant TestClosureQualNameLookupIsBatchedOncePerBuild
	// holds the walk to.
	w.prepareQualNames(imports)
	for _, importPath := range imports {
		for _, graphPath := range w.importedFiles(importPath, refs.importCallers(importPath)) {
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
	refs.current = rel
	refs.collect(result)
	refs.current = ""
	refs.semantic.record(rel, result)
}

// closureRefs is the vocabulary one file's extraction hands the closure: the
// bare symbol names its unresolved references park on, the import paths it
// names, and the symbol names it DEFINES. The first two are what the resolver
// binds from and the third is what it binds to, so a file placed by any of them
// is a file the pass would have had to see.
//
// Every import path is recorded with the files that named it. The resolver's
// import cascade is not a function of the specifier alone — a relative
// specifier is joined against the importing file's own directory, and the
// entry-point rule that keeps a bare JS/TS specifier off an arbitrary in-repo
// file only applies when the importer is JS/TS (resolver/jsts_imports.go:253,
// :294). Placing an import without knowing who wrote it is what made the
// closure's last-component arm wider than the cascade it claims parity with.
type closureRefs struct {
	names   map[string]struct{}
	imports map[string]map[string]struct{}
	defines map[string]struct{}
	// current is the repo-relative path of the file being collected. It is
	// what attributes each import path to its importer.
	current  string
	semantic *builderSemanticTarget
}

// addImport records one import specifier against the file currently being
// collected.
func (r *closureRefs) addImport(spec string) {
	if spec == "" {
		return
	}
	callers := r.imports[spec]
	if callers == nil {
		callers = map[string]struct{}{}
		r.imports[spec] = callers
	}
	if r.current != "" {
		callers[r.current] = struct{}{}
	}
}

// importCallers returns the sorted repo-relative importers of one specifier.
func (r *closureRefs) importCallers(spec string) []string {
	set := r.imports[spec]
	if len(set) == 0 {
		return nil
	}
	callers := make([]string, 0, len(set))
	for caller := range set {
		callers = append(callers, caller)
	}
	sort.Strings(callers)
	return callers
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
			r.addImport(importPath)
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
			r.addImport(rest[:i])
			r.addName(rest[i+len("::"):])
			return
		}
		r.addImport(rest)
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
	r.addImport(specifier)
	if i := strings.Index(specifier, "::"); i > 0 {
		r.addImport(specifier[:i])
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
// cascade the resolver's import binding uses. The generation has to carry
// every file the resolver could have bound this import to; a file it could
// not is payload the generation pays for nothing.
//
// The two arms mirror resolveImport's candidate scan (resolver.go:4036-4049)
// one for one:
//
//   - `dirIndex[importPath]`, the directory the path names outright. When it
//     yields a candidate the resolver stops there (`stop()`, resolver.go:4032,
//     applied at :3812) and so does this.
//   - `lastDirIndex[lastPathComponent(importPath)]`, every directory whose
//     last component matches. Reached only when the first arm found nothing.
//
// WHICH candidate the resolver then binds is not something the closure can
// predict, and that asymmetry is the whole reason this is a placement and not
// a lookup. The same-repo branch takes the FIRST candidate the scan reaches
// (resolver.go:4062-4070) with no precision test of any kind, in the arbitrary
// order buildDirIndexes happened to bucket the corpus in — measured, an
// `import "example.com/fixture/util"` from a Go file binds to a Python
// `deep/util/__init__.py` when that is the row that sorted first. So every
// candidate the resolver considers has to be placed, not the one this code
// would have picked.
//
// In particular `dirMatchesImport` (resolver.go:5980-5996) is NOT available as
// a narrowing here. It reads like the precision rule this wants — dir must be
// a genuine suffix of the import path — but the resolver applies it at exactly
// one place, the CROSS-repo arm of `consider` (resolver.go:4023-4027), and its
// own contract says why (resolver.go:5980-5983): "Used only to authorise
// *cross-repo* candidates … Same-repo candidates don't need it".
// jsts_imports.go:290-293 rejects the same idea by name from the other side,
// and jsts_imports.go:276-277 states the fact plainly: "the same-repo branch
// of `consider` accepts the first one with no further check". Every candidate
// this walk can see is same-repo
// (buildDirIndexes keeps only owned paths), so applying that gate here drops
// files the resolver binds — which is the one failure mode a closure may not
// have.
//
// What the resolver DOES rule out, and this mirrors, is:
//
//   - A RELATIVE specifier that the join ANSWERS is never placed by last
//     component. For a JS/TS importer the resolver joins `./auth` onto the
//     importing file's own directory and probes the joined stem
//     (resolver/jsts_imports.go:132-148, probed at :156-200); when that probe
//     finds a file it binds it and returns (resolver.go:3906-3914), never
//     reaching the cascade. The old arm both admitted every `*/auth/`
//     directory in the repository AND missed `web/auth.ts`, the one file the
//     resolver actually binds.
//
//     Only that arm suppresses the cascade. The relative joins for the other
//     languages — a C include (relative_imports.go:95-110), a PHP include
//     (:258-286), a Python/Dart relative import (:24-33) — live in
//     `resolveRelativeImports`, a serial pass that runs AFTER the whole
//     resolve loop and therefore after `resolveImport` has already run its
//     cascade for that edge. So a non-JS/TS importer's join is placement
//     evidence only, never suppression evidence; conflating the two DROPPED
//     every cascade candidate behind a `.py`/`.rb`/`.h` sibling.
//
//     The join is a FIRST TRY, though, not a terminal arm: `resolveImport`
//     takes the relative answer only `if to != ""` (resolver.go:3906), so on
//     a MISS it carries on with the RAW specifier, where
//     `lastPathComponent("./auth") == "auth"` (resolver.go:5960-5966) and
//     `bareJSTS` is false (isJSTSBareSpecifier rejects a `./` prefix,
//     jsts_imports.go:253-264) — so `consider` applies no gate and binds the
//     first same-repo candidate out of `lastDirIndex` (resolver.go:4043-4049).
//     A relative miss therefore falls through here too. Refusing to fall
//     through was measured to DROP `misc/auth/index.ts` for a
//     `deep/app.ts: import { auth } from './auth'` with no `deep/auth.*`.
//
//   - A BARE JS/TS specifier only ever binds a directory ENTRY POINT.
//     `consider` skips every candidate that is not `index.<ext>`
//     (resolver.go:4008-4010, the rule stated at jsts_imports.go:277-293),
//     because Node and tsc load a directory-shaped module through its entry
//     point and never through an arbitrary file inside it. This is the one
//     gate `consider` applies to a SAME-repo candidate, so it is the one gate
//     the closure can apply too. Without it an `import … from 'graphql'`
//     dragged every file under any `*/graphql/` directory into the generation.
//
// Two resolver gates have no counterpart here, both superset-only. The npm
// manifest gate (declaresExternalNpmDep, read at resolver.go:4000 and applied
// at :4033-4034) skips the cascade outright for a specifier the importer's
// package.json declares a dependency of; the closure carries no manifest
// lookup. The Go package-ownership gate (goImportCandidateGate.retainFile,
// go_package_ownership.go:56-58, called first in `consider` at
// resolver.go:4005-4007) is installed by the live indexer, not by this walk.
// Both residuals admit more than the resolver binds; neither can drop, and each
// has a differential fixture.
//
// The qualified-name arm (resolver.go:3961-3972) runs BEFORE this cascade and
// returns when it hits, so it IS mirrored — by qualNameImportedFiles, unioned
// in below. Mirroring it is not optional: the arm binds a NODE by its qualified
// name with no directory involved at all, ACROSS language families (measured on
// the parity fixture: a TypeScript `import … from 'Service/logger'` binds the
// Kubernetes resource node in `k8s/svc.yaml`, whose directory neither cascade
// key names), so a cascade-only placement DROPS what it binds rather than
// merely narrowing it. The mirror is PLACEMENT only and never suppresses the
// cascade: whether the arm hits is a question about the POST-change graph the
// resolver will run on, which this walk cannot answer.
//
// resolveRelativeImports' own arms (relative_imports.go) are deliberately NOT
// mirrored, and the reason is measured rather than assumed. That pass keys on
// what `e.To` still is when it runs (:171-253), and it runs at
// resolver.go:1808 — after the resolve loop has already put every
// `unresolved::import::…` edge through resolveImport, which ends by stamping
// `e.To = "external::" + importPath` (resolver.go:4123). Only two of the pass's
// four families survive that rewrite: the `pyrel::` branch (:185-191), whose
// targets the per-edge resolver leaves untouched by contract
// (resolver.go:3628-3635), and the `external::` branch's python/dart arms
// (:192-203). The C-family (:204-241) and PHP (:242-249) arms both test for an
// `unresolved::import::` prefix the rewrite has already destroyed, so on the
// whole-index path they bind nothing at all. Measured on the parity fixture,
// indexed by the real indexer and resolved by the real resolver:
//
//	csame/main.cpp   #include "sibling.h"          -> external::sibling.h
//	native/main.cpp  #include "helper.h"           -> external::helper.h
//	phpdir/app.php   require __DIR__ . '/lib.php'  -> external::/lib.php
//	pypkg/app.py     from . import leaf            -> external::pypkg/leaf
//
// (the python one misses for a second reason as well: resolvePython probes its
// stem against node IDs, which carry the repository prefix, while the stem
// arrives without one.) Mirroring an arm that binds nothing is cost with no
// no-drop benefit, so the closure places nothing for those shapes — and the
// differential is the alarm rather than a comment:
// TestClosureLeavesTheRelativeImportPassShapesUnplaced runs the real build over
// exactly those three shapes and requires the composed reader to equal a whole
// index WITHOUT them. The day that pass reaches those edges, it goes red and
// names the mirror that then has to be written.
func (w *closureWalk) importedFiles(importPath string, callers []string) []string {
	if importPath == "" {
		return nil
	}
	w.buildDirIndexes()
	if closureRelativeSpecifier(importPath) {
		joined, answered := w.relativeImportedFiles(importPath, callers)
		if answered {
			// resolveImport returned at :3906-3914, ahead of the qualified-name
			// arm and ahead of the cascade. Nothing further is reachable, so
			// the qualified-name answer for this specifier is never read — the
			// per-build batch carries it, but no round trip is spent on it.
			return joined
		}
		// The join answered for no importer (or not for every one of them),
		// so the resolver reached the qualified-name arm and the cascade for
		// at least one edge with this specifier. Union rather than replace:
		// the callers whose join DID hit keep their placement.
		return closureUnionPlacements(joined,
			closureUnionPlacements(w.qualNameImportedFiles(importPath),
				w.cascadeImportedFiles(importPath, false)))
	}
	// The entry-point gate is the one narrowing the cascade authorises, and it
	// is NOT conditioned on the qualified-name mirror. The gate filters the
	// candidates the CASCADE reaches; the arm's own candidates are placed
	// beside them by the union below. Widening the gate could never stand in
	// for a qualified-name candidate the mirror failed to see — that candidate
	// is a node in a file neither cascade key names, which is the whole reason
	// the arm is mirrored — so switching the gate off on some base shape would
	// buy no coverage and would disable the issue-#450 cost control for every
	// bare specifier of every build on that shape.
	return closureUnionPlacements(w.qualNameImportedFiles(importPath),
		w.cascadeImportedFiles(importPath, closureBareJSTSImport(importPath, callers)))
}

// closureQualNameLookup is the multi-valued qualified-name lookup the resolver's
// own arm reads (cachedFindNodesByQualName, resolver.go:2444, which fans out to
// graph.Store.GetNodesByQualNames, store.go:193-198). It lives on graph.Store,
// not on graph.Reader — reader.go:24 offers only the single-valued
// GetNodeByQualName — so a LayerBase carries it only by luck of its concrete
// type. Reaching it through an assertion rather than by widening LayerBase is
// what keeps every base implementation from having to carry it.
type closureQualNameLookup interface {
	GetNodesByQualNames(qualNames []string) map[string][]*graph.Node
}

// closureBatchedQualNames finds the batched lookup a base can answer from,
// unwrapping one composition layer on the way.
//
// Every base the coordinator hands a build is one of two shapes: the store
// itself (checkout_coordinator.go:1773, a *store_sqlite.Store, taken only when the
// base generation is zero) or the commitLayerBase ancestryLayerBase mints
// (:2143-2145), which is what the commit-layer build (:1788) and the
// dirty-layer reader (:2128) both take — EVERY incremental build.
// commitLayerBase embeds the graph.Reader INTERFACE (:3240-3251), so the batched method is not in its
// method set even though the reader inside it always carries it: graphview
// composes a *graph.OverlaidView (materialize.go:770-802), and OverlaidView
// implements GetNodesByQualNames by merging every overlay and surviving base
// candidate (overlay.go:457-505). Unwrapping that one layer is what keeps the
// mirror's candidate set COMPLETE on the incremental path; without it the
// mirror degrades to the single-valued lookup exactly where it matters most.
func closureBatchedQualNames(base LayerBase) closureQualNameLookup {
	if batch, ok := base.(closureQualNameLookup); ok {
		return batch
	}
	if composed, ok := base.(commitLayerBase); ok {
		if batch, ok := composed.Reader.(closureQualNameLookup); ok {
			return batch
		}
	}
	return nil
}

// prepareQualNames resolves the qualified-name arm for a whole specifier set in
// ONE batched lookup, and is the only place that lookup is issued.
//
// Batching is the point. The store serves GetNodesByQualNames with a single
// statement whatever the payload (store_lookups.go:164-177), while
// closureRefs.addImportSpecifier records BOTH a re-export's module half and its
// `<path>::<export>` half (:770-780) — so a per-specifier call would cost
// roughly two round trips per distinct import the walk reaches, most of them
// for a qualified name nothing in the corpus carries.
//
// Specifiers already answered are skipped, so a second call adds only what is
// new, and a specifier asked for outside any prepared batch — a direct
// importedFiles caller, which is every test that places one specifier — falls
// back to preparing itself.
//
// The relative specifiers go into the batch too, even though a relative
// specifier whose join ANSWERS never reads the result: importedFiles returns on
// the relative arm before the qualified-name one is reachable, exactly as
// resolveImport does. Leaving them out would trade a few bytes of one
// statement's bind payload for a round trip per relative MISS.
func (w *closureWalk) prepareQualNames(importPaths []string) {
	if w.qualNameFiles == nil {
		w.qualNameFiles = make(map[string][]string, len(importPaths))
		w.qualNamesBatched = closureBatchedQualNames(w.req.Base)
		w.qualNamesComplete = w.qualNamesBatched != nil
		if !w.qualNamesComplete && w.b != nil && w.b.Logger != nil {
			// Not silent. Every base the coordinator builds answers the batched
			// lookup (closureBatchedQualNames' own doc enumerates them, and
			// TestClosureQualNameArmIsEnumerableThroughEveryProductionBaseShape
			// pins it), so reaching this is a new base shape, and the mirror
			// sees only ONE of the arm's candidates on it. The conservative
			// superset for an arm whose candidate set cannot be enumerated is
			// the whole corpus, which is not a generation — so the closure
			// reports the shape instead of pretending to cover it.
			w.b.Logger.Warn("indexer: closure base cannot enumerate qualified-name candidates",
				zap.String("repo", w.req.RepoPrefix),
				zap.String("base", fmt.Sprintf("%T", w.req.Base)))
		}
	}
	var want []string
	for _, spec := range importPaths {
		if spec == "" {
			continue
		}
		if _, answered := w.qualNameFiles[spec]; answered {
			continue
		}
		w.qualNameFiles[spec] = nil
		want = append(want, spec)
	}
	if len(want) == 0 {
		return
	}
	if w.qualNamesBatched != nil {
		hits := w.qualNamesBatched.GetNodesByQualNames(want)
		for _, spec := range want {
			w.qualNameFiles[spec] = w.ownedCandidateFiles(hits[spec])
		}
		return
	}
	for _, spec := range want {
		if node := w.req.Base.GetNodeByQualName(spec); node != nil {
			w.qualNameFiles[spec] = w.ownedCandidateFiles([]*graph.Node{node})
		}
	}
}

// qualNameImportedFiles mirrors resolveImport's qualified-name arm
// (resolver.go:3961-3972): every node whose QualName IS this specifier is a
// candidate the resolver may bind before the directory cascade is ever reached,
// so the file holding it has to be in the generation.
//
// Keyed on the RAW specifier, exactly as the resolver keys it — the per-binding
// `<path>::<export>` payload is looked up verbatim there too, and the module
// half travels as its own specifier (closureRefs.addImportSpecifier records
// both), so splitting here would place the module half twice and look the
// per-binding half up under the wrong key.
//
// The placement is the candidate's FILE, not its directory. The arm binds a
// NODE, and the generation needs that node to exist for the import edge to bind
// the way a whole index binds it; there is no barrel/entry-point indirection to
// cover, which is the only reason closureJSTSEntryPoints places whole
// directories.
func (w *closureWalk) qualNameImportedFiles(importPath string) []string {
	if importPath == "" {
		return nil
	}
	if _, answered := w.qualNameFiles[importPath]; !answered {
		w.prepareQualNames([]string{importPath})
	}
	return w.qualNameFiles[importPath]
}

// qualNamesEnumerable reports whether the base offered the multi-valued lookup
// — whether the mirror above saw the arm's WHOLE candidate set or only the one
// row graph.Reader's single-valued lookup returns. Nothing narrows on it; it is
// the fact the warning and the production-shape test are about.
func (w *closureWalk) qualNamesEnumerable() bool {
	w.prepareQualNames(nil)
	return w.qualNamesComplete
}

// ownedCandidateFiles reduces a qualified-name candidate set to the sorted,
// deduplicated files THIS build could carry.
//
// Two filters, both the resolver's own. A candidate with no FilePath, or one
// belonging to another repository, names nothing this generation can hold — the
// synthetic external nodes the resolver mints (external_call::<eco>::<path>)
// carry exactly that shape together with a QualName equal to the import path,
// so without the ownership filter every external import would place a phantom
// path. And it is the candidate's FILE that is placed, never its directory.
func (w *closureWalk) ownedCandidateFiles(candidates []*graph.Node) []string {
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(candidates))
	var out []string
	for _, node := range candidates {
		if node == nil || node.FilePath == "" {
			continue
		}
		if _, owned := builderRelPath(w.req.RepoPrefix, node.FilePath); !owned {
			continue
		}
		if _, dup := seen[node.FilePath]; dup {
			continue
		}
		seen[node.FilePath] = struct{}{}
		out = append(out, node.FilePath)
	}
	sort.Strings(out)
	return out
}

// cascadeImportedFiles is the dirIndex/lastDirIndex candidate scan itself,
// keyed exactly as resolveImport keys it (resolver.go:4036-4049): the raw
// specifier for the exact-directory arm, its last path component for the
// fallback arm. The first arm short-circuits the second, mirroring `stop()`.
func (w *closureWalk) cascadeImportedFiles(importPath string, bare bool) []string {
	if files := closureJSTSEntryPoints(w.dirIndex[importPath], bare); len(files) > 0 {
		return files
	}
	return closureJSTSEntryPoints(w.lastDirIndex[path.Base(importPath)], bare)
}

// closureUnionPlacements merges two placements into one sorted, deduplicated
// list.
func closureUnionPlacements(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, f := range list {
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// relativeImportedFiles places a relative specifier the way the resolver does:
// joined onto each importing file's own directory, then probed against the
// base corpus's files. A directory-shaped join contributes that directory,
// which is the specifier naming a package rather than a module file.
//
// The second return value is whether the join ANSWERED — whether, for EVERY
// importer, resolveImport's relative arm terminated at :3679. That is the
// resolver's own predicate and nothing wider: the importer must be a JS/TS
// source (jsts_imports.go:99-101) and the joined stem must hit probeJSTSFile's
// own candidate set (jsts_imports.go:156-200, mirrored by relativeArmProbeHits
// below). Only then does the resolver skip the dirIndex/lastDirIndex cascade;
// an importer that misses either half reaches that cascade with the raw `./…`
// specifier, so the caller must fall through for it. Reporting this separately
// from the placement is what keeps a miss from being a DROP.
//
// Placement is deliberately WIDER than the verdict, in both directions: the
// join contributes `w.dirIndex[stem]` (the specifier naming a package
// directory) and every closureModuleProbes hit (including the non-JS/TS
// module shapes resolveRelativeImports binds in a later pass), none of which
// is evidence that resolveImport returned early. Over-placing costs a file in
// the generation; over-answering loses an edge.
//
// With no importer to join against there is nothing to join, and the answer
// is "not answered": the caller then applies the cascade, which is the
// conservative superset.
func (w *closureWalk) relativeImportedFiles(spec string, callers []string) ([]string, bool) {
	spec, _ = closureSplitImportSpec(spec)
	if spec == "" || len(callers) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(graphPath string) {
		if graphPath == "" {
			return
		}
		if _, dup := seen[graphPath]; dup {
			return
		}
		seen[graphPath] = struct{}{}
		out = append(out, graphPath)
	}
	answered := true
	for _, caller := range callers {
		stem := closureJoinRelative(path.Dir(builderGraphPath(w.req.RepoPrefix, caller)), spec)
		if stem == "" {
			answered = false
			continue
		}
		for _, file := range w.dirIndex[stem] {
			add(file)
		}
		for _, cand := range closureModuleProbes(stem) {
			if _, ok := w.fileIndex[cand]; ok {
				add(cand)
			}
		}
		// PLACEMENT is the wide probe list above; the VERDICT is the
		// resolver's own relative arm and nothing else. resolveImport
		// returns at :3679 only when resolveJSTSImportTarget answers, and
		// that requires BOTH halves of its own precondition:
		//
		//   - the importer is a JS/TS source (jsts_imports.go:99-101,
		//     `if !isJSTSPath(callerFile) { return "" }`). For every other
		//     importer language the relative arm can never terminate
		//     resolveImport, so the cascade is ALWAYS reached and the
		//     closure may never suppress it.
		//   - the joined stem hits probeJSTSFile's OWN candidate set
		//     (jsts_imports.go:156-200), which is narrower than
		//     closureModuleProbes: the extra `.py/.php/.rb/.dart/.h…`
		//     probes exist to place what resolveRelativeImports
		//     (relative_imports.go:24-33) binds in a LATER pass, and a hit
		//     on one of them says nothing about whether resolveImport's
		//     relative arm terminated.
		//
		// Answering on anything wider suppresses a cascade the resolver
		// demonstrably runs, which is a DROP — the one failure mode the
		// closure exists to prevent.
		if !closureJSTSPath(caller) || !w.relativeArmProbeHits(stem) {
			answered = false
		}
	}
	sort.Strings(out)
	return out, answered
}

// relativeArmProbeHits mirrors resolver.probeJSTSFile (jsts_imports.go:156-200)
// against the base corpus's file set: whether the joined stem names an indexed
// file by the resolver's own probe order — an author-written module extension
// verbatim, the TypeScript sources an emitted-JavaScript extension compiles
// from, the stem plus each module extension, then the stem's directory barrel.
//
// It is deliberately NOT closureModuleProbes. This function answers "did
// resolveImport's relative arm TERMINATE", which is a question about
// probeJSTSFile alone, including its single-file-component early return: a
// `.vue` / `.svelte` / `.astro` stem is probed verbatim and never extended
// (jsts_imports.go:165-172), and a stem carrying no JS/TS extension at all is
// never probed verbatim, because probeJSTSFile only tries the bare stem inside
// that switch. Every widening here would suppress a cascade the resolver runs.
//
// The probe set is the base corpus's file index MINUS what this change
// deletes. The two halves of that are not the same corpus and the difference
// is a correctness one: fileIndex is built from req.Base (buildDirIndexes), so
// it still carries a file the change removes, while resolveImport runs against
// the POST-change tree, where the join misses and the cascade runs. Answering
// on a deleted file therefore suppresses a cascade the resolver demonstrably
// reaches — a DROP. A file the change ADDS needs no such correction: it is
// absent from fileIndex, so the verdict is "not answered" and the caller takes
// the cascade, which is the conservative superset.
func (w *closureWalk) relativeArmProbeHits(stem string) bool {
	if stem == "" {
		return false
	}
	isFile := func(id string) bool {
		if _, ok := w.fileIndex[id]; !ok {
			return false
		}
		if rel, owned := builderRelPath(w.req.RepoPrefix, id); owned {
			if _, gone := w.deleted[rel]; gone {
				return false
			}
		}
		return true
	}
	switch ext := strings.ToLower(path.Ext(stem)); ext {
	case ".vue", ".svelte", ".astro":
		return isFile(stem)
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		if isFile(stem) {
			return true
		}
		base := strings.TrimSuffix(stem, ext)
		for _, srcExt := range resolver.EmittedJSSourceExts(ext) {
			if isFile(base + srcExt) {
				return true
			}
		}
	}
	for _, ext := range closureModuleExts {
		if isFile(stem + ext) {
			return true
		}
	}
	for _, ext := range closureModuleExts {
		if isFile(stem + "/index" + ext) {
			return true
		}
	}
	return false
}

// closureSplitImportSpec splits an `import::` payload into the module
// specifier and the per-binding export name, the way
// resolver.splitImportSpecSymbol does.
func closureSplitImportSpec(importPath string) (spec, symbol string) {
	if i := strings.LastIndex(importPath, "::"); i >= 0 {
		return importPath[:i], importPath[i+len("::"):]
	}
	return importPath, ""
}

// closureRelativeSpecifier reports whether a specifier addresses the file
// system relative to its importer rather than naming a package.
func closureRelativeSpecifier(importPath string) bool {
	spec, _ := closureSplitImportSpec(importPath)
	return strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../")
}

// closureBareJSTSImport mirrors resolver.isJSTSBareSpecifier over a whole
// importer set: the entry-point gate is the JS/TS module loader's rule, so it
// applies only when every file that named this specifier is JS/TS. A specifier
// shared with a non-JS/TS importer keeps the wider set.
func closureBareJSTSImport(importPath string, callers []string) bool {
	if len(callers) == 0 {
		return false
	}
	spec, _ := closureSplitImportSpec(importPath)
	switch {
	case spec == "", spec == ".", spec == "..":
		return false
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"), strings.HasPrefix(spec, "/"):
		return false
	}
	for _, caller := range callers {
		if !closureJSTSPath(caller) {
			return false
		}
	}
	return true
}

// closureJSTSPath mirrors resolver.isJSTSPath — the importer languages whose
// specifiers the JS/TS module loader's rules describe.
func closureJSTSPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs",
		".vue", ".svelte", ".astro":
		return true
	}
	return false
}

// closureJSTSEntryPoints keeps the candidate DIRECTORIES that hold a JS/TS
// module entry point when the specifier is a bare JS/TS one, and is the
// identity otherwise.
//
// The gate is per directory, not per file, because the two questions differ:
// the resolver binds the import EDGE to the entry point alone, but what the
// specifier makes reachable is the whole directory behind it
// (resolver.importedDirForSpec, resolver.go:5528-5546, which returns
// filePathDir of the first candidate that clears the same gate). A generation
// that carried only `index.ts` would leave a call into a sibling the barrel
// re-exports parked on a stub — a divergence from a whole index, which is the
// thing the closure exists to avoid. So the entry point is the EVIDENCE the
// directory is the module, and the directory is what joins.
func closureJSTSEntryPoints(files []string, bare bool) []string {
	if !bare || len(files) == 0 {
		return files
	}
	entry := make(map[string]struct{})
	for _, file := range files {
		if closureJSTSDirEntryPoint(file) {
			entry[path.Dir(file)] = struct{}{}
		}
	}
	if len(entry) == 0 {
		return nil
	}
	var kept []string
	for _, file := range files {
		if _, ok := entry[path.Dir(file)]; ok {
			kept = append(kept, file)
		}
	}
	return kept
}

// closureJSTSDirEntryPoint mirrors resolver.isJSTSDirEntryPoint: a file is its
// directory's module entry point when it is `index.<ext>` for a JS/TS module
// extension.
func closureJSTSDirEntryPoint(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	for _, ext := range closureModuleExts {
		if base == "index"+ext {
			return true
		}
	}
	return false
}

// closureModuleExts mirrors resolver.jsTSImportExts (jsts_imports.go:36-38),
// the extensions a resolved JS/TS module specifier may carry on disk.
var closureModuleExts = []string{
	".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs",
}

// closureModuleProbes spells the file candidates one joined relative stem may
// name, for PLACEMENT only: the stem verbatim, the TypeScript sources an
// emitted-JavaScript extension compiles from (resolver.EmittedJSSourceExts),
// each module extension, the directory barrels a package-shaped stem resolves
// through, and the non-JS/TS module shapes resolveRelativeImports
// (relative_imports.go:24-33) binds in its own later pass.
//
// It is a superset of probeJSTSFile and must never be used to decide whether
// resolveImport's relative arm terminated — relativeArmProbeHits owns that
// question. A hit here places a file (cost); a hit there SUPPRESSES the
// dirIndex/lastDirIndex cascade (correctness).
func closureModuleProbes(stem string) []string {
	probes := []string{stem}
	if ext := strings.ToLower(path.Ext(stem)); ext != "" {
		base := strings.TrimSuffix(stem, path.Ext(stem))
		for _, srcExt := range resolver.EmittedJSSourceExts(ext) {
			probes = append(probes, base+srcExt)
		}
	}
	for _, ext := range closureModuleExts {
		probes = append(probes, stem+ext)
	}
	for _, ext := range closureModuleExts {
		probes = append(probes, stem+"/index"+ext)
	}
	// The non-JS/TS languages whose relative imports name a module file or a
	// package directory: Python (`__init__.py`), PHP, Ruby, Dart and the
	// C-family headers a quoted include reaches.
	for _, ext := range []string{".py", ".php", ".rb", ".dart", ".h", ".hpp", ".hh", ".hxx"} {
		probes = append(probes, stem+ext)
	}
	probes = append(probes, stem+"/__init__.py")
	return probes
}

// closureJoinRelative joins a relative specifier onto a directory and
// collapses `.`/`..`, the way resolver.joinRelativePath does. It returns ""
// when the specifier walks above the root, which under a repo prefix means
// above the repository.
func closureJoinRelative(dir, rel string) string {
	var parts []string
	if dir != "" && dir != "." {
		parts = strings.Split(dir, "/")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}

// buildDirIndexes buckets the base corpus's own file nodes by directory. It is
// one bounded scan of the file nodes — not of the graph — taken at most once
// per build and only when an import has to be placed. fileIndex is the same
// scan's membership set, which is what a joined relative stem is probed
// against.
func (w *closureWalk) buildDirIndexes() {
	if w.dirIndex != nil {
		return
	}
	w.dirIndex = make(map[string][]string)
	w.lastDirIndex = make(map[string][]string)
	w.fileIndex = make(map[string]struct{})
	for node := range w.req.Base.NodesByKind(graph.KindFile) {
		if node == nil || node.FilePath == "" {
			continue
		}
		if _, owned := builderRelPath(w.req.RepoPrefix, node.FilePath); !owned {
			continue
		}
		w.fileIndex[node.FilePath] = struct{}{}
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
