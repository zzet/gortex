package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/query"
)

// registerChurnRateTool wires get_churn_rate — a pure graph scan over
// per-symbol churn metadata pre-computed by `gortex enrich churn`.
//
// At read time the handler does NOT shell out to git. Every value it
// returns lives in n.Meta["churn"] on the node, populated either by
// the CLI/git-hook (which writes through the on-disk backend) or by
// an in-process call to the enrich_churn MCP tool. When no node in
// scope has the data, the response is a structured error pointing
// the agent at the enrich command.
func (s *Server) registerChurnRateTool() {
	s.addTool(
		mcp.NewTool("get_churn_rate",
			mcp.WithDescription("Per-symbol git-commit density, read from pre-computed graph data. For each function/method in scope returns {symbol_id, name, file, churn_rate (commits per active day), commit_count, age_days, last_author, last_commit_at}. Sort and filter by churn_rate or commit_count to find unstable abstractions, hidden coupling, and bus-factor risks. Data is populated by `gortex enrich churn` (or the enrich_churn MCP tool); when nothing in scope has churn meta the tool returns a structured error with the suggested next command. No git subprocess at request time — sub-second on indexed repos."),
			mcp.WithString("path_prefix", mcp.Description("Scope analysis to nodes under this file-path prefix.")),
			mcp.WithNumber("min_commits", mcp.Description("Only return symbols with at least this many commits (default: 1).")),
			mcp.WithString("kinds", mcp.Description("Comma-separated kinds (default: function,method). Pass 'all' for every symbol.")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 100).")),
			mcp.WithString("sort_by", mcp.Description("Sort key: churn_rate (default), commit_count, age_days.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetChurnRate,
	)
}

type churnRow struct {
	ID           string  `json:"symbol_id"`
	Name         string  `json:"name"`
	File         string  `json:"file"`
	StartLine    int     `json:"start_line"`
	EndLine      int     `json:"end_line"`
	CommitCount  int     `json:"commit_count"`
	AgeDays      int     `json:"age_days"`
	ChurnRate    float64 `json:"churn_rate"`
	LastAuthor   string  `json:"last_author,omitempty"`
	LastCommitAt string  `json:"last_commit_at,omitempty"`
}

func (s *Server) handleGetChurnRate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	minCommits := max(req.GetInt("min_commits", 1), 0)
	limit := max(req.GetInt("limit", 100), 1)
	sortBy := strings.TrimSpace(req.GetString("sort_by", "churn_rate"))

	allowed := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(req.GetString("kinds", "")); k != "" && k != "all" {
		allowed = parseAnalyzeKindsFilter(k)
	} else if k == "all" {
		allowed = nil
	}

	rows := make([]churnRow, 0, 64)
	seenFiles := map[string]struct{}{}
	sawMeta := false

	usedSidecar := false
	// The sidecar is probed on the request's reader. An overlay view
	// carries no churn rows, so an overlay-active call falls through to
	// the meta scan below and reports churn for the symbols its buffers
	// still define.
	reader := s.readerFor(ctx)
	if sidecar, ok := reader.(graph.ChurnEnrichmentReader); ok {
		// Sidecar fast-path (change A): read the typed churn rows via an
		// index over the (small) enriched set, then resolve their nodes
		// in one batch — instead of scanning AllNodes and gob-decoding
		// every meta blob to peek at Meta["churn"].
		if enrich := sidecar.ChurnRows(""); len(enrich) > 0 {
			usedSidecar = true
			sawMeta = true
			ids := make([]string, 0, len(enrich))
			for _, e := range enrich {
				ids = append(ids, e.NodeID)
			}
			nodes := reader.GetNodesByIDs(ids)
			sessWS, _, bound := s.sessionScope(ctx)
			var opts query.QueryOptions
			if bound {
				opts = query.QueryOptions{WorkspaceID: sessWS}
			}
			for _, e := range enrich {
				n := nodes[e.NodeID]
				if n == nil {
					continue
				}
				if bound && !opts.ScopeAllows(n) {
					continue
				}
				if allowed != nil {
					if _, ok := allowed[n.Kind]; !ok {
						continue
					}
				}
				if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
					continue
				}
				if e.CommitCount < minCommits {
					continue
				}
				rows = append(rows, churnRowFromEnrichment(n, e))
				seenFiles[n.FilePath] = struct{}{}
			}
		}
	}
	if !usedSidecar {
		// Fallback: no sidecar rows yet (un-migrated DB, recompute-on-
		// next-enrich) or a backend without the capability — read
		// Meta["churn"] off a full AllNodes scan.
		for _, n := range s.scopedNodes(ctx) {
			if allowed != nil {
				if _, ok := allowed[n.Kind]; !ok {
					continue
				}
			}
			if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
				continue
			}
			row, ok := churnRowFromMeta(n)
			if !ok {
				continue
			}
			sawMeta = true
			if row.CommitCount < minCommits {
				continue
			}
			rows = append(rows, row)
			seenFiles[n.FilePath] = struct{}{}
		}
	}

	if !sawMeta {
		// No node in scope carries meta.churn — the agent needs to
		// run the enricher before this tool can answer. We surface
		// the gap loudly rather than returning an empty result that
		// looks like "nothing churns" (which is misleading).
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"error":      "no churn data in scope; run `gortex enrich churn` (or call the enrich_churn MCP tool) to populate meta.churn",
			"suggestion": "gortex enrich churn",
			"symbols":    []churnRow{},
			"total":      0,
			"truncated":  false,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		switch sortBy {
		case "commit_count":
			if rows[i].CommitCount != rows[j].CommitCount {
				return rows[i].CommitCount > rows[j].CommitCount
			}
		case "age_days":
			if rows[i].AgeDays != rows[j].AgeDays {
				return rows[i].AgeDays > rows[j].AgeDays
			}
		default: // churn_rate
			if rows[i].ChurnRate != rows[j].ChurnRate {
				return rows[i].ChurnRate > rows[j].ChurnRate
			}
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbols":       rows,
		"total":         len(rows),
		"truncated":     truncated,
		"scanned_files": len(seenFiles),
		"sort_by":       sortBy,
		"min_commits":   minCommits,
	})
}

// churnRowFromEnrichment builds a response row from a node + its typed
// sidecar churn enrichment (change A read path).
func churnRowFromEnrichment(n *graph.Node, e graph.ChurnEnrichment) churnRow {
	endLine := n.EndLine
	if endLine == 0 {
		endLine = n.StartLine
	}
	return churnRow{
		ID: n.ID, Name: n.Name, File: n.FilePath,
		StartLine: n.StartLine, EndLine: endLine,
		CommitCount:  e.CommitCount,
		AgeDays:      e.AgeDays,
		ChurnRate:    e.ChurnRate,
		LastAuthor:   e.LastAuthor,
		LastCommitAt: e.LastCommitAt,
	}
}

// churnRowFromMeta projects a node's meta.churn payload into the
// response row. Returns (zero, false) when the node has no churn
// metadata — the caller distinguishes "missing data" from
// "filtered out". The Meta layout matches what
// internal/churn.EnrichGraph writes:
//
//	meta.churn = {
//	  commit_count:   int,
//	  age_days:       int,
//	  churn_rate:     float64,
//	  last_author:    string,
//	  last_commit_at: RFC3339 string,
//	}
//
// Numeric fields tolerate both int and float64 because Meta round-
// trips through the on-disk backend or JSON (snapshots), which can widen
// ints to floats. Missing fields default to zero — they're stamped
// together so partial payloads are unexpected, but a defensive read
// is cheaper than asserting and crashing on an old snapshot.
func churnRowFromMeta(n *graph.Node) (churnRow, bool) {
	if n == nil || n.Meta == nil {
		return churnRow{}, false
	}
	raw, ok := n.Meta["churn"].(map[string]any)
	if !ok || len(raw) == 0 {
		return churnRow{}, false
	}
	endLine := n.EndLine
	if endLine == 0 {
		endLine = n.StartLine
	}
	row := churnRow{
		ID: n.ID, Name: n.Name, File: n.FilePath,
		StartLine: n.StartLine, EndLine: endLine,
		CommitCount: intFromAny(raw["commit_count"]),
		AgeDays:     intFromAny(raw["age_days"]),
		ChurnRate:   floatFromAny(raw["churn_rate"]),
	}
	if v, ok := raw["last_author"].(string); ok {
		row.LastAuthor = v
	}
	if v, ok := raw["last_commit_at"].(string); ok {
		row.LastCommitAt = v
	}
	return row, true
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

func floatFromAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
