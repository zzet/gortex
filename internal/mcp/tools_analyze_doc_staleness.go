package mcp

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// docStalenessLink is one content/rationale -> code link and its assessed state.
type docStalenessLink struct {
	Symbol string `json:"symbol"`
	State  string `json:"state"` // dangling | pending | live
}

// docStalenessRow rolls up one knowledge source (a content chunk or rationale
// node) and the staleness of the symbols it motivates.
type docStalenessRow struct {
	Source     string             `json:"source"`
	File       string             `json:"file,omitempty"`
	Name       string             `json:"name,omitempty"`
	WorstState string             `json:"worst_state"`
	Dangling   int                `json:"dangling"`
	Pending    int                `json:"pending"`
	TotalRefs  int                `json:"total_refs"`
	Links      []docStalenessLink `json:"links,omitempty"`
}

// docStalenessResult is the advisory rollup: knowledge sources that reference
// code which no longer exists (dangling) or isn't indexed yet (pending).
type docStalenessResult struct {
	Stale         []docStalenessRow `json:"stale"`
	AssessedLinks int               `json:"assessed_links"`
	Note          string            `json:"note,omitempty"`
}

// analyzeDocStaleness walks every EdgeMotivates and flags the knowledge sources
// whose code references have gone stale. It is deterministic and zero-FP by
// design: a "dangling" link names a symbol that is genuinely absent from the
// graph, a "pending" link points at an unresolved (e.g. cross-repo, not-yet-
// indexed) target. Timestamp / signature-materiality drift (a doc that predates
// a code change) is a future, blame-gated enhancement; this signal needs no git
// history and never false-positives.
func analyzeDocStaleness(g graph.Reader, limit int) docStalenessResult {
	type acc struct {
		row   docStalenessRow
		links []docStalenessLink
	}
	bySource := map[string]*acc{}
	assessed := 0
	for _, e := range g.AllEdges() {
		if e == nil || e.Kind != graph.EdgeMotivates {
			continue
		}
		assessed++
		state := "live"
		if g.GetNode(e.To) == nil {
			if graph.IsUnresolvedTarget(e.To) {
				state = "pending"
			} else {
				state = "dangling"
			}
		}
		a := bySource[e.From]
		if a == nil {
			a = &acc{row: docStalenessRow{Source: e.From}}
			if n := g.GetNode(e.From); n != nil {
				a.row.File = n.FilePath
				a.row.Name = n.Name
			}
			bySource[e.From] = a
		}
		a.row.TotalRefs++
		switch state {
		case "dangling":
			a.row.Dangling++
		case "pending":
			a.row.Pending++
		}
		a.links = append(a.links, docStalenessLink{Symbol: e.To, State: state})
	}

	var stale []docStalenessRow
	for _, a := range bySource {
		if a.row.Dangling == 0 && a.row.Pending == 0 {
			continue
		}
		if a.row.Dangling > 0 {
			a.row.WorstState = "dangling"
		} else {
			a.row.WorstState = "pending"
		}
		a.row.Links = a.links
		stale = append(stale, a.row)
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Dangling != stale[j].Dangling {
			return stale[i].Dangling > stale[j].Dangling
		}
		if stale[i].Pending != stale[j].Pending {
			return stale[i].Pending > stale[j].Pending
		}
		return stale[i].Source < stale[j].Source
	})
	if limit > 0 && len(stale) > limit {
		stale = stale[:limit]
	}
	res := docStalenessResult{Stale: stale, AssessedLinks: assessed}
	if assessed == 0 {
		res.Note = "No content->code links in the graph yet. Index a repo whose documents (ADRs, specs, slides) reference code, or record decisions with store_memory."
	}
	return res
}

func (s *Server) handleAnalyzeDocStaleness(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// One reader for the walk and for every visibility check below: a
	// link is "dangling" only when the symbol is absent from the state
	// this request reads, buffers included.
	reader := s.readerFor(ctx)
	res := analyzeDocStaleness(reader, req.GetInt("limit", 50))
	if s.scopeFiltersActive(ctx) {
		// Narrow to the session workspace + optional repo allow-set.
		// Keep a row iff its Source (the keyed knowledge node) is visible.
		// Within a kept row, prune only RESOLVED ("live") links whose
		// target falls out of scope: dangling/pending links point at
		// genuinely-absent nodes (GetNode==nil ⇒ analyzeNodeVisible==false)
		// and carry the analyzer's entire signal, so they are NEVER
		// dropped. Per-row counts and the global assessed-links tally
		// recompute from the in-scope data. Unbound requests skip this
		// branch — a byte-for-byte no-op.
		kept := make([]docStalenessRow, 0, len(res.Stale))
		for _, row := range res.Stale {
			if !s.analyzeNodeVisible(ctx, reader.GetNode(row.Source)) {
				continue
			}
			if len(row.Links) > 0 {
				links := make([]docStalenessLink, 0, len(row.Links))
				dangling, pending, total := 0, 0, 0
				for _, l := range row.Links {
					if l.State == "live" && !s.analyzeNodeVisible(ctx, reader.GetNode(l.Symbol)) {
						continue
					}
					links = append(links, l)
					total++
					switch l.State {
					case "dangling":
						dangling++
					case "pending":
						pending++
					}
				}
				row.Links = links
				row.Dangling = dangling
				row.Pending = pending
				row.TotalRefs = total
			}
			kept = append(kept, row)
		}
		res.Stale = kept

		// Recompute assessed_links as the in-scope motivates edges —
		// those whose knowledge source node is visible.
		assessed := 0
		for _, e := range reader.AllEdges() {
			if e == nil || e.Kind != graph.EdgeMotivates {
				continue
			}
			if s.analyzeNodeVisible(ctx, reader.GetNode(e.From)) {
				assessed++
			}
		}
		res.AssessedLinks = assessed
	}
	return s.respondJSONOrTOON(ctx, req, res)
}
