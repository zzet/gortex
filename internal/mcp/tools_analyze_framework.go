package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// handleAnalyzeRoutes surfaces the EdgeHandlesRoute graph layer:
// every (handler symbol, route contract) pair that the contracts
// pipeline detected as a real network route. Answers "which handler
// serves /v1/users/:id?" without making the agent walk EdgeProvides
// and filter by Meta["type"]="http" by hand.
//
// Filters:
//   - method: HTTP verb (GET/POST/...) or gRPC method (case-insensitive)
//   - path:   substring match on the contract's path / topic / channel
//   - type:   contract type — http / grpc / graphql / topic / ws.
//             Named `type` (not `kind`) because the analyze dispatcher
//             reserves `kind` for the analyzer name itself.
func (s *Server) handleAnalyzeRoutes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	methodFilter := strings.ToUpper(strings.TrimSpace(stringArg(args, "method")))
	pathFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "path")))
	kindFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "type")))

	type routeRow struct {
		Handler string `json:"handler"`
		Route   string `json:"route"`
		Method  string `json:"method,omitempty"`
		Path    string `json:"path,omitempty"`
		Kind    string `json:"kind"`
		File    string `json:"file"`
		Line    int    `json:"line"`
	}
	var rows []*routeRow
	for e := range edgesByKinds(s.graph, graph.EdgeHandlesRoute) {
		contractNode := s.graph.GetNode(e.To)
		if contractNode == nil {
			continue
		}
		ctype, _ := contractNode.Meta["type"].(string)
		if kindFilter != "" && ctype != kindFilter {
			continue
		}
		method, path := routeMethodAndPath(contractNode)
		if methodFilter != "" && strings.ToUpper(method) != methodFilter {
			continue
		}
		if pathFilter != "" && !strings.Contains(strings.ToLower(path), pathFilter) {
			continue
		}
		rows = append(rows, &routeRow{
			Handler: e.From,
			Route:   e.To,
			Method:  method,
			Path:    path,
			Kind:    ctype,
			File:    e.FilePath,
			Line:    e.Line,
		})
	}
	// routes reads EdgeHandlesRoute directly off s.graph; narrow each row to
	// the session workspace + optional repo allow-set. Unbound sessions see
	// every row, so this is a strict no-op there. total recomputes after this
	// block.
	//
	// The repo decision comes from the handler alone. A route contract is keyed
	// by method+path with no repo prefix, so a single node is shared by every
	// repo that handles the same route and carries exactly one repo's prefix.
	// Requiring that shared node to satisfy the repo allow-set dropped every row
	// for all but its owning repo — a scoped query returned 0 while the same
	// rows were plainly present unscoped, and analyze contracts accepted the
	// identical repo value. The handler endpoint is unambiguously attributed, so
	// it decides. The contract is still held to the session workspace ceiling.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*routeRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, s.graph.GetNode(r.Handler)) {
				continue
			}
			route := s.graph.GetNode(r.Route)
			if route == nil || !s.nodeInSessionScope(ctx, route) {
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].Method != rows[j].Method {
			return rows[i].Method < rows[j].Method
		}
		return rows[i].Handler < rows[j].Handler
	})
	// A zero-row answer here is ambiguous: this repository may register no
	// routes, or its contract tier may never have been built (a lost index
	// tail commits nothing, and per-file mtime admission never re-extracts
	// it). Say which one when the graph knows — see contract_tier.go. Only
	// an empty answer is qualified; rows on the table are their own proof
	// that the tier was built.
	var unbuilt []string
	if len(rows) == 0 {
		unbuilt = s.contractTierUnbuiltRepos(ctx, repoAllowFromContext(ctx))
	}
	if s.isGCX(ctx, req) {
		items := make([]routeItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, routeItem(*r))
		}
		res, err := s.gcxResponseWithBudget(req)(encodeAnalyze("routes", items))
		if err == nil {
			stampContractTierCaveat(res, unbuilt)
		}
		return res, err
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %-6s %s  →  %s  (%s:%d)\n", r.Kind, r.Method, r.Path, r.Handler, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no routes\n")
		}
		if len(unbuilt) > 0 {
			b.WriteString(contractTierCaveatLine(unbuilt))
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	payload := map[string]any{
		"routes": rows,
		"total":  len(rows),
	}
	if len(unbuilt) > 0 {
		payload["contract_tier"] = contractTierCaveat(unbuilt)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// handleAnalyzeRouteFrameworks lists the registered structural route passes
// (the FrameworkRoutePass registry) — each pass's name and language filter —
// alongside the count of route contract nodes per framework in the active
// graph. It is the queryable face of the route-extraction front door, the
// sibling of analyze kind=synthesizers for the post-resolution dispatch
// registry.
func (s *Server) handleAnalyzeRouteFrameworks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	frameworkCounts := map[string]int{}
	// route_frameworks tallies route contract nodes per framework straight off
	// s.graph.AllNodes(). Gate the contributing loop on visibility so the
	// per-framework counts (and total_passes) reflect only the session
	// workspace + optional repo allow-set. Unbound sessions count every
	// contract node, so the gate is a strict no-op there.
	scoped := s.scopeFiltersActive(ctx)
	for _, n := range s.graph.AllNodes() {
		if n == nil || n.Kind != graph.KindContract || n.Meta == nil {
			continue
		}
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		if fw := routeFramework(n); fw != "" {
			frameworkCounts[fw]++
		}
	}
	type passRow struct {
		Name      string   `json:"name"`
		Languages []string `json:"languages,omitempty"`
		Routes    int      `json:"routes"`
	}
	var passes []passRow
	for _, p := range contracts.RegisteredFrameworkRoutePasses() {
		passes = append(passes, passRow{Name: p.Name(), Languages: p.Languages(), Routes: frameworkCounts[p.Name()]})
	}
	if isCompact(req) {
		var b strings.Builder
		for _, p := range passes {
			fmt.Fprintf(&b, "%-20s %v  (%d routes)\n", p.Name, p.Languages, p.Routes)
		}
		if len(passes) == 0 {
			b.WriteString("no registered route frameworks\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"passes":                    passes,
		"route_counts_by_framework": frameworkCounts,
		"total_passes":              len(passes),
	})
}

// routeFramework reads the framework label off a contract node's Meta
// (top-level or the nested contract_meta map).
func routeFramework(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if meta, ok := n.Meta["contract_meta"].(map[string]any); ok {
		if fw, _ := meta["framework"].(string); fw != "" {
			return fw
		}
	}
	fw, _ := n.Meta["framework"].(string)
	return fw
}

// handleAnalyzeDrupalHooks rolls up every detected Drupal hook
// implementation, grouped by the hook it implements — the queryable face of
// the hook layer ("which modules implement hook_node_insert?").
// handleAnalyzeSwiftUIViews groups SwiftUI types by their classified role
// (component / app_entry), stamped on Meta["swiftui_role"] by the extractor.
func (s *Server) handleAnalyzeSwiftUIViews(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	roleFilter := strings.TrimSpace(stringArg(req.GetArguments(), "role"))
	byRole := map[string][]string{}
	// swiftui_views groups SwiftUI types straight off s.graph.AllNodes(). Gate
	// the contributing loop on visibility so each role's member list (and its
	// recomputed Count) covers only the session workspace + optional repo
	// allow-set; a role with no in-scope members never gets a map key, so it
	// drops out naturally. Unbound sessions keep every type (no-op gate).
	scoped := s.scopeFiltersActive(ctx)
	for _, n := range s.graph.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		role, _ := n.Meta["swiftui_role"].(string)
		if role == "" || (roleFilter != "" && role != roleFilter) {
			continue
		}
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		byRole[role] = append(byRole[role], n.ID)
	}
	type roleRow struct {
		Role  string   `json:"role"`
		Types []string `json:"types"`
		Count int      `json:"count"`
	}
	rows := make([]roleRow, 0, len(byRole))
	for r, ids := range byRole {
		sort.Strings(ids)
		rows = append(rows, roleRow{Role: r, Types: ids, Count: len(ids)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Role < rows[j].Role })
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s: %d\n", r.Role, r.Count)
		}
		if len(rows) == 0 {
			b.WriteString("no swiftui views\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{"roles": rows, "total": len(rows)})
}

// handleAnalyzeUIKitClasses groups UIKit types by their classified role
// (view_controller / view / cell), stamped on Meta["uikit_role"].
func (s *Server) handleAnalyzeUIKitClasses(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	roleFilter := strings.TrimSpace(stringArg(req.GetArguments(), "role"))
	byRole := map[string][]string{}
	// uikit_classes groups UIKit types straight off s.graph.AllNodes(). Gate
	// the contributing loop on visibility so each role's member list (and its
	// recomputed Count) covers only the session workspace + optional repo
	// allow-set; a role with no in-scope members never gets a map key, so it
	// drops out naturally. Unbound sessions keep every type (no-op gate).
	scoped := s.scopeFiltersActive(ctx)
	for _, n := range s.graph.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		role, _ := n.Meta["uikit_role"].(string)
		if role == "" || (roleFilter != "" && role != roleFilter) {
			continue
		}
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		byRole[role] = append(byRole[role], n.ID)
	}
	type roleRow struct {
		Role    string   `json:"role"`
		Classes []string `json:"classes"`
		Count   int      `json:"count"`
	}
	rows := make([]roleRow, 0, len(byRole))
	for r, ids := range byRole {
		sort.Strings(ids)
		rows = append(rows, roleRow{Role: r, Classes: ids, Count: len(ids)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Role < rows[j].Role })
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s: %d\n", r.Role, r.Count)
		}
		if len(rows) == 0 {
			b.WriteString("no uikit classes\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{"roles": rows, "total": len(rows)})
}

func (s *Server) handleAnalyzeDrupalHooks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	nameFilter := strings.TrimSpace(stringArg(req.GetArguments(), "name"))
	hooks := map[string][]string{}
	// drupal_hooks groups hook implementations straight off s.graph.AllNodes().
	// Gate the contributing loop on visibility so each hook's implementation
	// list (and its recomputed Count) covers only the session workspace +
	// optional repo allow-set; a hook with no in-scope implementations never
	// gets a map key, so it drops out naturally. Unbound sessions keep every
	// implementation (no-op gate).
	scoped := s.scopeFiltersActive(ctx)
	for _, n := range s.graph.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		hook, _ := n.Meta["drupal_hook"].(string)
		if hook == "" || (nameFilter != "" && hook != nameFilter) {
			continue
		}
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		hooks[hook] = append(hooks[hook], n.ID)
	}
	type hookRow struct {
		Hook            string   `json:"hook"`
		Implementations []string `json:"implementations"`
		Count           int      `json:"count"`
	}
	rows := make([]hookRow, 0, len(hooks))
	for h, impls := range hooks {
		sort.Strings(impls)
		rows = append(rows, hookRow{Hook: h, Implementations: impls, Count: len(impls)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Hook < rows[j].Hook })
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s: %d implementation(s)\n", r.Hook, r.Count)
		}
		if len(rows) == 0 {
			b.WriteString("no drupal hooks\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{"hooks": rows, "total": len(rows)})
}

// routeMethodAndPath pulls the most useful pair of fields out of a
// KindContract node's Meta. HTTP and WS routes use Meta["method"] +
// Meta["path"]; gRPC uses Meta["service"] + Meta["method"]; topic uses
// Meta["topic"]; GraphQL uses Meta["operation"] + Meta["field"].
func routeMethodAndPath(n *graph.Node) (string, string) {
	if n == nil {
		return "", ""
	}
	// The route fields live in the nested contract_meta map — the
	// contract's own Meta, copied in wholesale at node-build time. The
	// node's top-level Meta only carries type/role/symbol_id/line/
	// confidence, so reading these keys off n.Meta directly always
	// missed. Fall back to the top level for any node that does stamp
	// them there.
	meta, _ := n.Meta["contract_meta"].(map[string]any)
	if meta == nil {
		meta = n.Meta
	}
	method, _ := meta["method"].(string)
	path, _ := meta["path"].(string)
	if path != "" || method != "" {
		return method, path
	}
	if topic, ok := meta["topic"].(string); ok && topic != "" {
		return "", topic
	}
	if op, ok := meta["operation"].(string); ok && op != "" {
		field, _ := meta["field"].(string)
		return op, field
	}
	if svc, ok := meta["service"].(string); ok && svc != "" {
		return method, svc
	}
	return method, path
}

// handleAnalyzeModels surfaces the EdgeModelsTable graph layer: every
// model class that maps to a database table. Useful for "which model
// owns the orders table?" and "which tables does this codebase
// persist?" queries.
//
// Filters:
//   - orm:    orm flavour (gorm / sqlalchemy / django / activerecord / jpa / typeorm / ecto / efcore)
//   - table:  substring match on the table name
//   - model:  substring match on the model class name
func (s *Server) handleAnalyzeModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	ormFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "orm")))
	tableFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "table")))
	modelFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "model")))

	type modelRow struct {
		Model      string `json:"model"`
		Table      string `json:"table"`
		ORM        string `json:"orm"`
		Derivation string `json:"derivation,omitempty"`
		File       string `json:"file"`
		Line       int    `json:"line"`
	}
	var rows []*modelRow
	for e := range edgesByKinds(s.graph, graph.EdgeModelsTable) {
		modelNode := s.graph.GetNode(e.From)
		if modelNode == nil {
			continue
		}
		orm, _ := e.Meta["orm"].(string)
		if ormFilter != "" && strings.ToLower(orm) != ormFilter {
			continue
		}
		tableName, _ := e.Meta["table_name"].(string)
		if tableName == "" {
			tableNode := s.graph.GetNode(e.To)
			if tableNode != nil {
				tableName = tableNode.Name
			}
		}
		if tableFilter != "" && !strings.Contains(strings.ToLower(tableName), tableFilter) {
			continue
		}
		if modelFilter != "" && !strings.Contains(strings.ToLower(modelNode.Name), modelFilter) {
			continue
		}
		derivation, _ := e.Meta["derivation"].(string)
		rows = append(rows, &modelRow{
			Model:      modelNode.ID,
			Table:      tableName,
			ORM:        orm,
			Derivation: derivation,
			File:       e.FilePath,
			Line:       e.Line,
		})
	}
	// models reads EdgeModelsTable directly off s.graph; narrow each row to the
	// session workspace + optional repo allow-set. Table is a plain name (not a
	// node ID), so visibility hinges on the model node (e.From, r.Model) only.
	// Unbound sessions see every model (no-op gate); total recomputes below.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*modelRow, 0, len(rows))
		for _, r := range rows {
			if s.analyzeNodeVisible(ctx, s.graph.GetNode(r.Model)) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ORM != rows[j].ORM {
			return rows[i].ORM < rows[j].ORM
		}
		if rows[i].Table != rows[j].Table {
			return rows[i].Table < rows[j].Table
		}
		return rows[i].Model < rows[j].Model
	})
	if s.isGCX(ctx, req) {
		items := make([]modelItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, modelItem(*r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("models", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %-12s %s  →  %s  (%s:%d)\n", r.ORM, r.Derivation, r.Model, r.Table, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no model→table edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"models": rows,
		"total":  len(rows),
	})
}

// handleAnalyzeComponents surfaces the EdgeRendersChild graph layer:
// the parent → child component dependency tree. Two views:
//
//   - rollup (no `id`): per-component fan-in / fan-out summary so the
//     agent sees which components are central (high fan-in =
//     widely-rendered shared component; high fan-out = composite
//     view).
//   - per-component (id=<symbol>): list of every child the component
//     renders with their resolved targets.
func (s *Server) handleAnalyzeComponents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	if idFilter != "" {
		return s.componentsForOne(ctx, req, idFilter)
	}
	return s.componentsRollup(ctx, req, nameFilter)
}

// componentsRollup groups EdgeRendersChild edges per parent + per
// child to produce a fan-in / fan-out leaderboard.
func (s *Server) componentsRollup(ctx context.Context, req mcp.CallToolRequest, nameFilter string) (*mcp.CallToolResult, error) {
	type compRow struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		FanIn   int    `json:"fan_in"`
		FanOut  int    `json:"fan_out"`
		File    string `json:"file,omitempty"`
	}
	stats := map[string]*compRow{}
	get := func(id string) *compRow {
		row, ok := stats[id]
		if ok {
			return row
		}
		name := id
		file := ""
		if n := s.graph.GetNode(id); n != nil {
			name = n.Name
			file = n.FilePath
		} else if i := strings.LastIndex(id, "::"); i >= 0 {
			name = id[i+2:]
		}
		row = &compRow{ID: id, Name: name, File: file}
		stats[id] = row
		return row
	}
	// components reads EdgeRendersChild directly off s.graph. When the request
	// narrows scope, gate the edge loop on BOTH endpoints being visible so the
	// fan-in / fan-out tallies (and which nodes enter `stats`) cover only the
	// session workspace + optional repo allow-set — no out-of-scope neighbor
	// inflates a count. Unbound sessions count every edge (no-op gate).
	scoped := s.scopeFiltersActive(ctx)
	for e := range edgesByKinds(s.graph, graph.EdgeRendersChild) {
		if scoped && (!s.analyzeNodeVisible(ctx, s.graph.GetNode(e.From)) ||
			!s.analyzeNodeVisible(ctx, s.graph.GetNode(e.To))) {
			continue
		}
		parent := get(e.From)
		parent.FanOut++
		// Skip the child if it never resolved to a real node — leaving
		// it in the fan-in count would inflate uses-of-unresolved
		// references and pollute the rollup. Resolved targets show up
		// without the "unresolved::" prefix.
		if !strings.HasPrefix(e.To, "unresolved::") {
			child := get(e.To)
			child.FanIn++
		}
	}
	rows := make([]*compRow, 0, len(stats))
	for _, r := range stats {
		if nameFilter != "" && !strings.Contains(strings.ToLower(r.Name), nameFilter) {
			continue
		}
		if r.FanIn == 0 && r.FanOut == 0 {
			continue
		}
		// Belt-and-suspenders row gate: keep a row only when its component node
		// is itself visible. Redundant given the edge-loop gate above (under
		// scope, `stats` only holds visible nodes) but kept explicit per the
		// scope contract; a strict no-op for unbound sessions.
		if scoped && !s.analyzeNodeVisible(ctx, s.graph.GetNode(r.ID)) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		ai := rows[i].FanIn + rows[i].FanOut
		aj := rows[j].FanIn + rows[j].FanOut
		if ai != aj {
			return ai > aj
		}
		return rows[i].Name < rows[j].Name
	})
	if s.isGCX(ctx, req) {
		items := make([]componentRollupItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, componentRollupItem(*r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("components", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d in / %-3d out  %-30s  (%s)\n", r.FanIn, r.FanOut, r.Name, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no renders_child edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"components": rows,
		"total":      len(rows),
	})
}

// componentsForOne returns every child component a single parent
// renders, with the resolved-target indicator per row.
func (s *Server) componentsForOne(ctx context.Context, req mcp.CallToolRequest, parentID string) (*mcp.CallToolResult, error) {
	type childRow struct {
		To       string `json:"to"`
		Name     string `json:"name"`
		Resolved bool   `json:"resolved"`
		File     string `json:"file,omitempty"`
		Line     int    `json:"line"`
	}
	var rows []*childRow
	// components(id=…) reads the parent's out-edges directly off s.graph. Under
	// an active scope, emit no children when the requested parent is itself out
	// of scope, and prune children to visible resolved targets (e.To). Unbound
	// sessions see the parent and every child (both gates collapse to no-ops).
	scoped := s.scopeFiltersActive(ctx)
	parentInScope := !scoped || s.analyzeNodeVisible(ctx, s.graph.GetNode(parentID))
	for _, e := range s.graph.GetOutEdges(parentID) {
		if e.Kind != graph.EdgeRendersChild {
			continue
		}
		if scoped && (!parentInScope || !s.analyzeNodeVisible(ctx, s.graph.GetNode(e.To))) {
			continue
		}
		name, _ := e.Meta["child_name"].(string)
		if name == "" {
			if strings.HasPrefix(e.To, "unresolved::") {
				name = strings.TrimPrefix(e.To, "unresolved::")
			} else if n := s.graph.GetNode(e.To); n != nil {
				name = n.Name
			}
		}
		rows = append(rows, &childRow{
			To:       e.To,
			Name:     name,
			Resolved: !strings.HasPrefix(e.To, "unresolved::"),
			File:     e.FilePath,
			Line:     e.Line,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Line != rows[j].Line {
			return rows[i].Line < rows[j].Line
		}
		return rows[i].Name < rows[j].Name
	})
	if s.isGCX(ctx, req) {
		items := make([]componentChildItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, componentChildItem{
				To:       r.To,
				Name:     r.Name,
				Resolved: boolStr(r.Resolved),
				File:     r.File,
				Line:     r.Line,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("components.children", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			marker := "✓"
			if !r.Resolved {
				marker = "?"
			}
			fmt.Fprintf(&b, "%s %s  (%s:%d)\n", marker, r.Name, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no children\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"parent":   parentID,
		"children": rows,
		"total":    len(rows),
	})
}

// handleAnalyzeDbtModels surfaces the dbt / SQLMesh graph layer: every
// KindTable node the dbt / SQLMesh extractor emitted (models, seeds,
// snapshots, sources), with its column count and its lineage fan-out /
// fan-in over EdgeDependsOn. Answers "which models have no columns
// documented?", "what feeds stg_orders?", "which sources does nothing
// consume?" without walking the graph by hand.
//
// Filters:
//   - framework:     dbt | sqlmesh
//   - type:          resource type — model / seed / snapshot / source.
//                    Named `type` (not `kind`) because the analyze
//                    dispatcher reserves `kind` for the analyzer name.
//   - materialized:  substring match on the materialization
//   - name:          substring match on the model / source name
func (s *Server) handleAnalyzeDbtModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	frameworkFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "framework")))
	typeFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "type")))
	matFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "materialized")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	type dbtModelRow struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Framework    string `json:"framework"`
		ResourceType string `json:"resource_type"`
		Materialized string `json:"materialized,omitempty"`
		Schema       string `json:"schema,omitempty"`
		Columns      int    `json:"columns"`
		Upstream     int    `json:"upstream"`
		Downstream   int    `json:"downstream"`
		File         string `json:"file"`
		Line         int    `json:"line"`
	}

	// First pass: collect the model nodes (KindTable nodes the dbt /
	// SQLMesh extractor stamped with a `framework` meta key).
	rowByID := map[string]*dbtModelRow{}
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindTable {
			continue
		}
		framework, _ := n.Meta["framework"].(string)
		if framework != "dbt" && framework != "sqlmesh" {
			continue
		}
		resourceType, _ := n.Meta["resource_type"].(string)
		materialized, _ := n.Meta["materialized"].(string)
		schema, _ := n.Meta["schema"].(string)
		rowByID[n.ID] = &dbtModelRow{
			ID: n.ID, Name: n.Name, Framework: framework,
			ResourceType: resourceType, Materialized: materialized,
			Schema: schema, File: n.FilePath, Line: n.StartLine,
		}
	}

	// Second pass: tally columns (EdgeMemberOf → model) and lineage
	// (EdgeDependsOn between two model nodes) in one walk of AllEdges.
	for e := range edgesByKinds(s.graph, graph.EdgeMemberOf, graph.EdgeDependsOn) {
		switch e.Kind {
		case graph.EdgeMemberOf:
			if r := rowByID[e.To]; r != nil {
				r.Columns++
			}
		case graph.EdgeDependsOn:
			if r := rowByID[e.From]; r != nil {
				r.Upstream++
			}
			if r := rowByID[e.To]; r != nil {
				r.Downstream++
			}
		}
	}

	var rows []*dbtModelRow
	for _, r := range rowByID {
		if frameworkFilter != "" && r.Framework != frameworkFilter {
			continue
		}
		if typeFilter != "" && strings.ToLower(r.ResourceType) != typeFilter {
			continue
		}
		if matFilter != "" && !strings.Contains(strings.ToLower(r.Materialized), matFilter) {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(r.Name), nameFilter) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Framework != rows[j].Framework {
			return rows[i].Framework < rows[j].Framework
		}
		if rows[i].ResourceType != rows[j].ResourceType {
			return rows[i].ResourceType < rows[j].ResourceType
		}
		return rows[i].Name < rows[j].Name
	})

	if s.isGCX(ctx, req) {
		items := make([]dbtModelItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, dbtModelItem(*r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("dbt_models", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-7s %-9s %-14s %-30s  %2d cols  %d↑ %d↓  (%s:%d)\n",
				r.Framework, r.ResourceType, r.Materialized, r.Name,
				r.Columns, r.Upstream, r.Downstream, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no dbt / SQLMesh models\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"dbt_models": rows,
		"total":      len(rows),
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// routeItem is the GCX1 row layout for the routes analyzer.
type routeItem struct {
	Handler string `gcx:"handler"`
	Route   string `gcx:"route"`
	Method  string `gcx:"method"`
	Path    string `gcx:"path"`
	Kind    string `gcx:"kind"`
	File    string `gcx:"file"`
	Line    int    `gcx:"line"`
}

// modelItem is the GCX1 row layout for the models analyzer.
type modelItem struct {
	Model      string `gcx:"model"`
	Table      string `gcx:"table"`
	ORM        string `gcx:"orm"`
	Derivation string `gcx:"derivation"`
	File       string `gcx:"file"`
	Line       int    `gcx:"line"`
}

// componentRollupItem is the GCX1 row layout for the components rollup.
type componentRollupItem struct {
	ID     string `gcx:"id"`
	Name   string `gcx:"name"`
	FanIn  int    `gcx:"fan_in"`
	FanOut int    `gcx:"fan_out"`
	File   string `gcx:"file"`
}

// componentChildItem is the GCX1 row layout for per-component children.
type componentChildItem struct {
	To       string `gcx:"to"`
	Name     string `gcx:"name"`
	Resolved string `gcx:"resolved"`
	File     string `gcx:"file"`
	Line     int    `gcx:"line"`
}

// dbtModelItem is the GCX1 row layout for the dbt_models analyzer. The
// field set mirrors the JSON dbtModelRow one-for-one so the
// dbtModelItem(*r) conversion in handleAnalyzeDbtModels stays valid.
type dbtModelItem struct {
	ID           string `gcx:"id"`
	Name         string `gcx:"name"`
	Framework    string `gcx:"framework"`
	ResourceType string `gcx:"resource_type"`
	Materialized string `gcx:"materialized"`
	Schema       string `gcx:"schema"`
	Columns      int    `gcx:"columns"`
	Upstream     int    `gcx:"upstream"`
	Downstream   int    `gcx:"downstream"`
	File         string `gcx:"file"`
	Line         int    `gcx:"line"`
}
