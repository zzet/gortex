package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// recommendedCall is one entry in plan_turn's output. Each suggestion carries
// the tool name, pre-filled arguments the agent can pass straight back to the
// MCP client, and a short `why` explaining why this call ranks where it does.
type recommendedCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	Why  string         `json:"why"`
}

// handlePlanTurn is a cheap opening-move router. It runs the same keyword
// extraction + BM25 search that smart_context does, then emits a short
// ordered list of recommended tool calls — what to run *next*, not the
// context itself. Agents (and subagents especially) waste the first few
// turns picking the wrong tool; a ranked list of calls with args filled in
// collapses that exploration into one cheap query.
//
// The output is deliberately small (~200 tokens): this is a routing tool,
// not a context tool.
func (s *Server) handlePlanTurn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, err := req.RequireString("task")
	if err != nil {
		return mcp.NewToolResultError("task is required"), nil
	}
	maxCalls := req.GetInt("max_calls", 4)
	if maxCalls <= 0 {
		maxCalls = 4
	}

	keywords := extractKeywords(task)

	seen := make(map[string]bool)
	var candidates []*graph.Node

	// Exact-name match first. extractKeywords already prioritises
	// identifier-shape tokens; if the user actually typed the symbol
	// they care about (`renderToolsSearchResult`), the graph's name
	// index has the answer directly. engine.SearchSymbols is plain
	// BM25 — without a rerank pipeline it ranks ts_lex / set_contains
	// above a single exact-name match because parser tables saturate
	// the token corpus. The exact-name shortcut puts the user's
	// identifier at position 0; BM25 still fills the rest.
	reader := s.readerFor(ctx)
	for _, kw := range keywords {
		if len(kw) < 3 || !hasIdentifierShape(kw) {
			continue
		}
		for _, m := range s.scopedNodeSlice(ctx, reader.FindNodesByName(kw)) {
			if m.Kind == graph.KindFile || m.Kind == graph.KindImport {
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				candidates = append(candidates, m)
			}
		}
	}

	// BM25 search per keyword; dedup + filter out files/imports.
	for _, kw := range keywords {
		if len(kw) < 3 {
			continue
		}
		for _, m := range s.scopedNodeSlice(ctx, s.engineFor(ctx).SearchSymbols(kw, 10)) {
			if m.Kind == graph.KindFile || m.Kind == graph.KindImport {
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				candidates = append(candidates, m)
			}
		}
	}

	// Top 5 candidates are enough to derive recommendations — plan_turn is a
	// router, not a context-assembler.
	const maxCandidates = 5
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	recs := buildRecommendedCalls(task, candidates, maxCalls)

	topCandidateIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		topCandidateIDs = append(topCandidateIDs, c.ID)
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"task":              task,
		"keywords":          keywords,
		"recommended_calls": recs,
		"top_candidates":    topCandidateIDs,
		"candidate_count":   len(candidates),
	})
}

// buildRecommendedCalls composes the ranked suggestion list. The rules here
// are the codified "good first moves":
//  1. smart_context with the task — always a safe opening move for
//     exploratory work.
//  2. get_editing_context on the top candidate's file — the next step
//     once smart_context lands on a file to edit.
//  3. find_usages on the top candidate symbol — only useful if the symbol
//     is callable (function/method/type); for variables/packages it's
//     noise, so the check is explicit.
//  4. search_symbols with the raw task string as fallback when BM25
//     produced no candidates.
//
// Returns at most maxCalls entries. Never returns empty — even with no
// candidates, smart_context is worth suggesting as an opening move.
func buildRecommendedCalls(task string, candidates []*graph.Node, maxCalls int) []recommendedCall {
	recs := make([]recommendedCall, 0, maxCalls)

	recs = append(recs, recommendedCall{
		Tool: "smart_context",
		Args: map[string]any{"task": task},
		Why:  "Opening move — aggregates top symbols, entry file context, related tests in one call",
	})

	if len(candidates) > 0 {
		top := candidates[0]
		if top.FilePath != "" && len(recs) < maxCalls {
			recs = append(recs, recommendedCall{
				Tool: "get_editing_context",
				Args: map[string]any{"path": top.FilePath},
				Why:  fmt.Sprintf("Top-ranked symbol %q lives in %s — load its file context before editing", top.Name, top.FilePath),
			})
		}
		if isCallableKind(top.Kind) && len(recs) < maxCalls {
			recs = append(recs, recommendedCall{
				Tool: "find_usages",
				Args: map[string]any{"id": top.ID},
				Why:  fmt.Sprintf("Top candidate %q is a %s — see callers before changing its signature", top.Name, top.Kind),
			})
		}
	}

	// Raw-string search_symbols as a fallback path. Useful when the task
	// mentions a specific concept that keyword extraction may have split.
	if len(recs) < maxCalls {
		recs = append(recs, recommendedCall{
			Tool: "search_symbols",
			Args: map[string]any{"query": task, "compact": true, "limit": 10},
			Why:  "Broad BM25 search across the raw task text; use when candidates above aren't the right lead",
		})
	}

	if len(recs) > maxCalls {
		recs = recs[:maxCalls]
	}
	return recs
}

// isCallableKind returns true for symbol kinds where find_usages /
// get_callers are meaningful. Packages, imports, and files are excluded —
// usage queries on those return noise.
func isCallableKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
		return true
	}
	return false
}
