package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// externalModuleRow rolls up call/symbol counts for one external module
// (stdlib or module-cache entry). Surfaces in the JSON / compact /
// GCX1 outputs of `analyze kind=external_calls`.
type externalModuleRow struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Version    string `json:"version,omitempty"`
	ModuleKind string `json:"module_kind"`
	Symbols    int    `json:"symbols"`
	Calls      int    `json:"calls"`
}

// externalSymbolRow lists one external symbol attributed to a specific
// KindModule. Returned when the caller pins `id` to a module node ID.
type externalSymbolRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Path    string `json:"import_path"`
	Calls   int    `json:"calls"`
	Callers int    `json:"callers"`
}

// handleAnalyzeExternalCalls surfaces the cross-repo / external symbol
// attribution materialised by the goanalysis externals pass. It walks
// every edge whose target is an `ext::` node and groups by the owning
// module, yielding either a per-module rollup ("which stdlib /
// module-cache packages does this codebase reach into?") or a per-
// symbol detail when the caller pins a module via the `id` filter.
//
// Filters:
//
//   - id: KindModule node ID (e.g. `module::go:stdlib`,
//     `module::go:github.com/foo/bar@v1.2.3`). When set, returns one row
//     per external symbol attributed to that module with caller counts.
//   - module_kind: stdlib | module_cache. Restricts the rollup to one
//     module class.
//   - module_path: substring match on the module path (e.g. "github.com"
//     to filter to module-cache only).
//   - name: substring match on external symbol name (used with id).
func (s *Server) handleAnalyzeExternalCalls(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	moduleKindFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "module_kind")))
	modulePathFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "module_path")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	if idFilter != "" {
		return s.externalCallsForModule(ctx, req, idFilter, nameFilter)
	}
	return s.externalCallsRollup(ctx, req, moduleKindFilter, modulePathFilter)
}

// externalCallsRollup groups external attributions by module and returns
// per-module call/symbol counts.
func (s *Server) externalCallsRollup(ctx context.Context, req mcp.CallToolRequest, moduleKindFilter, modulePathFilter string) (*mcp.CallToolResult, error) {
	byModule := map[string]*externalModuleRow{}

	for _, n := range s.scopedNodes(ctx) {
		if n == nil || n.Kind != graph.KindModule {
			continue
		}
		// Skip synthetic module nodes — the resolver's external-call
		// synthesis pass materialises a KindModule placeholder per
		// un-indexed package so call chains keep the external hop, but
		// those carry no goanalysis attribution and would only show as
		// empty 0/0 rows here. This rollup is for type-checker-grounded
		// module usage; synthetic nodes are surfaced via their edges.
		if synthetic, _ := n.Meta["synthetic"].(bool); synthetic {
			continue
		}
		moduleKind, _ := n.Meta["module_kind"].(string)
		if moduleKindFilter != "" && moduleKind != moduleKindFilter {
			continue
		}
		modulePath, _ := n.Meta["path"].(string)
		if modulePathFilter != "" && !strings.Contains(strings.ToLower(modulePath), modulePathFilter) {
			continue
		}
		version, _ := n.Meta["version"].(string)
		byModule[n.ID] = &externalModuleRow{
			ID:         n.ID,
			Path:       modulePath,
			Version:    version,
			ModuleKind: moduleKind,
		}
	}
	if len(byModule) == 0 {
		return s.emitExternalCallsRollup(ctx, req, nil)
	}

	// Hoisted once — the caller-tally below runs per external symbol.
	reader := s.readerFor(ctx)
	for _, n := range s.scopedNodes(ctx) {
		if n == nil {
			continue
		}
		if !isExternalSymbolNode(n) {
			continue
		}
		moduleID, _ := n.Meta["module_id"].(string)
		row, ok := byModule[moduleID]
		if !ok {
			continue
		}
		row.Symbols++
		row.Calls += countCallersToExternal(reader, n.ID)
	}

	rows := make([]*externalModuleRow, 0, len(byModule))
	for _, r := range byModule {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		if rows[i].Symbols != rows[j].Symbols {
			return rows[i].Symbols > rows[j].Symbols
		}
		return rows[i].Path < rows[j].Path
	})

	return s.emitExternalCallsRollup(ctx, req, rows)
}

// externalCallsForModule lists every external symbol attributed to one
// KindModule, with its caller count.
func (s *Server) externalCallsForModule(ctx context.Context, req mcp.CallToolRequest, moduleID, nameFilter string) (*mcp.CallToolResult, error) {
	reader := s.readerFor(ctx)
	mod := reader.GetNode(moduleID)
	rows := []*externalSymbolRow{}
	if mod != nil && mod.Kind == graph.KindModule {
		for _, n := range s.scopedNodes(ctx) {
			if n == nil {
				continue
			}
			if !isExternalSymbolNode(n) {
				continue
			}
			id, _ := n.Meta["module_id"].(string)
			if id != moduleID {
				continue
			}
			if nameFilter != "" && !strings.Contains(strings.ToLower(n.Name), nameFilter) {
				continue
			}
			calls, callers := tallyExternalCallers(reader, n.ID)
			importPath, _ := n.Meta["import_path"].(string)
			rows = append(rows, &externalSymbolRow{
				ID:      n.ID,
				Name:    n.Name,
				Kind:    string(n.Kind),
				Path:    importPath,
				Calls:   calls,
				Callers: callers,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]externalSymbolItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, externalSymbolItem(*r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("external_calls.symbols", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-4d %-4d %s  (%s)\n", r.Calls, r.Callers, r.Name, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no external symbols for that module\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	out := map[string]any{
		"module":  moduleID,
		"symbols": rows,
		"total":   len(rows),
	}
	if mod != nil {
		out["module_kind"], _ = mod.Meta["module_kind"].(string)
		out["module_path"], _ = mod.Meta["path"].(string)
		if v, _ := mod.Meta["version"].(string); v != "" {
			out["version"] = v
		}
	}
	return s.respondJSONOrTOON(ctx, req, out)
}

func (s *Server) emitExternalCallsRollup(ctx context.Context, req mcp.CallToolRequest, rows []*externalModuleRow) (*mcp.CallToolResult, error) {
	if s.isGCX(ctx, req) {
		items := make([]externalModuleItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, externalModuleItem{
				ID:         r.ID,
				Path:       r.Path,
				Version:    r.Version,
				ModuleKind: r.ModuleKind,
				Symbols:    r.Symbols,
				Calls:      r.Calls,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("external_calls", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-4d %-4d  [%s] %s%s\n", r.Calls, r.Symbols, r.ModuleKind, r.Path, suffixVersion(r.Version))
		}
		if len(rows) == 0 {
			b.WriteString("no external attributions — run goanalysis enrichment first\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"modules": rows,
		"total":   len(rows),
	})
}

func suffixVersion(v string) string {
	if v == "" {
		return ""
	}
	return "@" + v
}

// countCallersToExternal counts every incoming non-EdgeDependsOnModule
// edge to an external symbol node — those are the calls / references
// that goanalysis attributed.
func countCallersToExternal(g graph.Reader, nodeID string) int {
	n := 0
	for _, e := range g.GetInEdges(nodeID) {
		if e.Kind == graph.EdgeDependsOnModule {
			continue
		}
		n++
	}
	return n
}

// tallyExternalCallers returns (totalCallEdges, distinctCallers) — the
// detail surface for the per-module symbol listing.
func tallyExternalCallers(g graph.Reader, nodeID string) (int, int) {
	calls := 0
	seen := map[string]struct{}{}
	for _, e := range g.GetInEdges(nodeID) {
		if e.Kind == graph.EdgeDependsOnModule {
			continue
		}
		calls++
		seen[e.From] = struct{}{}
	}
	return calls, len(seen)
}

// isExternalSymbolNode reports whether n is one of the synthetic nodes
// created by the goanalysis externals pass — distinguishable by the
// `external` Meta flag.
func isExternalSymbolNode(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == graph.KindModule {
		return false
	}
	if v, ok := n.Meta["external"].(bool); ok {
		return v
	}
	return strings.HasPrefix(n.ID, "ext::")
}

// externalModuleItem is the GCX1 row layout for the rollup.
type externalModuleItem struct {
	ID         string `gcx:"id"`
	Path       string `gcx:"path"`
	Version    string `gcx:"version"`
	ModuleKind string `gcx:"module_kind"`
	Symbols    int    `gcx:"symbols"`
	Calls      int    `gcx:"calls"`
}

// externalSymbolItem is the GCX1 row layout for per-module symbol detail.
type externalSymbolItem struct {
	ID      string `gcx:"id"`
	Name    string `gcx:"name"`
	Kind    string `gcx:"kind"`
	Path    string `gcx:"import_path"`
	Calls   int    `gcx:"calls"`
	Callers int    `gcx:"callers"`
}
