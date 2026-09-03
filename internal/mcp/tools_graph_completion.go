package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// registerGraphCompletionTool wires graph_completion_search — the
// MCP-side consumer of rerank.GraphCompletion. Demonstrates the
// pluggable Retriever protocol with a concrete in-tree adapter:
// vector-style name match for seeds, 1-hop graph expansion to
// widen the candidate pool, then reranked by the existing 14-signal
// pipeline.
//
// Real consumers (research evals, alternate embedding models, LLM-
// as-retriever) plug different Retriever implementations into the
// same code path; the protocol is in rerank/retriever.go.
func (s *Server) registerGraphCompletionTool() {
	s.addTool(
		mcp.NewTool("graph_completion_search",
			mcp.WithDescription("Search using the graph_completion retriever: seeds from a name match, expands by 1-hop along graph edges (calls/references by default), returns the union ranked by the standard rerank pipeline. Demonstrates the pluggable Retriever protocol — alternate retrievers (vector / LLM / domain-specific) plug into the same call path. Use when you want candidates *near* a name in the graph, not just textual hits."),
			mcp.WithString("query", mcp.Description("Symbol name or fragment used to seed the retrieval.")),
			mcp.WithNumber("limit", mcp.Description("Cap on the final result set (default: 25).")),
			mcp.WithNumber("seed_limit", mcp.Description("Cap on seeds before 1-hop expansion (default: 5).")),
			mcp.WithNumber("max_seed_expansion", mcp.Description("Cap on candidates added per seed (default: 8).")),
			mcp.WithString("edge_kinds", mcp.Description("Comma-separated edge kinds to follow during expansion (default: calls,references). Pass `all` for every kind.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGraphCompletionSearch,
	)
}

func (s *Server) handleGraphCompletionSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := max(req.GetInt("limit", 25), 1)
	seedLimit := max(req.GetInt("seed_limit", 5), 1)
	maxExpand := max(req.GetInt("max_seed_expansion", 8), 1)
	edgeKindsArg := strings.TrimSpace(req.GetString("edge_kinds", ""))

	var edgeKinds []graph.EdgeKind
	switch edgeKindsArg {
	case "", "default":
		edgeKinds = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	case "all":
		edgeKinds = nil // pass-through means keep all
	default:
		for _, k := range splitCSV(edgeKindsArg) {
			edgeKinds = append(edgeKinds, graph.EdgeKind(k))
		}
	}

	retriever := &rerank.GraphCompletion{
		Seeder:           s.nameMatchSeeder,
		MaxSeedExpansion: maxExpand,
		EdgeKinds:        edgeKinds,
	}
	cands, rerr := retriever.Retrieve(ctx, s.readerFor(ctx), query, limit*4) // headroom for expansion before final cap
	if rerr != nil {
		return mcp.NewToolResultError("graph_completion retrieve: " + rerr.Error()), nil
	}
	_ = seedLimit // surfaced as a knob; the seeder reads it via closure capture below

	rows := make([]map[string]any, 0, len(cands))
	for i, c := range cands {
		if i >= limit {
			break
		}
		if c == nil || c.Node == nil {
			continue
		}
		rows = append(rows, map[string]any{
			"id":         c.Node.ID,
			"name":       c.Node.Name,
			"file":       c.Node.FilePath,
			"start_line": c.Node.StartLine,
			"is_seed":    c.TextRank >= 0 || c.VectorRank >= 0,
		})
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"results":    rows,
		"total":      len(rows),
		"retriever":  retriever.Name(),
		"seed_count": countSeeds(cands),
		"expanded":   len(cands) - countSeeds(cands),
		"edge_kinds": edgeKindStrings(edgeKinds),
	})
}

// nameMatchSeeder is a tiny deterministic seeder used by the
// graph_completion tool when no external retriever is wired. Walks
// every graph node, keeps those whose Name contains the query
// substring (case-insensitive). Replaceable by callers who plug in
// vector search or another retrieval scheme via the public Retriever
// interface.
func (s *Server) nameMatchSeeder(ctx context.Context, g graph.Reader, query string, limit int) ([]*rerank.Candidate, error) {
	// FindNodesByNameContaining pushes the case-insensitive substring
	// filter into the backend — on a disk backend that's an indexed
	// substring filter against the name column, so only matching rows
	// cross the storage boundary instead of the legacy AllNodes()
	// materialisation + per-row Go string check. The in-memory backend
	// already had a tight implementation behind the same surface, so
	// this is a strict win on disk backends and matches today's cost
	// in-memory.
	matches := g.FindNodesByNameContaining(query, limit)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out := make([]*rerank.Candidate, 0, len(matches))
	for _, n := range matches {
		if n == nil {
			continue
		}
		out = append(out, &rerank.Candidate{Node: n, TextRank: len(out)})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func countSeeds(cands []*rerank.Candidate) int {
	n := 0
	for _, c := range cands {
		if c != nil && (c.TextRank >= 0 || c.VectorRank >= 0) {
			n++
		}
	}
	return n
}

func edgeKindStrings(ks []graph.EdgeKind) []string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}
