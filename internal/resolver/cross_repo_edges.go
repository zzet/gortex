package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const crossRepoAddBatchSize = 4096

// DetectCrossRepoEdges is the graph-wide materialisation pass for the
// cross-repo edge layer (M3). It walks every resolved calls / implements
// / extends edge and, whenever the From node and the To node live in
// two different repos, emits a parallel edge of the matching
// cross_repo_* kind and sets Edge.CrossRepo on the base edge so the
// bool flag and the dedicated kind never disagree.
//
// The pass is a full recompute and is idempotent: graph.AddEdge dedupes
// by edgeKey, so re-emitting an unchanged parallel edge is a no-op. It
// is also incremental-safe — graph.EvictFile removes a node's edges in
// both directions, so when either endpoint's file is reindexed the
// stale parallel edge is gone before this pass re-runs. Parallel
// cross_repo_* edges are themselves skipped (CrossRepoKindFor only maps
// the three base kinds), so the pass never feeds on its own output.
//
// Runs at every resolver "settle" point: the tail of
// CrossRepoResolver.ResolveAll / ResolveForRepo (cross-repo calls just
// lifted by the boundary resolver) and inside the indexers'
// RunGlobalGraphPasses (cross-repo implements / extends just produced
// by InferImplements / InferOverrides).
//
// Returns the count of cross-repo relationships found this pass — the
// number of parallel edges that exist after it, modulo graph dedup.
func DetectCrossRepoEdges(g graph.Store) int {
	edges, _ := DetectCrossRepoEdgesWithSkip(g)
	return edges
}

// DetectCrossRepoEdgesWithSkip runs DetectCrossRepoEdges and also reports
// whether it skipped the scan because the store holds fewer than two repos.
func DetectCrossRepoEdgesWithSkip(g graph.Store) (edges int, skipped bool) {
	if g == nil {
		return 0, false
	}
	if !hasMultipleRepoPrefixes(g) {
		return 0, true
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidates(g)), false
}

func hasMultipleRepoPrefixes(g graph.Store) bool {
	first := ""
	for _, prefix := range g.RepoPrefixes() {
		if prefix == "" {
			continue
		}
		if first == "" {
			first = prefix
		} else if prefix != first {
			return true
		}
	}
	return false
}

// DetectCrossRepoEdgesForRepos materializes only relationships incident to the
// changed repository frontier. SQLite pushes the prefix predicate into one
// endpoint join; the in-memory backend uses one batched endpoint projection.
func DetectCrossRepoEdgesForRepos(g graph.Store, repoPrefixes []string) int {
	if g == nil || len(repoPrefixes) == 0 {
		return 0
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidatesForRepos(g, repoPrefixes))
}

// DetectCrossRepoEdgesForReindexes materializes only base relationships that
// changed in the current bounded resolver chunk. Endpoint ownership is fetched
// once for the whole batch; no graph-wide candidate layer or endpoint N+1
// crosses into memory. This is the ResolveAll settle path—global indexer
// passes retain DetectCrossRepoEdges for newly inferred hierarchy edges.
func DetectCrossRepoEdgesForReindexes(g graph.Store, batch []graph.EdgeReindex) int {
	if g == nil || len(batch) == 0 {
		return 0
	}
	ids := make([]string, 0, len(batch)*2)
	seen := make(map[string]struct{}, len(batch)*2)
	for _, reindex := range batch {
		if reindex.Edge == nil {
			continue
		}
		for _, id := range []string{reindex.Edge.From, reindex.Edge.To} {
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	placements := graph.NodePlacementsByIDs(g, ids)
	rows := make([]graph.CrossRepoCandidateRow, 0, len(batch))
	for _, reindex := range batch {
		edge := reindex.Edge
		if edge == nil {
			continue
		}
		if _, ok := graph.CrossRepoKindFor(edge.Kind); !ok {
			continue
		}
		from, fromOK := placements[edge.From]
		to, toOK := placements[edge.To]
		if !fromOK || !toOK || from.RepoPrefix == "" || to.RepoPrefix == "" || from.RepoPrefix == to.RepoPrefix {
			continue
		}
		rows = append(rows, graph.CrossRepoCandidateRow{
			Edge: edge, FromRepo: from.RepoPrefix, ToRepo: to.RepoPrefix,
		})
	}
	return materializeCrossRepoCandidates(g, rows)
}

// crossRepoReindexes keeps the rows whose endpoints carry two different
// repository prefixes, so a whole-repository pass fetches endpoint ownership
// only for rebinds that can cross repositories.
func crossRepoReindexes(batch []graph.EdgeReindex) []graph.EdgeReindex {
	var out []graph.EdgeReindex
	for _, reindex := range batch {
		if reindex.Edge == nil {
			continue
		}
		from, to := graph.RepoPrefixOfID(reindex.Edge.From), graph.RepoPrefixOfID(reindex.Edge.To)
		if from == "" || to == "" || from == to || strings.Contains(from, "::") || strings.Contains(to, "::") {
			continue
		}
		out = append(out, reindex)
	}
	return out
}

func materializeCrossRepoCandidates(g graph.Store, rows []graph.CrossRepoCandidateRow) int {
	if len(rows) == 0 {
		return 0
	}

	type pendingRelationship struct {
		base     *graph.Edge
		parallel graph.Edge
		fromRepo string
		toRepo   string
	}
	pending := make([]pendingRelationship, 0, len(rows))
	sites := make([]graph.EdgeSite, 0, len(rows))
	seenSites := make(map[graph.EdgeSite]struct{}, len(rows))
	baseToMark := make([]*graph.Edge, 0, len(rows))
	for _, row := range rows {
		e := row.Edge
		if e == nil {
			continue
		}
		crKind, ok := graph.CrossRepoKindFor(e.Kind)
		if !ok {
			continue
		}
		if !e.CrossRepo {
			baseToMark = append(baseToMark, e)
		}
		e.CrossRepo = true
		parallel := graph.Edge{
			From:            e.From,
			To:              e.To,
			Kind:            crKind,
			FilePath:        e.FilePath,
			Line:            e.Line,
			Confidence:      e.Confidence,
			ConfidenceLabel: e.ConfidenceLabel,
			Origin:          e.Origin,
			CrossRepo:       true,
		}
		pending = append(pending, pendingRelationship{base: e, parallel: parallel, fromRepo: row.FromRepo, toRepo: row.ToRepo})
		site := graph.EdgeSite{From: parallel.From, Line: parallel.Line, Kind: parallel.Kind}
		if _, seen := seenSites[site]; !seen {
			seenSites[site] = struct{}{}
			sites = append(sites, site)
		}
	}
	if len(pending) == 0 {
		return 0
	}

	// Keep the bool flag on the base edge consistent with the dedicated kind.
	// SQLite candidate rows are compact projections rather than live pointers,
	// so persist only this promoted column with a set-oriented marker.
	if marker, ok := g.(graph.CrossRepoFlagMarker); ok && len(baseToMark) > 0 {
		marker.MarkEdgesCrossRepo(baseToMark)
	}

	existing := g.GetEdgeCandidates(nil, sites)
	additions := make([]*graph.Edge, 0, len(pending))
	for i := range pending {
		relationship := &pending[i]
		parallel := &relationship.parallel
		found := false
		for _, candidate := range existing.Site(parallel.From, parallel.Line, parallel.Kind) {
			if candidate != nil && candidate.To == parallel.To && candidate.FilePath == parallel.FilePath {
				found = true
				break
			}
		}
		if found {
			continue
		}
		parallel.Meta = map[string]any{
			"base_kind":   string(relationship.base.Kind),
			"source_repo": relationship.fromRepo,
			"target_repo": relationship.toRepo,
		}
		additions = append(additions, parallel)
		existing.Add(parallel)
	}
	for start := 0; start < len(additions); start += crossRepoAddBatchSize {
		end := start + crossRepoAddBatchSize
		if end > len(additions) {
			end = len(additions)
		}
		g.AddBatch(nil, additions[start:end])
	}
	return len(pending)
}

// crossRepoCandidates returns every edge whose Kind has a parallel
// cross_repo_* kind AND whose endpoints carry two distinct, non-empty
// RepoPrefix values. Routed through the storage layer's
// CrossRepoCandidates capability when the backend implements it (one
// query — a join with the kind + repo-prefix filters in WHERE); falls
// back to the AllEdges + per-edge GetNode walk otherwise.
//
// The base-kind set is derived from graph.CrossRepoKindFor by
// iterating the in-process registry — the disk backend uses the same
// kind list verbatim so single-repo graphs return no rows without a
// whole-table scan.
func crossRepoCandidates(g graph.Store) []graph.CrossRepoCandidateRow {
	baseKinds := graph.BaseKindsForCrossRepo()
	if cap, ok := g.(graph.CrossRepoCandidates); ok {
		return cap.CrossRepoCandidates(baseKinds)
	}
	return crossRepoCandidatesFallback(g, baseKinds, nil, nil)
}

func crossRepoCandidatesForRepos(g graph.Store, repoPrefixes []string) []graph.CrossRepoCandidateRow {
	baseKinds := graph.BaseKindsForCrossRepo()
	if cap, ok := g.(graph.ScopedCrossRepoCandidates); ok {
		return cap.CrossRepoCandidatesForRepos(baseKinds, repoPrefixes)
	}
	return crossRepoCandidatesFallback(g, baseKinds, stringSet(repoPrefixes), nil)
}

func crossRepoCandidatesForMutationFiles(g graph.Store, edgeSourceFiles, incidentNodeFiles []string) []graph.CrossRepoCandidateRow {
	baseKinds := graph.BaseKindsForCrossRepo()
	if cap, ok := g.(graph.MutationScopedCrossRepoCandidates); ok {
		return cap.CrossRepoCandidatesForMutation(baseKinds, edgeSourceFiles, incidentNodeFiles)
	}
	return crossRepoCandidatesFallbackScopes(g, baseKinds, nil, stringSet(edgeSourceFiles), stringSet(incidentNodeFiles))
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

// crossRepoCandidatesFallback is used only by stores without the optional
// predicate-shaped capability. It still avoids both graph-wide snapshot APIs
// and endpoint N+1 reads: kinds are streamed and endpoint nodes are fetched in
// one batch.
func crossRepoCandidatesFallback(g graph.Store, baseKinds []graph.EdgeKind, repos, files map[string]struct{}) []graph.CrossRepoCandidateRow {
	return crossRepoCandidatesFallbackScopes(g, baseKinds, repos, files, files)
}

func crossRepoCandidatesFallbackScopes(g graph.Store, baseKinds []graph.EdgeKind, repos, edgeFiles, incidentFiles map[string]struct{}) []graph.CrossRepoCandidateRow {
	if len(baseKinds) == 0 || (repos != nil && len(repos) == 0) ||
		(edgeFiles != nil && incidentFiles != nil && len(edgeFiles) == 0 && len(incidentFiles) == 0) {
		return nil
	}
	var edges []*graph.Edge
	for edge := range graph.EdgesLightSeq(g, baseKinds...) {
		if edge != nil {
			edges = append(edges, edge)
		}
	}
	endpointIDs := make([]string, 0, len(edges)*2)
	for _, edge := range edges {
		endpointIDs = append(endpointIDs, edge.From, edge.To)
	}
	placements := graph.NodePlacementsByIDs(g, endpointIDs)
	out := make([]graph.CrossRepoCandidateRow, 0, len(edges))
	for _, edge := range edges {
		from, fromOK := placements[edge.From]
		to, toOK := placements[edge.To]
		if !fromOK || !toOK || from.RepoPrefix == "" || to.RepoPrefix == "" || from.RepoPrefix == to.RepoPrefix {
			continue
		}
		if repos != nil {
			_, fromScoped := repos[from.RepoPrefix]
			_, toScoped := repos[to.RepoPrefix]
			if !fromScoped && !toScoped {
				continue
			}
		}
		if edgeFiles != nil || incidentFiles != nil {
			_, edgeScoped := edgeFiles[edge.FilePath]
			_, fromScoped := incidentFiles[from.FilePath]
			_, toScoped := incidentFiles[to.FilePath]
			if !edgeScoped && !fromScoped && !toScoped {
				continue
			}
		}
		out = append(out, graph.CrossRepoCandidateRow{Edge: edge, FromRepo: from.RepoPrefix, ToRepo: to.RepoPrefix})
	}
	return out
}
