package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// suggestedQuery is one cold-start exploration starting point: a query
// string the agent can hand straight to search_symbols / smart_context,
// plus the category and a one-line rationale.
type suggestedQuery struct {
	Query    string `json:"query"`
	Category string `json:"category"`
	Why      string `json:"why"`
}

// handleSuggestQueries returns 5-10 starter exploration queries for an
// unfamiliar repository, derived from its entry points, load-bearing
// hubs, community bridges, and largest subsystems.
func (s *Server) handleSuggestQueries(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 8)
	if limit < 1 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}

	scoped := s.scopedNodesLight(ctx)
	_, _, bound := s.sessionScope(ctx)
	var inScope map[string]bool
	if bound {
		inScope = make(map[string]bool, len(scoped))
		for _, n := range scoped {
			inScope[n.ID] = true
		}
	}

	suggestions := s.buildSuggestedQueries(ctx, scoped, inScope, limit)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"suggestions": suggestions,
		"count":       len(suggestions),
	})
}

// symbolStat carries the fan-in and community-crossing counts used to
// rank symbols into the hub and bridge categories.
type symbolStat struct {
	node      *graph.Node
	fanIn     int
	crossings int
}

// buildSuggestedQueries assembles the cold-start suggestion list. The
// categories are drawn in a fixed order — entry points first (where a
// reader starts), then bridges and hubs (the load-bearing seams), then
// subsystems and shared modules. The whole list is deterministic:
// every ranking sort carries an ID tie-break.
func (s *Server) buildSuggestedQueries(ctx context.Context, scoped []*graph.Node, inScope map[string]bool, limit int) []suggestedQuery {
	g := s.readerFor(ctx)
	var out []suggestedQuery
	seen := make(map[string]bool)
	add := func(query, category, why string) {
		query = strings.TrimSpace(query)
		key := strings.ToLower(query)
		if query == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, suggestedQuery{Query: query, Category: category, Why: why})
	}

	// 1. Entry points — where the program starts executing.
	for i, ep := range entryPoints(g, inScope, 3) {
		if i >= 2 {
			break
		}
		fp, _ := ep["file_path"].(string)
		add(entryPointQuery(fp), "entry_point", "program entry point — "+fp)
	}

	// Rank every code symbol by incoming call/reference edges (fan-in)
	// and by how many of those edges cross a community boundary. Done
	// directly off the graph rather than via FindHotspots, whose
	// mean+2σ threshold returns nothing on small repositories.
	//
	// EdgesByKind streams from the storage layer (one query per kind
	// on a disk backend, an indexed bucket scan in-memory) so the cost is
	// O(call+reference edges) once — replacing the per-node
	// GetInEdges loop that was N cgo round-trips materialising the
	// full in-edge bucket per candidate.
	nodeToComm := map[string]string{}
	if comms := s.getCommunities(); comms != nil {
		nodeToComm = comms.NodeToComm
	}
	statByID := make(map[string]*symbolStat, len(scoped))
	stats := make([]symbolStat, 0, len(scoped))
	for _, n := range scoped {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod && n.Kind != graph.KindType {
			continue
		}
		stats = append(stats, symbolStat{node: n})
	}
	for i := range stats {
		statByID[stats[i].node.ID] = &stats[i]
	}
	for _, k := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range g.EdgesByKind(k) {
			if e == nil {
				continue
			}
			st, ok := statByID[e.To]
			if !ok {
				continue
			}
			st.fanIn++
			myComm := nodeToComm[e.To]
			if c := nodeToComm[e.From]; myComm != "" && c != "" && c != myComm {
				st.crossings++
			}
		}
	}

	// 2. Bridges — symbols pulled at from the most other subsystems.
	bridges := append([]symbolStat(nil), stats...)
	sort.SliceStable(bridges, func(i, j int) bool {
		if bridges[i].crossings != bridges[j].crossings {
			return bridges[i].crossings > bridges[j].crossings
		}
		return bridges[i].node.ID < bridges[j].node.ID
	})
	added := 0
	for _, st := range bridges {
		if added >= 2 || st.crossings < 2 {
			break
		}
		add(st.node.Name, "bridge", fmt.Sprintf("bridges %d subsystems — a seam worth understanding", st.crossings))
		added++
	}

	// 3. Hubs — the highest-fan-in load-bearing symbols.
	hubs := append([]symbolStat(nil), stats...)
	sort.SliceStable(hubs, func(i, j int) bool {
		if hubs[i].fanIn != hubs[j].fanIn {
			return hubs[i].fanIn > hubs[j].fanIn
		}
		return hubs[i].node.ID < hubs[j].node.ID
	})
	added = 0
	for _, st := range hubs {
		if added >= 3 || st.fanIn < 2 {
			break
		}
		add(st.node.Name, "hub", fmt.Sprintf("load-bearing hub — %d callers depend on it", st.fanIn))
		added++
	}

	// 4. Subsystems — the largest community clusters.
	if comms := s.getCommunities(); comms != nil {
		clusters := communitiesInScope(comms.Communities, inScope)
		sort.SliceStable(clusters, func(i, j int) bool {
			if clusters[i].Size != clusters[j].Size {
				return clusters[i].Size > clusters[j].Size
			}
			return clusters[i].ID < clusters[j].ID
		})
		added = 0
		for _, c := range clusters {
			if added >= 3 {
				break
			}
			query := c.Hub
			if query == "" {
				query = subsystemQuery(c.Label)
			}
			if query == "" {
				continue
			}
			add(query, "subsystem", fmt.Sprintf("entry into the %q subsystem — %d symbols", c.Label, c.Size))
			added++
		}
	}

	// 5. Shared modules — the files almost everything imports.
	for i, f := range mostImportedFiles(g, inScope, 5) {
		if i >= 2 {
			break
		}
		path, _ := f["path"].(string)
		count, _ := f["import_count"].(int)
		add(moduleQuery(path), "shared_module", fmt.Sprintf("shared module — imported by %d files", count))
	}

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// entryPointQuery turns an entry-point file path into a search query —
// the enclosing directory ("cmd/gortex" → a path-class query) or, for a
// root-level file, its stem.
func entryPointQuery(filePath string) string {
	if filePath == "" {
		return ""
	}
	dir := filepath.Dir(filePath)
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return pathStem(filePath)
	}
	return dir
}

// moduleQuery turns a file path into a search query — its stem, which
// is the name an agent would actually search for.
func moduleQuery(filePath string) string { return pathStem(filePath) }

// pathStem returns the base name of a path with its extension removed.
func pathStem(p string) string {
	base := filepath.Base(p)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// subsystemQuery extracts a searchable keyword from a community label
// like "parser/languages +12 dirs · Type" — the leading whitespace-
// delimited token.
func subsystemQuery(label string) string {
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// communitiesInScope keeps communities with at least one member inside
// the session's workspace. A nil inScope returns every community.
func communitiesInScope(comms []analysis.Community, inScope map[string]bool) []analysis.Community {
	if inScope == nil {
		return append([]analysis.Community(nil), comms...)
	}
	out := make([]analysis.Community, 0, len(comms))
	for _, c := range comms {
		for _, m := range c.Members {
			if inScope[m] {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
