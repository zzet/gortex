package resolver

import (
	"context"
	"iter"
	"slices"
	"strings"

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
	node := cr.cachedGetNode(id)
	m.nodes[id] = node
	return node
}

func (cr *CrossRepoResolver) trackedNpmImport(callerRepo, callerWS, importPath string, memo *npmImportMemo) *graph.Node {
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
	node, seen := memo.probes[key]
	if !seen {
		for _, stem := range packages[dir].Entries {
			if file := probeJSTSFile(cr.cachedGetNode, stem); file != "" {
				node = cr.cachedGetNode(file)
				break
			}
		}
		memo.probes[key] = node
	}
	if node == nil {
		return nil
	}
	if symbol != "" {
		exported := memo.getNode(cr, node.ID+"::"+symbol)
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
	_, err := cr.resolveTrackedNpmImportsPass(ctx, edges, nil, stats)
	return err
}

func (cr *CrossRepoResolver) resolveTrackedNpmImportsScoped(ctx context.Context, edges []*graph.Edge, stats *CrossRepoStats) ([]*graph.Edge, error) {
	return cr.resolveTrackedNpmImportsPass(ctx, slices.Values(edges), edges, stats)
}

func (cr *CrossRepoResolver) resolveTrackedNpmImportsPass(ctx context.Context, edges iter.Seq[*graph.Edge], scopedEdges []*graph.Edge, stats *CrossRepoStats) ([]*graph.Edge, error) {
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
	scoped := scopedEdges != nil
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
	call.To = graph.UnresolvedMarker + name
	call.CrossRepo = false
	call.Origin, call.Tier, call.Confidence, call.ConfidenceLabel = "", "", 0, ""
}

func (cr *CrossRepoResolver) staleTrackedNpmRequireCallName(call *graph.Edge, memo *npmImportMemo) (string, bool) {
	if call == nil || !call.CrossRepo || call.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
		return "", false
	}
	marked, _ := call.Meta[NpmImportSpecifierMeta].(string)
	spec := marked
	if spec == "" {
		spec, _ = call.Meta[graph.JSLexicalRequireSpecMeta].(string)
	}
	if spec == "" {
		return "", false
	}
	eligible := cr.eligibleTrackedNpmImport(call, spec)
	if marked == "" && !eligible {
		return "", false
	}
	stillValid := false
	if eligible {
		from := memo.getNode(cr, call.From)
		callerRepo := ""
		if from != nil {
			callerRepo = from.RepoPrefix
		}
		if picked := cr.trackedNpmImport(callerRepo, candidateWorkspaceID(from), spec, memo); picked != nil {
			targetFile := cr.nodeFilePath(call.To)
			if targetFile != "" {
				if targetFile == cr.nodeFilePath(picked.ID) {
					stillValid = true
				} else {
					_, stillValid = cr.importClosureFiles(picked.ID)[targetFile]
				}
			}
		}
	}
	if stillValid {
		return "", false
	}
	_, name, hasSym := strings.Cut(call.To, "::")
	if !hasSym || name == "" {
		if n := memo.getNode(cr, call.To); n != nil {
			name = n.Name
		}
	}
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
// require() calls graph-wide in bounded pages, flushing each page atomically so
// cancellation never leaves live edges mutated without reindexing.
func (cr *CrossRepoResolver) restubStaleTrackedNpmRequireCallsAll(ctx context.Context, memo *npmImportMemo) {
	if cr.npmPackages == nil {
		return
	}
	var sites []graph.EdgeSite
	siteSeen := make(map[graph.EdgeSite]struct{})
	for e := range graph.EdgesLightSeq(cr.graph, graph.EdgeCrossRepoCalls) {
		if ctx.Err() != nil {
			return
		}
		if !isJSTSPath(e.FilePath) {
			continue
		}
		site := graph.EdgeSite{From: e.From, Line: e.Line, Kind: graph.EdgeCalls}
		if _, dup := siteSeen[site]; dup {
			continue
		}
		siteSeen[site] = struct{}{}
		sites = append(sites, site)
	}
	if len(sites) == 0 {
		return
	}
	seen := make(map[graph.EdgeIdentity]struct{})
	for start := 0; start < len(sites); start += resolvePendingPageRows {
		if ctx.Err() != nil {
			return
		}
		page := sites[start:min(start+resolvePendingPageRows, len(sites))]
		full := cr.graph.GetEdgeCandidates(nil, page)
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
				resetRestubbedNpmCall(call, name)
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
				_, name, hasSym := strings.Cut(oldTo, "::")
				if !hasSym || name == "" {
					if n := cr.cachedGetNode(oldTo); n != nil {
						name = n.Name
					}
				}
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
