package mcp

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// handleGetUntestedSymbols returns functions and methods in non-test files
// that no test file reaches via the call graph — the inverse of
// get_test_targets. Test reachability is computed in a single forward BFS
// from every symbol defined in a test file, walking `calls` and
// `references` edges; any non-test function/method not visited is
// considered uncovered.
//
// This is the "where should we add tests first" tool: results are sorted by
// fan_in descending so the most-used untested symbols surface at the top —
// they're the ones most likely to break something when changed without a
// safety net.
func (s *Server) handleGetUntestedSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 50)
	if limit <= 0 {
		limit = 50
	}
	filePrefix := req.GetString("file_prefix", "")
	minFanIn := req.GetInt("min_fan_in", 0)

	// One reader for the whole call: both passes below must see the same
	// state, and under an overlay that state is the caller's buffers.
	reader := s.readerFor(ctx)
	covered := reachableFromTests(reader)

	// Fan-in map for ranking — incoming calls/references only; imports and
	// defines would flood every exported symbol with meaningless coverage.
	// Backends that implement graph.InEdgeCounter serve this from one
	// count(*) join — on a disk backend the legacy AllEdges() loop
	// materialised every edge over the storage boundary just to bucket two kinds. The
	// fallback walks AllEdges() as before.
	fanIn := collectFanInByKind(reader, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences})

	type untestedEntry struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		FilePath string `json:"file_path"`
		Line     int    `json:"line"`
		FanIn    int    `json:"fan_in"`
	}

	var entries []untestedEntry
	totalCandidates := 0
	scoped := s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindFunction, graph.KindMethod})
	for _, n := range scoped {
		// Skip symbols defined inside test files — those ARE test code.
		if isTestFile(n.FilePath) {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, filePrefix) {
			continue
		}
		totalCandidates++
		if covered[n.ID] {
			continue
		}
		fi := fanIn[n.ID]
		if fi < minFanIn {
			continue
		}
		entries = append(entries, untestedEntry{
			ID:       n.ID,
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
			FanIn:    fi,
		})
	}

	// Rank by fan_in desc, then file_path asc for stable ordering.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FanIn != entries[j].FanIn {
			return entries[i].FanIn > entries[j].FanIn
		}
		if entries[i].FilePath != entries[j].FilePath {
			return entries[i].FilePath < entries[j].FilePath
		}
		return entries[i].ID < entries[j].ID
	})
	totalUncovered := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}

	var coverageRatio float64
	if totalCandidates > 0 {
		coverageRatio = float64(totalCandidates-totalUncovered) / float64(totalCandidates)
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"untested":         entries,
		"total_candidates": totalCandidates,
		"total_uncovered":  totalUncovered,
		"coverage_ratio":   coverageRatio,
		"truncated":        totalUncovered > len(entries),
	})
}

// reachableFromTests computes the set of symbol IDs reachable from any
// symbol defined in a test file, following outgoing `calls` and
// `references` edges. One pass over the graph at O(V+E) beats
// per-symbol BFS which would be O(V·(V+E)).
//
// Test files are detected via isTestFile so this works across languages
// (Go _test.go, Python test_*.py, JS .spec.ts, etc.) without per-language
// special-casing here.
//
// Seeds the frontier via NodesByKind(function|method) so disk backends
// only materialise the two kinds rather than the whole node table.
// The test-file predicate is a Go string heuristic — the backend has
// no equivalent — so it stays in the post-filter.
//
// The BFS itself runs through graph.ReachableForwardByKinds when the
// backend implements it (one query per layer over the frontier
// IN-list instead of N+1 GetOutEdges round-trips). Falls back to
// the per-id GetOutEdges loop on backends that don't — an overlay
// view is one of those, so an overlay-active request walks the
// buffers edge by edge instead of the backend's index.
func reachableFromTests(g graph.Reader) map[string]bool {
	// Seed: every function/method defined in a test file. NodesByKind
	// pushes the kind filter into the backend; isTestFile stays Go.
	seeds := make([]string, 0)
	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		for n := range g.NodesByKind(kind) {
			if n == nil || !isTestFile(n.FilePath) {
				continue
			}
			seeds = append(seeds, n.ID)
		}
	}
	if len(seeds) == 0 {
		return map[string]bool{}
	}

	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	if rf, ok := g.(graph.ReachableForwardByKinds); ok {
		if got := rf.ReachableForwardByKinds(seeds, kinds); got != nil {
			return got
		}
		return map[string]bool{}
	}

	// Fallback: layer-by-layer BFS using per-id GetOutEdges.
	covered := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		if !covered[id] {
			covered[id] = true
			frontier = append(frontier, id)
		}
	}
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, id := range frontier {
			for _, e := range g.GetOutEdges(id) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
					continue
				}
				if !covered[e.To] {
					covered[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		frontier = next
	}
	return covered
}

// collectFanInByKind returns the per-target incoming-edge count for
// every edge whose kind is in the allowlist. Prefers the
// graph.InEdgeCounter capability — backends that ship it run one
// count(*) per request instead of an AllEdges() materialisation
// + Go-side bucketing. An overlay view has no counter, so an
// overlay-active request buckets its edges Go-side.
func collectFanInByKind(g graph.Reader, kinds []graph.EdgeKind) map[string]int {
	if len(kinds) == 0 {
		return map[string]int{}
	}
	if ic, ok := g.(graph.InEdgeCounter); ok {
		if got := ic.InEdgeCountsByKind(kinds); got != nil {
			return got
		}
		return map[string]int{}
	}
	allowed := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	out := make(map[string]int)
	for _, e := range g.AllEdges() {
		if _, ok := allowed[e.Kind]; !ok {
			continue
		}
		out[e.To]++
	}
	return out
}
