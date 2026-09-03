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

// registerWakeupTool wires gortex_wakeup — a ~500-token markdown
// codebase digest assembled from the same substrate get_repo_outline
// already exposes (language mix, top communities, hotspots, entry
// points), formatted as paste-ready markdown for users who *can't*
// run MCP at all (web ChatGPT, hosted Codex, raw API).
//
// Same builder also feeds the `gortex wakeup` CLI subcommand so the
// MCP and CLI outputs stay byte-identical.
func (s *Server) registerWakeupTool() {
	s.addTool(
		mcp.NewTool("gortex_wakeup",
			mcp.WithDescription("Paste-ready ~500-token codebase digest. Composes language mix + top communities + load-bearing hotspots + entry points into a single markdown blob the agent can paste into a chat session at startup. Targets users without an MCP transport (web ChatGPT, hosted Codex, raw API). Token cap is approximate — under 600 in typical use."),
			mcp.WithNumber("max_tokens", mcp.Description("Approximate output cap (default: 500). Bytes-per-token heuristic is 4; we trim to that budget after rendering.")),
			mcp.WithNumber("top_communities", mcp.Description("Communities to include (default: 4).")),
			mcp.WithNumber("top_hotspots", mcp.Description("Hotspots to include (default: 5).")),
			mcp.WithNumber("top_entry_points", mcp.Description("Entry points to include (default: 5).")),
			mcp.WithString("format", mcp.Description("Output format: markdown (default — primary use case) or json. JSON wraps the markdown in a {markdown, tokens_est, sections} envelope for callers that want to introspect.")),
		),
		s.handleGortexWakeup,
	)
}

// WakeupOptions controls BuildWakeup output. Exposed so the
// `gortex wakeup` CLI subcommand can reuse the identical renderer
// without duplicating defaults.
type WakeupOptions struct {
	MaxTokens      int
	TopCommunities int
	TopHotspots    int
	TopEntryPoints int
	// PrecomputedHotspots, when non-nil, is the default-threshold
	// hotspot ranking the caller has already paid for. Threaded by
	// the MCP handler from the server-wide cache so the wakeup turn
	// skips a redundant FindHotspots (and its ComputeBetweenness
	// pass). nil means BuildWakeup computes it fresh — the CLI
	// `gortex wakeup` path.
	PrecomputedHotspots []analysis.HotspotEntry
}

// DefaultWakeupOptions returns the defaults the MCP handler uses.
// Pulled out so the CLI subcommand renders the same output.
func DefaultWakeupOptions() WakeupOptions {
	return WakeupOptions{
		MaxTokens:      500,
		TopCommunities: 4,
		TopHotspots:    5,
		TopEntryPoints: 5,
	}
}

// BuildWakeup renders the wakeup digest from a graph + cached
// communities. Returns the markdown body and an approximate token
// count (bytes / 4). Exposed so CLI and MCP paths share one
// implementation.
func BuildWakeup(g graph.Reader, communities *analysis.CommunityResult, opts WakeupOptions) (markdown string, tokensEst int) {
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 500
	}
	if opts.TopCommunities <= 0 {
		opts.TopCommunities = 4
	}
	if opts.TopHotspots <= 0 {
		opts.TopHotspots = 5
	}
	if opts.TopEntryPoints <= 0 {
		opts.TopEntryPoints = 5
	}

	// Wakeup is a whole-repo digest — language tally + hotspot list +
	// entry-point list, with no session scoping. The lang count can
	// come from Stats() (one indexed groupby on disk backends);
	// hotspots and entry points already iterate the function/method
	// subset via the analyzers / NodesByKindsScanner path, so the
	// AllNodes() pull the legacy build used to feed the lang summary
	// just adds a redundant 107k-row trip on a disk backend.
	stats := g.Stats()
	var b strings.Builder
	b.WriteString("# Codebase wakeup\n\n")

	langCounts := map[string]int{}
	for lang, c := range stats.ByLanguage {
		if lang == "" {
			continue
		}
		langCounts[lang] = c
	}
	type langRow struct {
		name  string
		count int
	}
	langs := make([]langRow, 0, len(langCounts))
	for k, v := range langCounts {
		langs = append(langs, langRow{k, v})
	}
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].count != langs[j].count {
			return langs[i].count > langs[j].count
		}
		return langs[i].name < langs[j].name
	})
	topLangs := langs
	if len(topLangs) > 3 {
		topLangs = topLangs[:3]
	}
	langSummary := []string{}
	for _, l := range topLangs {
		langSummary = append(langSummary, fmt.Sprintf("%s (%d)", l.name, l.count))
	}
	fileCount := stats.ByKind[string(graph.KindFile)]
	fmt.Fprintf(&b, "**Scale.** %d indexed symbols across %d files. Primary: %s.\n\n",
		stats.TotalNodes, fileCount, strings.Join(langSummary, ", "))

	// Communities.
	if communities != nil && len(communities.Communities) > 0 {
		comms := append([]analysis.Community(nil), communities.Communities...)
		sort.Slice(comms, func(i, j int) bool { return comms[i].Size > comms[j].Size })
		if len(comms) > opts.TopCommunities {
			comms = comms[:opts.TopCommunities]
		}
		b.WriteString("**Communities.**\n")
		for _, c := range comms {
			label := c.Label
			if label == "" {
				label = c.ID
			}
			hub := ""
			if c.Hub != "" {
				hub = " · hub " + c.Hub
			}
			fmt.Fprintf(&b, "- %s (%d members%s)\n", label, c.Size, hub)
		}
		b.WriteString("\n")
	}

	// Hotspots.
	var hotspots []analysis.HotspotEntry
	if opts.PrecomputedHotspots != nil {
		hotspots = opts.PrecomputedHotspots
	} else {
		hotspots = analysis.FindHotspots(g, communities, 0)
	}
	if len(hotspots) > opts.TopHotspots {
		hotspots = hotspots[:opts.TopHotspots]
	}
	if len(hotspots) > 0 {
		b.WriteString("**Load-bearing symbols.**\n")
		for _, h := range hotspots {
			fmt.Fprintf(&b, "- `%s` (in:%d, out:%d) — %s\n", h.Name, h.FanIn, h.FanOut, h.FilePath)
		}
		b.WriteString("\n")
	}

	// Entry points.
	entries := wakeupEntryPoints(g, opts.TopEntryPoints)
	if len(entries) > 0 {
		b.WriteString("**Entry points.**\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "- `%s` — %s\n", e.Name, e.FilePath)
		}
		b.WriteString("\n")
	}

	out := b.String()
	out = trimToTokens(out, opts.MaxTokens)
	return out, len(out) / 4
}

// wakeupEntryPoints returns functions/methods with zero incoming
// edges and at least one outgoing edge, ranked by out-degree.
//
// Uses NodeDegreeAggregator when the backend implements it (one
// batched in/out count instead of up to 3N GetInEdges/GetOutEdges
// round-trips on a disk backend — the sort path called GetOutEdges
// twice per candidate, the worst single hot spot in this file). We
// stash the fan-out alongside each node so the sort never has to
// re-query.
func wakeupEntryPoints(g graph.Reader, top int) []*graph.Node {
	type entry struct {
		node   *graph.Node
		fanOut int
	}
	// Pull only the callable subset via NodesByKindsScanner so disk
	// backends never materialise the whole node table for an entry-
	// point candidate set that only ranges across function + method.
	var pool []*graph.Node
	if scan, ok := g.(graph.NodesByKindsScanner); ok {
		pool = scan.NodesByKinds([]graph.NodeKind{graph.KindFunction, graph.KindMethod})
	} else {
		all := g.AllNodes()
		pool = make([]*graph.Node, 0, len(all))
		for _, n := range all {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				pool = append(pool, n)
			}
		}
	}
	entries := make([]entry, 0, len(pool))
	if agg, ok := g.(graph.NodeDegreeAggregator); ok && len(pool) > 0 {
		ids := make([]string, 0, len(pool))
		byID := make(map[string]*graph.Node, len(pool))
		for _, n := range pool {
			ids = append(ids, n.ID)
			byID[n.ID] = n
		}
		for _, r := range agg.NodeDegreeCounts(ids, nil) {
			if r.InCount > 0 || r.OutCount == 0 {
				continue
			}
			n := byID[r.NodeID]
			if n == nil {
				continue
			}
			entries = append(entries, entry{node: n, fanOut: r.OutCount})
		}
	} else {
		for _, n := range pool {
			if len(g.GetInEdges(n.ID)) > 0 {
				continue
			}
			out := len(g.GetOutEdges(n.ID))
			if out == 0 {
				continue
			}
			entries = append(entries, entry{node: n, fanOut: out})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].fanOut != entries[j].fanOut {
			return entries[i].fanOut > entries[j].fanOut
		}
		return entries[i].node.ID < entries[j].node.ID
	})
	if len(entries) > top {
		entries = entries[:top]
	}
	out := make([]*graph.Node, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.node)
	}
	return out
}

// trimToTokens caps the markdown to the requested approximate token
// budget. Heuristic: 4 bytes per token. Trims at a line boundary so
// the cut is visually clean.
func trimToTokens(s string, maxTokens int) string {
	limitBytes := maxTokens * 4
	if len(s) <= limitBytes {
		return s
	}
	cut := s[:limitBytes]
	if idx := strings.LastIndex(cut, "\n"); idx > limitBytes/2 {
		cut = cut[:idx]
	}
	return cut + "\n\n_… digest truncated to fit token budget …_\n"
}

func (s *Server) handleGortexWakeup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if w := s.warmupSnapshot(); w.enrichmentIncomplete() {
		return enrichmentPendingToolResult(w), nil
	}

	opts := DefaultWakeupOptions()
	if v := req.GetInt("max_tokens", 0); v > 0 {
		opts.MaxTokens = v
	}
	if v := req.GetInt("top_communities", 0); v > 0 {
		opts.TopCommunities = v
	}
	if v := req.GetInt("top_hotspots", 0); v > 0 {
		opts.TopHotspots = v
	}
	if v := req.GetInt("top_entry_points", 0); v > 0 {
		opts.TopEntryPoints = v
	}

	opts.PrecomputedHotspots = s.getHotspots()
	md, est := BuildWakeup(s.readerFor(ctx), s.getCommunities(), opts)

	format := strings.ToLower(strings.TrimSpace(req.GetString("format", "markdown")))
	if format == "markdown" || format == "" {
		return mcp.NewToolResultText(md), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"markdown":   md,
		"tokens_est": est,
	})
}
