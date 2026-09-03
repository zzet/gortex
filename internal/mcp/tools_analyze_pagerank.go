// pagerank — graph-EXTRACTION-flavoured centrality analysis.
//
// analyze kind=pagerank ranks symbols by PageRank authority: a
// symbol is "central" when central symbols depend on it, so a
// rarely-called API that's invoked from every domain layer ranks
// higher than a heavily-called test helper. This is qualitatively
// different from the degree-based `hotspots` analyzer — random-walk
// authority weights influence by reach, not by raw fan-in count.
//
// Routing:
//
//   - When the backing graph.Store implements graph.PageRanker
//     (today only store_sqlite), the analyzer delegates to the
//     engine-native parallel implementation (Ligra-based). Saves
//     the per-call cost of a fresh Go-side power iteration.
//
//   - Otherwise (in-memory store), falls back to
//     analysis.ComputePageRank — the same pure-Go implementation
//     the search rerank pipeline consumes via the cached
//     Server.pageRank field.

package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// pageRankRow is the per-symbol shape the analyzer returns.
type pageRankRow struct {
	ID       string  `json:"id"`
	Name     string  `json:"name,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	FilePath string  `json:"file_path,omitempty"`
	Line     int     `json:"line,omitempty"`
	Rank     float64 `json:"rank"`
}

func (s *Server) handleAnalyzePageRank(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	damping := 0.0
	if v, ok := args["damping"].(float64); ok && v > 0 && v < 1 {
		damping = v
	}
	maxIter := 0
	if v, ok := args["max_iterations"].(float64); ok && v > 0 {
		maxIter = int(v)
	}
	tolerance := 0.0
	if v, ok := args["tolerance"].(float64); ok && v > 0 {
		tolerance = v
	}
	nodeKinds := parseKindFilter(stringArg(args, "node_kinds"))

	// runPageRank applies the limit internally on BOTH the engine-native
	// and in-process paths. When the request narrows scope we must rank the
	// full graph, drop out-of-scope hits, and only THEN cap — capping first
	// would spend the limit on rows the caller can't see and return fewer
	// than `limit` visible symbols. So request unbounded under scope and
	// re-apply the limit after the visibility filter below.
	prLimit := limit
	if s.scopeFiltersActive(ctx) {
		prLimit = 0
	}

	hits, fullGraphBurst := s.runPageRank(graph.PageRankOpts{
		NodeKinds:     nodeKinds,
		DampingFactor: damping,
		MaxIterations: maxIter,
		Tolerance:     tolerance,
		Limit:         prLimit,
	})
	if fullGraphBurst {
		defer scheduleOSMemoryReleaseAfterBurst(s.logger, "analyze_pagerank")
	}

	reader := s.readerFor(ctx)

	// Narrow to the session workspace + optional repo allow-set, then
	// re-apply the original limit (runPageRank ran unbounded above under
	// scope). Without this the authority ranking would span every
	// workspace. Strict no-op for an unbound session with no RepoAllow.
	if s.scopeFiltersActive(ctx) {
		kept := make([]graph.PageRankHit, 0, len(hits))
		for _, h := range hits {
			if s.analyzeNodeVisible(ctx, reader.GetNode(h.NodeID)) {
				kept = append(kept, h)
			}
		}
		hits = kept
		if limit > 0 && limit < len(hits) {
			hits = hits[:limit]
		}
	}

	// Batch-materialise hit nodes in one backend round-trip instead
	// of per-id GetNode. On a disk backend each GetNode is a
	// round-trip; on the default limit (20) the per-id path issued 20
	// round-trips per pagerank invocation. Single GetNodesByIDs
	// collapses that into one bulk query while preserving rank order
	// (the local map lookup is keyed by NodeID).
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.NodeID != "" {
			ids = append(ids, h.NodeID)
		}
	}
	nodeByID := reader.GetNodesByIDs(ids)

	rows := make([]pageRankRow, 0, len(hits))
	for _, h := range hits {
		row := pageRankRow{ID: h.NodeID, Rank: h.Rank}
		if n := nodeByID[h.NodeID]; n != nil {
			row.Name = n.Name
			row.Kind = string(n.Kind)
			row.FilePath = n.FilePath
			row.Line = n.StartLine
		}
		rows = append(rows, row)
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("pagerank", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %s %s:%d rank=%.6f\n", r.Kind, r.ID, r.FilePath, r.Line, r.Rank)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{"pagerank": rows, "count": len(rows)})
}

// runPageRank picks the engine-native PageRanker when the
// backing store implements it, otherwise falls back to the
// in-process power iteration.
func (s *Server) runPageRank(opts graph.PageRankOpts) ([]graph.PageRankHit, bool) {
	if store := s.backendStore(); store != nil {
		if pr, ok := store.(graph.PageRanker); ok {
			hits, err := pr.PageRank(opts)
			if err == nil {
				return hits, false
			}
			// Fall through to the in-process path on backend
			// error rather than surface a half-completed
			// result; engine-native is a hot path optimisation,
			// not the source of truth.
		}
	}
	// Fallback: pure-Go power iteration on the in-memory mirror.
	// analysis.ComputePageRank doesn't accept the same options
	// as the engine-native call yet — it uses fixed damping /
	// iteration constants — so opts.DampingFactor / MaxIterations
	// / Tolerance are silently ignored on the fallback path. The
	// NodeKinds filter is honoured by post-filtering the result.
	res := analysis.ComputePageRank(s.graph)
	if res == nil || len(res.Scores) == 0 {
		return nil, true
	}
	allow := makeKindAllow(opts.NodeKinds)
	hits := make([]graph.PageRankHit, 0, len(res.Scores))
	for id, rank := range res.Scores {
		if !allow(s.graph.GetNode(id)) {
			continue
		}
		hits = append(hits, graph.PageRankHit{NodeID: id, Rank: rank})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Rank > hits[j].Rank })
	if opts.Limit > 0 && opts.Limit < len(hits) {
		hits = hits[:opts.Limit]
	}
	return hits, true
}

// backendStore returns the underlying graph.Store the indexer
// writes to — which is what implements the capability interfaces
// (PageRanker, CommunityDetector, …). Falls back to s.graph when
// no indexer is wired so test fixtures keep working.
func (s *Server) backendStore() graph.Store {
	if s.indexer != nil {
		return s.indexer.Graph()
	}
	return s.graph
}

// parseKindFilter parses a comma-separated list of graph node
// kinds (e.g. "function,method,type") into a typed slice. Empty
// input → empty slice (caller treats that as "no filter").
func parseKindFilter(in string) []graph.NodeKind {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil
	}
	parts := strings.Split(in, ",")
	out := make([]graph.NodeKind, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, graph.NodeKind(p))
	}
	return out
}

// handleAnalyzeLouvain returns the Louvain partitioning of the
// graph. When the backing store implements graph.CommunityDetector
// (today only store_sqlite), the partitioning is delegated to the
// engine-native implementation and threaded through the existing
// label / hub / cohesion / parent post-processing
// (analysis.DetectCommunitiesLouvainBackend) so the response is
// shape-identical to the in-process path. Otherwise the in-process
// DetectCommunitiesLouvain runs.
//
// Distinct from `analyze kind=clusters` which uses the Leiden
// algorithm (the Server's cached communities). Louvain produces
// different — typically more granular — partitions; this kind
// exposes it as a first-class result for clients that want the
// Louvain shape specifically.
func (s *Server) handleAnalyzeLouvain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := 50
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	result := s.runLouvain(ctx)
	if result == nil {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"communities": []any{},
			"modularity":  0.0,
		})
	}

	communities := result.Communities
	totalCommunities := len(result.Communities)

	// Narrow to the session workspace + optional repo allow-set when the
	// request scopes below the global graph: prune each community's members
	// to visible nodes, recompute Size/Files, and drop communities left
	// empty. Without this the partition would span every workspace. Hub is
	// a symbol name (not a node ID) so it is left untouched. Filter before
	// the limit cap so the cap and the total recompute over the visible
	// set. Strict no-op for an unbound session with no RepoAllow.
	if s.scopeFiltersActive(ctx) {
		reader := s.readerFor(ctx)
		kept := make([]analysis.Community, 0, len(communities))
		for _, c := range communities {
			visMembers := make([]string, 0, len(c.Members))
			files := make([]string, 0, len(c.Files))
			seenFile := make(map[string]struct{}, len(c.Files))
			for _, id := range c.Members {
				n := reader.GetNode(id)
				if !s.analyzeNodeVisible(ctx, n) {
					continue
				}
				visMembers = append(visMembers, id)
				if n.FilePath != "" {
					if _, dup := seenFile[n.FilePath]; !dup {
						seenFile[n.FilePath] = struct{}{}
						files = append(files, n.FilePath)
					}
				}
			}
			if len(visMembers) == 0 {
				continue
			}
			c.Members = visMembers
			c.Size = len(visMembers)
			c.Files = files
			kept = append(kept, c)
		}
		communities = kept
		totalCommunities = len(kept)
	}

	if limit > 0 && limit < len(communities) {
		communities = communities[:limit]
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("louvain", map[string]any{
			"communities": communities,
			"modularity":  result.Modularity,
		}))
	}
	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "modularity=%.4f communities=%d\n", result.Modularity, totalCommunities)
		for _, c := range communities {
			fmt.Fprintf(&b, "  %s size=%d cohesion=%.3f label=%s hub=%s\n",
				c.ID, c.Size, c.Cohesion, c.Label, c.Hub)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"communities": communities,
		"modularity":  result.Modularity,
		"total":       totalCommunities,
	})
}

// runLouvain picks the engine-native CommunityDetector when the
// backing store implements it, otherwise falls back to the
// pure-Go in-process Louvain. The output shape is identical
// either way (analysis.DetectCommunitiesLouvainBackend threads
// the engine-native partition through the same post-processing).
func (s *Server) runLouvain(ctx context.Context) *analysis.CommunityResult {
	if store := s.backendStore(); store != nil {
		if cd, ok := store.(graph.CommunityDetector); ok {
			if r := analysis.DetectCommunitiesLouvainBackend(s.graph, cd); r != nil {
				return r
			}
			// Engine-native error path falls through to the
			// in-process implementation rather than surfacing
			// a half-completed result.
		}
	}
	return analysis.DetectCommunitiesLouvain(s.readerFor(ctx))
}

// makeKindAllow returns a predicate that reports whether a node's
// kind passes the filter. nil node is always rejected (defensive).
func makeKindAllow(kinds []graph.NodeKind) func(*graph.Node) bool {
	if len(kinds) == 0 {
		return func(n *graph.Node) bool { return n != nil }
	}
	set := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	return func(n *graph.Node) bool {
		if n == nil {
			return false
		}
		_, ok := set[n.Kind]
		return ok
	}
}
