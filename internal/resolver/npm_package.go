package resolver

import (
	"context"
	"iter"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// NpmPackageTarget records a manifest claim even when Entries is empty or
// none of its graph-path candidates is indexed. WorkspaceID is independent of
// entry probing, so unindexed providers still count in their workspace tier.
type NpmPackageTarget struct {
	WorkspaceID string
	Entries     []string
}

// NpmPackageLookup maps a bare npm specifier to tracked package directories.
// The indexer owns manifests and workspace placement; the resolver probes the
// indexed entry candidates and applies repository/workspace policy.
type NpmPackageLookup func(specifier string) map[string]NpmPackageTarget

// NpmImportSpecifierMeta preserves the original npm import across provider eviction.
const NpmImportSpecifierMeta = "npm_import"

// SetNpmPackageLookup installs tracked-package evidence for the cross-repo pass.
// Call before resolution; nil keeps the external-package short-circuit unchanged.
func (cr *CrossRepoResolver) SetNpmPackageLookup(fn NpmPackageLookup) {
	cr.npmPackages = fn
}

// NpmRequireRecheck scopes the whole-graph re-check of require-bound
// cross-repo calls. It returns the repositories whose calls need re-checking
// (nil: every repository) and a done func the pass calls after a complete,
// uncancelled re-check. A call bound through a tracked npm package targets a
// repository that provided the package when it was bound, so while the
// package set is the one the last complete re-check saw, calls into other
// repositories cannot have gone stale.
type NpmRequireRecheck func() (targetRepos []string, done func())

// SetNpmRequireRecheck installs the re-check scope; nil re-checks every
// JS/TS cross-repo call.
func (cr *CrossRepoResolver) SetNpmRequireRecheck(fn NpmRequireRecheck) {
	cr.npmRequireRecheck = fn
}

type npmProbeKey struct {
	dir, spec string
}

type npmImportMemo struct {
	probes map[npmProbeKey]*graph.Node
	nodes  map[string]*graph.Node
}

func (m *npmImportMemo) getNode(cr *CrossRepoResolver, id string) *graph.Node {
	if node, seen := m.nodes[id]; seen {
		return node
	}
	node := cr.requireGetNode(id)
	m.nodes[id] = node
	return node
}

func (cr *CrossRepoResolver) trackedNpmImport(callerRepo, callerWS, importPath string, memo *npmImportMemo) *graph.Node {
	return cr.trackedNpmImportWith(callerRepo, callerWS, importPath, memo, func(id string) *graph.Node {
		return memo.getNode(cr, id)
	})
}

func (cr *CrossRepoResolver) trackedNpmImportWith(callerRepo, callerWS, importPath string, memo *npmImportMemo, getNode func(string) *graph.Node) *graph.Node {
	spec, symbol := splitImportSpecSymbol(importPath)
	packages := cr.npmPackages(spec)
	// Manifest ownership, not a successful probe, blocks linking a monorepo
	// import to a clone or fork of its own workspace package.
	for dir := range packages {
		if dir == callerRepo || strings.HasPrefix(dir, callerRepo+"/") {
			return nil
		}
	}
	var local, foreign []string
	for dir, pkg := range packages {
		ws := pkg.WorkspaceID
		if ws == "" {
			ws, _, _ = strings.Cut(dir, "/")
		}
		if ws == callerWS {
			local = append(local, dir)
		} else if cr.crossWorkspaceEligible(callerWS, ws, spec) {
			foreign = append(foreign, dir)
		}
	}
	candidates := local
	if len(candidates) == 0 {
		candidates = foreign
	}
	// Manifest claims, including unindexed entries, decide ambiguity.
	if len(candidates) != 1 {
		return nil
	}
	dir := candidates[0]
	key := npmProbeKey{dir: dir, spec: spec}
	var node *graph.Node
	var seen bool
	if memo != nil {
		node, seen = memo.probes[key]
	} else {
		cr.requireCacheMu.RLock()
		node, seen = cr.npmProbeCache[key]
		cr.requireCacheMu.RUnlock()
	}
	if !seen {
		for _, stem := range packages[dir].Entries {
			if file := probeJSTSFile(getNode, stem); file != "" {
				node = getNode(file)
				break
			}
		}
		if memo != nil {
			memo.probes[key] = node
		} else {
			cr.requireCacheMu.Lock()
			if cr.npmProbeCache == nil {
				cr.npmProbeCache = make(map[npmProbeKey]*graph.Node)
			}
			cr.npmProbeCache[key] = node
			cr.requireCacheMu.Unlock()
		}
	}
	if node == nil {
		return nil
	}
	if symbol != "" {
		exported := getNode(node.ID + "::" + symbol)
		if exported == nil || exported.Meta["visibility"] != "public" {
			return nil
		}
		node = exported
	}
	return node
}

// The per-repo pass has already made unindexed npm imports external. Retry only
// imports with positive tracked-package evidence before building call reachability.
func (cr *CrossRepoResolver) resolveTrackedNpmImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats) error {
	_, err := cr.resolveTrackedNpmImportsPass(ctx, edges, nil, false, stats)
	return err
}

func (cr *CrossRepoResolver) resolveTrackedNpmImportsScoped(ctx context.Context, edges []*graph.Edge, stats *CrossRepoStats) ([]*graph.Edge, error) {
	return cr.resolveTrackedNpmImportsPass(ctx, slices.Values(edges), edges, true, stats)
}

func (cr *CrossRepoResolver) resolveTrackedNpmImportsPass(ctx context.Context, edges iter.Seq[*graph.Edge], scopedEdges []*graph.Edge, scoped bool, stats *CrossRepoStats) ([]*graph.Edge, error) {
	defer cr.clearRequireCache()
	if cr.npmPackages == nil {
		return nil, ctx.Err()
	}
	// Scan metadata-free projections. Hydrate only imports whose package name
	// is tracked, so ordinary node_modules dependencies stay on the cheap path.
	var eligible []*graph.Edge
	for e := range edges {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e == nil || e.Kind != graph.EdgeImports {
			continue
		}
		spec := trackedNpmImportSpec(e)
		if marked, _ := e.Meta[NpmImportSpecifierMeta].(string); marked != "" && e.CrossRepo {
			eligible = append(eligible, e)
			continue
		}
		if spec == "" {
			// Light edges omit Meta. Hydrate resolved cross-repo JS/TS
			// imports so the persisted npm marker can be rechecked too.
			if e.CrossRepo && isJSTSPath(jsTSImportCallerFile(e)) {
				eligible = append(eligible, e)
			}
			continue
		}
		if !cr.eligibleTrackedNpmImport(e, spec) {
			continue
		}
		module, _ := splitImportSpecSymbol(spec)
		if len(cr.npmPackages(module)) != 0 {
			eligible = append(eligible, e)
		}
	}
	memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
	var restubbed []*graph.Edge
	var alreadyRestubbed map[graph.EdgeIdentity]struct{}
	if scoped {
		alreadyRestubbed = make(map[graph.EdgeIdentity]struct{})
	}
	// Close the light-edge cursor before reading or writing full edges. Hydrate
	// only one bounded page at a time; whole-graph passes never accumulate
	// hydrated edges across pages.
	for start := 0; start < len(eligible); start += resolvePendingPageRows {
		if err := ctx.Err(); err != nil {
			return restubbed, err
		}
		page := eligible[start:min(start+resolvePendingPageRows, len(eligible))]
		sites := make([]graph.EdgeSite, 0, len(page))
		for _, e := range page {
			sites = append(sites, graph.EdgeSite{From: e.From, Line: e.Line, Kind: graph.EdgeImports})
		}
		full := cr.graph.GetEdgeCandidates(nil, sites)
		var batch []graph.EdgeReindex
		for _, projected := range page {
			for _, e := range full.Site(projected.From, projected.Line, graph.EdgeImports) {
				if e.To != projected.To {
					continue
				}
				spec := trackedNpmImportSpec(e)
				if spec == "" {
					continue // A non-npm cross-repo import keeps its semantics.
				}
				oldTo := e.To
				if cr.resolveTrackedNpmImport(e, spec, stats, memo) {
					batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
					projected.To, projected.CrossRepo = e.To, e.CrossRepo
				}
			}
		}
		if len(batch) > 0 {
			cr.graph.ReindexEdges(batch)
			changed := cr.restubCallsForChangedNpmImports(batch, alreadyRestubbed)
			if scoped {
				restubbed = append(restubbed, changed...)
			}
		}
	}
	// Re-check cross-repo local require() calls (in both JS and TS, where TS
	// emits no EdgeImports) even when eligible is empty, so provider retargets
	// or ambiguity restub them before the pending call pass. Scoped passes only
	// inspect in-scope edges so unrelated consumers are never orphaned.
	if scoped {
		restubbed = append(restubbed, cr.restubStaleTrackedNpmRequireCallsScoped(ctx, scopedEdges, alreadyRestubbed, memo)...)
	} else {
		cr.restubStaleTrackedNpmRequireCallsAll(ctx, memo)
	}
	return restubbed, ctx.Err()
}

func resetRestubbedNpmCall(call *graph.Edge, name string) {
	delete(call.Meta, NpmImportSpecifierMeta)
	delete(call.Meta, "resolution")
	clearEdgeTerminal(call)
	call.To = graph.UnresolvedMarker + name
	call.CrossRepo = false
	call.Origin, call.Tier, call.Confidence, call.ConfidenceLabel = "", "", 0, ""
}

func requireRestubLocalName(call *graph.Edge, getNode func(string) *graph.Node) string {
	if call == nil {
		return ""
	}
	if local, _ := call.Meta[graph.JSLexicalRequireLocalMeta].(string); local != "" {
		return local
	}
	_, name, hasSym := strings.Cut(call.To, "::")
	if (!hasSym || name == "") && getNode != nil {
		if n := getNode(call.To); n != nil {
			name = n.Name
		}
	}
	return name
}

func (cr *CrossRepoResolver) staleTrackedNpmRequireCallName(call *graph.Edge, memo *npmImportMemo) (string, bool) {
	if call == nil || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
		return "", false
	}
	marked, _ := call.Meta[NpmImportSpecifierMeta].(string)
	reqSpec, _ := call.Meta[graph.JSLexicalRequireSpecMeta].(string)
	spec := marked
	if spec == "" {
		spec = reqSpec
	}
	if spec == "" {
		return "", false
	}
	memoGetNode := func(id string) *graph.Node {
		return memo.getNode(cr, id)
	}
	from := memoGetNode(call.From)
	callerRepo := ""
	if from != nil {
		callerRepo = from.RepoPrefix
	}
	callerWS := candidateWorkspaceID(from)
	localName, _ := call.Meta[graph.JSLexicalRequireLocalMeta].(string)
	lookupName, _ := call.Meta[graph.JSLexicalRequireSymbolMeta].(string)
	if lookupName == "" {
		lookupName = localName
	}
	if lookupName == "" {
		_, sym, hasSym := strings.Cut(call.To, "::")
		if hasSym && sym != "" {
			lookupName = sym
		} else if n := memoGetNode(call.To); n != nil {
			lookupName = n.Name
		}
	}

	stillValid := false
	if reqSpec != "" && marked != "" && reqSpec != marked {
		// Indirect require (local barrel, chained tracked-npm barrel, or
		// npm-aliased dependency): bypass eligibleTrackedNpmImport(call, marked)
		// on call.FilePath, re-evaluate reqSpec from call.FilePath, and run the
		// shared Steps 1-5 resolution with innermost-hop trackedNpmSpec.
		callerFile := call.FilePath
		if callerFile == "" && from != nil {
			callerFile = from.FilePath
		}
		resolved := cr.resolveJSRequireTargetMemo(call, callerFile, callerRepo, callerWS, reqSpec, memo, memoGetNode)
		if pickedFile := nodeFilePathWith(memoGetNode, resolved.target); pickedFile != "" && lookupName != "" {
			env := cr.jsRequireEnvMemo(callerRepo, callerWS, memo, memoGetNode)
			pick, resolvedNpmSpec := resolveJSRequireFromTargetFile(env, pickedFile, lookupName, resolved.trackedNpmSpec)
			if pick != nil && pick.ID == call.To && resolvedNpmSpec == marked {
				stillValid = true
			}
		}
	} else {
		eligible := cr.eligibleTrackedNpmImport(call, spec)
		if marked == "" && !eligible {
			return "", false
		}
		if eligible {
			if picked := cr.trackedNpmImportWith(callerRepo, callerWS, spec, memo, memoGetNode); picked != nil {
				if pickedFile := nodeFilePathWith(memoGetNode, picked.ID); pickedFile != "" {
					if localName != "" {
						env := cr.jsRequireEnvMemo(callerRepo, callerWS, memo, memoGetNode)
						pick, resolvedNpmSpec := resolveJSRequireFromTargetFile(env, pickedFile, lookupName, spec)
						if pick != nil && pick.ID == call.To && (marked == "" || resolvedNpmSpec == marked) {
							stillValid = true
						}
					} else {
						targetFile := nodeFilePathWith(memoGetNode, call.To)
						if targetFile != "" {
							if targetFile == pickedFile {
								stillValid = true
							} else {
								_, stillValid = cr.importClosureFilesMemo(picked.ID, memo, memoGetNode)[targetFile]
							}
						}
					}
				}
			}
		}
	}
	if stillValid {
		return "", false
	}
	name := requireRestubLocalName(call, memoGetNode)
	if name == "" {
		return "", false
	}
	return name, true
}

// restubStaleTrackedNpmRequireCallsScoped re-evaluates only in-scope cross-repo
// local require() calls so scoped passes never orphan unrelated consumers.
func (cr *CrossRepoResolver) restubStaleTrackedNpmRequireCallsScoped(
	ctx context.Context,
	edges []*graph.Edge,
	alreadyRestubbed map[graph.EdgeIdentity]struct{},
	memo *npmImportMemo,
) []*graph.Edge {
	if cr.npmPackages == nil || len(edges) == 0 {
		return nil
	}
	var batch []graph.EdgeReindex
	var obsolete []*graph.Edge
	var restubbed []*graph.Edge
	seen := make(map[graph.EdgeIdentity]struct{})
	for _, call := range edges {
		if ctx.Err() != nil {
			break
		}
		if call == nil || call.Kind != graph.EdgeCalls || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
			continue
		}
		id := graph.EdgeIdentityFor(call)
		if _, done := alreadyRestubbed[id]; done {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		name, stale := cr.staleTrackedNpmRequireCallName(call, memo)
		if !stale {
			continue
		}
		oldTo := call.To
		parallel := *call
		parallel.Kind = graph.EdgeCrossRepoCalls
		obsolete = append(obsolete, &parallel)
		resetRestubbedNpmCall(call, name)
		batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: oldTo})
		restubbed = append(restubbed, call)
	}
	if len(batch) > 0 {
		graph.RemoveEdgesExact(cr.graph, obsolete)
		cr.graph.ReindexEdges(batch)
	}
	return restubbed
}

// restubStaleTrackedNpmRequireCallsAll re-evaluates resolved cross-repo local
// require() calls graph-wide in bounded pages, flushing each page
// (RemoveEdgesExact + ReindexEdges) before checking cancellation for the next
// page so cancellation never leaves mutated live edges un-reindexed.
func (cr *CrossRepoResolver) restubStaleTrackedNpmRequireCallsAll(ctx context.Context, memo *npmImportMemo) {
	if cr.npmPackages == nil {
		return
	}
	var targetRepos []string
	var done func()
	if cr.npmRequireRecheck != nil {
		targetRepos, done = cr.npmRequireRecheck()
	}
	if targetRepos != nil && len(targetRepos) == 0 {
		if done != nil && ctx.Err() == nil {
			done()
		}
		return
	}
	var lightSeq iter.Seq[*graph.Edge]
	var lightErr func() error
	if checked, ok := cr.graph.(graph.CheckedLightEdgeSequencer); ok {
		lightSeq, lightErr = checked.EdgesLightSeqErr(ctx, graph.EdgeCrossRepoCalls)
	} else {
		lightSeq = graph.EdgesLightSeq(cr.graph, graph.EdgeCrossRepoCalls)
	}
	var sites []graph.EdgeSite
	siteSeen := make(map[graph.EdgeSite]struct{})
	for e := range lightSeq {
		if ctx.Err() != nil {
			return
		}
		if e == nil || (e.Origin != graph.OriginASTResolved && e.Origin != graph.OriginLSPResolved) {
			continue
		}
		if !isJSTSPath(e.FilePath) || (targetRepos != nil && !idInRepos(e.To, targetRepos)) {
			continue
		}
		site := graph.EdgeSite{From: e.From, Line: e.Line, Kind: graph.EdgeCalls}
		if _, dup := siteSeen[site]; dup {
			continue
		}
		siteSeen[site] = struct{}{}
		sites = append(sites, site)
	}
	if lightErr != nil {
		if err := lightErr(); err != nil {
			if cr.logger != nil {
				cr.logger.Debug("cross-repo resolve: require re-check aborted", zap.Error(err))
			}
			return
		}
	}
	checkedCandidates, hasCheckedCandidates := cr.graph.(graph.CheckedEdgeCandidateLookup)
	seen := make(map[graph.EdgeIdentity]struct{})
	for start := 0; start < len(sites); start += resolvePendingPageRows {
		if ctx.Err() != nil {
			return
		}
		page := sites[start:min(start+resolvePendingPageRows, len(sites))]
		var full graph.EdgeCandidateSet
		if hasCheckedCandidates {
			var err error
			full, err = checkedCandidates.GetEdgeCandidatesErr(ctx, nil, page)
			if err != nil {
				if cr.logger != nil {
					cr.logger.Debug("cross-repo resolve: require re-check aborted", zap.Error(err))
				}
				return
			}
		} else {
			full = cr.graph.GetEdgeCandidates(nil, page)
		}
		// Batch-warm caller, target, and file nodes for require() calls on this
		// page in one store query instead of N point GetNode calls.
		var warmIDs []string
		warmSeen := make(map[string]struct{})
		addWarm := func(id string) {
			if id == "" {
				return
			}
			if _, ok := memo.nodes[id]; ok {
				return
			}
			if _, dup := warmSeen[id]; dup {
				return
			}
			warmSeen[id] = struct{}{}
			warmIDs = append(warmIDs, id)
		}
		for _, site := range page {
			for _, call := range full.Site(site.From, site.Line, graph.EdgeCalls) {
				if call == nil || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
					continue
				}
				addWarm(call.From)
				addWarm(call.To)
				addWarm(call.FilePath)
			}
		}
		if len(warmIDs) > 0 {
			loaded := cr.graph.GetNodesByIDs(warmIDs)
			for _, id := range warmIDs {
				memo.nodes[id] = loaded[id]
			}
		}
		var batch []graph.EdgeReindex
		var obsolete []*graph.Edge
		for _, site := range page {
			for _, call := range full.Site(site.From, site.Line, graph.EdgeCalls) {
				if call == nil || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
					continue
				}
				id := graph.EdgeIdentityFor(call)
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				name, stale := cr.staleTrackedNpmRequireCallName(call, memo)
				if !stale {
					continue
				}
				oldTo := call.To
				parallel := *call
				parallel.Kind = graph.EdgeCrossRepoCalls
				obsolete = append(obsolete, &parallel)
				resetRestubbedNpmCall(call, name)
				batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: oldTo})
			}
		}
		if len(batch) > 0 {
			graph.RemoveEdgesExact(cr.graph, obsolete)
			cr.graph.ReindexEdges(batch)
		}
	}
	if done != nil && ctx.Err() == nil {
		done()
	}
}

// idInRepos reports whether id belongs to one of repos.
func idInRepos(id string, repos []string) bool {
	for _, repo := range repos {
		if strings.HasPrefix(id, repo+"/") {
			return true
		}
	}
	return false
}

// A call resolved through a binding cannot outlive that binding's target.
func (cr *CrossRepoResolver) restubCallsForChangedNpmImports(changes []graph.EdgeReindex, recorded map[graph.EdgeIdentity]struct{}) []*graph.Edge {
	callers := make(map[string]map[string]string)
	changedModuleSpecs := make(map[string]map[string]struct{})
	var ids []string
	for _, change := range changes {
		if strings.HasPrefix(change.OldTo, "external::") || graph.IsUnresolvedTarget(change.OldTo) {
			continue
		}
		if !strings.Contains(change.OldTo, "::") {
			if spec := trackedNpmImportSpec(change.Edge); spec != "" {
				mod, _ := splitImportSpecSymbol(spec)
				if changedModuleSpecs[change.Edge.From] == nil {
					changedModuleSpecs[change.Edge.From] = make(map[string]struct{})
				}
				changedModuleSpecs[change.Edge.From][mod] = struct{}{}
			}
			continue
		}
		if callers[change.OldTo] == nil {
			callers[change.OldTo] = make(map[string]string)
			ids = append(ids, change.OldTo)
		}
		_, name, _ := strings.Cut(change.OldTo, "::")
		if alias, _ := change.Edge.Meta["import_alias"].(string); alias != "" {
			name = alias
		}
		callers[change.OldTo][change.Edge.From] = name
	}
	var batch []graph.EdgeReindex
	var obsolete []*graph.Edge
	var restubbedList []*graph.Edge
	restubbed := make(map[graph.EdgeIdentity]struct{})
	recordID := func(id graph.EdgeIdentity) {
		restubbed[id] = struct{}{}
		if recorded != nil {
			recorded[id] = struct{}{}
		}
	}
	if len(ids) > 0 {
		for target, edges := range cr.graph.GetInEdgesByNodeIDs(ids) {
			for _, call := range edges {
				if call == nil || call.Kind != graph.EdgeCalls || !call.CrossRepo {
					continue
				}
				// Local require() bindings carry their own symbol name and must
				// never inherit an ES import alias from a sibling import statement.
				if call.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
					continue
				}
				name, affected := callers[target][cr.callerFileID(call)]
				if !affected {
					continue
				}
				recordID(graph.EdgeIdentityFor(call))
				parallel := *call
				parallel.Kind = graph.EdgeCrossRepoCalls
				obsolete = append(obsolete, &parallel)
				graph.StashRestubProvenance(call)
				call.To = graph.UnresolvedMarker + name
				call.CrossRepo, call.ConfidenceLabel = false, ""
				batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: target})
				restubbedList = append(restubbedList, call)
			}
		}
	}
	for callerFile, specs := range changedModuleSpecs {
		nodes := cr.graph.GetFileNodes(callerFile)
		nodeIDs := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if n != nil {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
		for _, edges := range graph.OutEdgesForNodes(cr.graph, nodeIDs) {
			for _, call := range edges {
				if call == nil || call.Kind != graph.EdgeCalls || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
					continue
				}
				id := graph.EdgeIdentityFor(call)
				if _, done := restubbed[id]; done {
					continue
				}
				marked, _ := call.Meta[NpmImportSpecifierMeta].(string)
				spec, _ := call.Meta[graph.JSLexicalRequireSpecMeta].(string)
				markedMod, _ := splitImportSpecSymbol(marked)
				specMod, _ := splitImportSpecSymbol(spec)
				_, hitMarked := specs[markedMod]
				_, hitSpec := specs[specMod]
				if !hitMarked && !hitSpec {
					continue
				}
				oldTo := call.To
				name := requireRestubLocalName(call, cr.requireGetNode)
				if name == "" {
					continue
				}
				recordID(id)
				parallel := *call
				parallel.Kind = graph.EdgeCrossRepoCalls
				obsolete = append(obsolete, &parallel)
				resetRestubbedNpmCall(call, name)
				batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: oldTo})
				restubbedList = append(restubbedList, call)
			}
		}
	}
	if len(batch) > 0 {
		graph.RemoveEdgesExact(cr.graph, obsolete)
		cr.graph.ReindexEdges(batch)
	}
	return restubbedList
}

func trackedNpmRequireCall(e *graph.Edge) bool {
	if e == nil || e.Kind != graph.EdgeCalls || !e.CrossRepo || e.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
		return false
	}
	spec, _ := e.Meta[NpmImportSpecifierMeta].(string)
	return spec != ""
}

func trackedNpmImportSpec(e *graph.Edge) string {
	if e == nil || e.Kind != graph.EdgeImports {
		return ""
	}
	if spec, _ := e.Meta[NpmImportSpecifierMeta].(string); spec != "" {
		return spec
	}
	if strings.HasPrefix(e.To, "external::") {
		return strings.TrimPrefix(e.To, "external::")
	}
	if target := graph.UnresolvedName(e.To); strings.HasPrefix(target, "import::") {
		return strings.TrimPrefix(target, "import::")
	}
	return ""
}

func (cr *CrossRepoResolver) resolveTrackedNpmImport(e *graph.Edge, importPath string, stats *CrossRepoStats, memo *npmImportMemo) bool {
	from := memo.getNode(cr, e.From)
	callerRepo := ""
	if from != nil {
		callerRepo = from.RepoPrefix
	}
	callerWS := candidateWorkspaceID(from)
	previousMarker, _ := e.Meta[NpmImportSpecifierMeta].(string)
	var picked *graph.Node
	if cr.eligibleTrackedNpmImport(e, importPath) {
		picked = cr.trackedNpmImport(callerRepo, callerWS, importPath, memo)
		module, _ := splitImportSpecSymbol(importPath)
		if len(cr.npmPackages(module)) > 0 {
			if e.Meta == nil {
				e.Meta = make(map[string]any)
			}
			e.Meta[NpmImportSpecifierMeta] = importPath
		}
	}
	if picked == nil {
		spec, _ := e.Meta[NpmImportSpecifierMeta].(string)
		if module, _ := splitImportSpecSymbol(spec); spec != "" && len(cr.npmPackages(module)) == 0 {
			// No tracked package claims the import any more; a cold index
			// parks it without the marker.
			delete(e.Meta, NpmImportSpecifierMeta)
			e.To = "external::" + spec
			e.CrossRepo = false
			return true
		}
		if spec != "" && e.To != "external::"+spec {
			e.To = "external::" + spec
			e.CrossRepo = false
			return true
		}
		marked, _ := e.Meta[NpmImportSpecifierMeta].(string)
		return marked != previousMarker
	}
	if e.To == picked.ID {
		return false
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta[NpmImportSpecifierMeta] = importPath
	e.To = picked.ID
	e.CrossRepo = true
	stats.Resolved++
	stats.CrossRepoEdges++
	stats.ByRepo[picked.RepoPrefix]++
	return true
}

// These checks use only the light projection and must precede manifest scans
// and full-edge hydration.
func (cr *CrossRepoResolver) eligibleTrackedNpmImport(e *graph.Edge, importPath string) bool {
	callerFile := jsTSImportCallerFile(e)
	if !isJSTSBareSpecifier(callerFile, importPath) || !declaresExternalNpmDep(cr.npmDep, callerFile, importPath) {
		return false
	}
	spec, _ := splitImportSpecSymbol(importPath)
	_, aliased := rewriteNpmAliasImport(cr.npmAlias, callerFile, spec)
	return !aliased
}
