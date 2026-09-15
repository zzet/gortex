package resolver

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type resolveAllPassIndexes struct {
	resolver   *Resolver
	fullPass   bool
	generation uint64

	dirAll       bool
	depAll       bool
	providesAll  bool
	dirRepos     map[string]struct{}
	depRepos     map[string]struct{}
	provideRepos map[string]struct{}

	// reachabilityFiles retains only stable direct-import directory sets for
	// caller files already seen in this pass. Page-local active maps remain on
	// Resolver and are cleared after every page.
	reachabilityFiles map[string]map[string]struct{}

	// importAdjacency retains raw ProjectImportAdjacency results for caller
	// files already seen in this pass — stable or not. Only the store read is
	// retained; the per-page derivation still reruns, so reachable dirs track
	// targets resolved later in the pass. Cleared wholesale on interleave and
	// on importAdjacencyGen drift (an imports-kind edge write landed).
	importAdjacency    map[string][]string
	importAdjacencyGen uint64
}

func newResolveAllPassIndexes(r *Resolver) *resolveAllPassIndexes {
	return &resolveAllPassIndexes{
		resolver:          r,
		fullPass:          true,
		dirRepos:          make(map[string]struct{}),
		depRepos:          make(map[string]struct{}),
		provideRepos:      make(map[string]struct{}),
		reachabilityFiles: make(map[string]map[string]struct{}),
		importAdjacency:   make(map[string][]string),
	}
}

func newPendingFrontierPassIndexes(r *Resolver) *resolveAllPassIndexes {
	return &resolveAllPassIndexes{
		resolver:          r,
		dirRepos:          make(map[string]struct{}),
		depRepos:          make(map[string]struct{}),
		provideRepos:      make(map[string]struct{}),
		reachabilityFiles: make(map[string]map[string]struct{}),
		importAdjacency:   make(map[string][]string),
	}
}

// pendingRepoPrefixes hydrates the page's source nodes while deriving its
// repository scope. The returned map is deliberately non-nil even when every
// source is missing: callers can distinguish "hydrated with no hits" from
// "not hydrated" without turning missing IDs into authoritative negatives.
func pendingRepoPrefixes(r *Resolver, pending []*graph.Edge) ([]string, map[string]*graph.Node) {
	idSet := make(map[string]struct{}, len(pending))
	for _, edge := range pending {
		if edge != nil && edge.From != "" {
			idSet[edge.From] = struct{}{}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sources := r.cachedParallelGetNodesByIDs(ids)
	if sources == nil {
		sources = make(map[string]*graph.Node)
	}
	// Repo prefixes known to the store. Used to reject an ID-derived guess
	// that names no repo: RepoPrefixOfID is a syntactic split on the first
	// "/", so for an id it cannot confirm it happily yields a directory
	// name ("internal/foo.go::Bar" → "internal"). Feeding that into the
	// dep / provides / dir indexes below keys them on something nothing
	// matches, and they come back empty with no error anywhere.
	known := make(map[string]struct{})
	if lister, ok := r.graph.(interface{ RepoPrefixes() []string }); ok {
		for _, p := range lister.RepoPrefixes() {
			known[p] = struct{}{}
		}
	}

	set := make(map[string]struct{})
	var rejected []string
	for _, edge := range pending {
		if edge == nil {
			continue
		}
		prefix := graph.RepoPrefixOfID(edge.From)
		if source := sources[edge.From]; source != nil && source.RepoPrefix != "" {
			// Confirmed by the hydrated node — authoritative.
			prefix = source.RepoPrefix
		} else if prefix != "" && len(known) > 0 {
			if _, ok := known[prefix]; !ok {
				// A guess that names no tracked repo. Drop it rather
				// than scope the pass to a key nothing can match.
				if _, dup := seenIn(rejected, prefix); !dup {
					rejected = append(rejected, prefix)
				}
				continue
			}
		}
		set[prefix] = struct{}{}
	}
	prefixes := make([]string, 0, len(set))
	for prefix := range set {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	if len(rejected) > 0 && r != nil && r.logger != nil {
		sort.Strings(rejected)
		r.logger.Warn("resolver: dropped ID-derived repo prefixes that name no tracked repo — "+
			"these came from node ids the pass could not hydrate, and scoping to them "+
			"would have keyed the dep/provides/dir indexes on a directory name",
			zap.Strings("rejected_prefixes", rejected),
			zap.Int("pending_edges", len(pending)))
	}
	return prefixes, sources
}

// seenIn reports whether v is already in xs. Linear, and deliberately so:
// the caller's slice holds distinct rejected prefixes, which is a handful
// even on a pathological page.
func seenIn(xs []string, v string) (int, bool) {
	for i, x := range xs {
		if x == v {
			return i, true
		}
	}
	return 0, false
}

func pendingNeedsDepIndex(pending []*graph.Edge) bool {
	for _, edge := range pending {
		if edge == nil {
			continue
		}
		target := graph.UnresolvedName(edge.To)
		if strings.HasPrefix(target, "import::") {
			return true
		}
	}
	return false
}

func pendingNeedsProvidesIndex(pending []*graph.Edge) bool {
	for _, edge := range pending {
		if edge == nil {
			continue
		}
		// Incremental preparation may carry only EdgeIdentity fields. Treat a
		// wildcard member target as sufficient evidence even when receiver_type
		// metadata is intentionally deferred to the fresh resolution read.
		if strings.HasPrefix(graph.UnresolvedName(edge.To), "*.") &&
			(edge.Meta == nil || edgeReceiverType(edge) != "") {
			return true
		}
	}
	return false
}

// prepare builds page indexes and returns the source-node hydration already
// paid for while deriving a scoped page's repository prefixes. A nil return
// means no hydration was needed, so warmLookupCacheWithSources retains its
// existing fetch path for full unscoped passes.
func (p *resolveAllPassIndexes) prepare(pending []*graph.Edge) map[string]*graph.Node {
	if len(pending) == 0 {
		return nil
	}
	prepareStart := time.Now()
	var prefixes []string
	var sources map[string]*graph.Node
	prefixStart := time.Now()
	if !p.fullPass || len(p.resolver.scope) > 0 {
		prefixes, sources = pendingRepoPrefixes(p.resolver, pending)
	}
	prefixElapsed := time.Since(prefixStart)
	var dirElapsed, depElapsed, providesElapsed time.Duration
	start := time.Now()
	// Directory candidates serve more than dependency imports; preserve the
	// exhaustive pre-change behavior until every consumer has a proven gate.
	p.ensureDir(prefixes)
	dirElapsed = time.Since(start)
	if pendingNeedsDepIndex(pending) {
		start = time.Now()
		p.ensureDep(prefixes)
		depElapsed = time.Since(start)
	}
	if pendingNeedsProvidesIndex(pending) {
		start := time.Now()
		p.ensureProvides(prefixes)
		providesElapsed = time.Since(start)
	}
	reachDirty := len(p.resolver.importDirtyFiles)
	if p.importAdjacencyGen != p.resolver.importEdgeGen {
		// A provenance-less imports-kind write landed since this retention
		// was built — adjacency rows may have changed anywhere. Clear
		// wholesale; the next projection repopulates at first-touch cost.
		clear(p.importAdjacency)
		p.importAdjacencyGen = p.resolver.importEdgeGen
	} else {
		// The common path: the pass resolved import edges with known
		// provenance. Drop exactly those files' retained projections.
		for filePath := range p.resolver.importDirtyFiles {
			delete(p.importAdjacency, filePath)
		}
	}
	clear(p.resolver.importDirtyFiles)
	reachabilityStart := time.Now()
	_, reachStats := p.resolver.buildReachabilityIndexForPendingCached(pending, sources, p.reachabilityFiles, p.importAdjacency)
	reachabilityElapsed := time.Since(reachabilityStart)
	p.generation = p.resolver.scratchGeneration
	p.resolver.logger.Info("resolver: prepare page indexes",
		zap.Int("pending", len(pending)),
		zap.Int("repos", len(prefixes)),
		zap.Duration("pending_prefix", prefixElapsed),
		zap.Duration("ensure_dir", dirElapsed),
		zap.Duration("ensure_dep", depElapsed),
		zap.Duration("ensure_provides", providesElapsed),
		zap.Duration("reachability", reachabilityElapsed),
		zap.Int("reach_files", reachStats.files),
		zap.Int("reach_cached", reachStats.cached),
		zap.Int("reach_missing", reachStats.missing),
		zap.Int("reach_unstable", reachStats.unstable),
		zap.Int("reach_adj_cached", reachStats.adjCached),
		zap.Int("reach_dirty", reachDirty),
		zap.Uint64("reach_gen", p.resolver.importEdgeGen),
		zap.Bool("reach_fallback", reachStats.fallback),
		zap.Int("reach_occupancy", reachStats.occupancy),
		zap.Duration("reach_project", reachStats.project),
		zap.Duration("reach_place", reachStats.place),
		zap.Duration("reach_match", reachStats.match),
		zap.Duration("elapsed", time.Since(prepareStart)))
	return sources
}

func (p *resolveAllPassIndexes) resetAfterInterleave() {
	p.resolver.clearGoPackageOwnership()
	p.dirAll = false
	p.depAll = false
	p.providesAll = false
	p.dirRepos = make(map[string]struct{})
	p.depRepos = make(map[string]struct{})
	p.provideRepos = make(map[string]struct{})
	p.reachabilityFiles = make(map[string]map[string]struct{})
	p.importAdjacency = make(map[string][]string)
}

// refreshAfterInterleave restores only scratch actually invalidated while
// ResolveAll yielded mu. The generation equality fast path adds no graph reads
// to the common case; a real same-instance interactive pass rebuilds the
// current page's bounded indexes and lookup cache exactly once after relock.
func (p *resolveAllPassIndexes) refreshAfterInterleave(ctx context.Context, pending []*graph.Edge, force bool) (bool, error) {
	if !force && p.generation == p.resolver.scratchGeneration {
		return false, nil
	}
	// A forced store-generation refresh may not have passed through this
	// Resolver's clearLookupCache. Drop page-local negatives before rebuilding
	// so a source inserted by the interleaving writer is immediately visible.
	// The pass-scoped hot cache goes with them: an interleaving writer may
	// have changed node rows or repository name groups.
	p.resolver.hotCache.flush()
	p.resolver.missingNodeByID = nil
	p.resetAfterInterleave()
	sources := p.prepare(pending)
	if err := p.resolver.warmLookupCacheWithSources(ctx, pending, sources); err != nil {
		return true, err
	}
	return true, nil
}

func (p *resolveAllPassIndexes) clearPage() {
	p.resolver.clearReachabilityIndex()
	p.resolver.clearLSPIndex()
}

func (p *resolveAllPassIndexes) prepareTail() {
	if len(p.resolver.scope) == 0 {
		p.ensureDir(nil)
		return
	}
	p.ensureDir(p.resolver.resolveScopePrefixes())
}

func (p *resolveAllPassIndexes) close() {
	p.reachabilityFiles = nil
	p.importAdjacency = nil
	p.resolver.clearPassIndexes()
}

func hasEmptyPrefix(prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix == "" {
			return true
		}
	}
	return false
}

func missingPrefixes(prefixes []string, seen map[string]struct{}) []string {
	missing := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		missing = append(missing, prefix)
	}
	return missing
}

func (p *resolveAllPassIndexes) ensureDir(prefixes []string) {
	if p.dirAll {
		return
	}
	// A full ResolveAll is allowed to build the full directory index, but only
	// after pending work exists. Scoped passes stay on the pending repo set.
	if (p.fullPass && len(p.resolver.scope) == 0) || hasEmptyPrefix(prefixes) {
		p.resolver.buildDirIndexes()
		p.dirAll = true
		return
	}
	missing := missingPrefixes(prefixes, p.dirRepos)
	if len(missing) == 0 {
		return
	}
	if p.resolver.dirIndex == nil {
		p.resolver.dirIndex = make(map[string][]graph.FileNodeIdentity, 128)
		p.resolver.lastDirIndex = make(map[string][]graph.FileNodeIdentity, 128)
	}
	for file := range graph.FileNodeIdentitiesSeq(p.resolver.graph, missing) {
		dir := filePathDir(file.FilePath)
		p.resolver.dirIndex[dir] = append(p.resolver.dirIndex[dir], file)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			p.resolver.lastDirIndex[last] = append(p.resolver.lastDirIndex[last], file)
		}
	}
}

func (p *resolveAllPassIndexes) ensureDep(prefixes []string) {
	if p.depAll {
		return
	}
	if (p.fullPass && len(p.resolver.scope) == 0) || hasEmptyPrefix(prefixes) {
		p.resolver.buildDepModuleIndex()
		p.depAll = true
		return
	}
	missing := missingPrefixes(prefixes, p.depRepos)
	if len(missing) == 0 {
		return
	}
	if p.resolver.depModuleIndex == nil {
		p.resolver.depModuleIndex = make(map[string][]depModuleEntry)
	}
	for node := range graph.RepoNodeIdentitiesSeq(p.resolver.graph, missing, graph.KindContract) {
		if !strings.HasPrefix(node.ID, "dep::") {
			continue
		}
		modulePath := strings.TrimPrefix(node.ID, "dep::")
		if modulePath == "" || strings.Contains(modulePath, "::") {
			continue
		}
		p.resolver.depModuleIndex[node.RepoPrefix] = append(
			p.resolver.depModuleIndex[node.RepoPrefix],
			depModuleEntry{modulePath: modulePath, nodeID: node.ID},
		)
	}
	for _, prefix := range missing {
		entries := p.resolver.depModuleIndex[prefix]
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].modulePath) > len(entries[j].modulePath)
		})
	}
}

func (p *resolveAllPassIndexes) ensureProvides(prefixes []string) {
	if p.providesAll {
		return
	}
	if (p.fullPass && len(p.resolver.scope) == 0) || hasEmptyPrefix(prefixes) {
		p.resolver.buildProvidesForIndex()
		p.providesAll = true
		return
	}
	missing := missingPrefixes(prefixes, p.provideRepos)
	if len(missing) == 0 {
		return
	}
	if p.resolver.providesForIdx == nil {
		p.resolver.providesForIdx = make(map[string]map[string]struct{})
	}
	for row := range graph.EdgesInScopeSeq(p.resolver.graph, missing, nil, graph.EdgeProvides) {
		edge := row.Edge
		if edge == nil || edge.Meta == nil {
			continue
		}
		providesFor, _ := edge.Meta[graph.MetaDIProvidesFor].(string)
		binding, _ := edge.Meta[graph.MetaDIBinding].(string)
		if providesFor == "" || binding != graph.DIBindingUseClass {
			continue
		}
		name := edge.To
		if graph.IsUnresolvedTarget(name) {
			name = graph.UnresolvedName(name)
		} else if cut := strings.LastIndex(name, "::"); cut >= 0 {
			name = name[cut+2:]
		}
		if p.resolver.providesForIdx[providesFor] == nil {
			p.resolver.providesForIdx[providesFor] = make(map[string]struct{})
		}
		p.resolver.providesForIdx[providesFor][name] = struct{}{}
	}
}

// scopedBackendResolver is an optional backend capability. A scoped
// ResolveAll must never dispatch the unscoped BackendResolver hook: doing so
// turns one changed repository into a workspace-wide SQL pass.
type scopedBackendResolver interface {
	ResolveAllBulkScoped(repoPrefixes []string) (totalResolved int, err error)
}

// backendResolveWorkIndicator lets a backend that can synthesize unresolved
// edges advertise that work without forcing every zero-pending warm pass to
// execute ResolveAllBulk. Backends whose bulk hook only drains existing
// unresolved edges do not need to implement it.
type backendResolveWorkIndicator interface {
	BackendResolveWorkPending(repoPrefixes []string) (bool, error)
}

func (r *Resolver) resolveScopePrefixes() []string {
	prefixes := make([]string, 0, len(r.scope))
	for prefix := range r.scope {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

// prepareResolveAllStream establishes the cheap pending-work gate before any
// workspace index is built. When a backend bulk pass applies, its mutations
// precede the returned high-water snapshot so the Go resolver sees exactly the
// remaining (and any backend-created) unresolved work.
func (r *Resolver) prepareResolveAllStream(ctx context.Context) *unresolvedEdgeStream {
	stream := newUnresolvedEdgeStreamContext(ctx, r.graph)
	if stream.initErr != nil || !backendResolverEnabled() {
		return stream
	}
	if err := ctx.Err(); err != nil {
		stream.initErr = err
		return stream
	}

	prefixes := r.resolveScopePrefixes()
	// Native pagers may deliberately omit the exact pre-count to avoid a
	// whole-frontier census. A positive high-water mark is sufficient to prove
	// that unresolved work existed at the pass boundary.
	hasWork := stream.scan.HighWaterID > 0 || stream.scan.PendingBefore > 0
	if !hasWork {
		if indicator, ok := r.graph.(backendResolveWorkIndicator); ok {
			pending, err := indicator.BackendResolveWorkPending(prefixes)
			if err != nil {
				r.logger.Warn("resolver: backend work probe", zap.Error(err))
				return stream
			}
			hasWork = pending
		}
	}
	if !hasWork {
		return stream
	}

	var run func() (int, error)
	if len(prefixes) > 0 {
		if scoped, ok := r.graph.(scopedBackendResolver); ok {
			run = func() (int, error) { return scoped.ResolveAllBulkScoped(prefixes) }
		}
	} else if backend, ok := r.graph.(graph.BackendResolver); ok {
		run = backend.ResolveAllBulk
	}
	if run == nil {
		return stream
	}

	// The provisional stream may own a legacy disk spool. Close it before the
	// backend mutation and establish a fresh high-water snapshot afterwards.
	stream.close()
	bulkStart := time.Now()
	resolved, err := run()
	r.logger.Info("resolver: backend bulk pass",
		zap.Int("resolved", resolved),
		zap.Bool("scoped", len(prefixes) > 0),
		zap.Duration("elapsed", time.Since(bulkStart)),
		zap.Error(err))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &unresolvedEdgeStream{ctx: ctx, initErr: ctxErr}
	}
	return newUnresolvedEdgeStreamContext(ctx, r.graph)
}
