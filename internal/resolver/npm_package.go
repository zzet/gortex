package resolver

import (
	"context"
	"iter"
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
		if exported := memo.getNode(cr, node.ID+"::"+symbol); exported != nil && exported.Meta["visibility"] == "public" {
			node = exported
		}
	}
	return node
}

// The per-repo pass has already made unindexed npm imports external. Retry only
// imports with positive tracked-package evidence before building call reachability.
func (cr *CrossRepoResolver) resolveTrackedNpmImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats) error {
	if cr.npmPackages == nil {
		return ctx.Err()
	}
	// Scan metadata-free projections. Hydrate only imports whose package name
	// is tracked, so ordinary node_modules dependencies stay on the cheap path.
	var eligible []*graph.Edge
	for e := range edges {
		if err := ctx.Err(); err != nil {
			return err
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
	if len(eligible) == 0 {
		return ctx.Err()
	}
	memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
	// Close the light-edge cursor before reading or writing full edges. Hydrate
	// only one bounded page at a time; metadata never accumulates across pages.
	for start := 0; start < len(eligible); start += resolvePendingPageRows {
		if err := ctx.Err(); err != nil {
			return err
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
			cr.restubCallsForChangedNpmImports(batch)
		}
	}
	return ctx.Err()
}

// A call resolved through a binding cannot outlive that binding's target.
func (cr *CrossRepoResolver) restubCallsForChangedNpmImports(changes []graph.EdgeReindex) {
	callers := make(map[string]map[string]string)
	var ids []string
	for _, change := range changes {
		if !strings.Contains(change.OldTo, "::") || strings.HasPrefix(change.OldTo, "external::") || graph.IsUnresolvedTarget(change.OldTo) {
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
	if len(ids) == 0 {
		return
	}
	var batch []graph.EdgeReindex
	var obsolete []*graph.Edge
	for target, edges := range cr.graph.GetInEdgesByNodeIDs(ids) {
		for _, call := range edges {
			if call == nil || call.Kind != graph.EdgeCalls || !call.CrossRepo {
				continue
			}
			name, affected := callers[target][cr.callerFileID(call)]
			if !affected {
				continue
			}
			parallel := *call
			parallel.Kind = graph.EdgeCrossRepoCalls
			obsolete = append(obsolete, &parallel)
			graph.StashRestubProvenance(call)
			call.To = graph.UnresolvedMarker + name
			call.CrossRepo, call.ConfidenceLabel = false, ""
			batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: target})
		}
	}
	if len(batch) > 0 {
		graph.RemoveEdgesExact(cr.graph, obsolete)
		cr.graph.ReindexEdges(batch)
	}
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
	var picked *graph.Node
	if cr.eligibleTrackedNpmImport(e, importPath) {
		picked = cr.trackedNpmImport(callerRepo, callerWS, importPath, memo)
	}
	if picked == nil {
		if spec, _ := e.Meta[NpmImportSpecifierMeta].(string); spec != "" && e.To != "external::"+spec {
			e.To = "external::" + spec
			e.CrossRepo = false
			return true
		}
		return false
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
