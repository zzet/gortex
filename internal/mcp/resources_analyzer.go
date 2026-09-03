package mcp

import (
	"context"
	"os"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/audit"
	"github.com/zzet/gortex/internal/graph"
)

// registerAnalyzerResources surfaces long-form rollups whose only
// "argument" is the current state of the indexed code. Each is a
// read-only rollup the agent fetches by URI; on graph re-warm the
// resource broadcaster pushes `notifications/resources/updated` so
// subscribed clients can refresh without polling.
//
// The rollups intentionally pre-bake a small, opinionated slice
// (top-N, summary counts) — agents that need the full analyzer output
// fall back to the corresponding `analyze` tool invocation.
func (s *Server) registerAnalyzerResources() {
	s.addResource(
		mcp.NewResource(
			"gortex://report",
			"Orientation Report",
			mcp.WithResourceDescription("High-level codebase rollup: graph size, top languages, hotspot count, community count, dead-code count, todo count. Read at session start as a single fetch instead of N separate tool calls."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceReport,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://god-nodes",
			"God Nodes",
			mcp.WithResourceDescription("Top symbols by complexity score (fan-in*2 + fan-out*1.5 + community-crossings*3, normalized 0-100). Use to spot over-coupled functions that need decomposition. Subset of `analyze kind:hotspots`."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceGodNodes,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://surprises",
			"Codebase Surprises",
			mcp.WithResourceDescription("Anomalies worth investigating: import cycles, unreachable symbols, and the largest cross-community call hubs. Synthesises `analyze cycles`, `analyze dead_code`, and `analyze hotspots` into one rollup."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceSurprises,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://audit",
			"Agent Config Audit",
			mcp.WithResourceDescription("Stale symbol refs, dead file paths, and bloat scores in CLAUDE.md / AGENTS.md / Cursor rules / Copilot / Windsurf instruction files. Same payload as the `audit_agent_config` tool with discovery defaults."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceAudit,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://questions",
			"Open Questions",
			mcp.WithResourceDescription("Aggregated TODO / FIXME / XXX / HACK / QUESTION comments grouped by tag and assignee. Same payload as `analyze kind:todos` with stable file:line ordering."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceQuestions,
	)
}

func (s *Server) handleResourceReport(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	// scopedNodes confines the report to the session's workspace; for
	// an unbound session it is every node, so the rollup matches the
	// legacy global view. inScope bounds the analyzer- and edge-driven
	// counts; nil for an unbound session.
	scoped := s.scopedNodesLight(ctx)
	_, _, bound := s.sessionScope(ctx)
	var inScope map[string]bool
	if bound {
		inScope = make(map[string]bool, len(scoped))
		for _, n := range scoped {
			inScope[n.ID] = true
		}
	}

	byLang := make(map[string]int)
	byKind := make(map[string]int)
	var todoCount int
	for _, n := range scoped {
		if n.Language != "" {
			byLang[n.Language]++
		}
		byKind[string(n.Kind)]++
		if n.Kind == graph.KindTodo {
			todoCount++
		}
	}
	g := s.readerFor(ctx)
	totalEdges := 0
	for _, e := range g.AllEdges() {
		if inScope != nil && (!inScope[e.From] || !inScope[e.To]) {
			continue
		}
		totalEdges++
	}

	topLangs := topNFromCountMap(byLang, 5)
	topKinds := topNFromCountMap(byKind, 5)

	var hotspotCount int
	if len(scoped) >= 10 {
		for _, h := range s.getHotspots() {
			if inScope == nil || inScope[h.ID] {
				hotspotCount++
			}
		}
	}

	deadCount := 0
	for _, d := range analysis.FindDeadCode(g, s.getProcesses(), nil) {
		if inScope == nil || inScope[d.ID] {
			deadCount++
		}
	}

	commCount := 0
	if c := s.getCommunities(); c != nil {
		for _, com := range c.Communities {
			if inScope == nil {
				commCount++
				continue
			}
			for _, m := range com.Members {
				if inScope[m] {
					commCount++
					break
				}
			}
		}
	}

	procCount := 0
	if p := s.getProcesses(); p != nil {
		for _, proc := range p.Processes {
			if inScope == nil || inScope[proc.EntryPoint] {
				procCount++
			}
		}
	}

	return jsonResource(req.Params.URI, map[string]any{
		"total_nodes":   len(scoped),
		"total_edges":   totalEdges,
		"top_languages": topLangs,
		"top_kinds":     topKinds,
		"communities":   commCount,
		"processes":     procCount,
		"hotspots":      hotspotCount,
		"dead_code":     deadCount,
		"open_todos":    todoCount,
	})
}

func (s *Server) handleResourceGodNodes(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	if s.readerFor(ctx).NodeCount() < 10 {
		return jsonResource(req.Params.URI, map[string]any{
			"god_nodes": []any{},
			"message":   "codebase too small for meaningful hotspot analysis (need at least 10 symbols)",
		})
	}

	entries := s.getHotspots()
	totalCount := len(entries)
	truncated := false
	if len(entries) > 20 {
		entries = entries[:20]
		truncated = true
	}

	return jsonResource(req.Params.URI, map[string]any{
		"god_nodes": entries,
		"total":     totalCount,
		"truncated": truncated,
	})
}

func (s *Server) handleResourceSurprises(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	communities := s.getCommunities()
	g := s.readerFor(ctx)

	cycles := analysis.DetectCycles(g, communities, "")
	if len(cycles) > 10 {
		cycles = cycles[:10]
	}

	deadAll := analysis.FindDeadCode(g, s.getProcesses(), nil)
	deadTrunc := false
	if len(deadAll) > 20 {
		deadAll = deadAll[:20]
		deadTrunc = true
	}

	var topHubs []analysis.HotspotEntry
	if g.NodeCount() >= 10 {
		hot := s.getHotspots()
		// Top hubs == hotspots with at least one community crossing.
		for _, h := range hot {
			if h.CommunityCrossings > 0 {
				topHubs = append(topHubs, h)
			}
			if len(topHubs) == 10 {
				break
			}
		}
	}

	return jsonResource(req.Params.URI, map[string]any{
		"cycles":            cycles,
		"dead_code":         deadAll,
		"dead_code_truncated": deadTrunc,
		"cross_community_hubs": topHubs,
	})
}

func (s *Server) handleResourceAudit(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	// Match the tool's fallback chain. Resources can't take args, so
	// skip the explicit-arg step and start from the indexer root.
	root := ""
	if s.indexer != nil {
		root = s.indexer.RootPath()
	}
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root == "" {
		return jsonResource(req.Params.URI, map[string]any{
			"error": "could not determine repo root for audit",
		})
	}

	files := audit.DiscoverConfigFiles(root)
	if len(files) == 0 {
		return jsonResource(req.Params.URI, map[string]any{
			"files_scanned": 0,
			"message":       "no agent config files found",
			"root":          root,
		})
	}

	return jsonResource(req.Params.URI, audit.Audit(s.readerFor(ctx), root, files))
}

func (s *Server) handleResourceQuestions(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	type questionRow struct {
		ID       string `json:"id"`
		Tag      string `json:"tag"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Assignee string `json:"assignee,omitempty"`
		Due      string `json:"due,omitempty"`
		Ticket   string `json:"ticket,omitempty"`
		Text     string `json:"text,omitempty"`
	}

	var rows []questionRow
	byTag := make(map[string]int)
	withAssignee := 0
	// scopedNodes confines the TODO rollup to the session's workspace.
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindTodo {
			continue
		}
		tag, _ := n.Meta["tag"].(string)
		assignee, _ := n.Meta["assignee"].(string)
		ticket, _ := n.Meta["ticket"].(string)
		due, _ := n.Meta["due"].(string)
		text, _ := n.Meta["text"].(string)

		rows = append(rows, questionRow{
			ID:       n.ID,
			Tag:      tag,
			File:     n.FilePath,
			Line:     n.StartLine,
			Assignee: assignee,
			Due:      due,
			Ticket:   ticket,
			Text:     text,
		})
		byTag[tag]++
		if assignee != "" {
			withAssignee++
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	return jsonResource(req.Params.URI, map[string]any{
		"questions":     rows,
		"total":         len(rows),
		"by_tag":        byTag,
		"with_assignee": withAssignee,
	})
}

// topNFromCountMap returns the largest n entries from a string→int
// counter, sorted by count descending then key ascending so the report
// is deterministic across calls.
func topNFromCountMap(m map[string]int, n int) []map[string]any {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]map[string]any, 0, len(all))
	for _, p := range all {
		out = append(out, map[string]any{
			"name":  p.k,
			"count": p.v,
		})
	}
	return out
}

