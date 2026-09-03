// kcore — find the densely connected core of the graph.
//
// k-core decomposition assigns every node a k-degree: the largest
// k for which the node remains in the k-core after iteratively
// pruning nodes with degree < k. Nodes with high k-degree sit at
// the densely connected centre of the graph — useful for "what's
// the core infrastructure every other layer depends on", and as a
// complement to PageRank (which weights by random-walk authority,
// not local density).
//
// Routing:
//
//   - When the backing graph.Store implements graph.KCorer (today
//     only store_sqlite), the analyzer delegates to the engine-
//     native parallel implementation.
//
//   - Otherwise analysis.ComputeKCore runs in-process. The
//     implementation is the classic Batagelj & Zaversnik bucket
//     algorithm — O(V + E), no recursion.

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

// kcoreRow is the per-symbol shape the analyzer returns.
type kcoreRow struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Line     int    `json:"line,omitempty"`
	KDegree  int    `json:"k_degree"`
}

func (s *Server) handleAnalyzeKCore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	minDegree := 0
	if v, ok := args["min_degree"].(float64); ok && v > 0 {
		minDegree = int(v)
	}

	hits := s.runKCore(ctx, graph.KCoreOpts{
		NodeKinds: parseKindFilter(stringArg(args, "node_kinds")),
	})
	reader := s.readerFor(ctx)

	// Filter by min_degree (drop trivial low-core nodes), then cap.
	if minDegree > 0 {
		filtered := hits[:0]
		for _, h := range hits {
			if h.KDegree >= int64(minDegree) {
				filtered = append(filtered, h)
			}
		}
		hits = filtered
	}

	// Narrow to the session workspace + optional repo allow-set when the
	// request scopes below the global graph. Without this the
	// densely-connected core would span every workspace. Filter after the
	// min_degree pass and before the limit cap so the cap lands on the
	// visible set and count recomputes. Strict no-op for an unbound session
	// with no RepoAllow.
	if s.scopeFiltersActive(ctx) {
		kept := hits[:0]
		for _, h := range hits {
			if s.analyzeNodeVisible(ctx, reader.GetNode(h.NodeID)) {
				kept = append(kept, h)
			}
		}
		hits = kept
	}

	if limit > 0 && limit < len(hits) {
		hits = hits[:limit]
	}

	// Batch-materialise hit nodes in one backend round-trip — same
	// rationale as analyze(pagerank). Preserves the descending
	// k-degree order from runKCore.
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.NodeID != "" {
			ids = append(ids, h.NodeID)
		}
	}
	nodeByID := reader.GetNodesByIDs(ids)

	rows := make([]kcoreRow, 0, len(hits))
	for _, h := range hits {
		row := kcoreRow{ID: h.NodeID, KDegree: int(h.KDegree)}
		if n := nodeByID[h.NodeID]; n != nil {
			row.Name = n.Name
			row.Kind = string(n.Kind)
			row.FilePath = n.FilePath
			row.Line = n.StartLine
		}
		rows = append(rows, row)
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("kcore", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %s %s:%d k=%d\n", r.Kind, r.ID, r.FilePath, r.Line, r.KDegree)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{"kcore": rows, "count": len(rows)})
}

// runKCore picks the engine-native KCorer when available,
// otherwise falls back to the in-process implementation. Returns
// hits sorted by k-degree descending (the engine-native CALL
// returns them unordered; the in-process ComputeKCore returns
// already sorted — normalise both here so the handler doesn't
// have to re-sort).
func (s *Server) runKCore(ctx context.Context, opts graph.KCoreOpts) []graph.KCoreHit {
	if store := s.backendStore(); store != nil {
		if kc, ok := store.(graph.KCorer); ok {
			hits, err := kc.KCoreDecomposition(opts)
			if err == nil {
				sort.Slice(hits, func(i, j int) bool {
					if hits[i].KDegree != hits[j].KDegree {
						return hits[i].KDegree > hits[j].KDegree
					}
					return hits[i].NodeID < hits[j].NodeID
				})
				return hits
			}
			// Engine-native error falls through.
		}
	}
	res := analysis.ComputeKCore(s.readerFor(ctx), analysis.KCoreOptions{
		NodeKinds: opts.NodeKinds,
		EdgeKinds: opts.EdgeKinds,
	})
	out := make([]graph.KCoreHit, len(res))
	for i, h := range res {
		out[i] = graph.KCoreHit{NodeID: h.NodeID, KDegree: int64(h.KDegree)}
	}
	return out
}
