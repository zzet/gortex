package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// navCursor is the per-session stateful navigation cursor. It tracks a
// current symbol and a back-history so the nav tool can move through the
// graph one hop at a time without the caller re-passing the symbol ID.
type navCursor struct {
	mu      sync.Mutex
	current string   // current symbol ID; "" when the cursor is unset
	history []string // previous positions, oldest first; back pops the tail
}

// cursorFor returns the calling session's navigation cursor, allocating
// it on first use. Mirrors responseBufferFor — the cursor is bound to
// the MCP session lifecycle and freed with the rest of sessionState on
// disconnect.
func (s *Server) cursorFor(ctx context.Context) *navCursor {
	sess := s.sessionFor(ctx)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cursor == nil {
		sess.cursor = &navCursor{}
	}
	return sess.cursor
}

// registerNavTool wires nav — a verb-dispatched stateful navigation
// tool. One tool with an `action` selector (the same shape as analyze's
// `kind` dispatcher) moves a per-session cursor through the graph:
// goto / into / up / sibling / back / where / read.
func (s *Server) registerNavTool() {
	s.addTool(
		mcp.NewTool("nav",
			mcp.WithDescription("Cursor-based stateful graph navigation. Holds a per-session cursor so you can move one hop at a time without re-passing the symbol ID. Actions:\n"+
				"  goto    — set the cursor to `id`\n"+
				"  into    — move to a callee of the current symbol\n"+
				"  up      — move to a caller of the current symbol\n"+
				"  sibling — move to another member of the current symbol's parent\n"+
				"  back    — pop the previous position off the history\n"+
				"  where   — report the current symbol without moving\n"+
				"  read    — return the current symbol's source\n"+
				"Every response echoes the new cursor and an adjacency preview (callee / caller / sibling counts). For into / up / sibling, `select` picks which neighbour (numeric index or a name substring; default first)."),
			mcp.WithString("action", mcp.Required(), mcp.Description("One of: goto, into, up, sibling, back, where, read.")),
			mcp.WithString("id", mcp.Description("Target symbol node ID — required for action=goto.")),
			mcp.WithString("select", mcp.Description("For into / up / sibling: which neighbour to move to — a 0-based numeric index or a name substring. Defaults to the first neighbour.")),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon.")),
		),
		s.handleNav,
	)
}

// handleNav dispatches a nav action against the session cursor.
func (s *Server) handleNav(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "")))
	if action == "" {
		return mcp.NewToolResultError("nav: action is required (goto, into, up, sibling, back, where, read)"), nil
	}

	eng := s.engineFor(ctx)
	cur := s.cursorFor(ctx)
	cur.mu.Lock()
	defer cur.mu.Unlock()

	// Stale-cursor guard: a re-index may have removed the node the
	// cursor points at. Detect it once, up front, and reset gracefully
	// so no action operates on a dangling ID.
	staleReset := false
	if cur.current != "" && eng.GetSymbol(cur.current) == nil {
		cur.current = ""
		cur.history = nil
		staleReset = true
	}

	switch action {
	case "goto":
		id, err := req.RequireString("id")
		if err != nil || strings.TrimSpace(id) == "" {
			return mcp.NewToolResultError("nav goto: id is required"), nil
		}
		if eng.GetSymbol(id) == nil {
			return mcp.NewToolResultError("nav goto: symbol not found: " + id), nil
		}
		if cur.current != "" {
			cur.history = append(cur.history, cur.current)
		}
		cur.current = id
		return s.navRespond(ctx, req, eng, cur, "goto", staleReset, "")

	case "into":
		return s.navMove(ctx, req, eng, cur, "into", staleReset)

	case "up":
		return s.navMove(ctx, req, eng, cur, "up", staleReset)

	case "sibling":
		return s.navMove(ctx, req, eng, cur, "sibling", staleReset)

	case "back":
		if len(cur.history) == 0 {
			return mcp.NewToolResultError("nav back: history is empty"), nil
		}
		prev := cur.history[len(cur.history)-1]
		cur.history = cur.history[:len(cur.history)-1]
		// Skip history entries that a re-index has since removed.
		for eng.GetSymbol(prev) == nil && len(cur.history) > 0 {
			prev = cur.history[len(cur.history)-1]
			cur.history = cur.history[:len(cur.history)-1]
		}
		if eng.GetSymbol(prev) == nil {
			cur.current = ""
			return mcp.NewToolResultError("nav back: no live position left in history"), nil
		}
		cur.current = prev
		return s.navRespond(ctx, req, eng, cur, "back", staleReset, "")

	case "where":
		if cur.current == "" {
			return mcp.NewToolResultError("nav where: cursor is unset — use action=goto first"), nil
		}
		return s.navRespond(ctx, req, eng, cur, "where", staleReset, "")

	case "read":
		if cur.current == "" {
			return mcp.NewToolResultError("nav read: cursor is unset — use action=goto first"), nil
		}
		return s.navRead(ctx, req, eng, cur, staleReset)

	default:
		return mcp.NewToolResultError("nav: unknown action " + action +
			" (expected goto, into, up, sibling, back, where, read)"), nil
	}
}

// navMove handles the into / up / sibling actions: it builds the
// candidate neighbour list, picks one via the `select` disambiguator,
// and advances the cursor.
func (s *Server) navMove(ctx context.Context, req mcp.CallToolRequest, eng engineLike, cur *navCursor, action string, staleReset bool) (*mcp.CallToolResult, error) {
	if cur.current == "" {
		return mcp.NewToolResultError("nav " + action + ": cursor is unset — use action=goto first"), nil
	}

	var candidates []*graph.Node
	switch action {
	case "into":
		candidates = navCallees(eng, cur.current)
	case "up":
		candidates = navCallers(eng, cur.current)
	case "sibling":
		candidates = navSiblings(eng, cur.current)
	}
	if len(candidates) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("nav %s: no %s available from %s",
			action, navNeighbourNoun(action), cur.current)), nil
	}

	pick, perr := navSelect(candidates, req.GetString("select", ""))
	if perr != nil {
		return mcp.NewToolResultError("nav " + action + ": " + perr.Error()), nil
	}

	cur.history = append(cur.history, cur.current)
	cur.current = pick.ID
	return s.navRespond(ctx, req, eng, cur, action, staleReset, "")
}

// navSelect resolves the `select` disambiguator against a candidate
// list. An empty selector picks the first candidate. A numeric selector
// is a 0-based index. Any other value is a case-insensitive name
// substring; the first match wins (candidates are already deterministically
// ordered by their producers).
func navSelect(candidates []*graph.Node, selector string) (*graph.Node, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return candidates[0], nil
	}
	if idx, err := strconv.Atoi(selector); err == nil {
		if idx < 0 || idx >= len(candidates) {
			return nil, fmt.Errorf("select index %d out of range (have %d)", idx, len(candidates))
		}
		return candidates[idx], nil
	}
	lower := strings.ToLower(selector)
	for _, c := range candidates {
		if strings.Contains(strings.ToLower(c.Name), lower) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("select %q matched none of the %d candidates", selector, len(candidates))
}

// navNeighbourNoun maps an action to the noun used in its error text.
func navNeighbourNoun(action string) string {
	switch action {
	case "into":
		return "callees"
	case "up":
		return "callers"
	case "sibling":
		return "siblings"
	}
	return "neighbours"
}

// engineLike is the slice of *query.Engine the nav helpers need. Keeping
// it as an interface lets the helpers stay decoupled from the concrete
// engine and keeps them trivially unit-testable.
type engineLike interface {
	GetSymbol(id string) *graph.Node
	GetOutEdges(id string) []*graph.Edge
	GetInEdges(id string) []*graph.Edge
}

// navCallees returns the distinct call targets of id, ordered by edge
// line so the list is deterministic across calls.
func navCallees(eng engineLike, id string) []*graph.Node {
	return navNeighbours(eng, eng.GetOutEdges(id), graph.EdgeCalls, true)
}

// navCallers returns the distinct callers of id.
func navCallers(eng engineLike, id string) []*graph.Node {
	callers := navNeighbours(eng, eng.GetInEdges(id), graph.EdgeCalls, false)
	// A function registered as a callback is invoked through its registrar,
	// which static call extraction can't see — so include the registrars as
	// callers (via the resolver's callback-registration reference edges).
	seen := make(map[string]bool, len(callers))
	for _, n := range callers {
		seen[n.ID] = true
	}
	for _, e := range eng.GetInEdges(id) {
		if e.Kind != graph.EdgeReferences || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_registration" {
			continue
		}
		if seen[e.From] {
			continue
		}
		if n := eng.GetSymbol(e.From); n != nil {
			seen[e.From] = true
			callers = append(callers, n)
		}
	}
	return callers
}

// navSiblings returns the other members of id's parent. The parent is
// reached via id's own EdgeMemberOf out-edge; the siblings are the
// parent's EdgeMemberOf in-edges, with id itself removed.
func navSiblings(eng engineLike, id string) []*graph.Node {
	var parentID string
	for _, e := range eng.GetOutEdges(id) {
		if e.Kind == graph.EdgeMemberOf {
			parentID = e.To
			break
		}
	}
	if parentID == "" {
		return nil
	}
	all := navNeighbours(eng, eng.GetInEdges(parentID), graph.EdgeMemberOf, false)
	out := all[:0]
	for _, n := range all {
		if n.ID != id {
			out = append(out, n)
		}
	}
	return out
}

// navNeighbours walks edges of the given kind and returns the distinct
// nodes on the far end. forward selects the To endpoint (out-edges);
// otherwise the From endpoint (in-edges). Unresolved / external targets
// are skipped.
func navNeighbours(eng engineLike, edges []*graph.Edge, kind graph.EdgeKind, forward bool) []*graph.Node {
	seen := make(map[string]bool)
	var out []*graph.Node
	for _, e := range edges {
		if e.Kind != kind {
			continue
		}
		var id string
		if forward {
			id = e.To
		} else {
			id = e.From
		}
		if seen[id] || graph.IsUnresolvedTarget(id) || strings.HasPrefix(id, "external::") {
			continue
		}
		n := eng.GetSymbol(id)
		if n == nil {
			continue
		}
		seen[id] = true
		out = append(out, n)
	}
	return out
}

// navAdjacency is the short neighbour-count preview attached to every
// nav response so the caller knows which moves are available next.
type navAdjacency struct {
	Callees  int `json:"callees"`
	Callers  int `json:"callers"`
	Siblings int `json:"siblings"`
}

// navCursorView is the JSON shape of the cursor echoed on every response.
type navCursorView struct {
	Current     string `json:"current"`
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name,omitempty"`
	FilePath    string `json:"file_path,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	HistorySize int    `json:"history_size"`
}

// navRespond builds the standard nav response: the action taken, the new
// cursor, and an adjacency preview.
func (s *Server) navRespond(ctx context.Context, req mcp.CallToolRequest, eng engineLike, cur *navCursor, action string, staleReset bool, _ string) (*mcp.CallToolResult, error) {
	payload := s.navBasePayload(eng, cur, action, staleReset)
	return s.respondJSONOrTOON(ctx, req, payload)
}

// navBasePayload assembles the cursor + adjacency fields shared by every
// nav response.
func (s *Server) navBasePayload(eng engineLike, cur *navCursor, action string, staleReset bool) map[string]any {
	node := eng.GetSymbol(cur.current)
	view := navCursorView{Current: cur.current, HistorySize: len(cur.history)}
	if node != nil {
		view.Kind = string(node.Kind)
		view.Name = node.Name
		view.FilePath = node.FilePath
		view.StartLine = node.StartLine
	}
	adj := navAdjacency{
		Callees:  len(navCallees(eng, cur.current)),
		Callers:  len(navCallers(eng, cur.current)),
		Siblings: len(navSiblings(eng, cur.current)),
	}
	payload := map[string]any{
		"action":    action,
		"cursor":    view,
		"adjacency": adj,
	}
	if staleReset {
		payload["stale_cursor_reset"] = true
	}
	return payload
}

// navRead returns the current symbol's source alongside the standard
// cursor + adjacency fields.
func (s *Server) navRead(ctx context.Context, req mcp.CallToolRequest, eng engineLike, cur *navCursor, staleReset bool) (*mcp.CallToolResult, error) {
	node := eng.GetSymbol(cur.current)
	if node == nil {
		return mcp.NewToolResultError("nav read: cursor symbol not found: " + cur.current), nil
	}
	payload := s.navBasePayload(eng, cur, "read", staleReset)

	if node.StartLine == 0 || node.EndLine == 0 {
		payload["source"] = ""
		payload["note"] = "symbol has no line range"
		return s.respondJSONOrTOON(ctx, req, payload)
	}
	absPath, resolveErr := s.resolveNodePath(ctx, node)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}
	source, startLine, _, err := s.readLinesForCtx(ctx, absPath, node.StartLine, node.EndLine, 0)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("nav read: could not read source: %v", err)), nil
	}
	payload["source"] = source
	payload["start_line"] = startLine
	payload["end_line"] = node.EndLine
	return s.respondJSONOrTOON(ctx, req, payload)
}
