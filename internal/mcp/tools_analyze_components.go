// wcc / scc — connected-component diagnostics.
//
// `analyze kind=wcc` returns the weakly connected components: pairs
// of symbols reachable from each other ignoring edge direction. A
// healthy index has a small number of large WCCs (the connected
// codebase) plus a long tail of singletons (isolated extracted
// symbols). A WCC count that explodes between reindexes signals
// extraction drift, not code change.
//
// `analyze kind=scc` returns the strongly connected components:
// pairs of symbols mutually reachable along directed edges. Every
// non-trivial SCC (size > 1) is a recursion ring — mutual
// recursion in calls, two-way references between data types,
// circular module dependencies. Useful for cycle audits beyond
// what kind=cycles surfaces today.
//
// Routing:
//
//   - When the backing graph.Store implements graph.ComponentFinder
//     (today only store_sqlite), both kinds delegate to the
//     engine-native algorithm.
//
//   - Otherwise the in-process analysis.ComputeWCC /
//     analysis.ComputeSCC runs. SCC uses an iterative Tarjan so a
//     deep call graph won't blow the goroutine stack.

package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// componentRow is the per-component shape the analyzer returns.
type componentRow struct {
	ID      int      `json:"id"`
	Size    int      `json:"size"`
	Members []string `json:"members"`
}

// handleAnalyzeConnectedComponents serves both `analyze kind=wcc`
// and `analyze kind=scc`. The directed flag picks SCC; unset picks
// WCC.
func (s *Server) handleAnalyzeConnectedComponents(
	ctx context.Context, req mcp.CallToolRequest, directed bool,
) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	minSize := 0
	if v, ok := args["min_size"].(float64); ok && v > 0 {
		minSize = int(v)
	}
	memberLimit := 100
	if v, ok := args["member_limit"].(float64); ok && v > 0 {
		memberLimit = int(v)
	}

	kindLabel := "wcc"
	if directed {
		kindLabel = "scc"
	}

	results := s.runComponents(ctx, directed, analysis.ComponentOptions{MinSize: minSize})

	// Narrow to the session workspace + optional repo allow-set when the
	// request scopes below the global graph: prune each component's
	// members to visible nodes, recompute Size, and drop components left
	// empty. Without this the components would span every workspace. The
	// row ID is an opaque component index (not a node ID), so it is left
	// untouched. Filter before the limit cap and the per-component
	// member-limit truncation so both land on the visible set. Strict
	// no-op for an unbound session with no RepoAllow.
	if s.scopeFiltersActive(ctx) {
		reader := s.readerFor(ctx)
		kept := make([]analysis.ComponentResult, 0, len(results))
		for _, r := range results {
			visMembers := make([]string, 0, len(r.Members))
			for _, id := range r.Members {
				if s.analyzeNodeVisible(ctx, reader.GetNode(id)) {
					visMembers = append(visMembers, id)
				}
			}
			if len(visMembers) == 0 {
				continue
			}
			r.Members = visMembers
			r.Size = len(visMembers)
			kept = append(kept, r)
		}
		results = kept
	}

	if limit > 0 && limit < len(results) {
		results = results[:limit]
	}

	rows := make([]componentRow, 0, len(results))
	for _, r := range results {
		members := r.Members
		if memberLimit > 0 && memberLimit < len(members) {
			members = members[:memberLimit]
		}
		rows = append(rows, componentRow{ID: r.ID, Size: r.Size, Members: members})
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze(kindLabel, rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "id=%d size=%d members=%v\n", r.ID, r.Size, r.Members)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"components": rows,
		"total":      len(rows),
		"kind":       kindLabel,
	})
}

// runComponents picks the engine-native path when the backing
// store implements graph.ComponentFinder, otherwise falls back to
// the in-process analysis.ComputeWCC / ComputeSCC.
func (s *Server) runComponents(ctx context.Context, directed bool, opts analysis.ComponentOptions) []analysis.ComponentResult {
	if store := s.backendStore(); store != nil {
		if cf, ok := store.(graph.ComponentFinder); ok {
			hits, err := callComponentFinder(cf, directed, graph.ComponentOpts{
				NodeKinds: opts.NodeKinds,
				EdgeKinds: opts.EdgeKinds,
			})
			if err == nil {
				return collectHits(hits, opts.MinSize)
			}
			// Engine-native error falls through to the in-process
			// path rather than returning a half-done result.
		}
	}
	reader := s.readerFor(ctx)
	if directed {
		return analysis.ComputeSCC(reader, opts)
	}
	return analysis.ComputeWCC(reader, opts)
}

func callComponentFinder(cf graph.ComponentFinder, directed bool, opts graph.ComponentOpts) ([]graph.ComponentHit, error) {
	if directed {
		return cf.StronglyConnectedComponents(opts)
	}
	return cf.WeaklyConnectedComponents(opts)
}

// collectHits groups CommunityHits by ID, applies MinSize, sorts
// for determinism, and renumbers — mirrors analysis.collectComponents
// without exporting that internal helper.
func collectHits(hits []graph.ComponentHit, minSize int) []analysis.ComponentResult {
	groups := make(map[int64][]string)
	for _, h := range hits {
		groups[h.ComponentID] = append(groups[h.ComponentID], h.NodeID)
	}
	out := make([]analysis.ComponentResult, 0, len(groups))
	for _, members := range groups {
		if minSize > 0 && len(members) < minSize {
			continue
		}
		sort.Strings(members)
		out = append(out, analysis.ComponentResult{Members: members, Size: len(members)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		if len(out[i].Members) > 0 && len(out[j].Members) > 0 {
			return out[i].Members[0] < out[j].Members[0]
		}
		return false
	})
	for i := range out {
		out[i].ID = i
	}
	return out
}
