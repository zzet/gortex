package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// whyEntry is one knowledge source that motivates the queried symbol.
type whyEntry struct {
	SourceID      string `json:"source_id"`
	Kind          string `json:"kind"` // rationale | content
	RationaleKind string `json:"rationale_kind,omitempty"`
	AssetKind     string `json:"asset_kind,omitempty"`
	Name          string `json:"name,omitempty"`
	Text          string `json:"text,omitempty"`
	Signal        string `json:"signal,omitempty"`
}

// whyResult is the why-query response: the rationale that motivates a symbol.
type whyResult struct {
	Symbol    string     `json:"symbol"`
	Count     int        `json:"count"`
	Rationale []whyEntry `json:"rationale"`
	Note      string     `json:"note,omitempty"`
}

func (s *Server) registerWhyTool() {
	s.addTool(
		mcp.NewTool("why",
			mcp.WithDescription("Explain WHY a code symbol exists: a graph-first, one-hop walk over the rationale that motivates it — projected store_memory decisions / incidents and the content documents (ADRs, specs, slides) whose text names it. Use to recover the reasoning behind code before changing it. Curated rationale ranks before lexical content links."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID or name to explain.")),
			mcp.WithNumber("limit", mcp.Description("Max rationale sources (default: 20).")),
			mcp.WithString("scope", mcp.Description("Memory scope to reconcile on read: workspace (default) or global.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleWhy,
	)
}

func (s *Server) handleWhy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	id, candidates := s.resolveSymbolTarget(ctx, target)
	if len(candidates) > 0 {
		return s.symbolDisambiguationResult(ctx, req, "why", target, candidates)
	}
	// Reconcile-on-read: project any memories that predate the daemon so the
	// graph-first traversal below sees them.
	s.reconcileRationale(strings.TrimSpace(req.GetString("scope", "")))

	entries := s.whyEntriesFor(ctx, id)
	if limit := req.GetInt("limit", 20); limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	res := whyResult{Symbol: id, Count: len(entries), Rationale: entries}
	if len(entries) == 0 {
		res.Note = "No recorded rationale links this symbol. Try search_symbols corpus:content for related documents, or store_memory to record the decision."
	}
	return s.respondJSONOrTOON(ctx, req, res)
}

// whyEntriesFor walks the incoming EdgeMotivates edges of a symbol and returns
// the knowledge sources that motivate it — projected rationale nodes and
// content chunks — ranked curated-rationale-first. One graph hop, no fan-out.
func (s *Server) whyEntriesFor(ctx context.Context, id string) []whyEntry {
	var entries []whyEntry
	seen := map[string]bool{}
	reader := s.readerFor(ctx)
	for _, e := range s.engineFor(ctx).GetInEdges(id) {
		if e == nil || e.Kind != graph.EdgeMotivates || seen[e.From] {
			continue
		}
		seen[e.From] = true
		n := reader.GetNode(e.From)
		if n == nil {
			continue
		}
		entry := whyEntry{SourceID: e.From, Name: n.Name, Signal: whyMetaStr(e.Meta, "signal")}
		switch n.Kind {
		case graph.KindRationale:
			entry.Kind = "rationale"
			entry.RationaleKind = whyMetaStr(n.Meta, "rationale_kind")
			entry.Text = whyMetaStr(n.Meta, "section_text")
		case graph.KindDoc:
			entry.Kind = "content"
			entry.AssetKind = whyMetaStr(n.Meta, "asset_kind")
			entry.Text = whyMetaStr(n.Meta, "section_text")
		default:
			entry.Kind = string(n.Kind)
		}
		entries = append(entries, entry)
	}
	// Curated rationale (a projected memory) outranks a lexical content match.
	sort.SliceStable(entries, func(i, j int) bool { return whyRank(entries[i]) < whyRank(entries[j]) })
	return entries
}

// whyRank orders rationale (curated memory) before content (lexical match).
func whyRank(e whyEntry) int {
	if e.Kind == "rationale" {
		return 0
	}
	return 1
}

func whyMetaStr(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}
