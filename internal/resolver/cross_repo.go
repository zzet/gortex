package resolver

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// CrossRepoStats holds counts from a cross-repo resolution pass.
type CrossRepoStats struct {
	Resolved        int            `json:"resolved"`
	Unresolved      int            `json:"unresolved"`
	CrossRepoEdges  int            `json:"cross_repo_edges"`
	ByRepo          map[string]int `json:"by_repo"`
	peakPendingPage int
	peakLookupKeys  int
}

// CrossWorkspaceDepRule names one allowed dependency from a source
// workspace into another. Mirrors config.CrossWorkspaceDep but lives
// here so the resolver doesn't import internal/config (avoids a cycle
// once future steps wire workspace plumbing through manager.go).
type CrossWorkspaceDepRule struct {
	// Workspace is the *target* workspace slug — the workspace whose
	// nodes are eligible to be referenced from the source workspace.
	Workspace string
	// Modules is the list of import-path prefixes that the source
	// workspace is allowed to follow into the target. Iteration 1
	// only supports prefix-style matches (longest prefix wins).
	Modules []string
}

// CrossWorkspaceDepLookup returns the list of declared cross-workspace
// dependencies for a *source* workspace. Empty / nil result means the
// source workspace has no declared cross-workspace deps and so the
// resolver must keep cross-workspace candidates ineligible.
type CrossWorkspaceDepLookup func(sourceWorkspaceID string) []CrossWorkspaceDepRule

// CrossRepoResolver resolves unresolved edges across repository boundaries.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single Resolve* pass — they let resolveImport look up candidate
// file nodes by directory in O(1) instead of scanning the whole graph
// (which is O(N) per import edge, O(N×M) total). Maps are nil between
// passes so we don't pay the memory cost while idle.
//
// mu is the graph-wide resolver lock shared with every Resolver built
// from the same Graph. Private to CrossRepoResolver is not enough: the
// coordinated incremental tail and master resolution both iterate shared
// predicate projections and mutate Edge.To in place. Sharing
// g.ResolveMutex() serialises both resolver types against the same graph;
// the incremental coordinator additionally keeps its repository lane and
// topology-writer gate held around the one batched cross-repo pass.
//
// crossWorkspaceLookup is the workspace-boundary check. Empty (nil)
// means the resolver is in legacy mode: cross-repo / cross-workspace
// candidates resolve as if no boundary existed — for callers that
// haven't plumbed config through yet. When set, candidates whose
// WorkspaceID differs from
// the caller's are accepted only when the source workspace declared
// the target workspace via `cross_workspace_deps` AND, for import
// edges, the import path has a declared-module prefix.
type CrossRepoResolver struct {
	graph graph.Store
	// nodeByID / nodesByName: per-pass batched lookup cache, the
	// cross-repo mirror of the fields on Resolver (resolver.go).
	// Populated by warmLookupCache before the per-edge fan-out and
	// cleared on return; cachedGetNode / cachedFindNodesByName consult
	// them first. Without it the cross-repo pass fires one
	// GetNode/FindNodesByName query per pending edge — across 200k+
	// unresolved edges that is a warmup hang on disk backends.
	logger   *zap.Logger
	nodeByID map[string]*graph.Node
	// hotCache retains node-by-ID and global name-bucket lookups across the
	// pages of one cross-repo pass (see resolve_hot_cache.go). Nodes are not
	// created during the pass (it only reindexes edges), so retention is
	// sound; an interleaving writer is detected via the store mutation
	// revision at the chunk yield and flushes it.
	hotCache *resolveHotCache
	// placeholderSrcIdx is the cross-repo mirror of the field of the same
	// name on Resolver: the per-ResolveAll-pass set of dataflow placeholder
	// sources consulted before any reconciliation probe (see
	// placeholder_sources.go). Reset at pass start.
	placeholderSrcIdx placeholderSourceIndex
	nodesByName       map[string][]*graph.Node
	nodesByNameRepo   map[string]map[string][]*graph.Node
	nodesByQualName   map[string][]*graph.Node
	dirIndex          map[string][]graph.FileNodeIdentity
	lastDirIndex      map[string][]graph.FileNodeIdentity
	// reachableReposByFile maps a caller file's ID to the set of repo
	// prefixes that file imports (derived from resolved EdgeImports
	// edges). It is the import-reachability evidence gate: a name-only
	// cross-repo function/method/type candidate is eligible only when
	// the caller's file actually imports the candidate's repo. Without
	// it, `FindNodesByName` spanning a multi-repo graph resolves short
	// common names (`len`, `string`, `Language`, `set`) to whichever
	// repo sorts first — the name-collision false positives the analyzer
	// surfaced. Built once per Resolve* pass, torn down after.
	reachableReposByFile map[string]map[string]struct{}
	// depModuleIndex bridges Go imports to dep::<module> contract
	// nodes from the caller's go.mod. Same shape and rationale as
	// the field of the same name on Resolver — see resolver.go for
	// the full doc. Cross-repo always scopes by callerRepo, so a
	// dep declared by repo A's go.mod never satisfies an import in
	// repo B even if the module path matches.
	depModuleIndex map[string][]depModuleEntry
	mu             *sync.Mutex
	// validateLiveness turns on the concurrent-edit guards in resolveEdge.
	// Set only on the chunked resolve path (ResolveAll with chunking), where
	// the pass releases mu between chunks so an interactive single-file edit
	// can interleave and evict nodes/edges the once-built per-pass indexes
	// still reference. With it on, resolveEdge skips an edge that is no
	// longer live (reindexing an evicted edge half-resurrects it and can
	// panic) and refuses a resolution whose target node was evicted (a
	// dangling edge). Off (the default, and the whole-pass-locked path) it is
	// a no-op: nothing can mutate the graph mid-pass, so every edge and
	// candidate is live by construction.
	validateLiveness     bool
	crossWorkspaceLookup CrossWorkspaceDepLookup
	// npmAlias rewrites a JS/TS import specifier that matches an
	// npm-alias dependency key in the importing file's nearest-
	// ancestor package.json. Same contract as the field of the
	// same name on Resolver — see npm_alias.go.
	npmAlias NpmAliasResolver
	// npmDep reports whether a bare JS/TS specifier is declared by the
	// importer as a dependency resolving outside the repository. Same
	// contract as the field of the same name on Resolver — see
	// npm_dependency.go.
	npmDep NpmDependencyLookup
	// pathAlias expands a JS/TS tsconfig/jsconfig path-alias / baseUrl
	// import specifier to the repo-prefixed file stem it targets. Same
	// contract as the field of the same name on Resolver — see
	// jsts_imports.go.
	pathAlias PathAliasResolver
	// workspaceMembers maps a file path to the package-manager
	// workspace it belongs to, used to prefer a same-workspace
	// candidate on a same-named import collision. Same contract as
	// the field of the same name on Resolver — see
	// workspace_membership.go.
	workspaceMembers WorkspaceMembership

	// Cross-daemon proxy-edge minting (off by default). When edgesEnabled
	// and prober != nil, a function call that local resolution leaves
	// unresolved is, as a last resort, stitched to a proxy node standing
	// in for a symbol a remote daemon owns — but only on positive remote
	// evidence (a find_declaration hit AND a non-empty import hint).
	edgesEnabled bool
	prober       RemoteDeclarationProber
	proxyBudget  int

	// retargetedTestCallFiles mirrors Resolver.retargetedTestCallFiles for
	// the cross-repository pass: caller files of test-classified calls this
	// pass bound, drained via TakeRetargetedTestCallFiles so the indexer
	// can reconcile their tests projections.
	retargetedMu            sync.Mutex
	retargetedTestCallFiles map[string]struct{}
}

// noteRetargetedCall mirrors Resolver.noteRetargetedCall for the
// cross-repository pass.
func (cr *CrossRepoResolver) noteRetargetedCall(e *graph.Edge) {
	if e == nil || e.Kind != graph.EdgeCalls || e.FilePath == "" {
		return
	}
	if graph.IsUnresolvedTarget(e.To) {
		return
	}
	if !isTestFilePath(e.FilePath) && !nodeStampedTest(cr.cachedGetNode(e.From)) {
		return
	}
	cr.retargetedMu.Lock()
	if cr.retargetedTestCallFiles == nil {
		cr.retargetedTestCallFiles = make(map[string]struct{})
	}
	cr.retargetedTestCallFiles[e.FilePath] = struct{}{}
	cr.retargetedMu.Unlock()
}

// TakeRetargetedTestCallFiles drains the accumulated test-caller frontier,
// sorted for determinism.
func (cr *CrossRepoResolver) TakeRetargetedTestCallFiles() []string {
	cr.retargetedMu.Lock()
	defer cr.retargetedMu.Unlock()
	if len(cr.retargetedTestCallFiles) == 0 {
		return nil
	}
	files := make([]string, 0, len(cr.retargetedTestCallFiles))
	for file := range cr.retargetedTestCallFiles {
		files = append(files, file)
	}
	cr.retargetedTestCallFiles = nil
	sort.Strings(files)
	return files
}

// NewCrossRepo creates a CrossRepoResolver for the given graph.
func NewCrossRepo(g graph.Store) *CrossRepoResolver {
	return &CrossRepoResolver{graph: g, mu: g.ResolveMutex(), logger: zap.NewNop()}
}

// SetLogger attaches a logger so ResolveAll emits pass progress (the
// cross-repo mirror of Resolver.SetLogger). A nil logger becomes a no-op.
func (cr *CrossRepoResolver) SetLogger(l *zap.Logger) {
	if l == nil {
		l = zap.NewNop()
	}
	cr.logger = l
}

// SetCrossWorkspaceDepLookup wires the boundary rule. After this
// call, the resolver will refuse cross-workspace candidates that
// aren't covered by an explicit declaration in the source workspace's
// `cross_workspace_deps`. Legacy graphs (no WorkspaceID on either
// side) keep working — when both From and To carry empty workspace
// slugs the boundary check trivially passes.
func (cr *CrossRepoResolver) SetCrossWorkspaceDepLookup(lookup CrossWorkspaceDepLookup) {
	cr.crossWorkspaceLookup = lookup
}

// callerWorkspaceID returns the workspace slug for the From-side of
// an edge. Falls back to RepoPrefix to match Contract.Effective-
// Workspace's "missing → repo-name" rule.
func (cr *CrossRepoResolver) callerWorkspaceID(e *graph.Edge) string {
	from := cr.cachedGetNode(e.From)
	if from == nil {
		return ""
	}
	if from.WorkspaceID != "" {
		return from.WorkspaceID
	}
	return from.RepoPrefix
}

// candidateWorkspaceID extracts the same slug from a candidate node.
func candidateWorkspaceID(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if n.WorkspaceID != "" {
		return n.WorkspaceID
	}
	return n.RepoPrefix
}

func fileCandidateWorkspaceID(file graph.FileNodeIdentity) string {
	if file.WorkspaceID != "" {
		return file.WorkspaceID
	}
	return file.RepoPrefix
}

// crossWorkspaceEligible reports whether sourceWS is permitted to
// reach a candidate in targetWS, optionally constrained by the
// candidate's import path. importPath == "" means "any module"
// (function/method calls — they don't carry an import path so the
// only check is workspace-pair declaration).
func (cr *CrossRepoResolver) crossWorkspaceEligible(sourceWS, targetWS, importPath string) bool {
	if sourceWS == targetWS {
		return true
	}
	if cr.crossWorkspaceLookup == nil {
		// Legacy / unwired callers: no boundary enforcement.
		return true
	}
	rules := cr.crossWorkspaceLookup(sourceWS)
	for _, rule := range rules {
		if rule.Workspace != targetWS {
			continue
		}
		if importPath == "" {
			// Function/method call into a declared cross-workspace
			// dep is allowed once the workspace pair is declared —
			// iteration 1 doesn't try to require an import-path
			// match for non-import edges.
			return true
		}
		for _, m := range rule.Modules {
			if m == importPath || strings.HasPrefix(importPath, m+"/") {
				return true
			}
		}
	}
	return false
}

// pickImportCandidate chooses the best cross-repo file candidate for an
// import: a candidate in the importer's own workspace wins outright;
// otherwise the first candidate the cross-workspace policy permits is
// used. Returns nil when no candidate clears the boundary, so the caller
// falls through to its dep-module / external handling.
//
// This replaces the old "first dir match, then a single boundary check
// that bailed to external" rule, which mis-resolved when two same-named
// modules live in different workspaces — the worktree-instance case,
// where the importer's workspace has its own copy of the imported module
// but the canonical copy (in another workspace) sorted first.
func (cr *CrossRepoResolver) pickImportCandidate(callerWS, importPath string, candidates []graph.FileNodeIdentity) (graph.FileNodeIdentity, bool) {
	for _, candidate := range candidates {
		if fileCandidateWorkspaceID(candidate) == callerWS {
			return candidate, true
		}
	}
	for _, candidate := range candidates {
		if cr.crossWorkspaceEligible(callerWS, fileCandidateWorkspaceID(candidate), importPath) {
			return candidate, true
		}
	}
	return graph.FileNodeIdentity{}, false
}

// pickQualNameCandidate applies repository/workspace policy to the complete
// qualified-name candidate set. The boolean reports an ambiguous eligible
// foreign set, which must stay unresolved rather than bind by row order.
func (cr *CrossRepoResolver) pickQualNameCandidate(callerRepo, callerWS, qualName string, candidates []*graph.Node) (*graph.Node, bool) {
	if candidate := lowestIDQualNameCandidate(candidates, func(n *graph.Node) bool {
		return n.RepoPrefix == callerRepo && candidateWorkspaceID(n) == callerWS
	}); candidate != nil {
		return candidate, false
	}
	if candidate := lowestIDQualNameCandidate(candidates, func(n *graph.Node) bool {
		return n.RepoPrefix == callerRepo
	}); candidate != nil {
		return candidate, false
	}
	if candidate := lowestIDQualNameCandidate(candidates, func(n *graph.Node) bool {
		return candidateWorkspaceID(n) == callerWS
	}); candidate != nil {
		return candidate, false
	}

	var eligible *graph.Node
	for _, candidate := range candidates {
		if candidate == nil || !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(candidate), qualName) {
			continue
		}
		if eligible != nil {
			return nil, true
		}
		eligible = candidate
	}
	return eligible, false
}

// ResolveAll resolves all unresolved edges in the graph, trying same-repo
// matches first, then cross-repo search. Sets Edge.CrossRepo = true for
// cross-repo matches.
func (cr *CrossRepoResolver) ResolveAll() *CrossRepoStats {
	stats, err := cr.ResolveAllContext(context.Background())
	if err != nil {
		cr.logger.Error("cross-repo resolve: ResolveAll", zap.Error(err))
	}
	return stats
}

// ResolveAllContext is ResolveAll with cancellation propagated through the
// unresolved-edge pager. The compatibility wrapper above preserves the
// historical best-effort API for non-lifecycle callers.
func (cr *CrossRepoResolver) ResolveAllContext(ctx context.Context) (*CrossRepoStats, error) {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	// Fresh placeholder-source set per pass — same rationale as ResolveAll
	// on the master resolver.
	cr.placeholderSrcIdx = placeholderSourceIndex{}
	// Share the master resolver's stable high-water/keyset stream. The cold
	// residual can exceed 200k edges; retaining it plus the cross-repo name,
	// raw-name, repo and qualified-name caches was the second whole-corpus heap
	// spike after Resolver.ResolveAll itself.
	pendingStream := newUnresolvedEdgeStreamContext(ctx, cr.graph)
	defer pendingStream.close()
	pendingBefore := pendingStream.scan.PendingBefore
	if !pendingStream.countKnown {
		pendingBefore = 0
	}
	var pendingTotal atomic.Int64
	pendingTotal.Store(int64(pendingBefore))
	var pendingLoaded atomic.Int64
	pending, streamDone, err := pendingStream.nextPage()
	if err != nil {
		return stats, err
	}
	if !pendingStream.countKnown {
		pendingBefore = len(pending)
		pendingTotal.Store(int64(pendingBefore))
	}
	pendingLoaded.Store(int64(len(pending)))
	if len(pending) == 0 && streamDone {
		return stats, nil
	}

	cr.buildDirIndexes()
	defer cr.clearDirIndexes()
	cr.buildDepModuleIndex()
	defer cr.clearDepModuleIndex()
	cr.buildReachableReposIndex()
	defer cr.clearReachableReposIndex()

	if resolveHotCacheEnabled() {
		cr.hotCache = newResolveHotCache(resolveHotCacheBudgetBytes(), len(pending))
		defer func() {
			if c := cr.hotCache; c != nil {
				cr.logger.Info("cross-repo resolve: hot cache stats",
					zap.Int64("node_hits", c.nodeHits),
					zap.Int64("node_misses", c.nodeMisses),
					zap.Int64("name_hits", c.nameHits),
					zap.Int64("name_misses", c.nameMisses))
			}
			cr.hotCache = nil
		}()
	}

	passStart := time.Now()
	cr.logger.Info("cross-repo resolve: pass start",
		zap.Int("pending", pendingBefore),
		zap.Int("first_page", len(pending)))
	var processed atomic.Int64
	progressDone := make(chan struct{})
	var progressOnce sync.Once
	stopProgress := func() {
		progressOnce.Do(func() { close(progressDone) })
	}
	defer stopProgress()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				cr.logger.Info("cross-repo resolve: compute progress",
					zap.Int64("processed", processed.Load()),
					zap.Int64("pending_loaded", pendingLoaded.Load()),
					zap.Int64("pending_total", pendingTotal.Load()),
					zap.Duration("elapsed", time.Since(passStart)))
			}
		}
	}()

	// Resolve one bounded page at a time. Lookup maps are warmed and cleared per
	// page, so raw/bare name negatives and repo buckets cannot accumulate with
	// corpus size. Worker batches remain bounded by the smaller super-chunk.
	cr.validateLiveness = resolveChunkEnabled()
	superChunk := resolvePendingPageRows
	if cr.validateLiveness && resolveChunkSize() < superChunk {
		superChunk = resolveChunkSize()
	}
	if superChunk < 1 {
		superChunk = 1
	}
	reindexTotal := 0
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		cr.warmLookupCache(pending)
		if len(pending) > stats.peakPendingPage {
			stats.peakPendingPage = len(pending)
		}
		lookupKeys := len(cr.nodeByID) + len(cr.nodesByName) + len(cr.nodesByNameRepo) + len(cr.nodesByQualName)
		if lookupKeys > stats.peakLookupKeys {
			stats.peakLookupKeys = lookupKeys
		}
		for base := 0; base < len(pending); base += superChunk {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			hi := base + superChunk
			if hi > len(pending) {
				hi = len(pending)
			}
			sc := pending[base:hi]

			workers := runtime.NumCPU()
			if workers > len(sc) {
				workers = len(sc)
			}
			if workers < 1 {
				workers = 1
			}
			perWorkerBatch := make([][]graph.EdgeReindex, workers)
			perWorkerStats := make([]*CrossRepoStats, workers)
			var wg sync.WaitGroup
			chunk := (len(sc) + workers - 1) / workers
			for w := 0; w < workers; w++ {
				start := w * chunk
				end := start + chunk
				if end > len(sc) {
					end = len(sc)
				}
				if start >= end {
					continue
				}
				wg.Add(1)
				go func(idx int, slice []*graph.Edge) {
					defer wg.Done()
					ws := &CrossRepoStats{ByRepo: make(map[string]int)}
					var batch []graph.EdgeReindex
					for _, edge := range slice {
						if ctx.Err() != nil {
							break
						}
						cr.resolveEdge(edge, ws, &batch)
						processed.Add(1)
					}
					perWorkerStats[idx] = ws
					perWorkerBatch[idx] = batch
				}(w, sc[start:end])
			}
			wg.Wait()
			if err := ctx.Err(); err != nil {
				return stats, err
			}

			var scBatch []graph.EdgeReindex
			for i := range perWorkerBatch {
				scBatch = append(scBatch, perWorkerBatch[i]...)
			}
			for _, ws := range perWorkerStats {
				if ws == nil {
					continue
				}
				stats.Resolved += ws.Resolved
				stats.Unresolved += ws.Unresolved
				stats.CrossRepoEdges += ws.CrossRepoEdges
				for repo, n := range ws.ByRepo {
					stats.ByRepo[repo] += n
				}
			}
			if len(scBatch) > 0 {
				if cr.validateLiveness {
					scBatch = cr.filterLiveReindex(scBatch)
				}
				cr.graph.ReindexEdges(scBatch)
				reconcilePlaceholderSources(cr.graph, &cr.placeholderSrcIdx, scBatch)
				reindexTotal += len(scBatch)
				DetectCrossRepoEdgesForReindexes(cr.graph, scBatch)
			}
			if cr.validateLiveness && (hi < len(pending) || !streamDone) {
				revBefore, revKnown := loadMutationRevision(cr.graph)
				cr.mu.Unlock()
				runtime.Gosched()
				cr.mu.Lock()
				if err := ctx.Err(); err != nil {
					return stats, err
				}
				if revKnown {
					if revAfter, _ := loadMutationRevision(cr.graph); revAfter != revBefore {
						// An interleaving writer may have created nodes or
						// changed name buckets; drop cross-page retention.
						cr.hotCache.flush()
					}
				}
			}
		}
		cr.clearLookupCache()
		if streamDone {
			break
		}
		pending, streamDone, err = pendingStream.nextPage()
		if err != nil {
			return stats, err
		}
		if !pendingStream.countKnown {
			pendingBefore += len(pending)
			pendingTotal.Store(int64(pendingBefore))
		}
		pendingLoaded.Add(int64(len(pending)))
	}
	stopProgress()
	cr.logger.Info("cross-repo resolve: compute done",
		zap.Int64("pending", pendingLoaded.Load()),
		zap.Int("reindex_batch", reindexTotal),
		zap.Int("super_chunk", superChunk),
		zap.Duration("elapsed", time.Since(passStart)))
	return stats, nil
}

// ResolveForRepo resolves only unresolved edges originating from nodes
// in the specified repository.
func (cr *CrossRepoResolver) ResolveForRepo(repoPrefix string) *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	// One backend query for every out-edge from this repo's nodes,
	// instead of GetRepoNodes followed by GetOutEdges per node. On
	// disk backends (SQLite, DuckDB) the per-node loop
	// was O(repo_nodes) round-trips per pass — single-digit minutes
	// of warmup on a multi-repo workspace where this method runs
	// once per tracked repo.
	stats := cr.resolveScopedLocked(cr.graph.GetRepoEdges(repoPrefix))
	DetectCrossRepoEdgesForRepos(cr.graph, []string{repoPrefix})
	return stats
}

// ResolveForFile is the watcher fast path: it re-resolves only the
// out-edges of the changed file, not the whole repo. The watcher fires
// after every single-file save, and the old ResolveForRepo path
// materialised the repo's ENTIRE edge set (hundreds of thousands of
// edges, each with its meta blob) on every keystroke-save — the
// dominant per-edit allocation flood and the cause of the
// "buffer pool is full" crash on a small resident pool. Scoping to the
// changed file's edges turns that into a GetFileNodes lookup plus one
// batched GetOutEdgesByNodeIDs, bounded by the file's size.
//
// relPath must be the repo-relative graph key — callers convert an
// absolute watcher path via Indexer.RelKey first. A path matching no
// nodes is a no-op.
//
// Scope note: this legacy API resolves only edges the changed file owns.
// Coordinated MultiIndexer mutation paths use ResolveFilesAndIncoming instead,
// so a complete Git/storm batch also re-checks other files' unresolved edges
// that point at symbols newly defined by the changed files. ResolveForRepo
// remains for callers that explicitly request a repository-wide recompute.
func (cr *CrossRepoResolver) ResolveForFile(repoPrefix, relPath string) *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	graphPath := relPath
	if repoPrefix != "" && relPath != repoPrefix && !strings.HasPrefix(relPath, repoPrefix+"/") {
		graphPath = repoPrefix + "/" + strings.TrimPrefix(relPath, "/")
	}
	nodes := cr.graph.GetFileNodes(graphPath)
	if len(nodes) == 0 {
		return &CrossRepoStats{ByRepo: make(map[string]int)}
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	var edges []*graph.Edge
	for _, es := range cr.graph.GetOutEdgesByNodeIDs(ids) {
		edges = append(edges, es...)
	}
	stats := cr.resolveScopedLocked(edges)
	DetectCrossRepoEdgesForFiles(cr.graph, []string{graphPath})
	return stats
}

// resolveScopedLocked lifts every unresolved target among edges to its
// real cross-repo node, then materialises the cross_repo_* parallel-edge
// layer. Shared by repository, legacy file, and batched files-plus-incoming
// scopes. Caller holds cr.mu.
func (cr *CrossRepoResolver) resolveScopedLocked(edges []*graph.Edge) *CrossRepoStats {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	pending := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if edge != nil && graph.IsUnresolvedTarget(edge.To) {
			pending = append(pending, edge)
		}
	}
	if len(pending) == 0 {
		return stats
	}

	cr.buildDirIndexes()
	defer cr.clearDirIndexes()
	cr.buildDepModuleIndex()
	defer cr.clearDepModuleIndex()
	cr.buildReachableReposIndex()
	defer cr.clearReachableReposIndex()
	cr.warmLookupCache(pending)
	defer cr.clearLookupCache()

	var reindexBatch []graph.EdgeReindex
	for _, edge := range pending {
		cr.resolveEdge(edge, stats, &reindexBatch)
	}
	if len(reindexBatch) > 0 {
		cr.graph.ReindexEdges(reindexBatch)
		// nil index: scoped batches are repo- or file-sized, direct
		// probes beat a whole-graph placeholder-source stream.
		reconcilePlaceholderSources(cr.graph, nil, reindexBatch)
	}
	return stats
}

// buildDirIndexes walks the graph once and populates two lookup maps
// used by resolveImport — the only resolution path that previously
// scanned every node per edge.
//
//   - dirIndex     keys on filepath.Dir(file.FilePath) for exact matches
//     (importPath equal to the file's directory).
//   - lastDirIndex keys on the last path component of that directory,
//     covering the common case where an import path is a single name
//     like "logger" and we want any file under .../logger/.
//
// These maps are torn down via clearDirIndexes when the pass completes
// so we don't keep ~N pointers alive between resolves.
func (cr *CrossRepoResolver) buildDirIndexes() {
	cr.dirIndex = make(map[string][]graph.FileNodeIdentity, 128)
	cr.lastDirIndex = make(map[string][]graph.FileNodeIdentity, 128)
	for file := range graph.FileNodeIdentitiesSeq(cr.graph, nil) {
		dir := filepath.Dir(file.FilePath)
		cr.dirIndex[dir] = append(cr.dirIndex[dir], file)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			cr.lastDirIndex[last] = append(cr.lastDirIndex[last], file)
		}
	}
}

// buildDepModuleIndex mirrors Resolver.buildDepModuleIndex — see that
// method for the full rationale. Cross-repo always scopes the lookup
// by callerRepo, so the same dep node reachable here is the one in the
// importing file's own go.mod.
func (cr *CrossRepoResolver) buildDepModuleIndex() {
	by := make(map[string][]depModuleEntry)
	for n := range graph.RepoNodeIdentitiesSeq(cr.graph, nil, graph.KindContract) {
		if !strings.HasPrefix(n.ID, "dep::") {
			continue
		}
		mp := strings.TrimPrefix(n.ID, "dep::")
		if mp == "" || strings.Contains(mp, "::") {
			continue
		}
		by[n.RepoPrefix] = append(by[n.RepoPrefix], depModuleEntry{
			modulePath: mp,
			nodeID:     n.ID,
		})
	}
	for k := range by {
		entries := by[k]
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].modulePath) > len(entries[j].modulePath)
		})
	}
	cr.depModuleIndex = by
}

func (cr *CrossRepoResolver) clearDepModuleIndex() {
	cr.depModuleIndex = nil
}

// lookupDepModule returns the dep::<module> contract ID whose module path is a
// prefix of importPath, scoped to callerRepo. Empty means no match.
func (cr *CrossRepoResolver) lookupDepModule(callerRepo, importPath string) string {
	for _, entry := range cr.depModuleIndex[callerRepo] {
		if importPath == entry.modulePath || strings.HasPrefix(importPath, entry.modulePath+"/") {
			return entry.nodeID
		}
	}
	return ""
}

func (cr *CrossRepoResolver) clearDirIndexes() {
	cr.dirIndex = nil
	cr.lastDirIndex = nil
}

// buildReachableReposIndex walks every resolved EdgeImports edge and
// records, per caller file, the set of repo prefixes that file imports.
// This is the positive evidence the cross-repo name-only fallbacks
// consult: a candidate in repo R is eligible for caller file F only
// when F imports R. Per-repo resolution (resolver.go) runs first and
// resolves imports — including cross-repo imports, with a precise
// import-path match — so by the time this index is built the import
// graph is settled enough to be trustworthy evidence.
func (cr *CrossRepoResolver) buildReachableReposIndex() {
	idx := make(map[string]map[string]struct{})
	// Materialise metadata-free import projections and batch-load only target
	// placement fields. A per-edge point lookup here is a query round-trip per
	// import on a disk backend, which under the cross-repo pass's import
	// population was a multi-minute cold-warmup stall.
	var imports []*graph.Edge
	ids := make(map[string]struct{})
	for e := range graph.EdgesLightSeq(cr.graph, graph.EdgeImports) {
		imports = append(imports, e)
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	if len(imports) == 0 {
		cr.reachableReposByFile = idx
		return
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	placements := graph.NodePlacementsByIDs(cr.graph, idList)
	for _, e := range imports {
		// Only resolved imports carry evidence — an unresolved import
		// target tells us nothing about which repo the caller reaches.
		to, ok := placements[e.To]
		if !ok || to.RepoPrefix == "" {
			continue
		}
		set := idx[e.From]
		if set == nil {
			set = make(map[string]struct{})
			idx[e.From] = set
		}
		set[to.RepoPrefix] = struct{}{}
	}
	cr.reachableReposByFile = idx
}

func (cr *CrossRepoResolver) clearReachableReposIndex() {
	cr.reachableReposByFile = nil
}

// reachabilityChecker returns a per-edge closure that reports whether the
// caller of e may reach a candidate in targetRepo. It captures the caller's
// repo + import-reachability set ONCE; the per-call repoReachable re-derived
// both via cachedGetNode on every candidate, so a common cross-repo name
// with thousands of candidates paid O(candidates) redundant cache lookups
// per edge — the bulk of cr's compute wall time. Same semantics as
// repoReachable; only the per-candidate cost differs.
func (cr *CrossRepoResolver) reachabilityChecker(e *graph.Edge) func(targetRepo string) bool {
	callerRepo := cr.callerRepoPrefix(e)
	reachableRepos := cr.reachableReposByFile[cr.callerFileID(e)]
	return func(targetRepo string) bool {
		if targetRepo == "" || targetRepo == callerRepo {
			return true
		}
		if reachableRepos == nil {
			return false
		}
		_, ok := reachableRepos[targetRepo]
		return ok
	}
}

// callerFileID returns the graph ID of the file that owns the edge's
// From symbol. File node IDs equal their path, and EdgeImports edges
// are keyed From=fileID, so this is the lookup key for
// reachableReposByFile. Falls back to the edge's own FilePath when the
// From node can't be resolved.
func (cr *CrossRepoResolver) callerFileID(e *graph.Edge) string {
	if from := cr.cachedGetNode(e.From); from != nil {
		if from.Kind == graph.KindFile {
			return from.ID
		}
		if from.FilePath != "" {
			return from.FilePath
		}
	}
	return e.FilePath
}

// resolveEdge dispatches one unresolved edge through the cross-repo
// resolution paths and, when the resolution lifted the To target,
// appends a re-bind job to batch instead of committing a per-edge
// ReindexEdge transaction. The caller flushes the accumulated batch
// after the whole pass via ReindexEdges so disk backends amortise
// the commit cost.
// warmLookupCache batches the per-edge GetNode / FindNodesByName the
// cross-repo worker loop would otherwise fire serially — the mirror of
// Resolver.warmLookupCache (resolver.go). It includes the authoritative
// negative: a queried name with no node records an empty result, so the
// 200k+ external-call stubs return from the cache instead of each
// scanning the unindexed name column (the warmup hang).
func (cr *CrossRepoResolver) warmLookupCache(pending []*graph.Edge) {
	if len(pending) == 0 {
		return
	}
	idSet := make(map[string]struct{}, len(pending))
	nameSet := make(map[string]struct{}, len(pending))
	qualNameSet := make(map[string]struct{})
	for _, e := range pending {
		if e == nil {
			continue
		}
		if e.From != "" {
			idSet[e.From] = struct{}{}
		}
		bare := graph.UnresolvedName(e.To)
		if name := identifierFromTarget(bare); name != "" {
			nameSet[name] = struct{}{}
		}
		// Seed the RAW unresolved name too. This is pure scan-avoidance and
		// changes no resolution outcome: the legit cross-repo matches use the
		// bare identifier (seeded above) and resolve fine. The problem is the
		// EXTERNAL / unresolvable residual that dominates this pass (stdlib +
		// out-of-tree "calls" that never match a node): resolveFunctionCall
		// looks them up by their full target (e.g. "extern::pkg::Foo"), which
		// the stripped pre-warm key ("Foo") didn't cover, so they missed the
		// cache and fell through to a per-edge FindNodesByName scan — the
		// parallel cross-repo storm. Seeding the raw form lets them hit the
		// authoritative negative instead of scanning.
		if bare != "" {
			nameSet[bare] = struct{}{}
		}
		// Import targets: mirror resolveEdge's dispatch (TrimPrefix of the
		// bare unresolved:: form) so the seeded name matches the complete
		// qualified-name candidate lookup used by resolveImport.
		if t := strings.TrimPrefix(e.To, unresolvedPrefix); strings.HasPrefix(t, "import::") {
			if qn := strings.TrimPrefix(t, "import::"); qn != "" {
				qualNameSet[qn] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	cr.nodeByID = cr.hotCachedNodesByIDs(ids)
	cr.nodesByName = cr.hotCachedNodesByNames(names)
	// Authoritative negatives: record an empty result for every queried
	// name that has no node, so the cached lookup returns empty instead
	// of falling through to a per-edge FindNodesByName scan.
	if cr.nodesByName == nil {
		cr.nodesByName = make(map[string][]*graph.Node, len(nameSet))
	}
	for n := range nameSet {
		if _, ok := cr.nodesByName[n]; !ok {
			cr.nodesByName[n] = nil
		}
	}
	// Fold every candidate node into the id cache too, so a downstream
	// GetNode on a chosen target hits instead of going to the store.
	if cr.nodeByID == nil && len(cr.nodesByName) > 0 {
		cr.nodeByID = make(map[string]*graph.Node, len(cr.nodesByName))
	}
	for _, hits := range cr.nodesByName {
		for _, n := range hits {
			if n == nil || n.ID == "" {
				continue
			}
			if _, ok := cr.nodeByID[n.ID]; !ok {
				cr.nodeByID[n.ID] = n
			}
		}
	}
	// Index the name hits by repo so resolveFunctionCall / resolveMethodCall
	// collect ONLY the caller's reachable-repo, same-language candidates
	// instead of fetching every same-named node across all repos + languages
	// and discarding the unreachable majority per edge (the cross-repo
	// candidate-iteration cost). Every pre-warmed name gets an entry (empty
	// for an authoritative negative) so scopedCandidates can distinguish
	// "pre-warmed, no node" (return empty) from "not pre-warmed" (fall
	// through to the flat cache).
	cr.nodesByNameRepo = make(map[string]map[string][]*graph.Node, len(cr.nodesByName))
	for name, hits := range cr.nodesByName {
		byRepo := make(map[string][]*graph.Node)
		for _, n := range hits {
			if n == nil {
				continue
			}
			byRepo[n.RepoPrefix] = append(byRepo[n.RepoPrefix], n)
		}
		cr.nodesByNameRepo[name] = byRepo
	}
	// Pre-warm every import qualified-name candidate plus authoritative
	// negatives, avoiding one indexed store probe per cross-repo import edge.
	if len(qualNameSet) > 0 {
		qns := make([]string, 0, len(qualNameSet))
		for q := range qualNameSet {
			qns = append(qns, q)
		}
		cr.nodesByQualName = cr.graph.GetNodesByQualNames(qns)
		if cr.nodesByQualName == nil {
			cr.nodesByQualName = make(map[string][]*graph.Node, len(qualNameSet))
		}
		for q := range qualNameSet {
			if _, ok := cr.nodesByQualName[q]; !ok {
				cr.nodesByQualName[q] = nil
			}
		}
	}
}

func (cr *CrossRepoResolver) clearLookupCache() {
	cr.nodeByID = nil
	cr.nodesByName = nil
	cr.nodesByNameRepo = nil
	cr.nodesByQualName = nil
}

// scopedCandidates returns the candidates named `name` the caller of e could
// plausibly resolve to: nodes in the caller's own repo, a repo its file
// imports (reachableReposByFile), or no repo (synthetic) — AND of the
// caller's language (a Go call can't bind a same-named TypeScript symbol).
// This applies the import + language prune at the SOURCE: cachedFindNodesByName
// returns every same-named node across all repos and languages (thousands for
// a common name), which the per-edge loops then iterate and discard; the
// per-pass name→repo index collects only the relevant few. Names absent from
// the index (not pre-warmed) fall through to the flat cache, preserving the
// negative-cache + correctness contract.
func (cr *CrossRepoResolver) scopedCandidates(e *graph.Edge, name string) []*graph.Node {
	byRepo, ok := cr.nodesByNameRepo[name]
	if !ok {
		return cr.cachedFindNodesByName(name)
	}
	if len(byRepo) == 0 {
		return nil // pre-warmed, no node (authoritative negative)
	}
	caller := cr.cachedGetNode(e.From)
	callerRepo, callerLang, callerFile := "", "", e.FilePath
	if caller != nil {
		callerRepo = caller.RepoPrefix
		callerLang = caller.Language
		if caller.Kind == graph.KindFile {
			callerFile = caller.ID
		} else if caller.FilePath != "" {
			callerFile = caller.FilePath
		}
	}
	reachableRepos := cr.reachableReposByFile[callerFile]
	var out []*graph.Node
	keep := func(repo string) {
		for _, n := range byRepo[repo] {
			if callerLang == "" || n.Language == "" || n.Language == callerLang {
				out = append(out, n)
			}
		}
	}
	keep(callerRepo)
	if callerRepo != "" {
		keep("") // synthetic / no-repo nodes are always reachable
	}
	for r := range reachableRepos {
		if r != callerRepo && r != "" {
			keep(r)
		}
	}
	return out
}

// cachedGetNode consults the per-pass id cache first, falling through to
// the store on a miss (positive-only: absence means "not pre-warmed").
func (cr *CrossRepoResolver) cachedGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if cr.nodeByID != nil {
		if n, ok := cr.nodeByID[id]; ok {
			return n
		}
	}
	return cr.graph.GetNode(id)
}

// cachedFindNodesByName consults the per-pass name cache first. A
// pre-warmed name with no node returns empty (authoritative negative);
// a name absent from the cache falls through to the store.
func (cr *CrossRepoResolver) cachedFindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	if cr.nodesByName != nil {
		if hits, ok := cr.nodesByName[name]; ok {
			return hits
		}
	}
	return cr.graph.FindNodesByName(name)
}

// cachedFindNodesByQualName serves resolveImport's complete candidate lookup
// from the per-pass cache, including authoritative negative slices.
func (cr *CrossRepoResolver) cachedFindNodesByQualName(qualName string) []*graph.Node {
	if qualName == "" {
		return nil
	}
	if cr.nodesByQualName != nil {
		if nodes, ok := cr.nodesByQualName[qualName]; ok {
			return nodes
		}
	}
	return cr.graph.GetNodesByQualNames([]string{qualName})[qualName]
}

func (cr *CrossRepoResolver) resolveEdge(e *graph.Edge, stats *CrossRepoStats, batch *[]graph.EdgeReindex) {
	oldTo := e.To
	// Shared with the master resolver: a derived tests clone is never
	// bound independently on any path (see resolutionExempt).
	if resolutionExempt(e) {
		stats.Unresolved++
		return
	}
	// UnresolvedName handles BOTH the bare `unresolved::X` and the
	// multi-repo `<repo>::unresolved::X` forms; a plain TrimPrefix only
	// strips the bare form, leaving prefixed stubs (which fix-1's widened
	// EdgesWithUnresolvedTarget now feeds this pass) with target=full-id —
	// so the lookup key matched no node and missed the per-pass name cache,
	// turning every prefixed stub into a futile per-edge FindNodesByName
	// scan. Mirrors the master Resolver.resolveEdge.
	target := graph.UnresolvedName(e.To)
	if target == "" {
		target = strings.TrimPrefix(e.To, unresolvedPrefix)
	}

	switch {
	case strings.HasPrefix(target, "import::"):
		cr.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		cr.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	case e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeComposes:
		// Type-hierarchy edges never resolve to a function/method.
		// CrossRepoResolver has no type-only resolution path, and a
		// cross-repo supertype requires the child's file to import the
		// parent's repo — which would have let per-repo resolution
		// (or a precise import) land it already. Leave it unresolved
		// rather than let resolveFunctionCall match a coincidental
		// cross-repo function of the same name.
		stats.Unresolved++
	default:
		cr.resolveFunctionCall(e, target, stats)
		// Last-resort cross-daemon proxy-edge stitch: only when local
		// resolution left the edge unresolved, proxy-edge minting is on,
		// and a prober is wired. The evidence gate inside tryRemoteStitch
		// refuses to probe on a bare name.
		if e.To == oldTo && cr.edgesEnabled && cr.prober != nil {
			cr.tryRemoteStitch(e, target, stats)
		}
	}

	if e.To != oldTo {
		*batch = append(*batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		cr.noteRetargetedCall(e)
	}
}

// filterLiveReindex validates a resolved batch before applying it on the
// chunked path: a concurrent edit during an inter-chunk yield may have evicted
// an edge (reindexing it half-resurrects it and can panic) or its resolved
// target node (a dangling edge). Drop evicted edges; revert a resolution whose
// target is gone. O(batch) — only the edges that actually resolved, NOT the
// whole pending set (an O(pending*out-degree) per-edge check stalled the pass).
func (cr *CrossRepoResolver) filterLiveReindex(batch []graph.EdgeReindex) []graph.EdgeReindex {
	sites := make([]graph.EdgeSite, 0, len(batch))
	targetIDs := make([]string, 0, len(batch))
	for _, reindex := range batch {
		if reindex.Edge == nil {
			continue
		}
		sites = append(sites, graph.EdgeSite{
			From: reindex.Edge.From, Line: reindex.Edge.Line, Kind: reindex.Edge.Kind,
		})
		if !isSyntheticResolveTarget(reindex.Edge.To) {
			targetIDs = append(targetIDs, reindex.Edge.To)
		}
	}
	liveCandidates := cr.graph.GetEdgeCandidates(nil, sites)
	targets := cr.graph.GetNodesByIDs(targetIDs)
	out := batch[:0]
	for _, reindex := range batch {
		edge := reindex.Edge
		if edge == nil {
			continue
		}
		live := false
		for _, candidate := range liveCandidates.Site(edge.From, edge.Line, edge.Kind) {
			if candidate == edge || (candidate != nil && candidate.To == reindex.OldTo && candidate.FilePath == edge.FilePath) {
				live = true
				break
			}
		}
		if !live {
			continue
		}
		if !isSyntheticResolveTarget(edge.To) && targets[edge.To] == nil {
			edge.To = reindex.OldTo
			continue
		}
		out = append(out, reindex)
	}
	return out
}

// isSyntheticResolveTarget reports whether a resolved target is an intentional
// placeholder rather than a concrete graph node (so the chunked path's
// target-liveness guard must not treat its absence as an evicted candidate).
func isSyntheticResolveTarget(to string) bool {
	return strings.HasPrefix(to, "external::") || strings.HasPrefix(to, "extern::")
}

// resolveChunkEnabled reports whether the global resolve passes process their
// pending set in super-chunks, releasing the resolve mutex between chunks so
// interactive single-file edits can interleave instead of waiting out the
// whole pass. Default ON; GORTEX_RESOLVE_CHUNK=0 restores the prior
// whole-pass-locked behaviour.
func resolveChunkEnabled() bool {
	if v := os.Getenv("GORTEX_RESOLVE_CHUNK"); v != "" {
		return v != "0" && !strings.EqualFold(v, "false")
	}
	return true
}

// resolveChunkSize is the number of pending edges resolved + applied per chunk
// before the resolve mutex is yielded. GORTEX_RESOLVE_CHUNK_SIZE overrides;
// default 2048 — large enough to amortise the per-chunk worker barrier, small
// enough that a waiting edit is delayed by at most one chunk's compute.
func resolveChunkSize() int {
	if v := os.Getenv("GORTEX_RESOLVE_CHUNK_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2048
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (cr *CrossRepoResolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := cr.cachedGetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}

// isCrossRepoHop reports whether resolving from callerRepo to targetRepo
// actually crosses a repository boundary.
//
// An empty targetRepo is NOT a crossing. Synthetic global externals —
// `dep::…`, `external::…`, non-Go `module::…` — carry no repo prefix because
// they model third-party symbols owned by no repository and visible from
// every one. Stamping Edge.CrossRepo on them inflates cross-repo edge counts
// and files them under stats.ByRepo[""], a key that names no repo, so
// `analyze kind=cross_repo` reports boundary crossings that never happened.
//
// This was invisible while a lone repo indexed unprefixed: caller and
// candidate were both "" so the same-repo tier claimed global externals
// first. Once every repo carries a prefix they fall through to the
// cross-repo fallback tier instead, where the reachability gate admits any
// empty target unconditionally.
func isCrossRepoHop(callerRepo, targetRepo string) bool {
	return targetRepo != "" && targetRepo != callerRepo
}

// csharpVerdictCaller reports whether e's unresolved state is the C#
// member gates' verdict, which no name-only tier may overturn. Keyed to
// exactly "csharp" — razor/fsharp callers never enter those gates, so
// blocking their fallback would only lose edges. A missing caller node
// fails CLOSED on the .cs file path: the dangling-From window is the
// eviction window, where resurrecting a refused bind matters most.
func (cr *CrossRepoResolver) csharpVerdictCaller(e *graph.Edge) bool {
	if cn := cr.cachedGetNode(e.From); cn != nil {
		return cn.Language == "csharp"
	}
	return strings.HasSuffix(e.FilePath, ".cs")
}

func (cr *CrossRepoResolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	candidates := cr.scopedCandidates(e, funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	// A restubbed C# member call travels as a bare name and lands here
	// instead of resolveMethodCall — the member_call evidence in Meta
	// still marks it as a member-gate verdict.
	if mc, _ := e.Meta["member_call"].(bool); mc && cr.csharpVerdictCaller(e) {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	reachable := cr.reachabilityChecker(e)

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback: first function/method match that clears
	// BOTH evidence gates —
	//   (a) import-reachability: the caller's file must actually import
	//       the candidate's repo. Without this, a bare name like `len`
	//       or `String` resolves to whichever repo sorts first.
	//   (b) workspace boundary: same-workspace cross-repo is allowed;
	//       cross-workspace requires a declared cross_workspace_deps
	//       entry covering the workspace pair.
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		stats.Resolved++
		if isCrossRepoHop(callerRepo, c.RepoPrefix) {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
		}
		return
	}

	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveImport(e *graph.Edge, importPath string, stats *CrossRepoStats) {
	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	ambiguousQualName := false

	// npm-alias rewrite: see Resolver.resolveImport. Applied here too
	// so a JS/TS import of an alias key resolves cross-repo to a
	// locally-vendored real package when the per-repo pass left it
	// unresolved.
	importPath, npmAliased := rewriteNpmAliasImport(cr.npmAlias, e.FilePath, importPath)

	// JS/TS relative + tsconfig-path-alias / baseUrl import: resolve the
	// specifier onto the in-repo file (or exported symbol) it names, the
	// same as Resolver.resolveImport. Aliases are repo-local, so this is
	// mostly a no-op once the per-repo pass has run; kept for parity so a
	// JS/TS import edge reaching the cross-repo pass still unresolved gets
	// the same treatment (issue #136).
	if to := resolveJSTSImportTarget(cr.cachedGetNode, cr.pathAlias, jsTSImportCallerFile(e), importPath); to != "" {
		e.To = to
		if picked := cr.cachedGetNode(to); picked != nil && isCrossRepoHop(callerRepo, picked.RepoPrefix) {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[picked.RepoPrefix]++
		}
		stats.Resolved++
		return
	}

	// Look at every package node with this qualified name. Exact caller repo,
	// then caller workspace, wins before cross-workspace dependency policy is
	// consulted; an eligible foreign row can no longer shadow a local worktree.
	if candidates := cr.cachedFindNodesByQualName(importPath); len(candidates) > 0 {
		picked, ambiguous := cr.pickQualNameCandidate(callerRepo, callerWS, importPath, candidates)
		if picked != nil {
			e.To = picked.ID
			if isCrossRepoHop(callerRepo, picked.RepoPrefix) {
				e.CrossRepo = true
				stats.CrossRepoEdges++
				stats.ByRepo[picked.RepoPrefix]++
			}
			stats.Resolved++
			return
		}
		ambiguousQualName = ambiguous
		// No unique policy-eligible candidate: retain directory/dependency
		// evidence fallbacks below before deciding that the import is ambiguous.
	}

	// Look for file nodes whose directory matches the import path. Two
	// inverted indexes (built once per Resolve* pass) replace what used
	// to be an O(N) scan of the entire graph per import edge.
	//
	// 1. Exact dir match — `dirIndex[importPath]` covers the case where
	//    the import literally equals a known directory.
	// 2. Last-component match — `lastDirIndex[lastPathComponent(...)]`
	//    covers the common case where an import path is just a name
	//    (e.g. "logger") and any file under .../logger/ is a candidate.
	//
	// Falls back to a full graph scan if the indexes are unset (defensive
	// — only happens when resolveImport is called outside a Resolve* pass).
	// When a package-manager workspace lookup is installed every
	// same-repo candidate is collected so a same-named collision
	// across two workspace members can be resolved to the importer's
	// own workspace; otherwise the first same-repo hit short-circuits
	// the scan as before.
	collectAll := cr.workspaceMembers != nil
	// A JS/TS bare specifier names a node_modules package or a workspace
	// member — only a directory entry point is evidence. Mirrors
	// Resolver.resolveImport; see isJSTSDirEntryPoint (issue #450).
	bareJSTS := isJSTSBareSpecifier(jsTSImportCallerFile(e), importPath)
	// Declared node_modules package — no in-repo directory is a candidate.
	// Mirrors Resolver.resolveImport; see declaresExternalNpmDep.
	externalNpmDep := bareJSTS && declaresExternalNpmDep(cr.npmDep, jsTSImportCallerFile(e), importPath)
	var sameRepo graph.FileNodeIdentity
	var sameRepoFound bool
	var sameRepoAll, crossRepoAll []graph.FileNodeIdentity
	consider := func(file graph.FileNodeIdentity) {
		if bareJSTS && !isJSTSDirEntryPoint(file.FilePath) {
			return
		}
		if file.RepoPrefix == callerRepo {
			if !sameRepoFound {
				sameRepo, sameRepoFound = file, true
			}
			if collectAll {
				sameRepoAll = append(sameRepoAll, file)
			}
			return
		}
		// Cross-repo file candidate: require a precise import-path
		// suffix match. lastDirIndex / the full-scan fallback key on the
		// last path component only, so without this gate an import of
		// `.../tree-sitter-c/bindings/go` resolves to whichever
		// `*/bindings/go` directory sorts first. Collect every match so
		// the workspace-aware pick below can prefer the importer's own
		// workspace instead of the first one encountered.
		if dirMatchesImport(filepath.Dir(file.FilePath), importPath) {
			crossRepoAll = append(crossRepoAll, file)
		}
	}
	stop := func() bool { return sameRepoFound && !collectAll }
	if externalNpmDep {
		// Declared node_modules package — skip the cascade outright.
	} else if cr.dirIndex != nil {
		for _, file := range cr.dirIndex[importPath] {
			consider(file)
			if stop() {
				break
			}
		}
		if !sameRepoFound || collectAll {
			for _, file := range cr.lastDirIndex[lastPathComponent(importPath)] {
				consider(file)
				if stop() {
					break
				}
			}
		}
	} else {
		for file := range graph.FileNodeIdentitiesSeq(cr.graph, nil) {
			dir := filepath.Dir(file.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(file)
				if stop() {
					break
				}
			}
		}
	}

	if sameRepoFound {
		// Name-collision tie-break: prefer the same-repo file in the
		// importing file's own package-manager workspace.
		if workspaceFile, ok := cr.preferSameWorkspaceFile(e.FilePath, sameRepoAll); ok {
			sameRepo = workspaceFile
		}
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	// Cross-repo directory match: prefer a candidate in the caller's own
	// workspace, then any the cross-workspace policy permits. Never bail
	// on the first ineligible candidate — a same-workspace instance (a
	// worktree of the imported module tracked under its own prefix) may
	// appear later in the list.
	if picked, ok := cr.pickImportCandidate(callerWS, importPath, crossRepoAll); ok {
		e.To = picked.ID
		stats.Resolved++
		if isCrossRepoHop(callerRepo, picked.RepoPrefix) {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[picked.RepoPrefix]++
		}
		return
	}

	// No file node matched. Try the dep::<module> contract from the
	// caller's go.mod before giving up. The dep node lives in the
	// caller's own repo, so this is a same-repo edge.
	if depNodeID := cr.lookupDepModule(callerRepo, importPath); depNodeID != "" {
		e.To = depNodeID
		stats.Resolved++
		return
	}

	// npm-alias sub-path: a rewritten import like `@acme/shared-lib/util`
	// addresses a path inside the real package — fall back to the
	// package node itself. See Resolver.resolveImport.
	if npmAliased {
		if pkg := npmPackagePrefix(importPath); pkg != "" {
			if candidates := cr.cachedFindNodesByQualName(pkg); len(candidates) > 0 {
				node, ambiguous := cr.pickQualNameCandidate(callerRepo, callerWS, pkg, candidates)
				if node != nil {
					e.To = node.ID
					if isCrossRepoHop(callerRepo, node.RepoPrefix) {
						e.CrossRepo = true
						stats.CrossRepoEdges++
						stats.ByRepo[node.RepoPrefix]++
					}
					stats.Resolved++
					return
				}
				ambiguousQualName = ambiguousQualName || ambiguous
			}
		}
	}

	if ambiguousQualName {
		stats.Unresolved++
		return
	}

	// External/unresolvable import.
	e.To = "external::" + importPath
	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveMethodCall(e *graph.Edge, methodName string, stats *CrossRepoStats) {
	candidates := cr.scopedCandidates(e, methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	receiverType := edgeReceiverType(e)
	reachable := cr.reachabilityChecker(e)

	// If we have a type hint, try exact type match first. Both exact-type
	// tiers stamp OriginASTResolved like the main resolver's Passes 1/2 —
	// unstamped, their receiver-typed evidence would be indistinguishable
	// from the name-tier binds semantic enrichment is allowed to reclaim.
	if receiverType != "" {
		// Same-repo + exact type.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod &&
				c.RepoPrefix == callerRepo &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				e.Origin = graph.OriginASTResolved
				stats.Resolved++
				return
			}
		}
		// Cross-repo + exact type — bounded by the import-reachability
		// and workspace evidence gates.
		for _, c := range candidates {
			if c.Kind != graph.KindMethod || nodeReceiverType(c) != receiverType {
				continue
			}
			if !reachable(c.RepoPrefix) {
				continue
			}
			if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
				continue
			}
			e.To = c.ID
			e.Confidence = 0.85
			e.Origin = graph.OriginASTResolved
			stats.Resolved++
			if isCrossRepoHop(callerRepo, c.RepoPrefix) {
				e.CrossRepo = true
				stats.CrossRepoEdges++
				stats.ByRepo[c.RepoPrefix]++
			}
			return
		}
	}

	// Fallback: name-only matching (methods first, then functions for pkg.Func() calls).
	//
	// C# member calls never take it: the main resolver owns C# member
	// semantics (instance-member precedence, receiver shape, using
	// visibility) and leaves a call unresolved as a VERDICT — ambiguity
	// or provable inapplicability. A name-only bind here would resurrect
	// exactly the misbinds those gates refuse (an extension from an
	// invisible namespace, a shape-conflicted overload) with no
	// confidence and no origin. The exact-receiver tiers above remain
	// available to C#.
	if cr.csharpVerdictCaller(e) {
		stats.Unresolved++
		return
	}
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindMethod {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		stats.Resolved++
		if isCrossRepoHop(callerRepo, c.RepoPrefix) {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
		}
		return
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		stats.Resolved++
		if isCrossRepoHop(callerRepo, c.RepoPrefix) {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
		}
		return
	}

	stats.Unresolved++
}

// hotCacheNameSpace keeps cross-repo global name buckets from colliding with
// the master resolver's repository/language-scoped keys in a shared cache.
const hotCacheNameSpace = "xrepo"

// hotCachedNodesByIDs answers as many IDs as possible from the pass-scoped
// hot cache and fetches only the misses. Result shape matches
// graph.GetNodesByIDs: positives only.
func (cr *CrossRepoResolver) hotCachedNodesByIDs(ids []string) map[string]*graph.Node {
	if cr.hotCache == nil {
		return cr.graph.GetNodesByIDs(ids)
	}
	out := make(map[string]*graph.Node, len(ids))
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := cr.hotCache.getNode(id); ok {
			out[id] = n
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		for id, n := range cr.graph.GetNodesByIDs(missing) {
			if n == nil {
				continue
			}
			out[id] = n
			cr.hotCache.putNode(n)
		}
	}
	return out
}

// hotCachedNodesByNames answers global name buckets from the pass-scoped hot
// cache, including cached negatives — no nodes are created while the pass
// holds its mutex, and an interleaving writer flushes the cache at the chunk
// yield. Only cache misses reach the store.
func (cr *CrossRepoResolver) hotCachedNodesByNames(names []string) map[string][]*graph.Node {
	if cr.hotCache == nil {
		return cr.graph.FindNodesByNames(names)
	}
	out := make(map[string][]*graph.Node, len(names))
	missing := make([]string, 0, len(names))
	for _, name := range names {
		if hits, ok := cr.hotCache.getNames(hotNameKey("", hotCacheNameSpace, name)); ok {
			if hits != nil {
				out[name] = hits
			}
		} else {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fetched := cr.graph.FindNodesByNames(missing)
		for _, name := range missing {
			hits := fetched[name]
			if hits != nil {
				out[name] = hits
			}
			cr.hotCache.putNames(hotNameKey("", hotCacheNameSpace, name), hits)
		}
	}
	return out
}
