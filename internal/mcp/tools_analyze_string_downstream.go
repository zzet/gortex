// Analyzers that consume the KindString registry as their primary
// input — the downstream side of the string-anchored extractor
// pipeline:
//
//   - log_events: aggregate log-message KindString nodes (the data-
//     side companion to KindEvent log emissions) by literal value,
//     with emitter list and severity level.
//   - sql_rebuild: short-circuit re-derive the KindTable / KindColumn
//     / EdgeQueries layer from the sql-context KindString registry,
//     without re-parsing source.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	gortexsql "github.com/zzet/gortex/internal/sql"
)

// handleAnalyzeLogEvents walks KindString nodes with
// context="log_message" — the registry shadow of log KindEvent
// emissions — and groups by literal value. Each row carries the
// string node id, the literal value, the inferred severity level
// (from Meta), and the unique emitter symbols.
//
// Distinct from event_emitters in two ways: the source is the
// KindString registry (richer per-literal grouping; multiple
// canonicalised event nodes can collapse onto the same literal) and
// the row includes a severity column derived from the emitter's
// matched method.
//
// Filters:
//
//   - level: log severity (case-insensitive). Matches against the
//     edge's `level` meta first, falling back to the method name so
//     callers using either taxonomy match.
//   - value: literal value (case-insensitive substring match).
func (s *Server) handleAnalyzeLogEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	levelFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "level")))
	valueFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "value")))

	type logRow struct {
		ID       string   `json:"id"`
		Value    string   `json:"value"`
		Level    string   `json:"level,omitempty"`
		Emits    int      `json:"emits"`
		Emitters []string `json:"emitters,omitempty"`
	}
	reader := s.readerFor(ctx)
	byString := map[string]*logRow{}
	for e := range edgesByKinds(reader, graph.EdgeEmits) {
		n := reader.GetNode(e.To)
		if n == nil || n.Kind != graph.KindString {
			continue
		}
		ctxLabel, _ := n.Meta["context"].(string)
		if ctxLabel != "log_message" {
			continue
		}
		level, _ := e.Meta["level"].(string)
		if level == "" {
			level, _ = n.Meta["level"].(string)
		}
		if levelFilter != "" {
			method, _ := e.Meta["method"].(string)
			if !levelMatches(levelFilter, level) && !levelMatches(levelFilter, method) {
				continue
			}
		}
		if valueFilter != "" && !strings.Contains(strings.ToLower(n.Name), valueFilter) {
			continue
		}
		row, ok := byString[e.To]
		if !ok {
			row = &logRow{ID: e.To, Value: n.Name, Level: level}
			byString[e.To] = row
		}
		if row.Level == "" && level != "" {
			row.Level = level
		}
		row.Emits++
		row.Emitters = appendUnique(row.Emitters, e.From)
	}
	rows := make([]*logRow, 0, len(byString))
	for _, r := range byString {
		sort.Strings(r.Emitters)
		rows = append(rows, r)
	}
	// Scope filter: keep a row iff its string node (subject) is visible to
	// the current request, and prune its emitter (actor) list to visible
	// emitters. Emits is an edge count (not a node ID) so it is left
	// intact; total recomputes below. No-op for an unbound request.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*logRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, reader.GetNode(r.ID)) {
				continue
			}
			emitters := make([]string, 0, len(r.Emitters))
			for _, em := range r.Emitters {
				if s.analyzeNodeVisible(ctx, reader.GetNode(em)) {
					emitters = append(emitters, em)
				}
			}
			r.Emitters = emitters
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Emits != rows[j].Emits {
			return rows[i].Emits > rows[j].Emits
		}
		if rows[i].Level != rows[j].Level {
			return rows[i].Level < rows[j].Level
		}
		return rows[i].Value < rows[j].Value
	})

	if s.isGCX(ctx, req) {
		items := make([]logEventItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, logEventItem{
				ID:       r.ID,
				Value:    r.Value,
				Level:    r.Level,
				Emits:    r.Emits,
				Emitters: strings.Join(r.Emitters, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("log_events", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			level := r.Level
			if level == "" {
				level = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Emits, level, r.Value)
		}
		if len(rows) == 0 {
			b.WriteString("no log events\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"events": rows,
		"total":  len(rows),
	})
}

// handleAnalyzeSQLRebuild is the short-circuit operator: it walks
// the KindString context="sql" registry already present in
// the graph and rederives the KindTable / KindColumn / EdgeQueries
// / EdgeReadsCol / EdgeWritesCol layer from those nodes alone,
// without re-parsing source. Idempotent (graph.AddNode / AddEdge
// dedupe by ID + edgeKey) — running it twice on a stable graph
// reports `tables_created: 0, emitters_linked: 0`.
//
// Returns a single-row summary of counts: how many string nodes
// were visited, how many table / column nodes were created, how
// many EdgeQueries / EdgeReadsCol / EdgeWritesCol edges were
// emitted, and how many (caller, table) pairs were linked.
//
// Use cases:
//
//   - Re-derive the SQL layer after enabling the SQL coverage gate
//     on an existing index without forcing a full reindex.
//   - Recover the SQL layer after a snapshot round-trip that
//     dropped Node.Meta / Edge.Meta (per the graph's JSON exclusion
//     of Meta).
//   - Health-check the registry — a non-zero `skipped` count means
//     KindString sql nodes exist that produced zero tables, which
//     hints at parser regressions in sql.ExtractTables.
func (s *Server) handleAnalyzeSQLRebuild(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stats := gortexsql.RebuildTablesFromStringRegistry(s.graph)

	if s.isGCX(ctx, req) {
		items := []sqlRebuildItem{{
			StringsVisited: stats.StringsVisited,
			TablesCreated:  stats.TablesCreated,
			ColumnsCreated: stats.ColumnsCreated,
			QueryEdges:     stats.QueryEdges,
			ReadColEdges:   stats.ReadColEdges,
			WriteColEdges:  stats.WriteColEdges,
			EmittersLinked: stats.EmittersLinked,
			Skipped:        stats.Skipped,
		}}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("sql_rebuild", items))
	}

	if isCompact(req) {
		return mcp.NewToolResultText(fmt.Sprintf(
			"strings=%d tables+%d cols+%d queries+%d reads+%d writes+%d emitters+%d skipped=%d\n",
			stats.StringsVisited, stats.TablesCreated, stats.ColumnsCreated,
			stats.QueryEdges, stats.ReadColEdges, stats.WriteColEdges,
			stats.EmittersLinked, stats.Skipped,
		)), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"strings_visited":     stats.StringsVisited,
		"tables_created":      stats.TablesCreated,
		"columns_created":     stats.ColumnsCreated,
		"query_edges_created": stats.QueryEdges,
		"reads_col_edges":     stats.ReadColEdges,
		"writes_col_edges":    stats.WriteColEdges,
		"emitters_linked":     stats.EmittersLinked,
		"skipped":             stats.Skipped,
	})
}

// handleAnalyzeSQLCallSites lists the call sites that execute SQL,
// grouped by the calling symbol, with the tables each one touches and
// a read / write split. Legacy calls materialise the table / EdgeQueries layer
// first; the compact read-only adapter fixes materialize=false and requires an
// explicit effectful sql_rebuild operation when that layer is absent.
//
// Filters: name (call-site symbol name, case-insensitive), limit.
func (s *Server) handleAnalyzeSQLCallSites(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// The rebuild writes nodes and edges, so it stays on the base store — a
	// request view is read-only. The rows below read through the request
	// reader, which serves the freshly materialised base layer plus whatever
	// the caller's buffers changed on top of it.
	if requestBoolDefault(req, "materialize", true) {
		gortexsql.RebuildTablesFromStringRegistry(s.graph)
	}
	args := req.GetArguments()
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type sqlCallSite struct {
		Symbol  string   `json:"symbol"`
		Name    string   `json:"name"`
		File    string   `json:"file,omitempty"`
		Tables  []string `json:"tables,omitempty"`
		Queries int      `json:"queries"`
		Reads   int      `json:"reads"`
		Writes  int      `json:"writes"`
	}
	reader := s.readerFor(ctx)
	bySite := map[string]*sqlCallSite{}
	for e := range edgesByKinds(reader, graph.EdgeQueries) {
		row, ok := bySite[e.From]
		if !ok {
			name, file := e.From, ""
			if n := reader.GetNode(e.From); n != nil {
				name, file = n.Name, n.FilePath
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &sqlCallSite{Symbol: e.From, Name: name, File: file}
			bySite[e.From] = row
		}
		row.Queries++
		if op, _ := e.Meta["op"].(string); op == "write" {
			row.Writes++
		} else {
			row.Reads++
		}
		if t := reader.GetNode(e.To); t != nil && t.Name != "" {
			row.Tables = appendUnique(row.Tables, t.Name)
		}
	}

	rows := make([]*sqlCallSite, 0, len(bySite))
	for _, r := range bySite {
		sort.Strings(r.Tables)
		rows = append(rows, r)
	}
	// Scope filter: keep only call sites whose calling symbol is visible
	// to the current request. Tables are names (not node IDs) so they need
	// no pruning. total/truncated recompute below. No-op for an unbound
	// request.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*sqlCallSite, 0, len(rows))
		for _, r := range rows {
			if s.analyzeNodeVisible(ctx, reader.GetNode(r.Symbol)) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Queries != rows[j].Queries {
			return rows[i].Queries > rows[j].Queries
		}
		return rows[i].Symbol < rows[j].Symbol
	})
	truncated := false
	if nameFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("sql_call_sites", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d r%d/w%d %s [%s]\n",
				r.Queries, r.Reads, r.Writes, r.Name, strings.Join(r.Tables, ","))
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no SQL call sites\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"call_sites": rows,
		"total":      len(rows),
		"truncated":  truncated,
	})
}
