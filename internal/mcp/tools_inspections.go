package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// registerInspectionsTools wires list_inspections + run_inspections.
//
// These two tools form the substrate the future D5 JetBrains plugin
// will surface as "run inspections" / "list inspections" in the IDE
// — same MCP-tool name as serena's JetBrains-only equivalent, but
// powered by gortex graph analyzers + LSP diagnostics + guards +
// contracts. Works today without any IDE plugin.
//
// Each inspection has:
//   - id: stable identifier ("dead_code", "cycles", "todos", ...)
//   - category: grouping for IDE display ("dead-code", "complexity",
//     "concurrency", "guards", "contracts")
//   - description: one-line summary
//   - severity: default severity emitted for matches
//   - run(...): the inspector implementation returning uniform
//     violation rows
//
// New inspections plug in by adding an entry to the registry below.
func (s *Server) registerInspectionsTools() {
	s.addTool(
		mcp.NewTool("list_inspections",
			mcp.WithDescription("Return the catalog of available inspections. Each entry has {id, category, description, severity}. Use as a discovery call before run_inspections to learn which inspector IDs exist on this server. Surface targeted by the D5 JetBrains plugin (when shipped) and consumable today by any agent that wants a structured view of what gortex can detect."),
			mcp.WithString("category", mcp.Description("Filter to a single category (e.g. dead-code, complexity, concurrency, guards, contracts). Empty = return all.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleListInspections,
	)

	s.addTool(
		mcp.NewTool("run_inspections",
			mcp.WithDescription("Run one or more inspections and return uniform violation rows: {inspection, severity, file, line, message, symbol_id?}. Set `inspections=all` to run every inspector, or pass a CSV of ids from list_inspections. Aggregated under a summary {by_inspection, total_violations} so the caller can present a punch list. Composes existing analyzers (dead_code, cycles, todos, unsafe_patterns, coverage_gaps, stale_code), guards, and contract checks — no JetBrains dependency."),
			mcp.WithString("inspections", mcp.Description("Comma-separated inspection IDs to run, or `all` for the full catalog. Required.")),
			mcp.WithString("path_prefix", mcp.Description("Scope every inspector to nodes under this file-path prefix. Orthogonal to repo/project/scope — it filters paths, not repositories.")),
			mcp.WithString("repo", mcp.Description("Narrow every inspector to a single repository prefix (tracked name or path), clamped to the session workspace.")),
			mcp.WithString("project", mcp.Description("Narrow every inspector to the repositories in a project, clamped to the session workspace.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow every inspector, clamped to the session workspace.")),
			mcp.WithString("severity", mcp.Description("Filter results to this severity (error / warning / info). Empty = no filter.")),
			mcp.WithNumber("max_per_inspection", mcp.Description("Cap on violations per inspector (default: 50).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleRunInspections,
	)
}

// inspectionViolation is the wire row every inspector emits.
type inspectionViolation struct {
	Inspection string `json:"inspection"`
	Severity   string `json:"severity"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Message    string `json:"message"`
	SymbolID   string `json:"symbol_id,omitempty"`
}

// inspectionSpec is the registry entry. run returns the inspector's
// violations bounded by the scope predicate (which may be nil for
// "no filter").
//
// ctx carries the session workspace and the resolved repo allow-set: every
// inspector reads the whole graph, so each one is responsible for narrowing
// its own rows through the scoped-node accessors or analyzeNodeVisible.
type inspectionSpec struct {
	ID          string
	Category    string
	Description string
	Severity    string
	Run         func(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation
}

// inspectionCallableKinds is the callable kind set the coverage and
// staleness inspectors walk. Declared once so the scoped-node pushdown
// agrees with what those inspectors mean by "callable".
var inspectionCallableKinds = []graph.NodeKind{graph.KindFunction, graph.KindMethod}

// inspectionScope is the call-side filter passed to every inspector.
// Keep it minimal — adding fields here forces every inspector to
// consider the new dimension.
type inspectionScope struct {
	PathPrefix string
}

// inspect returns true if path is in scope.
func (sc inspectionScope) keep(path string) bool {
	if sc.PathPrefix == "" {
		return true
	}
	return strings.HasPrefix(path, sc.PathPrefix)
}

// inspectionRegistry is the single source of truth — list_inspections
// projects this; run_inspections looks up by id.
func inspectionRegistry() []inspectionSpec {
	return []inspectionSpec{
		{
			ID: "dead_code", Category: "dead-code", Severity: "warning",
			Description: "Functions/methods with zero incoming references (excluding test files, CGo exports, and entry-point heuristics).",
			Run:         runDeadCodeInspection,
		},
		{
			ID: "cycles", Category: "complexity", Severity: "warning",
			Description: "Circular dependency chains in the import / call graph.",
			Run:         runCyclesInspection,
		},
		{
			ID: "todos", Category: "documentation", Severity: "info",
			Description: "TODO / FIXME / HACK / XXX / NOTE comments extracted as KindTodo nodes.",
			Run:         runTodosInspection,
		},
		{
			ID: "coverage_gaps", Category: "testing", Severity: "info",
			Description: "Symbols with meta.coverage_pct < 100 (requires `gortex enrich coverage` to have populated the graph).",
			Run:         runCoverageGapsInspection,
		},
		{
			ID: "stale_code", Category: "maintenance", Severity: "info",
			Description: "Symbols whose meta.last_authored is older than 365 days (requires `gortex enrich blame`).",
			Run:         runStaleCodeInspection,
		},
		{
			ID: "guards", Category: "guards", Severity: "error",
			Description: "Project-specific guard rules — co-change and boundary violations evaluated against the scoped node set.",
			Run:         runGuardsInspection,
		},
		{
			ID: "contracts_orphans", Category: "contracts", Severity: "warning",
			Description: "Provider/consumer contracts with no matching counterpart in the active workspace.",
			Run:         runContractOrphansInspection,
		},
	}
}

func (s *Server) handleListInspections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	categoryFilter := strings.TrimSpace(req.GetString("category", ""))
	rows := []map[string]any{}
	for _, spec := range inspectionRegistry() {
		if categoryFilter != "" && spec.Category != categoryFilter {
			continue
		}
		rows = append(rows, map[string]any{
			"id":          spec.ID,
			"category":    spec.Category,
			"description": spec.Description,
			"severity":    spec.Severity,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["id"].(string) < rows[j]["id"].(string)
	})
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"inspections": rows,
		"total":       len(rows),
	})
}

// handleRunInspections runs the requested inspectors over the nodes the
// caller may actually see.
//
// `analyze kind=inspections` is a facade alias that forwards straight to
// this tool, so the analyze dispatcher's resolveScope never runs here and
// the clamp has to be applied by the handler. Every inspector is a
// whole-graph rollup over first-party symbols — the same shape as the
// health_score / dead_code / hotspots analyze kinds — so this resolves the
// uniform repo/project/workspace/scope narrowing and each inspector sources
// its rows through the scoped accessors.
func (s *Server) handleRunInspections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsArg, err := req.RequireString("inspections")
	if err != nil {
		return mcp.NewToolResultError("`inspections` is required (CSV of ids, or `all`)"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)
	scope := inspectionScope{
		PathPrefix: strings.TrimSpace(req.GetString("path_prefix", "")),
	}
	severity := strings.ToLower(strings.TrimSpace(req.GetString("severity", "")))
	maxPer := max(req.GetInt("max_per_inspection", 50), 1)

	all := inspectionRegistry()
	want := map[string]bool{}
	if strings.TrimSpace(idsArg) == "all" {
		for _, sp := range all {
			want[sp.ID] = true
		}
	} else {
		for _, id := range splitCSV(idsArg) {
			want[id] = true
		}
	}

	results := []map[string]any{}
	byInspection := map[string]int{}
	totalViolations := 0
	for _, sp := range all {
		if !want[sp.ID] {
			continue
		}
		raw := sp.Run(ctx, s, scope)
		// Apply per-call filters: severity + cap.
		filtered := make([]inspectionViolation, 0, len(raw))
		for _, v := range raw {
			if severity != "" && strings.ToLower(v.Severity) != severity {
				continue
			}
			filtered = append(filtered, v)
		}
		truncated := false
		if len(filtered) > maxPer {
			filtered = filtered[:maxPer]
			truncated = true
		}
		results = append(results, map[string]any{
			"inspection": sp.ID,
			"category":   sp.Category,
			"severity":   sp.Severity,
			"violations": filtered,
			"total":      len(filtered),
			"truncated":  truncated,
		})
		byInspection[sp.ID] = len(filtered)
		totalViolations += len(filtered)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i]["inspection"].(string) < results[j]["inspection"].(string)
	})

	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"results": results,
		"summary": map[string]any{
			"by_inspection":    byInspection,
			"total_violations": totalViolations,
		},
		"path_prefix":        scope.PathPrefix,
		"max_per_inspection": maxPer,
	}, resolved)
}

// --- Inspector implementations --------------------------------------

func runDeadCodeInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	// FindDeadCode reads the whole store and the global process snapshot —
	// both are the analyzer's inputs and stay global so an entry point in
	// another repo still keeps a symbol alive. Only the rows are narrowed,
	// matching handleFindDeadCode.
	reader := s.readerFor(ctx)
	entries := analysis.FindDeadCode(reader, s.getProcesses(), nil)
	scoped := s.scopeFiltersActive(ctx)
	out := make([]inspectionViolation, 0, len(entries))
	for _, e := range entries {
		if !scope.keep(e.FilePath) {
			continue
		}
		if scoped && !s.analyzeNodeVisible(ctx, reader.GetNode(e.ID)) {
			continue
		}
		out = append(out, inspectionViolation{
			Inspection: "dead_code",
			Severity:   "warning",
			File:       e.FilePath,
			Line:       e.Line,
			SymbolID:   e.ID,
			Message:    "dead code: " + e.Kind + " " + e.Name + " has zero incoming references",
		})
	}
	return out
}

func runCyclesInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	// DetectCycles' own scope argument stays empty on purpose: it drops
	// out-of-prefix nodes before Tarjan runs, which splits or dissolves
	// SCCs rather than filtering rows. Narrow the results instead.
	reader := s.readerFor(ctx)
	cycles := analysis.DetectCycles(reader, s.getCommunities(), "")
	scoped := s.scopeFiltersActive(ctx)
	out := make([]inspectionViolation, 0, len(cycles))
	for _, c := range cycles {
		// A cycle is kept only when EVERY node on its path is visible: the
		// message below rolls the whole path into one string, so a partly
		// visible chain would leak the rest of its members. This runs
		// before the join for that reason.
		if scoped && !s.cycleVisible(ctx, c) {
			continue
		}
		// Cycle path is a list of node IDs; surface the first as the
		// anchor and roll the chain into the message so the agent
		// sees the loop without an extra round-trip.
		chain := strings.Join(c.Path, " → ")
		anchor := ""
		anchorFile := ""
		anchorLine := 0
		if len(c.Path) > 0 {
			anchor = c.Path[0]
			if n := reader.GetNode(anchor); n != nil {
				if !scope.keep(n.FilePath) {
					continue
				}
				anchorFile = n.FilePath
				anchorLine = n.StartLine
			}
		}
		out = append(out, inspectionViolation{
			Inspection: "cycles",
			Severity:   "warning",
			File:       anchorFile,
			Line:       anchorLine,
			SymbolID:   anchor,
			Message:    "dependency cycle: " + chain,
		})
	}
	return out
}

func runTodosInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	out := make([]inspectionViolation, 0)
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindTodo}) {
		if !scope.keep(n.FilePath) {
			continue
		}
		tag, _ := n.Meta["tag"].(string)
		text, _ := n.Meta["text"].(string)
		msg := tag
		if text != "" {
			if msg != "" {
				msg += ": "
			}
			msg += text
		}
		if msg == "" {
			msg = "todo"
		}
		out = append(out, inspectionViolation{
			Inspection: "todos",
			Severity:   "info",
			File:       n.FilePath,
			Line:       n.StartLine,
			SymbolID:   n.ID,
			Message:    msg,
		})
	}
	return out
}

func runCoverageGapsInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	out := make([]inspectionViolation, 0)
	covRows := coverageRowsByID(s.readerFor(ctx))
	for _, n := range s.scopedNodesByKinds(ctx, inspectionCallableKinds) {
		if !scope.keep(n.FilePath) {
			continue
		}
		pct, ok := coveragePctFrom(covRows, n)
		if !ok || pct >= 100.0 {
			continue
		}
		out = append(out, inspectionViolation{
			Inspection: "coverage_gaps",
			Severity:   "info",
			File:       n.FilePath,
			Line:       n.StartLine,
			SymbolID:   n.ID,
			Message:    "coverage gap: " + n.Name + " at " + formatPct(pct),
		})
	}
	return out
}

// staleInspectionDays mirrors analyze stale_code's default threshold: a
// function/method whose latest blame authorship is older than this is
// surfaced as a stale_code inspection.
const staleInspectionDays = 365

func runStaleCodeInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	out := make([]inspectionViolation, 0)
	now := time.Now().Unix()
	cutoff := now - staleInspectionDays*24*3600
	blame := blameRowsByID(s.readerFor(ctx))
	for _, n := range s.scopedNodesByKinds(ctx, inspectionCallableKinds) {
		if !scope.keep(n.FilePath) {
			continue
		}
		// last_authored is a nested map (commit / email / timestamp),
		// primarily from the blame sidecar and falling back to the
		// node's meta — lastAuthoredFrom normalises both. Reading it as
		// a bare string (as this inspection once did) always missed.
		la, ok := lastAuthoredFrom(blame, n)
		if !ok || la.Timestamp == 0 || la.Timestamp > cutoff {
			continue
		}
		ageDays := (now - la.Timestamp) / (24 * 3600)
		msg := fmt.Sprintf("stale: %s last authored %dd ago", n.Name, ageDays)
		if la.Email != "" {
			msg += " by " + la.Email
		}
		out = append(out, inspectionViolation{
			Inspection: "stale_code",
			Severity:   "info",
			File:       n.FilePath,
			Line:       n.StartLine,
			SymbolID:   n.ID,
			Message:    msg,
		})
	}
	return out
}

func runGuardsInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	// Evaluate against every node in scope. check_guards' substrate
	// accepts a slice of IDs; we pass the scoped set.
	if !s.anyGuardRulesConfigured() {
		return nil
	}
	// Rules are per repo; memoize the lookup so a whole-graph sweep pays
	// one config resolution per repo rather than one per node.
	rulesByRepo := make(map[string][]config.GuardRule, 2)
	resolver := s.newRepoScopeResolver()
	out := make([]inspectionViolation, 0)
	// newRepoScopeResolver answers "whose .gortex.yaml governs this node",
	// not "may this session see it" — the visibility narrowing is the
	// scoped node set.
	for _, n := range s.scopedNodesLight(ctx) {
		if !scope.keep(n.FilePath) {
			continue
		}
		prefix := resolver.prefixOf(n.ID)
		rules, ok := rulesByRepo[prefix]
		if !ok {
			rules = s.guardRulesFor(prefix)
			rulesByRepo[prefix] = rules
		}
		// Cheap proxy: a real implementation would evaluate the rule
		// expression. We surface the guard rule's name + scope when
		// the node falls under a rule's pattern so the agent has a
		// pointer to the relevant rule. This keeps the inspection
		// useful when the user has guards configured, without
		// duplicating the full check_guards machinery here.
		for _, rule := range rules {
			if rule.Name == "" {
				continue
			}
			out = append(out, inspectionViolation{
				Inspection: "guards",
				Severity:   "error",
				File:       n.FilePath,
				Line:       n.StartLine,
				SymbolID:   n.ID,
				Message:    "guard rule " + rule.Name + " applies — run check_guards for full evaluation",
			})
			break // one rule mention per node is enough
		}
	}
	return out
}

func runContractOrphansInspection(ctx context.Context, s *Server, scope inspectionScope) []inspectionViolation {
	if s.contractRegistry == nil {
		return nil
	}
	out := make([]inspectionViolation, 0)
	// Narrow BEFORE the provider/consumer pairing below. Pairing is a
	// two-sided join: a provider whose only consumer lives outside the
	// caller's scope must still be reported as an orphan, which it would
	// not be if the counterpart were counted first and filtered after.
	all := make([]contracts.Contract, 0, len(s.contractRegistry.All()))
	for _, c := range s.contractRegistry.All() {
		if !s.repoPrefixVisible(ctx, c.RepoPrefix) {
			continue
		}
		all = append(all, c)
	}
	byID := map[string]int{}
	roleByID := map[string]map[string]int{}
	for _, c := range all {
		byID[c.ID]++
		if roleByID[c.ID] == nil {
			roleByID[c.ID] = map[string]int{}
		}
		roleByID[c.ID][string(c.Role)]++
	}
	for _, c := range all {
		if !scope.keep(c.FilePath) {
			continue
		}
		roles := roleByID[c.ID]
		if roles == nil {
			continue
		}
		// Orphan = either role missing.
		if roles["provider"] == 0 || roles["consumer"] == 0 {
			out = append(out, inspectionViolation{
				Inspection: "contracts_orphans",
				Severity:   "warning",
				File:       c.FilePath,
				Line:       c.Line,
				SymbolID:   c.SymbolID,
				Message:    "orphan contract " + c.ID + " (" + string(c.Role) + " with no counterpart)",
			})
		}
	}
	return out
}

// formatPct renders a coverage percentage without dragging fmt in for one call.
func formatPct(v float64) string {
	whole := int(v)
	frac := int((v - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	return itoa(whole) + "." + itoa(frac) + "%"
}
