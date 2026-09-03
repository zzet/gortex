package mcp

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// registerReplayEpisodeTool wires replay_episode — the incident
// investigation aid. From an anchor symbol (where the bug surfaced),
// composes a four-section postmortem-ready response: recent edits
// in the blast radius, the call graph context, coverage gaps, and
// any memories tagged "incident" that anchor to touched symbols.
func (s *Server) registerReplayEpisodeTool() {
	s.addTool(
		mcp.NewTool("replay_episode",
			mcp.WithDescription("Walk from a symptom symbol back through the substrate that explains why it broke. Returns a timeline of recent edits in the blast radius, the immediate callers, coverage gaps among the touched symbols, and any incident-tagged memories. Composes get_callers + meta.{last_commit_at, last_author, coverage_pct} + the session symbol history + the memory store — no LLM. Use to reconstruct a story for a postmortem or to narrow which recent change likely caused an alert."),
			mcp.WithString("anchor_symbol", mcp.Description("Symbol ID where the issue manifested (e.g. pkg/handler.go::Handle). The replay walks callers depth-first from here.")),
			mcp.WithNumber("window_days", mcp.Description("Only include edits whose meta.last_commit_at is within this many days (default: 30). Set to 0 to disable the time filter.")),
			mcp.WithNumber("depth", mcp.Description("Caller traversal depth for the blast-radius walk (default: 3).")),
			mcp.WithNumber("limit", mcp.Description("Cap per section — timeline / callers / coverage_gaps / memories (default: 25 each).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleReplayEpisode,
	)
}

type replayTimelineRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	FilePath      string `json:"file_path"`
	LastCommitAt  string `json:"last_commit_at,omitempty"`
	LastAuthor    string `json:"last_author,omitempty"`
	SessionEdits  int    `json:"session_edits,omitempty"`
	SignatureFlux bool   `json:"signature_flux,omitempty"`
}

type replayCallerRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
	Depth    int    `json:"depth"`
}

type replayCoverageRow struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	FilePath    string  `json:"file_path"`
	CoveragePct float64 `json:"coverage_pct"`
	HasCoverage bool    `json:"has_coverage"`
}

type replayMemoryRow struct {
	ID        string   `json:"id"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body"`
	Kind      string   `json:"kind,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

func (s *Server) handleReplayEpisode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	anchor := strings.TrimSpace(req.GetString("anchor_symbol", ""))
	if anchor == "" {
		return mcp.NewToolResultError("replay_episode requires `anchor_symbol`"), nil
	}
	depth := max(req.GetInt("depth", 3), 1)
	windowDays := req.GetInt("window_days", 30)
	limit := max(req.GetInt("limit", 25), 1)

	anchorNode := s.readerFor(ctx).GetNode(anchor)
	if anchorNode == nil {
		return mcp.NewToolResultError("anchor symbol not found: " + anchor), nil
	}

	// Build the blast radius: anchor + every caller within `depth`.
	// Reuses the graph's edge index — no recursion into all-callers
	// helpers so the implementation is self-contained and easy to
	// reason about.
	radius := s.walkCallers(ctx, anchor, depth)
	radius[anchor] = 0

	timeline := s.replayTimeline(ctx, radius, windowDays, limit)
	callers := s.replayCallers(ctx, radius, anchor, limit)
	coverage := s.replayCoverageGaps(ctx, radius, limit)
	memories := s.replayMemories(radius, limit)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"anchor": map[string]any{
			"id":        anchorNode.ID,
			"name":      anchorNode.Name,
			"file_path": anchorNode.FilePath,
		},
		"window_days":   windowDays,
		"depth":         depth,
		"radius_size":   len(radius),
		"timeline":      timeline,
		"callers":       callers,
		"coverage_gaps": coverage,
		"memories":      memories,
	})
}

// walkCallers performs a bounded BFS over EdgeCalls (and dispatch
// kinds where applicable) and returns the set of caller IDs keyed
// by traversal depth from the anchor. Depth=1 are direct callers.
func (s *Server) walkCallers(ctx context.Context, anchor string, maxDepth int) map[string]int {
	g := s.readerFor(ctx)
	radius := map[string]int{}
	frontier := []string{anchor}
	depth := 0
	for len(frontier) > 0 && depth < maxDepth {
		depth++
		next := make([]string, 0, len(frontier)*4)
		for _, id := range frontier {
			for _, e := range g.GetInEdges(id) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeImplements && e.Kind != graph.EdgeExtends {
					continue
				}
				if _, seen := radius[e.From]; seen {
					continue
				}
				radius[e.From] = depth
				next = append(next, e.From)
			}
		}
		frontier = next
	}
	return radius
}

func (s *Server) replayTimeline(ctx context.Context, radius map[string]int, windowDays, limit int) []replayTimelineRow {
	cutoff := time.Time{}
	if windowDays > 0 {
		cutoff = time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour)
	}
	// Batch-fetch every node in the radius; the radius is the BFS
	// frontier (often hundreds of IDs), and per-id GetNode on a disk
	// backend would issue that many round-trips per replay call.
	ids := make([]string, 0, len(radius))
	for id := range radius {
		ids = append(ids, id)
	}
	nodeByID := s.readerFor(ctx).GetNodesByIDs(ids)
	rows := make([]replayTimelineRow, 0, len(radius))
	for id := range radius {
		n := nodeByID[id]
		if n == nil {
			continue
		}
		row := replayTimelineRow{ID: n.ID, Name: n.Name, FilePath: n.FilePath}

		// meta.last_commit_at — RFC3339 string, falls back to nothing.
		if ts, ok := n.Meta["last_commit_at"].(string); ok {
			t, err := time.Parse(time.RFC3339, ts)
			if err == nil && (windowDays == 0 || !t.Before(cutoff)) {
				row.LastCommitAt = ts
			} else if err != nil || (!cutoff.IsZero() && t.Before(cutoff)) {
				// Outside the window: drop the row entirely unless
				// there's still session activity to report.
				if s.symHistory != nil && len(s.symHistory.Get(id)) > 0 {
					row.LastCommitAt = ts
				} else {
					continue
				}
			}
		} else if s.symHistory == nil || len(s.symHistory.Get(id)) == 0 {
			// No blame data and no session edits — nothing to show.
			continue
		}

		if author, ok := n.Meta["last_author"].(string); ok {
			row.LastAuthor = author
		}
		if s.symHistory != nil {
			mods := s.symHistory.Get(id)
			row.SessionEdits = len(mods)
			for _, m := range mods {
				if m.SignatureChanged {
					row.SignatureFlux = true
					break
				}
			}
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		// Newest first.
		if rows[i].LastCommitAt != rows[j].LastCommitAt {
			return rows[i].LastCommitAt > rows[j].LastCommitAt
		}
		if rows[i].SessionEdits != rows[j].SessionEdits {
			return rows[i].SessionEdits > rows[j].SessionEdits
		}
		return rows[i].ID < rows[j].ID
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func (s *Server) replayCallers(ctx context.Context, radius map[string]int, anchor string, limit int) []replayCallerRow {
	// Batch-fetch the radius minus the anchor; same rationale as
	// replayTimeline — per-id GetNode on a disk backend costs one
	// round-trip per BFS node.
	ids := make([]string, 0, len(radius))
	for id := range radius {
		if id == anchor {
			continue
		}
		ids = append(ids, id)
	}
	nodeByID := s.readerFor(ctx).GetNodesByIDs(ids)
	rows := make([]replayCallerRow, 0, len(radius))
	for id, d := range radius {
		if id == anchor {
			continue
		}
		n := nodeByID[id]
		if n == nil {
			continue
		}
		rows = append(rows, replayCallerRow{
			ID:       n.ID,
			Name:     n.Name,
			FilePath: n.FilePath,
			Depth:    d,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Depth != rows[j].Depth {
			return rows[i].Depth < rows[j].Depth
		}
		return rows[i].ID < rows[j].ID
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func (s *Server) replayCoverageGaps(ctx context.Context, radius map[string]int, limit int) []replayCoverageRow {
	// Batch-fetch the radius — same rationale as replayTimeline.
	ids := make([]string, 0, len(radius))
	for id := range radius {
		ids = append(ids, id)
	}
	nodeByID := s.readerFor(ctx).GetNodesByIDs(ids)
	covRows := s.coverageByID()
	rows := make([]replayCoverageRow, 0)
	for id := range radius {
		n := nodeByID[id]
		if n == nil {
			continue
		}
		pct, has := coveragePctFrom(covRows, n)
		if has && pct >= 100.0 {
			continue
		}
		rows = append(rows, replayCoverageRow{
			ID: n.ID, Name: n.Name, FilePath: n.FilePath,
			CoveragePct: pct, HasCoverage: has,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		// Lowest coverage first; nodes with no coverage sort to the bottom
		// so the agent prioritises measurable risk.
		if rows[i].HasCoverage != rows[j].HasCoverage {
			return rows[i].HasCoverage && !rows[j].HasCoverage
		}
		if rows[i].HasCoverage && rows[i].CoveragePct != rows[j].CoveragePct {
			return rows[i].CoveragePct < rows[j].CoveragePct
		}
		return rows[i].ID < rows[j].ID
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// replayMemories returns incident-tagged memories whose anchor
// symbols overlap with the radius. If the memory store isn't
// initialised, the section is empty rather than failing the call.
func (s *Server) replayMemories(radius map[string]int, limit int) []replayMemoryRow {
	rows := make([]replayMemoryRow, 0)
	if s.memories == nil {
		return rows
	}
	// Query once for each kind of anchor: by symbol over the radius.
	// We need anything tagged 'incident' OR whose kind is 'incident' that
	// anchors to a radius member. Query supports either filter, but
	// not both at once — issue two pulls and dedupe.
	seen := map[string]bool{}
	collect := func(filter MemoryQueryFilter) {
		for _, m := range s.memories.Query(filter) {
			if seen[m.ID] {
				continue
			}
			// Anchor overlap: at least one of the memory's anchor
			// symbols lies in the radius.
			match := false
			for _, sid := range m.SymbolIDs {
				if _, ok := radius[sid]; ok {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			seen[m.ID] = true
			body := m.Body
			if len(body) > 400 {
				body = body[:400] + "…"
			}
			rows = append(rows, replayMemoryRow{
				ID:        m.ID,
				Title:     m.Title,
				Body:      body,
				Kind:      m.Kind,
				Tags:      m.Tags,
				UpdatedAt: m.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
	}
	collect(MemoryQueryFilter{Kind: "incident", Limit: 200})
	collect(MemoryQueryFilter{Tag: "incident", Limit: 200})

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].UpdatedAt > rows[j].UpdatedAt
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}
