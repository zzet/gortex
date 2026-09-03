package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// ---------------------------------------------------------------------------
// analyze kind=tests_as_edges
// ---------------------------------------------------------------------------
//
// A first-class view over the EdgeTests layer — the test → code edges
// the indexer stamps for every call from a test function to a
// non-test symbol. get_untested_symbols answers the negative ("what
// has no tests"); this answers the positive: which symbols ARE
// exercised, and by which tests.
//
// group_by=symbol (default) lists each tested symbol with the tests
// covering it; group_by=test inverts it — each test with the symbols
// it exercises. Both carry a summary of the edge layer's size.

type testEdgeRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type testEdgeRow struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Kind    string        `json:"kind"`
	File    string        `json:"file"`
	Line    int           `json:"line"`
	Related []testEdgeRef `json:"related"`
	Count   int           `json:"count"`
}

func (s *Server) handleAnalyzeTestsAsEdges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 200)
	groupBy := strings.ToLower(strings.TrimSpace(stringArg(args, "group_by")))
	if groupBy == "" {
		groupBy = "symbol"
	}
	if groupBy != "symbol" && groupBy != "test" {
		return mcp.NewToolResultError("analyze tests_as_edges: group_by must be 'symbol' or 'test'"), nil
	}

	// Collect the EdgeTests layer. From = test function, To = the
	// non-test symbol it exercises.
	reader := s.readerFor(ctx)
	testsBySymbol := make(map[string][]string)
	symbolsByTest := make(map[string][]string)
	edgeCount := 0
	for e := range edgesByKinds(reader, graph.EdgeTests) {
		edgeCount++
		testsBySymbol[e.To] = append(testsBySymbol[e.To], e.From)
		symbolsByTest[e.From] = append(symbolsByTest[e.From], e.To)
	}

	primary := testsBySymbol
	if groupBy == "test" {
		primary = symbolsByTest
	}

	// Batch-fetch every primary key and every related ID in one bulk
	// round-trip. On a repo with thousands of EdgeTests edges the old
	// per-id GetNode pattern burned one round-trip per row plus
	// one per related ID on a disk backend — easily 5-10k round-trips per
	// analyze kind=tests_as_edges call.
	idSet := make(map[string]struct{}, len(primary))
	for id, relatedIDs := range primary {
		idSet[id] = struct{}{}
		for _, rid := range relatedIDs {
			idSet[rid] = struct{}{}
		}
	}
	allIDs := make([]string, 0, len(idSet))
	for id := range idSet {
		allIDs = append(allIDs, id)
	}
	nodeByID := reader.GetNodesByIDs(allIDs)

	rows := make([]testEdgeRow, 0, len(primary))
	for id, relatedIDs := range primary {
		n := nodeByID[id]
		if n == nil {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		related := make([]testEdgeRef, 0, len(relatedIDs))
		seen := make(map[string]bool, len(relatedIDs))
		for _, rid := range relatedIDs {
			if seen[rid] {
				continue
			}
			seen[rid] = true
			name := rid
			if rn := nodeByID[rid]; rn != nil {
				name = rn.Name
			}
			related = append(related, testEdgeRef{ID: rid, Name: name})
		}
		sort.Slice(related, func(i, j int) bool { return related[i].ID < related[j].ID })
		rows = append(rows, testEdgeRow{
			ID:      n.ID,
			Name:    n.Name,
			Kind:    string(n.Kind),
			File:    n.FilePath,
			Line:    n.StartLine,
			Related: related,
			Count:   len(related),
		})
	}

	// Scope narrowing: keep a row iff its primary symbol is visible to
	// the request (workspace ceiling + optional repo allow-set), prune
	// its related peers to the visible set, recompute Count, and rebuild
	// the summary from the in-scope rows. Unbound requests skip this
	// branch — a byte-for-byte no-op.
	testEdges := edgeCount
	testedSymbols := len(testsBySymbol)
	testFunctions := len(symbolsByTest)
	if s.scopeFiltersActive(ctx) {
		kept := make([]testEdgeRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, nodeByID[r.ID]) {
				continue
			}
			related := make([]testEdgeRef, 0, len(r.Related))
			for _, ref := range r.Related {
				if s.analyzeNodeVisible(ctx, nodeByID[ref.ID]) {
					related = append(related, ref)
				}
			}
			r.Related = related
			r.Count = len(related)
			kept = append(kept, r)
		}
		rows = kept

		edges := 0
		relatedSet := make(map[string]struct{})
		for _, r := range rows {
			edges += r.Count
			for _, ref := range r.Related {
				relatedSet[ref.ID] = struct{}{}
			}
		}
		testEdges = edges
		if groupBy == "symbol" {
			testedSymbols = len(rows)
			testFunctions = len(relatedSet)
		} else {
			testFunctions = len(rows)
			testedSymbols = len(relatedSet)
		}
	}

	// Most-covered first — the symbols with the deepest test backing,
	// or the tests exercising the most code.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].ID < rows[j].ID
	})
	total := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	relatedKey := "tests"
	if groupBy == "test" {
		relatedKey = "covers"
	}
	// Re-key the generic "related" field to the group-appropriate name
	// in the JSON payload via a thin projection.
	projected := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		projected = append(projected, map[string]any{
			"id":         r.ID,
			"name":       r.Name,
			"kind":       r.Kind,
			"file":       r.File,
			"line":       r.Line,
			relatedKey:   r.Related,
			"edge_count": r.Count,
		})
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d %s:%d  %s\n", r.Count, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no test edges in scope\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"group_by": groupBy,
		"rows":     projected,
		"total":    total,
		"summary": map[string]any{
			"test_edges":     testEdges,
			"tested_symbols": testedSymbols,
			"test_functions": testFunctions,
		},
		"truncated": truncated,
	}
	if truncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}
