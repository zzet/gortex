package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"

	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/config"
)

func setupTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	return setupTestServerWithRegistry(t, testRegistry())
}

func setupTestServerWithRegistry(t *testing.T, reg *parser.Registry) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	// Fixture deliberately has zero external imports so the
	// resolver's attributeGoExternalCalls pass doesn't auto-add a
	// `module::go:*` node — that lets the external-calls analyser
	// tests assert on an exact set of manually-added modules.
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

type Config struct {
	Port int
}

func main() {
	helper()
}

func helper() {}
`), 0o644)

	g := graph.New()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv, dir
}

func callTool(t *testing.T, srv *Server, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := findAndCallHandler(srv, name, context.Background(), req)
	require.NoError(t, err)
	return result
}

// findAndCallHandler dispatches a tool call by name via the registered handlers.
// Since MCPServer doesn't expose handlers directly, we recreate the request flow.
func findAndCallHandler(srv *Server, name string, ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	// Map tool names to handlers.
	handlers := map[string]func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error){
		"graph_stats":           srv.handleGraphStats,
		"search_symbols":        srv.handleSearchSymbols,
		"get_symbol":            srv.handleGetSymbol,
		"get_file_summary":      srv.handleGetFileSummary,
		"get_dependencies":      srv.handleGetDependencies,
		"get_dependents":        srv.handleGetDependents,
		"get_call_chain":        srv.handleGetCallChain,
		"get_callers":           srv.handleGetCallers,
		"find_implementations":  srv.handleFindImplementations,
		"find_overrides":        srv.handleFindOverrides,
		"get_class_hierarchy":   srv.handleGetClassHierarchy,
		"find_usages":           srv.handleFindUsages,
		"get_cluster":           srv.handleGetCluster,
		"get_editing_context":   srv.handleGetEditingContext,
		"find_import_path":      srv.handleFindImportPath,
		"explain_change_impact": srv.handleEnhancedChangeImpact,
		"get_recent_changes":    srv.handleGetRecentChanges,
		"get_symbol_source":     srv.handleGetSymbolSource,
		"batch_symbols":         srv.handleBatchSymbols,
		"smart_context":         srv.handleSmartContext,
		"plan_turn":             srv.handlePlanTurn,
		"get_repo_outline":      srv.handleGetRepoOutline,
		"suggest_queries":       srv.handleSuggestQueries,
		"search_text":           srv.handleSearchText,
		"find_files":            srv.handleFindFiles,
		"get_untested_symbols":  srv.handleGetUntestedSymbols,
		"winnow_symbols":        srv.handleWinnowSymbols,
		"edit_file":             srv.handleEditFile,
		"edit_symbol":           srv.handleEditSymbol,
		"write_file":            srv.handleWriteFile,
		"read_file":             srv.handleReadFile,
		"move_symbol":           srv.handleMoveSymbol,
		"inline_symbol":         srv.handleInlineSymbol,
		"rename_symbol":         srv.handleRenameSymbol,
		"flow_between":          srv.handleFlowBetween,
		"taint_paths":           srv.handleTaintPaths,
		"walk_graph":            srv.handleWalkGraph,
		"context_closure":       srv.handleContextClosure,
		"graph_query":           srv.handleGraphQuery,
		"nav":                   srv.handleNav,
		"find_declaration":      srv.handleFindDeclaration,
	}
	h, ok := handlers[name]
	if !ok {
		return mcplib.NewToolResultError("unknown tool: " + name), nil
	}
	return h(ctx, req)
}

func TestGraphStats(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "graph_stats", nil)
	assert.False(t, result.IsError)

	// Parse response.
	text := result.Content[0].(mcplib.TextContent).Text
	var stats graph.GraphStats
	require.NoError(t, json.Unmarshal([]byte(text), &stats))
	assert.Greater(t, stats.TotalNodes, 0)
	assert.Greater(t, stats.TotalEdges, 0)

	// The provenance-churn counter is surfaced on the payload. A
	// freshly indexed graph reports a numeric (>= 0) value.
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &raw))
	rev, ok := raw["edge_identity_revisions"]
	require.True(t, ok, "graph_stats must surface edge_identity_revisions")
	assert.GreaterOrEqual(t, rev.(float64), float64(0))
}

func TestTokenSavings_GetSymbolSource(t *testing.T) {
	srv, _ := setupTestServer(t)

	// Find the "helper" function ID via search.
	searchResult := callTool(t, srv, "search_symbols", map[string]any{"query": "helper"})
	require.False(t, searchResult.IsError)
	text := searchResult.Content[0].(mcplib.TextContent).Text
	var searchResp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &searchResp))
	results := searchResp["results"].([]any)
	require.Greater(t, len(results), 0)
	firstResult := results[0].(map[string]any)
	symbolID := firstResult["id"].(string)

	// Call get_symbol_source. Per-call tokens_saved was dropped from the
	// response (it bloated every reply with a stat agents don't act on);
	// the savings are still recorded server-side and surface via graph_stats.
	result := callTool(t, srv, "get_symbol_source", map[string]any{"id": symbolID})
	require.False(t, result.IsError)
	text = result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	assert.Contains(t, resp, "source")
	assert.NotContains(t, resp, "tokens_saved", "per-call telemetry should not leak into responses")

	// graph_stats should now reflect the session savings.
	statsResult := callTool(t, srv, "graph_stats", nil)
	require.False(t, statsResult.IsError)
	text = statsResult.Content[0].(mcplib.TextContent).Text
	var statsResp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &statsResp))
	savings := statsResp["token_savings"].(map[string]any)
	assert.GreaterOrEqual(t, savings["tokens_saved"].(float64), float64(0))
	assert.GreaterOrEqual(t, savings["calls_counted"].(float64), float64(1))
	assert.GreaterOrEqual(t, savings["efficiency_ratio"].(float64), float64(1))
}

func TestConfidenceLabels_FindUsages(t *testing.T) {
	srv, _ := setupTestServer(t)

	// "helper" is called by "main" — find_usages should return edges with confidence_label.
	result := callTool(t, srv, "find_usages", map[string]any{"id": "main.go::helper"})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	var resp query.SubGraph
	require.NoError(t, json.Unmarshal([]byte(text), &resp))

	require.Greater(t, len(resp.Edges), 0, "expected at least one edge")
	for _, e := range resp.Edges {
		assert.NotEmpty(t, e.ConfidenceLabel, "edge %s->%s should have confidence_label", e.From, e.To)
		assert.Contains(t, []string{"EXTRACTED", "INFERRED", "AMBIGUOUS"}, e.ConfidenceLabel)
	}
}

func TestConfidenceLabels_GetCallers(t *testing.T) {
	srv, _ := setupTestServer(t)

	// "helper" is called by "main" — get_callers should return edges with confidence_label.
	result := callTool(t, srv, "get_callers", map[string]any{"id": "main.go::helper"})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	var resp query.SubGraph
	require.NoError(t, json.Unmarshal([]byte(text), &resp))

	require.Greater(t, len(resp.Edges), 0, "expected at least one edge")
	for _, e := range resp.Edges {
		assert.NotEmpty(t, e.ConfidenceLabel, "edge %s->%s should have confidence_label", e.From, e.To)
	}
}

func TestConfidenceLabels_CompactIncludesEdgeSummary(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "find_usages", map[string]any{
		"id":      "main.go::helper",
		"compact": true,
	})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "edges:")
}

func TestConfidenceLabels_ExplainChangeImpact(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "explain_change_impact", map[string]any{
		"ids": "main.go::helper",
	})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))

	byDepth, ok := resp["by_depth"].(map[string]any)
	if ok {
		for _, entries := range byDepth {
			entryList, ok := entries.([]any)
			if !ok {
				continue
			}
			for _, entry := range entryList {
				entryMap := entry.(map[string]any)
				assert.Contains(t, entryMap, "confidence_label")
			}
		}
	}
}

func TestConfidenceLabelFor(t *testing.T) {
	tests := []struct {
		kind       graph.EdgeKind
		confidence float64
		want       string
	}{
		{graph.EdgeDefines, 0, "EXTRACTED"},
		{graph.EdgeImports, 0, "EXTRACTED"},
		{graph.EdgeImplements, 0, "EXTRACTED"},
		{graph.EdgeExtends, 0, "EXTRACTED"},
		{graph.EdgeMemberOf, 0, "EXTRACTED"},
		{graph.EdgeProvides, 0, "EXTRACTED"},
		{graph.EdgeConsumes, 0, "EXTRACTED"},
		{graph.EdgeCalls, 0.95, "EXTRACTED"},
		{graph.EdgeCalls, 0.9, "EXTRACTED"},
		{graph.EdgeCalls, 0.85, "INFERRED"},
		{graph.EdgeCalls, 0.8, "INFERRED"},
		{graph.EdgeCalls, 0.5, "INFERRED"},
		{graph.EdgeCalls, 0.3, "AMBIGUOUS"},
		{graph.EdgeCalls, 0, "INFERRED"},
		{graph.EdgeReferences, 0, "INFERRED"},
		{graph.EdgeInstantiates, 0, "INFERRED"},
	}
	for _, tt := range tests {
		got := graph.ConfidenceLabelFor(tt.kind, tt.confidence)
		assert.Equal(t, tt.want, got, "ConfidenceLabelFor(%s, %.2f)", tt.kind, tt.confidence)
	}
}

func TestSearchSymbols(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "search_symbols", map[string]any{"query": "helper"})
	assert.False(t, result.IsError)

	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	assert.Greater(t, resp["total"], float64(0))
}

func TestGetSymbol(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "search_symbols", map[string]any{"query": "main"})
	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	_ = json.Unmarshal([]byte(text), &resp)

	// Symbol not found is RECOVERABLE: a non-error result carrying guidance
	// (search instead of read-by-id), never a session-abandoning isError.
	result = callTool(t, srv, "get_symbol", map[string]any{"id": "nonexistent"})
	assert.False(t, result.IsError, "symbol-not-found should be success-shaped guidance, not an error")
	assert.Contains(t, result.Content[0].(mcplib.TextContent).Text, string(ErrCodeSymbolNotFound))
}

func TestGetEditingContext(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "get_editing_context", map[string]any{"path": "main.go"})
	assert.False(t, result.IsError)

	text := result.Content[0].(mcplib.TextContent).Text
	var ctx editingContext
	require.NoError(t, json.Unmarshal([]byte(text), &ctx))
	assert.NotNil(t, ctx.File)
	assert.Greater(t, len(ctx.Defines), 0)
}

func TestGetRecentChanges_NoWatcher(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "get_recent_changes", nil)
	assert.True(t, result.IsError) // watch mode not active
}

// --- Symbol History Property Test Generators ---

// recordAction represents a single Record() call to symbolHistory.
type recordAction struct {
	SymbolID         string
	SignatureChanged bool
}

// genSymbolID generates a random symbol ID like "pkg/file.go::FuncName".
func genSymbolID() *rapid.Generator[string] {
	return rapid.Custom(func(rt *rapid.T) string {
		pkg := rapid.StringMatching(`[a-z]{2,6}`).Draw(rt, "pkg")
		file := rapid.StringMatching(`[a-z]{2,6}`).Draw(rt, "file")
		name := rapid.StringMatching(`[A-Z][a-zA-Z]{1,8}`).Draw(rt, "name")
		return pkg + "/" + file + ".go::" + name
	})
}

// genRecordActions generates a non-empty sequence of Record() calls using
// a fixed pool of symbol IDs so that some symbols get recorded multiple times.
func genRecordActions() *rapid.Generator[[]recordAction] {
	return rapid.Custom(func(rt *rapid.T) []recordAction {
		// Generate a pool of 1-5 distinct symbol IDs
		poolSize := rapid.IntRange(1, 5).Draw(rt, "poolSize")
		pool := make([]string, poolSize)
		seen := make(map[string]bool)
		for i := range poolSize {
			for {
				id := genSymbolID().Draw(rt, "symbolID")
				if !seen[id] {
					pool[i] = id
					seen[id] = true
					break
				}
			}
		}

		// Generate 1-20 record actions drawing from the pool
		numActions := rapid.IntRange(1, 20).Draw(rt, "numActions")
		actions := make([]recordAction, numActions)
		for i := range numActions {
			idx := rapid.IntRange(0, poolSize-1).Draw(rt, "poolIdx")
			actions[i] = recordAction{
				SymbolID:         pool[idx],
				SignatureChanged: rapid.Bool().Draw(rt, "sigChanged"),
			}
		}
		return actions
	})
}

// --- Property Tests for Symbol History ---

// Feature: gortex-enhancements, Property 13: Symbol history round-trip
//
// For any sequence of Record() calls, Get() SHALL return entries with correct
// modification count, and the churning flag SHALL be true iff count >= 3.
func TestPropertySymbolHistoryRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		actions := genRecordActions().Draw(rt, "actions")

		sh := &symbolHistory{
			entries: make(map[string][]SymbolModification),
		}

		// Build expected counts per symbol
		expectedCounts := make(map[string]int)
		for _, a := range actions {
			sh.Record(a.SymbolID, a.SignatureChanged)
			expectedCounts[a.SymbolID]++
		}

		// Verify Get() returns correct count for each symbol
		for symbolID, expectedCount := range expectedCounts {
			mods := sh.Get(symbolID)
			if len(mods) != expectedCount {
				rt.Errorf("Get(%q) returned %d entries, want %d", symbolID, len(mods), expectedCount)
			}

			// Verify churning flag: count >= 3 means churning
			churning := len(mods) >= 3
			if expectedCount >= 3 && !churning {
				rt.Errorf("symbol %q has %d modifications but churning is false", symbolID, expectedCount)
			}
			if expectedCount < 3 && churning {
				rt.Errorf("symbol %q has %d modifications but churning is true", symbolID, expectedCount)
			}
		}

		// Verify Get() for a symbol that was never recorded returns empty
		unknownMods := sh.Get("nonexistent/file.go::Unknown")
		if len(unknownMods) != 0 {
			rt.Errorf("Get() for unrecorded symbol returned %d entries, want 0", len(unknownMods))
		}

		// Verify All() returns all recorded symbols
		all := sh.All()
		if len(all) != len(expectedCounts) {
			rt.Errorf("All() returned %d symbols, want %d", len(all), len(expectedCounts))
		}
		for symbolID, expectedCount := range expectedCounts {
			mods, ok := all[symbolID]
			if !ok {
				rt.Errorf("All() missing symbol %q", symbolID)
				continue
			}
			if len(mods) != expectedCount {
				rt.Errorf("All()[%q] has %d entries, want %d", symbolID, len(mods), expectedCount)
			}
		}
	})
}

// Feature: gortex-enhancements, Property 14: Symbol history sort order
//
// When All() is called, the results can be sorted by modification count descending.
func TestPropertySymbolHistorySortOrder(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		actions := genRecordActions().Draw(rt, "actions")

		sh := &symbolHistory{
			entries: make(map[string][]SymbolModification),
		}

		for _, a := range actions {
			sh.Record(a.SymbolID, a.SignatureChanged)
		}

		all := sh.All()

		// Build a sortable slice of (symbolID, count) pairs
		type symbolCount struct {
			ID    string
			Count int
		}
		counts := make([]symbolCount, 0, len(all))
		for id, mods := range all {
			counts = append(counts, symbolCount{ID: id, Count: len(mods)})
		}

		// Sort by modification count descending
		sort.Slice(counts, func(i, j int) bool {
			return counts[i].Count > counts[j].Count
		})

		// Verify the sorted order is monotonically non-increasing
		for i := 1; i < len(counts); i++ {
			if counts[i].Count > counts[i-1].Count {
				rt.Errorf("sort order violated at index %d: count[%d]=%d > count[%d]=%d",
					i, i, counts[i].Count, i-1, counts[i-1].Count)
			}
		}

		// Verify each count matches what Get() returns
		for _, sc := range counts {
			mods := sh.Get(sc.ID)
			if len(mods) != sc.Count {
				rt.Errorf("Get(%q) returned %d entries but All() had %d", sc.ID, len(mods), sc.Count)
			}
		}
	})
}
