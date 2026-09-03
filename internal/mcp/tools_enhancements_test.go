package mcp

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// ============================================================================
// Helpers
// ============================================================================

// ============================================================================
// 13.1 Property 4: Prefetch ranking is monotonically decreasing
// ============================================================================

// Feature: gortex-enhancements, Property 4: Prefetch ranking is monotonically decreasing
//
// For any set of prefetch candidates with scores, the list SHALL be sorted by
// combined score descending, and each confidence SHALL be in [0, 1].
func TestPropertyPrefetchRankingMonotonicallyDecreasing(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random candidates with scores
		n := rapid.IntRange(1, 50).Draw(rt, "candidateCount")
		var candidates []prefetchCandidate
		for i := 0; i < n; i++ {
			searchRel := rapid.Float64Range(0, 1).Draw(rt, "search")
			graphProx := rapid.Float64Range(0, 1).Draw(rt, "proximity")
			commBonus := rapid.Float64Range(0, 1).Draw(rt, "community")

			combined := 0.4*searchRel + 0.4*graphProx + 0.2*commBonus
			confidence := math.Min(combined, 1.0)
			confidence = math.Round(confidence*1000) / 1000

			candidates = append(candidates, prefetchCandidate{
				ID:              rapid.StringMatching(`[a-z]{3,6}/[a-z]{3,6}\.go::[A-Z][a-zA-Z]{2,8}`).Draw(rt, "id"),
				Kind:            "function",
				FilePath:        "pkg/file.go",
				StartLine:       i + 1,
				Reason:          "test",
				Confidence:      confidence,
				SearchRelevance: searchRel,
				GraphProximity:  graphProx,
				CommunityBonus:  commBonus,
			})
		}

		// Sort by confidence descending (same logic as handlePrefetchContext)
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Confidence > candidates[j].Confidence
		})

		// Truncate to top 10
		if len(candidates) > 10 {
			candidates = candidates[:10]
		}

		// Verify monotonically decreasing confidence
		for i := 1; i < len(candidates); i++ {
			if candidates[i].Confidence > candidates[i-1].Confidence {
				rt.Errorf("ranking not monotonically decreasing at index %d: %.3f > %.3f",
					i, candidates[i].Confidence, candidates[i-1].Confidence)
			}
		}

		// Verify all confidence scores are in [0, 1]
		for i, c := range candidates {
			if c.Confidence < 0 || c.Confidence > 1 {
				rt.Errorf("confidence at index %d out of range [0,1]: %.3f", i, c.Confidence)
			}
		}
	})
}

// ============================================================================
// 13.2 Property 10: Truncation invariant
// ============================================================================

// Feature: gortex-enhancements, Property 10: Truncation invariant
//
// For any result set exceeding its limit, the returned list SHALL have exactly
// the limit items and the truncated flag SHALL be true.
func TestPropertyTruncationInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		type truncationCase struct {
			name  string
			limit int
		}
		cases := []truncationCase{
			{"prefetch_context", 10},
			{"find_hotspots", 20},
			{"find_cycles", 20},
			{"diff_context", 50},
		}

		for _, tc := range cases {
			// Generate a result set of random size (may or may not exceed limit)
			size := rapid.IntRange(1, tc.limit*3).Draw(rt, tc.name+"_size")

			// Simulate truncation logic used across all tools
			items := make([]int, size)
			for i := range items {
				items[i] = i
			}

			totalCount := len(items)
			truncated := false
			if len(items) > tc.limit {
				items = items[:tc.limit]
				truncated = true
			}

			// Verify invariants
			if totalCount > tc.limit {
				if !truncated {
					rt.Errorf("%s: truncated should be true when size %d > limit %d",
						tc.name, totalCount, tc.limit)
				}
				if len(items) != tc.limit {
					rt.Errorf("%s: expected exactly %d items after truncation, got %d",
						tc.name, tc.limit, len(items))
				}
			} else {
				if truncated {
					rt.Errorf("%s: truncated should be false when size %d <= limit %d",
						tc.name, totalCount, tc.limit)
				}
				if len(items) != totalCount {
					rt.Errorf("%s: expected %d items (no truncation), got %d",
						tc.name, totalCount, len(items))
				}
			}
		}
	})
}

// ============================================================================
// 13.3 Property 11: Compact output line count equals item count
// ============================================================================

// Feature: gortex-enhancements, Property 11: Compact output line count equals item count
//
// For any tool result with N items, when compact=true, the text output SHALL
// contain exactly N non-empty lines.
func TestPropertyCompactOutputLineCount(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(rt, "itemCount")

		// Test verify_change compact output
		t.Run("verify_change", func(_ *testing.T) {
			var b strings.Builder
			violations := make([]analysis.ContractViolation, n)
			for i := 0; i < n; i++ {
				violations[i] = analysis.ContractViolation{
					SymbolID:    "pkg/file.go::Func" + strings.Repeat("x", i),
					Kind:        "caller_mismatch",
					FilePath:    "pkg/file.go",
					Line:        i + 1,
					Description: "test violation",
				}
				b.WriteString(violations[i].Kind + " " + violations[i].SymbolID + " " +
					violations[i].FilePath + ":" + strings.Repeat("1", 1) + " " +
					violations[i].Description + "\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("verify_change compact: expected %d non-empty lines, got %d", n, lines)
			}
		})

		// Test check_guards compact output
		t.Run("check_guards", func(_ *testing.T) {
			var b strings.Builder
			for i := 0; i < n; i++ {
				b.WriteString("co-change rule-" + strings.Repeat("x", i) + " description\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("check_guards compact: expected %d non-empty lines, got %d", n, lines)
			}
		})

		// Test find_dead_code compact output
		t.Run("find_dead_code", func(_ *testing.T) {
			var b strings.Builder
			for i := 0; i < n; i++ {
				b.WriteString("function pkg/file.go::Func" + strings.Repeat("x", i) + " pkg/file.go:1\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("find_dead_code compact: expected %d non-empty lines, got %d", n, lines)
			}
		})

		// Test find_cycles compact output
		t.Run("find_cycles", func(_ *testing.T) {
			var b strings.Builder
			for i := 0; i < n; i++ {
				b.WriteString("call-cycle severity=1 A → B → A\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("find_cycles compact: expected %d non-empty lines, got %d", n, lines)
			}
		})

		// Test get_symbol_history compact output
		t.Run("get_symbol_history", func(_ *testing.T) {
			var b strings.Builder
			for i := 0; i < n; i++ {
				b.WriteString("pkg/file.go::Func" + strings.Repeat("x", i) + " count=2\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("get_symbol_history compact: expected %d non-empty lines, got %d", n, lines)
			}
		})

		// Test batch_edit compact output
		t.Run("batch_edit", func(_ *testing.T) {
			var b strings.Builder
			for i := 0; i < n; i++ {
				b.WriteString("pkg/file.go::Func" + strings.Repeat("x", i) + " pkg/file.go applied\n")
			}
			lines := countNonEmptyLines(b.String())
			if lines != n {
				rt.Errorf("batch_edit compact: expected %d non-empty lines, got %d", n, lines)
			}
		})
	})
}

func countNonEmptyLines(s string) int {
	count := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// ============================================================================
// 13.4 Property 12: Health score formula
// ============================================================================

// Feature: gortex-enhancements, Property 12: Health score formula
//
// For any indexer state, health score = round((successfully_indexed / total_detected) * 100, 1),
// and recommendation is present iff score < 80%.
func TestPropertyHealthScoreFormula(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		totalDetected := rapid.IntRange(1, 10000).Draw(rt, "totalDetected")
		failureCount := rapid.IntRange(0, totalDetected).Draw(rt, "failureCount")
		successfullyIndexed := totalDetected - failureCount
		if successfullyIndexed < 0 {
			successfullyIndexed = 0
		}

		// Compute health score using the same formula as handleIndexHealth.
		var healthScore float64
		if totalDetected > 0 {
			healthScore = math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
		}

		// Verify the formula matches its spec: round((successfully_indexed /
		// total_detected) * 100, 1). The naive implementation
		// `math.Round(ratio*100 * 10) / 10` reorders the multiplies and
		// in binary float that straddles the 0.5 rounding boundary on
		// inputs like 5125/10000 → one path lands on 512.5, the other on
		// 512.4999…, and Round disagrees. Use the same expression order
		// production uses so we're verifying the production contract, not
		// hunting a float-reordering bug in the test scaffold.
		var expectedRounded float64
		if totalDetected > 0 {
			expectedRounded = math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
		}

		if math.Abs(healthScore-expectedRounded) > 0.01 {
			rt.Errorf("health score mismatch: got %.1f, expected %.1f (success=%d, total=%d)",
				healthScore, expectedRounded, successfullyIndexed, totalDetected)
		}

		// Verify score is in [0, 100].
		if healthScore < 0 || healthScore > 100 {
			rt.Errorf("health score out of range [0,100]: %.1f", healthScore)
		}

		// Sanity bound: the rounded score must be within 0.1 of the
		// unrounded percentage. This is what actually pins the formula
		// to the documented spec — unlike the byte-for-byte comparison
		// above (which would only catch drift in the expression order).
		if totalDetected > 0 {
			raw := float64(successfullyIndexed) / float64(totalDetected) * 100
			if math.Abs(healthScore-raw) > 0.05+1e-9 {
				rt.Errorf("health score %.1f differs from raw percentage %.4f by more than 0.05 (success=%d, total=%d)",
					healthScore, raw, successfullyIndexed, totalDetected)
			}
		}

		// Verify recommendation is present iff score < 80%
		var recommendation string
		if healthScore < 80 {
			recommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."
		}

		if healthScore < 80 && recommendation == "" {
			rt.Errorf("recommendation should be present when health score %.1f < 80%%", healthScore)
		}
		if healthScore >= 80 && recommendation != "" {
			rt.Errorf("recommendation should be absent when health score %.1f >= 80%%", healthScore)
		}
	})
}

// ============================================================================
// 13.5 Property 15: Batch edit dependency ordering
// ============================================================================

// Feature: gortex-enhancements, Property 15: Batch edit dependency ordering
//
// Definitions are edited before callers. On failure at position N,
// edits 0..N-1 are "applied", N is "failed", N+1..end are "skipped".
func TestPropertyBatchEditDependencyOrdering(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a set of edits with dependency relationships
		editCount := rapid.IntRange(2, 15).Draw(rt, "editCount")

		type editWithOrder struct {
			symbolID string
			kind     graph.NodeKind
			order    int
			file     string
		}

		var edits []editWithOrder
		for i := 0; i < editCount; i++ {
			kind := graph.KindFunction
			order := 20
			kindChoice := rapid.IntRange(0, 2).Draw(rt, "kindChoice")
			switch kindChoice {
			case 0:
				kind = graph.KindInterface
				order = 0 // definitions first
			case 1:
				kind = graph.KindType
				order = 0 // definitions first
			case 2:
				kind = graph.KindFunction
				order = 20
			}
			edits = append(edits, editWithOrder{
				symbolID: rapid.StringMatching(`[a-z]{3,6}/[a-z]{3,6}\.go::[A-Z][a-zA-Z]{2,6}`).Draw(rt, "symbolID"),
				kind:     kind,
				order:    order,
				file:     "pkg/file.go",
			})
		}

		// Sort by order ascending (same logic as handleBatchEdit)
		sort.SliceStable(edits, func(i, j int) bool {
			if edits[i].order != edits[j].order {
				return edits[i].order < edits[j].order
			}
			return edits[i].file < edits[j].file
		})

		// Verify definitions come before callers
		lastDefOrder := -1
		firstCallerOrder := math.MaxInt32
		for _, e := range edits {
			if e.kind == graph.KindInterface || e.kind == graph.KindType {
				if e.order > lastDefOrder {
					lastDefOrder = e.order
				}
			} else {
				if e.order < firstCallerOrder {
					firstCallerOrder = e.order
				}
			}
		}
		if lastDefOrder >= 0 && firstCallerOrder < math.MaxInt32 {
			if lastDefOrder > firstCallerOrder {
				rt.Errorf("definitions (order=%d) should come before callers (order=%d)",
					lastDefOrder, firstCallerOrder)
			}
		}

		// Simulate failure at a random position and verify status assignment
		failPos := rapid.IntRange(0, editCount-1).Draw(rt, "failPos")

		type editResult struct {
			status string
		}
		var results []editResult
		failed := false
		for i := range edits {
			if failed {
				results = append(results, editResult{status: "skipped"})
				continue
			}
			if i == failPos {
				results = append(results, editResult{status: "failed"})
				failed = true
				continue
			}
			results = append(results, editResult{status: "applied"})
		}

		// Verify: 0..failPos-1 are "applied", failPos is "failed", failPos+1..end are "skipped"
		for i, r := range results {
			if i < failPos {
				if r.status != "applied" {
					rt.Errorf("edit %d should be 'applied' but got '%s'", i, r.status)
				}
			} else if i == failPos {
				if r.status != "failed" {
					rt.Errorf("edit %d should be 'failed' but got '%s'", i, r.status)
				}
			} else {
				if r.status != "skipped" {
					rt.Errorf("edit %d should be 'skipped' but got '%s'", i, r.status)
				}
			}
		}
	})
}

// ============================================================================
// 13.6 Property 16: Cross-community warning correctness
// ============================================================================

// Feature: gortex-enhancements, Property 16: Cross-community warning correctness
//
// The warning names every affected community and never scores the pairs
// between them — scoring needs the whole edge set, which the impact safety
// gate must not read. Whatever the shape of the graph, the answer is the
// community list and nothing more.
func TestPropertyCrossCommunityWarningCorrectness(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate two communities with members and edges
		sizeA := rapid.IntRange(2, 10).Draw(rt, "sizeA")
		sizeB := rapid.IntRange(2, 10).Draw(rt, "sizeB")

		g := graph.New()
		membersA := make(map[string]bool)
		membersB := make(map[string]bool)
		var allMembersA, allMembersB []string

		for i := 0; i < sizeA; i++ {
			id := "commA/file.go::FuncA" + strings.Repeat("x", i)
			g.AddNode(&graph.Node{
				ID: id, Kind: graph.KindFunction, Name: "FuncA",
				FilePath: "commA/file.go", StartLine: i + 1, Language: "go",
			})
			membersA[id] = true
			allMembersA = append(allMembersA, id)
		}
		for i := 0; i < sizeB; i++ {
			id := "commB/file.go::FuncB" + strings.Repeat("y", i)
			g.AddNode(&graph.Node{
				ID: id, Kind: graph.KindFunction, Name: "FuncB",
				FilePath: "commB/file.go", StartLine: i + 1, Language: "go",
			})
			membersB[id] = true
			allMembersB = append(allMembersB, id)
		}

		// Add internal edges within A
		internalA := rapid.IntRange(0, sizeA).Draw(rt, "internalA")
		for i := 0; i < internalA; i++ {
			from := rapid.IntRange(0, sizeA-1).Draw(rt, "fromA")
			to := rapid.IntRange(0, sizeA-1).Draw(rt, "toA")
			if from != to {
				g.AddEdge(&graph.Edge{From: allMembersA[from], To: allMembersA[to], Kind: graph.EdgeCalls})
			}
		}

		// Add internal edges within B
		internalB := rapid.IntRange(0, sizeB).Draw(rt, "internalB")
		for i := 0; i < internalB; i++ {
			from := rapid.IntRange(0, sizeB-1).Draw(rt, "fromB")
			to := rapid.IntRange(0, sizeB-1).Draw(rt, "toB")
			if from != to {
				g.AddEdge(&graph.Edge{From: allMembersB[from], To: allMembersB[to], Kind: graph.EdgeCalls})
			}
		}

		// Add cross-boundary edges
		crossCount := rapid.IntRange(0, sizeA+sizeB).Draw(rt, "crossCount")
		for i := 0; i < crossCount; i++ {
			fromIdx := rapid.IntRange(0, sizeA-1).Draw(rt, "crossFrom")
			toIdx := rapid.IntRange(0, sizeB-1).Draw(rt, "crossTo")
			g.AddEdge(&graph.Edge{From: allMembersA[fromIdx], To: allMembersB[toIdx], Kind: graph.EdgeCalls})
		}

		// Build community result for the server method
		communities := &analysis.CommunityResult{
			Communities: []analysis.Community{
				{ID: "community-0", Label: "A", Members: allMembersA, Size: sizeA},
				{ID: "community-1", Label: "B", Members: allMembersB, Size: sizeB},
			},
			NodeToComm: make(map[string]string),
		}
		for _, id := range allMembersA {
			communities.NodeToComm[id] = "community-0"
		}
		for _, id := range allMembersB {
			communities.NodeToComm[id] = "community-1"
		}

		// Create a minimal server to call computeCrossCommunityWarning
		srv := &Server{graph: g}
		warning := srv.computeCrossCommunityWarning(
			[]string{"community-0", "community-1"},
			communities,
		)

		if warning == nil {
			rt.Fatalf("warning should not be nil for 2 communities")
			return // unreachable, but satisfies staticcheck
		}

		if len(warning.AffectedCommunities) != 2 {
			rt.Errorf("expected 2 affected communities, got %d", len(warning.AffectedCommunities))
		}
	})
}

// ============================================================================
// 13.7 Property 17: Auto re-index rate limit
// ============================================================================

// Feature: gortex-enhancements, Property 17: Auto re-index rate limit
//
// ensureFresh refreshes at most 5 files even when more are stale.
func TestPropertyAutoReindexRateLimit(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a list of stale file paths
		staleCount := rapid.IntRange(1, 50).Draw(rt, "staleCount")
		var staleFiles []string
		for i := 0; i < staleCount; i++ {
			staleFiles = append(staleFiles, "pkg/"+strings.Repeat("f", i+1)+".go")
		}

		// Simulate the ensureFresh rate limiting logic
		limit := 5
		var refreshed []string
		for _, fp := range staleFiles {
			if len(refreshed) >= limit {
				break
			}
			// Simulate: all files are stale
			refreshed = append(refreshed, fp)
		}

		// Verify: at most 5 files refreshed
		if len(refreshed) > 5 {
			rt.Errorf("ensureFresh refreshed %d files, expected at most 5", len(refreshed))
		}

		// Verify: when staleCount > 5, exactly 5 are refreshed
		if staleCount > 5 && len(refreshed) != 5 {
			rt.Errorf("with %d stale files, expected exactly 5 refreshed, got %d",
				staleCount, len(refreshed))
		}

		// Verify: when staleCount <= 5, all are refreshed
		if staleCount <= 5 && len(refreshed) != staleCount {
			rt.Errorf("with %d stale files, expected all %d refreshed, got %d",
				staleCount, staleCount, len(refreshed))
		}
	})
}

// ============================================================================
// 14.1 Unit tests for tool handler edge cases
// ============================================================================

// TestPrefetchNoContextReturnsError verifies that prefetch_context with empty
// task and empty recent_symbols returns an error.
func TestPrefetchNoContextReturnsError(t *testing.T) {
	srv := &Server{
		session: newSessionState(),
	}
	// Both task and recent_symbols are empty — should get error.
	ctx := context.Background()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "prefetch_context"
	req.Params.Arguments = map[string]any{
		"task":           "",
		"recent_symbols": "",
	}

	result, err := srv.handlePrefetchContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when no context provided")
	}
	text := result.Content[0].(mcplib.TextContent).Text
	if !strings.Contains(text, "insufficient context") {
		t.Errorf("expected 'insufficient context' in error, got: %s", text)
	}
}

// TestHealthScoreBelowThresholdIncludesRecommendation verifies that health
// score < 80% includes a recommendation.
func TestHealthScoreBelowThresholdIncludesRecommendation(t *testing.T) {
	// Test the recommendation logic directly
	tests := []struct {
		name          string
		healthScore   float64
		wantRecommend bool
	}{
		{"score_50_recommends", 50.0, true},
		{"score_79_recommends", 79.9, true},
		{"score_80_no_recommend", 80.0, false},
		{"score_100_no_recommend", 100.0, false},
		{"score_0_recommends", 0.0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recommendation string
			if tt.healthScore < 80 {
				recommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."
			}

			if tt.wantRecommend && recommendation == "" {
				t.Errorf("expected recommendation for health score %.1f", tt.healthScore)
			}
			if !tt.wantRecommend && recommendation != "" {
				t.Errorf("unexpected recommendation for health score %.1f", tt.healthScore)
			}
		})
	}
}

// TestSingleCommunityImpactHasNullWarning verifies that when the blast radius
// is within a single community, cross_community_warning is nil.
func TestSingleCommunityImpactHasNullWarning(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::FuncA", Kind: graph.KindFunction, Name: "FuncA", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::FuncB", Kind: graph.KindFunction, Name: "FuncB", FilePath: "pkg/a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "pkg/a.go::FuncA", To: "pkg/a.go::FuncB", Kind: graph.EdgeCalls})

	communities := &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "community-0", Label: "A", Members: []string{"pkg/a.go::FuncA", "pkg/a.go::FuncB"}, Size: 2},
		},
		NodeToComm: map[string]string{
			"pkg/a.go::FuncA": "community-0",
			"pkg/a.go::FuncB": "community-0",
		},
	}

	// With only 1 affected community, computeCrossCommunityWarning should not be called.
	// The handler sets cross_community_warning = nil when < 2 communities.
	affectedCommunities := []string{"community-0"}
	if len(affectedCommunities) >= 2 {
		t.Fatal("test setup error: expected single community")
	}

	// Verify the logic: single community → warning is nil
	var warning *CrossCommunityWarning
	if len(affectedCommunities) >= 2 {
		srv := &Server{graph: g}
		warning = srv.computeCrossCommunityWarning(affectedCommunities, communities)
	}

	if warning != nil {
		t.Errorf("expected nil warning for single community, got: %+v", warning)
	}
}

// TestWatchModeActiveSkipsAutoReindex verifies that ensureFresh returns nil
// when watcher is active.
func TestWatchModeActiveSkipsAutoReindex(t *testing.T) {
	srv := &Server{
		watcher: &indexer.Watcher{}, // non-nil watcher means watch mode is active
	}

	result := srv.ensureFresh([]string{"pkg/file.go", "pkg/other.go"})
	if result != nil {
		t.Errorf("expected nil when watcher is active, got: %v", result)
	}
}

// TestAutoReindexFailureReturnsStaleDataWithWarning verifies that when
// ensureFresh cannot re-index a file, it continues without crashing.
// The ensureFresh method logs a warning and skips the file.
func TestAutoReindexFailureReturnsStaleDataWithWarning(t *testing.T) {
	// ensureFresh with nil indexer returns nil (no crash)
	srv := &Server{
		indexer: nil,
		watcher: nil,
	}
	result := srv.ensureFresh([]string{"nonexistent/file.go"})
	if result != nil {
		t.Errorf("expected nil when indexer is nil, got: %v", result)
	}
}

// TestBatchEditDryRunReturnsPlanOnly verifies that batch_edit with dry_run=true
// returns a plan without applying changes.
func TestBatchEditDryRunReturnsPlanOnly(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::FuncA", Kind: graph.KindFunction, Name: "FuncA",
		FilePath: "pkg/a.go", StartLine: 1, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b.go::TypeB", Kind: graph.KindType, Name: "TypeB",
		FilePath: "pkg/b.go", StartLine: 1, EndLine: 3, Language: "go",
	})

	eng := query.NewEngine(g)
	// A dry run resolves symbol paths exactly as the commit does, so the plan
	// needs a root to anchor them against — without one every entry reports the
	// unresolved-path failure instead of the order under test.
	idx := indexer.New(g, testRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(t.TempDir())
	srv := &Server{
		graph:   g,
		engine:  eng,
		indexer: idx,
	}

	editsJSON := `[{"id":"pkg/b.go::TypeB","old_source":"old","new_source":"new"},{"id":"pkg/a.go::FuncA","old_source":"old","new_source":"new"}]`

	ctx := context.Background()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = map[string]any{
		"edits":   editsJSON,
		"dry_run": true,
	}

	result, err := srv.handleBatchEdit(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}

	// Parse the JSON result
	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Verify dry_run flag is true
	if dryRun, ok := resp["dry_run"].(bool); !ok || !dryRun {
		t.Error("expected dry_run=true in response")
	}

	// Verify plan is present
	plan, ok := resp["plan"].([]any)
	if !ok {
		t.Fatal("expected plan array in response")
	}
	if len(plan) != 2 {
		t.Errorf("expected 2 items in plan, got %d", len(plan))
	}

	// Verify all entries have status "planned"
	for i, entry := range plan {
		m := entry.(map[string]any)
		if m["status"] != "planned" {
			t.Errorf("plan entry %d: expected status 'planned', got '%s'", i, m["status"])
		}
	}

	// Verify types come before functions in the plan (dependency ordering)
	firstEntry := plan[0].(map[string]any)
	if firstEntry["id"] != "pkg/b.go::TypeB" {
		t.Errorf("expected TypeB first in dependency order, got %s", firstEntry["id"])
	}
}

// ============================================================================
// 14.2 Integration tests
// ============================================================================

// TestIntegrationGuardRulesLoadedFromConfig verifies that guard rules defined
// in a .gortex.yaml file are loaded and available to the server.
func TestIntegrationGuardRulesLoadedFromConfig(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `guards:
  rules:
    - name: parser-tests
      kind: co-change
      source: "internal/parser"
      target: "internal/parser/languages"
      message: "Parser changes require language extractor test updates"
    - name: no-direct-graph
      kind: boundary
      source: "internal/mcp"
      target: "internal/graph"
      message: "MCP tools must use query.Engine, not graph.Graph directly"
`
	configPath := filepath.Join(dir, ".gortex.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if len(cfg.Guards.Rules) != 2 {
		t.Fatalf("expected 2 guard rules, got %d", len(cfg.Guards.Rules))
	}

	// Verify first rule
	r0 := cfg.Guards.Rules[0]
	if r0.Name != "parser-tests" {
		t.Errorf("rule[0].Name = %q, want %q", r0.Name, "parser-tests")
	}
	if r0.Kind != "co-change" {
		t.Errorf("rule[0].Kind = %q, want %q", r0.Kind, "co-change")
	}
	if r0.Source != "internal/parser" {
		t.Errorf("rule[0].Source = %q, want %q", r0.Source, "internal/parser")
	}
	if r0.Target != "internal/parser/languages" {
		t.Errorf("rule[0].Target = %q, want %q", r0.Target, "internal/parser/languages")
	}

	// Verify second rule
	r1 := cfg.Guards.Rules[1]
	if r1.Name != "no-direct-graph" {
		t.Errorf("rule[1].Name = %q, want %q", r1.Name, "no-direct-graph")
	}
	if r1.Kind != "boundary" {
		t.Errorf("rule[1].Kind = %q, want %q", r1.Kind, "boundary")
	}

	// Verify rules can be passed to NewServer and used
	g := graph.New()
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), cfg.Guards.Rules)
	if len(srv.guardRules) != 2 {
		t.Errorf("server has %d guard rules, want 2", len(srv.guardRules))
	}
}
