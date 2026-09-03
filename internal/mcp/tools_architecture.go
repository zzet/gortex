package mcp

import (
	"context"
	"maps"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/graphview"
)

// registerArchitectureTool wires get_architecture — the single-shot
// "what does this repo look like" snapshot. Composes outline +
// processes + cross-repo edge rollup + contracts summary into one
// response so an agent can orient in one call instead of fanning
// out to graph_stats + get_communities + analyze cross_repo +
// contracts list + DiscoverProcesses.
func (s *Server) registerArchitectureTool() {
	s.addTool(
		mcp.NewTool("get_architecture",
			mcp.WithDescription("Single-shot architectural snapshot: language mix, top communities, hotspots, entry points, discovered processes, cross-repo edge rollup, and contract counts. Composes the substrate `gortex://surprises` + `get_repo_outline` + `analyze cross_repo` + `contracts list` already expose into one structured response. Use at the start of an architecture review or onboarding session."),
			mcp.WithNumber("top_communities", mcp.Description("Cap on number of communities returned (default: 5).")),
			mcp.WithNumber("top_hotspots", mcp.Description("Cap on number of hotspots returned (default: 5).")),
			mcp.WithNumber("top_processes", mcp.Description("Cap on number of discovered processes returned (default: 5).")),
			mcp.WithNumber("top_entry_points", mcp.Description("Cap on entry-point list (default: 10).")),
			mcp.WithString("path_prefix", mcp.Description("Restrict communities / hotspots / processes / entry points to those touching this file-path prefix.")),
			mcp.WithString("resolution", mcp.Description("Add a hierarchical multi-resolution rollup of the graph at this tier — one of file, package, service, or system. The response gains a `hierarchy` block holding rollup nodes (one per group, with a leaf count) and weighted rollup edges (weight = the count of underlying leaf-level edges crossing the two groups) — the architecture at that tier, with no function-leaf nodes. Omit (or pass symbol) to skip the rollup.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetArchitecture,
	)
}

// architectureProcess is the wire shape for a discovered process.
// Trimmed from analysis.Process — step lists can be hundreds of
// nodes and the agent rarely needs them inline.
type architectureProcess struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	EntryPoint string   `json:"entry_point"`
	StepCount  int      `json:"step_count"`
	Files      []string `json:"files"`
	Score      float64  `json:"score"`
}

// crossRepoRow rolls up a single cross-repo edge instance into one
// row for the architecture response. The full edge dump goes through
// analyze cross_repo when an agent needs it.
type crossRepoRow struct {
	Kind     string `json:"kind"`
	FromRepo string `json:"from_repo"`
	ToRepo   string `json:"to_repo"`
	Count    int    `json:"count"`
}

func (s *Server) handleGetArchitecture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	topCommunities := max(req.GetInt("top_communities", 5), 1)
	topHotspots := max(req.GetInt("top_hotspots", 5), 1)
	topProcesses := max(req.GetInt("top_processes", 5), 1)
	topEntryPoints := max(req.GetInt("top_entry_points", 10), 1)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))

	// scoped + inScope are only needed when the session is bound or
	// the caller supplied a path-prefix narrowing. Otherwise every
	// node is in scope and downstream membership tests are tautologies
	// the helpers handle via nil inScope.
	_, _, bound := s.sessionScope(ctx)
	needScoped := bound || pathPrefix != ""

	// Node / edge reads run on the request's reader so an overlay-active
	// caller is described by its own buffers. Hoisted once — the snapshot
	// reads it from four sections.
	reader := s.readerFor(ctx)

	var scoped []*graph.Node
	var inScope map[string]bool
	var totalNodesScoped int
	if needScoped {
		scoped = s.scopedNodesLight(ctx)
		inScope = make(map[string]bool, len(scoped))
		for _, n := range scoped {
			if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
				continue
			}
			inScope[n.ID] = true
		}
		totalNodesScoped = len(inScope)
	} else {
		totalNodesScoped = reader.NodeCount()
	}

	// 1. Summary — language mix + node/edge counts.
	summary := architectureSummary(scoped, inScope, totalNodesScoped, reader)

	// 2. Communities — same shape as the outline tool, capped here.
	communitiesSection := architectureCommunities(s.getCommunities(), inScope, topCommunities)

	// 3. Hotspots — load-bearing symbols, scoped + capped.
	hotspots := architectureHotspots(s.getHotspots(), inScope, topHotspots)

	// 4. Entry points — functions with zero in-edges that have
	// out-edges (called by no one, calls into the system). Sorted
	// by out-degree so the most-impactful entry points surface first.
	entries := architectureEntryPoints(inScope, reader, topEntryPoints)

	// 5. Processes — analysis.DiscoverProcesses output, trimmed.
	processes := architectureProcesses(s.getProcesses(), inScope, topProcesses)

	// The communities, hotspots and processes sections come from the
	// server-wide analysis caches, which are computed over the base corpus
	// rather than through this request's reader. Under a view three of the
	// snapshot's sections therefore describe the base, so the response says
	// so instead of reading as a wholly view-scoped answer.
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph)

	// 6. Cross-repo edges — rollup by (from_repo, to_repo, kind) so
	// the architecture view shows which repos talk to which without
	// dumping every individual call site.
	crossRepo := architectureCrossRepo(reader)

	// 7. Contracts — count by type and role, plus a per-workspace
	// rollup. The full contract list lives behind `contracts list`.
	contractsSection := architectureContracts(s.contractRegistry)

	out := map[string]any{
		"summary":      summary,
		"communities":  communitiesSection,
		"hotspots":     hotspots,
		"entry_points": entries,
		"processes":    processes,
		"cross_repo":   crossRepo,
		"contracts":    contractsSection,
	}

	// 8. Hierarchy — optional multi-resolution rollup. When the caller
	// asks for a resolution tier, collapse the leaf graph to that tier
	// so the response carries the architecture at the requested
	// granularity (file / package / service / system) with no
	// function-leaf nodes. Computed on demand from the base graph, so it is
	// base-scoped for the same reason the cached sections are and rides on
	// the same annotation.
	if hierarchy, errMsg := architectureHierarchy(s.graph, s.getCommunities(), req.GetString("resolution", "")); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	} else if hierarchy != nil {
		out["hierarchy"] = hierarchy
	}

	return s.respondJSONOrTOON(ctx, req, out)
}

// architectureHierarchy builds the optional multi-resolution rollup
// block for get_architecture. An empty resolution argument means the
// caller did not ask for a rollup — it returns (nil, ""). An
// unrecognised tier returns ("", message) so the handler can surface a
// clean error. Otherwise it rolls the base graph up to the requested
// tier via analysis.BuildHierarchy and returns the wire shape.
func architectureHierarchy(g graph.Store, cr *analysis.CommunityResult, resolution string) (map[string]any, string) {
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution == "" {
		return nil, ""
	}
	level := analysis.ResolutionLevel(resolution)
	if !analysis.ValidResolutionLevel(level) {
		return nil, "get_architecture: unknown resolution " + resolution +
			" (expected: symbol, file, package, service, system)"
	}

	view := analysis.BuildHierarchy(g, level, cr)

	nodes := make([]map[string]any, 0, len(view.Nodes))
	for _, n := range view.Nodes {
		nodes = append(nodes, map[string]any{
			"id":         n.ID,
			"label":      n.Label,
			"leaf_count": n.LeafCount,
		})
	}
	edges := make([]map[string]any, 0, len(view.Edges))
	for _, e := range view.Edges {
		edges = append(edges, map[string]any{
			"from":   e.From,
			"to":     e.To,
			"weight": e.Weight,
		})
	}
	return map[string]any{
		"level":      string(view.Level),
		"node_count": len(view.Nodes),
		"edge_count": len(view.Edges),
		"leaf_count": view.LeafCount,
		"nodes":      nodes,
		"edges":      edges,
		"self_loops": view.SelfLoops,
	}, ""
}

// architectureSummary builds the language mix + node/edge count
// header. Edges are bounded to the scoped subgraph so multi-repo
// callers don't see cross-workspace numbers. nil inScope is the
// signal that every node is in scope — the helper short-circuits
// the lang count through Stats() and the edge count through
// EdgeCount() rather than materialising the whole graph over cgo.
func architectureSummary(allScoped []*graph.Node, inScope map[string]bool, totalNodes int, g graph.Reader) map[string]any {
	langCounts := map[string]int{}
	if inScope == nil {
		// Unbound session + no path-prefix — pull the aggregate from
		// the backend's cached stats. One indexed groupby vs a
		// whole-table scan over cgo.
		stats := g.Stats()
		maps.Copy(langCounts, stats.ByLanguage)
	} else {
		for _, n := range allScoped {
			if !inScope[n.ID] || n.Language == "" {
				continue
			}
			langCounts[n.Language]++
		}
	}
	type langRow struct {
		Name  string `json:"name"`
		Nodes int    `json:"nodes"`
	}
	var languages []langRow
	for name, n := range langCounts {
		languages = append(languages, langRow{Name: name, Nodes: n})
	}
	sort.Slice(languages, func(i, j int) bool {
		if languages[i].Nodes != languages[j].Nodes {
			return languages[i].Nodes > languages[j].Nodes
		}
		return languages[i].Name < languages[j].Name
	})

	// Common case — unbound session + no path-prefix — every node
	// is in scope so the edge count is exactly the backend's
	// EdgeCount(), which is an O(1) lookup. Skips materialising
	// every edge over cgo just to count them.
	var totalEdges int
	if inScope == nil {
		totalEdges = g.EdgeCount()
	} else {
		for _, e := range g.AllEdges() {
			if !inScope[e.From] {
				continue
			}
			if !inScope[e.To] {
				continue
			}
			totalEdges++
		}
	}

	primary := ""
	if len(languages) > 0 {
		primary = languages[0].Name
	}

	unscopedCount := totalNodes
	if inScope != nil {
		unscopedCount = len(allScoped)
	}
	return map[string]any{
		"total_nodes":          totalNodes,
		"total_nodes_unscoped": unscopedCount,
		"total_edges":          totalEdges,
		"primary_language":     primary,
		"languages":            languages,
	}
}

func architectureCommunities(cr *analysis.CommunityResult, inScope map[string]bool, top int) map[string]any {
	out := map[string]any{"count": 0}
	if cr == nil {
		return out
	}
	kept := make([]analysis.Community, 0, len(cr.Communities))
	for _, c := range cr.Communities {
		// nil inScope means "every node is in scope" — keep the
		// community unconditionally. Otherwise drop the community
		// when no member lands inside the session's workspace.
		if inScope != nil {
			match := false
			for _, m := range c.Members {
				if inScope[m] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		kept = append(kept, c)
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].Size > kept[j].Size
	})
	pruned := kept
	if len(pruned) > top {
		pruned = pruned[:top]
	}
	rows := make([]map[string]any, 0, len(pruned))
	for _, c := range pruned {
		rows = append(rows, map[string]any{
			"id":       c.ID,
			"label":    c.Label,
			"hub":      c.Hub,
			"size":     c.Size,
			"cohesion": c.Cohesion,
			"files":    c.Files,
		})
	}
	out["count"] = len(kept)
	out["modularity"] = cr.Modularity
	out["top"] = rows
	return out
}

func architectureHotspots(hotspots []analysis.HotspotEntry, inScope map[string]bool, top int) []map[string]any {
	out := []map[string]any{}
	for _, h := range hotspots {
		if len(out) >= top {
			break
		}
		if inScope != nil && !inScope[h.ID] {
			continue
		}
		out = append(out, map[string]any{
			"id":               h.ID,
			"name":             h.Name,
			"kind":             h.Kind,
			"file_path":        h.FilePath,
			"fan_in":           h.FanIn,
			"fan_out":          h.FanOut,
			"betweenness":      h.Betweenness,
			"complexity_score": h.ComplexityScore,
		})
	}
	return out
}

// architectureEntryPoints returns functions/methods with zero
// incoming edges and at least one outgoing edge — the "called by
// no one, calls into the system" pattern.
//
// The candidate pool is either the kind-filtered subset of an in-scope
// node map (bound session / path-prefix narrowing) or — when inScope
// is nil — the function+method slice pulled directly from the storage
// layer via NodesByKindsScanner. The legacy code path walked the full
// scoped-nodes slice every call just to keep the callable subset.
//
// Uses NodeDegreeAggregator when the backend implements it (one
// batched in/out count instead of 2N GetInEdges/GetOutEdges
// round-trips on a disk backend — the per-node loop was the entire
// wall-clock cost of this section on large repos). Both capability
// probes miss on an overlay view, which takes the per-node walks over
// the overlaid edge relation instead.
func architectureEntryPoints(inScope map[string]bool, g graph.Reader, top int) []map[string]any {
	type entryCandidate struct {
		node   *graph.Node
		fanOut int
	}
	// Pre-filter on kind Go-side first. When inScope is nil pull
	// only function/method via the kind scanner; otherwise project
	// the same subset out of the supplied scope set.
	var pool []*graph.Node
	if inScope == nil {
		if scan, ok := g.(graph.NodesByKindsScanner); ok {
			pool = scan.NodesByKinds([]graph.NodeKind{graph.KindFunction, graph.KindMethod})
		} else {
			all := g.AllNodes()
			pool = make([]*graph.Node, 0, len(all))
			for _, n := range all {
				if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
					pool = append(pool, n)
				}
			}
		}
	} else {
		// Materialise the callable subset out of the in-scope node
		// id set. The caller's scoped slice already lives in memory,
		// so this stays cheap — but the inScope map carries bools,
		// not nodes, so we re-resolve via GetNode for each id.
		pool = make([]*graph.Node, 0, len(inScope))
		for id := range inScope {
			n := g.GetNode(id)
			if n == nil {
				continue
			}
			if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
				continue
			}
			pool = append(pool, n)
		}
	}
	cands := make([]entryCandidate, 0, len(pool))
	if agg, ok := g.(graph.NodeDegreeAggregator); ok && len(pool) > 0 {
		ids := make([]string, 0, len(pool))
		byID := make(map[string]*graph.Node, len(pool))
		for _, n := range pool {
			ids = append(ids, n.ID)
			byID[n.ID] = n
		}
		for _, r := range agg.NodeDegreeCounts(ids, nil) {
			if r.InCount > 0 || r.OutCount == 0 {
				continue
			}
			n := byID[r.NodeID]
			if n == nil {
				continue
			}
			cands = append(cands, entryCandidate{node: n, fanOut: r.OutCount})
		}
	} else {
		for _, n := range pool {
			if len(g.GetInEdges(n.ID)) > 0 {
				continue
			}
			out := len(g.GetOutEdges(n.ID))
			if out == 0 {
				continue
			}
			cands = append(cands, entryCandidate{node: n, fanOut: out})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].fanOut != cands[j].fanOut {
			return cands[i].fanOut > cands[j].fanOut
		}
		return cands[i].node.ID < cands[j].node.ID
	})
	if len(cands) > top {
		cands = cands[:top]
	}
	out := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		out = append(out, map[string]any{
			"id":        c.node.ID,
			"name":      c.node.Name,
			"file_path": c.node.FilePath,
			"fan_out":   c.fanOut,
		})
	}
	return out
}

func architectureProcesses(pr *analysis.ProcessResult, inScope map[string]bool, top int) []architectureProcess {
	if pr == nil {
		return []architectureProcess{}
	}
	kept := make([]analysis.Process, 0, len(pr.Processes))
	for _, p := range pr.Processes {
		if inScope != nil && !inScope[p.EntryPoint] {
			continue
		}
		kept = append(kept, p)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Score != kept[j].Score {
			return kept[i].Score > kept[j].Score
		}
		return kept[i].StepCount > kept[j].StepCount
	})
	if len(kept) > top {
		kept = kept[:top]
	}
	out := make([]architectureProcess, 0, len(kept))
	for _, p := range kept {
		out = append(out, architectureProcess{
			ID:         p.ID,
			Name:       p.Name,
			EntryPoint: p.EntryPoint,
			StepCount:  p.StepCount,
			Files:      p.Files,
			Score:      p.Score,
		})
	}
	return out
}

// architectureCrossRepo bundles every cross_repo_* edge into a
// (from_repo, to_repo, kind) → count rollup. Empty list when no
// cross-repo edges exist (single-repo mode).
//
// Picks the CrossRepoEdgeAggregator capability when the backend
// implements it (one server-side aggregate replaces the AllEdges +
// per-edge GetNode pair — typically ~286k edge rows + thousands
// of GetNode round-trips on a disk backend for <100 rows of output). Falls
// back to the AllEdges-driven loop on backends that don't.
func architectureCrossRepo(g graph.Reader) []crossRepoRow {
	type key struct {
		kind, fromRepo, toRepo string
	}
	counts := map[key]int{}
	if ag, ok := g.(graph.CrossRepoEdgeAggregator); ok {
		for _, r := range ag.CrossRepoEdgeCounts() {
			counts[key{kind: string(r.Kind), fromRepo: r.FromRepo, toRepo: r.ToRepo}] = r.Count
		}
	} else {
		for _, e := range g.AllEdges() {
			if _, isCross := graph.BaseKindForCrossRepo(e.Kind); !isCross {
				continue
			}
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from == nil || to == nil {
				continue
			}
			k := key{kind: string(e.Kind), fromRepo: from.RepoPrefix, toRepo: to.RepoPrefix}
			counts[k]++
		}
	}
	rows := make([]crossRepoRow, 0, len(counts))
	for k, c := range counts {
		rows = append(rows, crossRepoRow{
			Kind: k.kind, FromRepo: k.fromRepo, ToRepo: k.toRepo, Count: c,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		if rows[i].FromRepo != rows[j].FromRepo {
			return rows[i].FromRepo < rows[j].FromRepo
		}
		return rows[i].ToRepo < rows[j].ToRepo
	})
	return rows
}

func architectureContracts(registry *contracts.Registry) map[string]any {
	out := map[string]any{
		"total":        0,
		"by_type":      map[string]int{},
		"by_role":      map[string]int{},
		"by_workspace": map[string]int{},
	}
	if registry == nil {
		return out
	}
	all := registry.All()
	byType := map[string]int{}
	byRole := map[string]int{}
	byWS := map[string]int{}
	for _, c := range all {
		byType[string(c.Type)]++
		byRole[string(c.Role)]++
		ws := c.EffectiveWorkspace()
		if ws == "" {
			ws = "(unscoped)"
		}
		byWS[ws]++
	}
	out["total"] = len(all)
	out["by_type"] = byType
	out["by_role"] = byRole
	out["by_workspace"] = byWS
	return out
}
