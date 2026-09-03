package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// registerKnowledgeGapsTool wires get_knowledge_gaps — a cold-start
// audit composing four signals the graph already carries:
// disconnected nodes, thin communities, single-file communities, and
// untested hotspots. Returns the bundled view so an agent can decide
// where to invest test-writing / refactor effort before opening
// individual analyze calls.
func (s *Server) registerKnowledgeGapsTool() {
	s.addTool(
		mcp.NewTool("get_knowledge_gaps",
			mcp.WithDescription("Surface places the codebase under-documents itself. Composes disconnected nodes (zero in/out edges), thin communities (<thin_community_size members), single-file communities, and untested hotspots (high fan-in, low coverage_pct). Returns a bundled rollup so callers can rank where to invest test-writing / refactor effort. Cold-start audit aid: complements get_repo_outline by pointing at the weak spots, not the load-bearing ones."),
			mcp.WithNumber("thin_community_size", mcp.Description("Communities with fewer members are flagged 'thin' (default: 3).")),
			mcp.WithNumber("min_coverage_pct", mcp.Description("Hotspots with coverage_pct below this are 'untested' (default: 50). Hotspots without any coverage data are always included.")),
			mcp.WithNumber("hotspot_limit", mcp.Description("Top-N highest-fan-in nodes to evaluate against the coverage threshold (default: 20).")),
			mcp.WithNumber("limit_per_category", mcp.Description("Cap each rollup category (default: 20).")),
			mcp.WithString("path_prefix", mcp.Description("Scope analysis to nodes under this file-path prefix — e.g. 'internal/auth/'.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetKnowledgeGaps,
	)
}

// gapDisconnected — function/method with zero incoming and outgoing
// edges. Almost always either dead code or an isolated utility
// nobody wired up.
type gapDisconnected struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// gapCommunity — for thin and single-file communities the caller
// needs the same fields, so they share a row type.
type gapCommunity struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Size  int      `json:"size"`
	Files []string `json:"files"`
}

// gapUntestedHotspot — high-fan-in node whose coverage_pct is below
// the threshold, or absent entirely. fan_in is the in-edge count
// computed locally — independent of the hotspots analyzer's mean+2σ
// gate so we surface load-bearing nodes even in small repos where
// the analyzer is conservative.
type gapUntestedHotspot struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	FanIn       int     `json:"fan_in"`
	Coverage    float64 `json:"coverage_pct"`
	HasCoverage bool    `json:"has_coverage"`
}

func (s *Server) handleGetKnowledgeGaps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	thinSize := req.GetInt("thin_community_size", 3)
	if thinSize < 1 {
		thinSize = 3
	}
	minCov := req.GetFloat("min_coverage_pct", 50.0)
	if minCov < 0 {
		minCov = 0
	}
	hotspotLimit := max(req.GetInt("hotspot_limit", 20), 1)
	perCategoryLimit := max(req.GetInt("limit_per_category", 20), 1)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))

	// degreeByID maps node id -> (in, out) edge counts for every
	// function/method in scope, computed once via the backend's
	// NodeDegreeByKinds path when available. The legacy
	// NodeDegreeCounts route shipped a 30k-element IN-list per call
	// on a disk backend; NodeDegreeByKinds runs the same aggregate over the
	// kind-filtered node set so the planner never builds the list.
	degreeByID, scoped := s.scopedFunctionDegrees(ctx, pathPrefix)

	disconnected := s.collectDisconnected(ctx, scoped, pathPrefix, perCategoryLimit, degreeByID)
	thin, singleFile := s.collectCommunityGaps(thinSize, pathPrefix, perCategoryLimit)
	untested := s.collectUntestedHotspots(ctx, scoped, pathPrefix, hotspotLimit, minCov, perCategoryLimit, degreeByID)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"disconnected_nodes":      disconnected,
		"thin_communities":        thin,
		"single_file_communities": singleFile,
		"untested_hotspots":       untested,
		"summary": map[string]any{
			"disconnected_count": len(disconnected),
			"thin_count":         len(thin),
			"single_file_count":  len(singleFile),
			"untested_count":     len(untested),
		},
		"thresholds": map[string]any{
			"thin_community_size": thinSize,
			"min_coverage_pct":    minCov,
			"hotspot_limit":       hotspotLimit,
			"limit_per_category":  perCategoryLimit,
		},
	})
}

// scopedFunctionDegrees returns the per-node in/out degree map and
// the scoped function/method node list, in two pushdown calls.
// NodeDegreeByKinds runs server-side over the kind-filtered node
// table — the previous path fed NodeDegreeCounts a 30k-element
// IN-list, which the planner had to materialise before joining. The
// scoped node list is built from NodesByKinds (or AllNodes when the
// backend has no NodesByKindsScanner) and post-filtered for the
// session workspace, matching scopedNodesByKinds' contract.
func (s *Server) scopedFunctionDegrees(ctx context.Context, pathPrefix string) (map[string]graph.NodeDegreeRow, []*graph.Node) {
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}
	scoped := s.scopedNodesByKinds(ctx, kinds)
	var degByID map[string]graph.NodeDegreeRow
	if dk, ok := s.readerFor(ctx).(graph.NodeDegreeByKinds); ok {
		rows := dk.NodeDegreeByKinds(kinds, pathPrefix)
		degByID = make(map[string]graph.NodeDegreeRow, len(rows))
		for _, r := range rows {
			degByID[r.NodeID] = r
		}
	}
	return degByID, scoped
}

// collectDisconnected returns function/method nodes with zero
// incoming and zero outgoing edges in the scoped subgraph. The
// kind filter mirrors handleAnalyzeCoverageGaps' default — variables
// and constants always look disconnected, so including them would
// flood the result.
//
// Reads from the prebuilt degree map when present (the storage
// backend computed it once in scopedFunctionDegrees), falls back to
// per-node GetInEdges / GetOutEdges otherwise. The legacy
// NodeDegreeAggregator path is kept as a tertiary fallback for
// backends that publish NodeDegreeCounts but not NodeDegreeByKinds.
func (s *Server) collectDisconnected(ctx context.Context, scoped []*graph.Node, pathPrefix string, limit int, degreeByID map[string]graph.NodeDegreeRow) []gapDisconnected {
	candidates := make([]*graph.Node, 0, len(scoped))
	for _, n := range scoped {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		candidates = append(candidates, n)
	}

	out := make([]gapDisconnected, 0)
	switch {
	case degreeByID != nil:
		for _, n := range candidates {
			r, ok := degreeByID[n.ID]
			if !ok {
				// Absent from the aggregate => zero edges, by
				// definition of the kind-filtered aggregate.
				out = append(out, gapDisconnected{
					ID: n.ID, Name: n.Name, Kind: string(n.Kind),
					File: n.FilePath, Line: n.StartLine,
				})
				continue
			}
			if r.InCount > 0 || r.OutCount > 0 {
				continue
			}
			out = append(out, gapDisconnected{
				ID: n.ID, Name: n.Name, Kind: string(n.Kind),
				File: n.FilePath, Line: n.StartLine,
			})
		}
	default:
		reader := s.readerFor(ctx)
		if agg, ok := reader.(graph.NodeDegreeAggregator); ok && len(candidates) > 0 {
			ids := make([]string, 0, len(candidates))
			byID := make(map[string]*graph.Node, len(candidates))
			for _, n := range candidates {
				ids = append(ids, n.ID)
				byID[n.ID] = n
			}
			for _, r := range agg.NodeDegreeCounts(ids, nil) {
				if r.InCount > 0 || r.OutCount > 0 {
					continue
				}
				n := byID[r.NodeID]
				if n == nil {
					continue
				}
				out = append(out, gapDisconnected{
					ID: n.ID, Name: n.Name, Kind: string(n.Kind),
					File: n.FilePath, Line: n.StartLine,
				})
			}
		} else {
			for _, n := range candidates {
				if len(reader.GetInEdges(n.ID)) > 0 || len(reader.GetOutEdges(n.ID)) > 0 {
					continue
				}
				out = append(out, gapDisconnected{
					ID: n.ID, Name: n.Name, Kind: string(n.Kind),
					File: n.FilePath, Line: n.StartLine,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// collectCommunityGaps walks the cached community result and
// produces two parallel rollups in one pass: thin communities (under
// the size threshold) and single-file communities (every member from
// the same file — usually a sign the cluster never crossed a module
// boundary).
func (s *Server) collectCommunityGaps(thinSize int, pathPrefix string, limit int) (thin, singleFile []gapCommunity) {
	thin = make([]gapCommunity, 0)
	singleFile = make([]gapCommunity, 0)

	cr := s.getCommunities()
	if cr == nil {
		return thin, singleFile
	}

	for _, c := range cr.Communities {
		// Path-prefix scope: keep the community if at least one
		// file lies under the prefix. Empty prefix = no filter.
		if pathPrefix != "" {
			match := false
			for _, f := range c.Files {
				if strings.HasPrefix(f, pathPrefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		row := gapCommunity{ID: c.ID, Label: c.Label, Size: c.Size, Files: c.Files}
		if c.Size < thinSize {
			thin = append(thin, row)
		}
		if len(c.Files) == 1 {
			singleFile = append(singleFile, row)
		}
	}

	sort.Slice(thin, func(i, j int) bool { return thin[i].Size < thin[j].Size })
	sort.Slice(singleFile, func(i, j int) bool { return singleFile[i].Size > singleFile[j].Size })

	if len(thin) > limit {
		thin = thin[:limit]
	}
	if len(singleFile) > limit {
		singleFile = singleFile[:limit]
	}
	return thin, singleFile
}

// collectUntestedHotspots ranks scoped function/method nodes by
// in-edge count, takes the top `hotspotLimit`, and keeps those with
// coverage_pct < minCov or no coverage data at all. Independent of
// analyze hotspots (which gates on mean+2σ) so it still surfaces
// load-bearing nodes in small repos.
//
// Reads from the prebuilt NodeDegreeByKinds aggregate when present;
// falls back to NodeDegreeAggregator (the older IN-list shape) for
// backends that only publish that one, and finally to per-node
// GetInEdges for everyone else.
func (s *Server) collectUntestedHotspots(ctx context.Context, scoped []*graph.Node, pathPrefix string, hotspotLimit int, minCov float64, limit int, degreeByID map[string]graph.NodeDegreeRow) []gapUntestedHotspot {
	type ranked struct {
		node  *graph.Node
		fanIn int
	}
	pool := make([]*graph.Node, 0, len(scoped))
	for _, n := range scoped {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pool = append(pool, n)
	}
	candidates := make([]ranked, 0, len(pool))
	switch {
	case degreeByID != nil:
		for _, n := range pool {
			r := degreeByID[n.ID]
			candidates = append(candidates, ranked{node: n, fanIn: r.InCount})
		}
	default:
		reader := s.readerFor(ctx)
		if agg, ok := reader.(graph.NodeDegreeAggregator); ok && len(pool) > 0 {
			ids := make([]string, 0, len(pool))
			byID := make(map[string]*graph.Node, len(pool))
			for _, n := range pool {
				ids = append(ids, n.ID)
				byID[n.ID] = n
			}
			for _, r := range agg.NodeDegreeCounts(ids, nil) {
				n := byID[r.NodeID]
				if n == nil {
					continue
				}
				candidates = append(candidates, ranked{node: n, fanIn: r.InCount})
			}
		} else {
			for _, n := range pool {
				candidates = append(candidates, ranked{node: n, fanIn: len(reader.GetInEdges(n.ID))})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fanIn > candidates[j].fanIn
	})
	if len(candidates) > hotspotLimit {
		candidates = candidates[:hotspotLimit]
	}

	out := make([]gapUntestedHotspot, 0)
	covRows := s.coverageByID()
	for _, c := range candidates {
		// A "hotspot" with zero callers isn't a hotspot — drop it.
		// Disconnected functions are already covered by the
		// disconnected_nodes rollup.
		if c.fanIn == 0 {
			continue
		}
		pct, has := coveragePctFrom(covRows, c.node)
		if has && pct >= minCov {
			continue
		}
		out = append(out, gapUntestedHotspot{
			ID:          c.node.ID,
			Name:        c.node.Name,
			File:        c.node.FilePath,
			Line:        c.node.StartLine,
			FanIn:       c.fanIn,
			Coverage:    pct,
			HasCoverage: has,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FanIn != out[j].FanIn {
			return out[i].FanIn > out[j].FanIn
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
