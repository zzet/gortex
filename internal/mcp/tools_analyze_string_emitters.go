package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// handleAnalyzeStringEmitters walks EdgeEmits edges to KindString
// nodes and groups by (context, value). Mirrors handleAnalyzeEventEmitters
// but works against the broader string domain (metrics, error
// messages, raw routes; later HTML class/id and i18n keys).
//
// Filters:
//
//   - context: metric|error_msg|route — narrows to one string domain.
//   - name: string value (case-insensitive substring match). Use to
//     find emitters of a specific metric, error message, or route.
func (s *Server) handleAnalyzeStringEmitters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	contextFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "context")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	type stringRow struct {
		ID       string   `json:"id"`
		Context  string   `json:"context"`
		Value    string   `json:"value"`
		Emits    int      `json:"emits"`
		Emitters []string `json:"emitters,omitempty"`
	}
	// Hoisted before the loop: the loop body shadows `ctx` with the string
	// node's context label, so the request reader has to be resolved here.
	reader := s.readerFor(ctx)
	byString := map[string]*stringRow{}
	for e := range edgesByKinds(reader, graph.EdgeEmits) {
		n := reader.GetNode(e.To)
		if n == nil || n.Kind != graph.KindString {
			continue
		}
		ctx, _ := n.Meta["context"].(string)
		if contextFilter != "" && ctx != contextFilter {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(n.Name), nameFilter) {
			continue
		}
		row, ok := byString[e.To]
		if !ok {
			row = &stringRow{
				ID:      e.To,
				Context: ctx,
				Value:   n.Name,
			}
			byString[e.To] = row
		}
		row.Emits++
		row.Emitters = appendUnique(row.Emitters, e.From)
	}
	rows := make([]*stringRow, 0, len(byString))
	for _, r := range byString {
		sort.Strings(r.Emitters)
		rows = append(rows, r)
	}
	// Scope filter: keep a row iff its string node (subject) is visible to
	// the current request, and prune its emitter (actor) list to visible
	// emitters. Emits is an edge count (not a node ID) so it is left
	// intact; total recomputes below. No-op for an unbound request.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*stringRow, 0, len(rows))
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
		if rows[i].Context != rows[j].Context {
			return rows[i].Context < rows[j].Context
		}
		return rows[i].Value < rows[j].Value
	})

	if s.isGCX(ctx, req) {
		items := make([]stringEmitterItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, stringEmitterItem{
				ID:       r.ID,
				Context:  r.Context,
				Value:    r.Value,
				Emits:    r.Emits,
				Emitters: strings.Join(r.Emitters, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("string_emitters", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Emits, r.Context, r.Value)
		}
		if len(rows) == 0 {
			b.WriteString("no string emitters\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"strings": rows,
		"total":   len(rows),
	})
}

// stringEmitterItem is the GCX1 row layout for the string_emitters
// analyzer. Mirrors eventEmitterItem.
type stringEmitterItem struct {
	ID       string `gcx:"id"`
	Context  string `gcx:"context"`
	Value    string `gcx:"value"`
	Emits    int    `gcx:"emits"`
	Emitters string `gcx:"emitters"`
}
