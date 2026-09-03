package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// registerNotesTools wires the session-memory triplet:
//
//   - save_note      — create or update a note (auto-linked to symbols
//                      mentioned in the body)
//   - query_notes    — list / search notes by symbol / file / tag /
//                      session / text
//   - distill_session — fold a session's notes into a digest the agent
//                      can paste back into context after compaction
//
// Each handler is scope-aware: notes inherit the session's workspace
// boundary, and `query_notes` never returns notes from a different
// workspace.
func (s *Server) registerNotesTools() {
	s.addTool(
		mcp.NewTool("save_note",
			mcp.WithDescription("Persist an agent-authored note for the current session. Optionally attach to a symbol or file; the body is scanned for symbol references and auto-linked. Use to record decisions, gotchas, follow-ups, or context the agent will need after a compaction. Pass `id` to update an existing note."),
			mcp.WithString("body", mcp.Description("Free-form note text. Symbol IDs (file/path.go::Name) and bare identifier names are auto-linked when they resolve in the graph.")),
			mcp.WithString("symbol_id", mcp.Description("Primary symbol the note attaches to (optional)")),
			mcp.WithString("file_path", mcp.Description("Primary file the note attaches to (optional)")),
			mcp.WithString("tags", mcp.Description("Comma-separated labels — e.g. 'decision,bug,follow-up'. Notes tagged 'decision' are surfaced separately in distill_session.")),
			mcp.WithString("links", mcp.Description("Comma-separated symbol IDs to attach explicitly (merged with auto-detected links)")),
			mcp.WithBoolean("pinned", mcp.Description("Pinned notes are always returned by distill_session and never evicted by the store cap.")),
			mcp.WithString("id", mcp.Description("Existing note ID — passing it switches the call from create to update.")),
			mcp.WithBoolean("no_autolink", mcp.Description("Skip the body→symbol auto-linker (set true when the body intentionally mentions identifiers that should not be linked).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleSaveNote,
	)

	s.addTool(
		mcp.NewTool("query_notes",
			mcp.WithDescription("Search saved notes for the current session / workspace. Filter by symbol, file, tag, free-text body match, session, or recency."),
			mcp.WithString("symbol_id", mcp.Description("Return notes attached or auto-linked to this symbol")),
			mcp.WithString("file_path", mcp.Description("Return notes attached to this file path")),
			mcp.WithString("tag", mcp.Description("Return notes carrying this tag (case-insensitive)")),
			mcp.WithString("text", mcp.Description("Case-insensitive substring filter on the body")),
			mcp.WithString("session_id", mcp.Description("Limit to a specific session. Defaults to the current session; pass 'all' to query every session.")),
			mcp.WithString("since", mcp.Description("Only return notes updated at or after this RFC-3339 timestamp")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 50)")),
			mcp.WithBoolean("pinned_only", mcp.Description("Return only pinned notes")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleQueryNotes,
	)

	s.addTool(
		mcp.NewTool("distill_session",
			mcp.WithDescription("Aggregate a session's saved notes into a digest: top symbols / files / tags, pinned notes, recent excerpts, and a short summary. Use after a context compaction to recover what the agent was doing."),
			mcp.WithString("session_id", mcp.Description("Session to distill. Defaults to the current session; pass 'all' to distill every session for the current workspace.")),
			mcp.WithNumber("max_symbols", mcp.Description("Cap on the top-symbols list (default: 10)")),
			mcp.WithNumber("max_files", mcp.Description("Cap on the top-files list (default: 10)")),
			mcp.WithNumber("max_tags", mcp.Description("Cap on the top-tags list (default: 10)")),
			mcp.WithNumber("max_recent", mcp.Description("Number of recent note excerpts to include (default: 8)")),
			mcp.WithNumber("excerpt_chars", mcp.Description("Body excerpt cap in bytes (default: 240)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleDistillSession,
	)
}

// handleSaveNote — create-or-update entry point. Update mode is
// selected by passing an existing `id`; everything else is a create.
func (s *Server) handleSaveNote(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notes == nil {
		return mcp.NewToolResultError("notes storage not initialised"), nil
	}

	id := strings.TrimSpace(req.GetString("id", ""))
	body := req.GetString("body", "")
	symbolID := strings.TrimSpace(req.GetString("symbol_id", ""))
	filePath := strings.TrimSpace(req.GetString("file_path", ""))
	tags := splitCSV(req.GetString("tags", ""))
	links := splitCSV(req.GetString("links", ""))
	pinned := req.GetBool("pinned", false)
	noAutolink := req.GetBool("no_autolink", false)

	if id == "" && body == "" && symbolID == "" && filePath == "" {
		return mcp.NewToolResultError("save_note requires at least one of: body, symbol_id, file_path"), nil
	}

	// Update path. body / tags / links default to "leave alone" when
	// the caller didn't supply them. pinned is always re-applied
	// (agents should pass it explicitly on updates that care).
	if id != "" {
		var bodyPtr *string
		if body != "" {
			bodyPtr = &body
		}
		pinnedPtr := &pinned
		var addLinks []string
		if !noAutolink && body != "" {
			addLinks = append(addLinks, autoLinkBody(body, s.readerFor(ctx), sessionWorkspaceIDOrEmpty(s, ctx), defaultAutoLinkOptions())...)
		}
		addLinks = append(addLinks, links...)
		updated, err := s.notes.Update(id, bodyPtr, tags, pinnedPtr, addLinks)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("update note: %v", err)), nil
		}
		return s.respondJSONOrTOON(ctx, req, noteEntryToWire(updated))
	}

	// Create path. Stamp scope from session + (when available) the
	// attached symbol's node, so notes inherit the workspace boundary
	// without forcing the caller to think about it.
	workspaceID, projectID, _ := s.sessionScope(ctx)
	repoPrefix, _ := s.sessionLocality(ctx)

	if symbolID != "" {
		if node := s.readerFor(ctx).GetNode(symbolID); node != nil {
			if workspaceID == "" {
				workspaceID = node.WorkspaceID
			}
			if projectID == "" {
				projectID = node.ProjectID
			}
			if repoPrefix == "" {
				repoPrefix = node.RepoPrefix
			}
		}
	}

	autoLinks := links
	if !noAutolink && body != "" {
		autoLinks = append(autoLinks, autoLinkBody(body, s.readerFor(ctx), workspaceID, defaultAutoLinkOptions())...)
	}
	// Always ensure the explicit symbol_id ends up on the link list
	// so query_notes by symbol_id matches even when the body did not
	// mention it.
	if symbolID != "" {
		autoLinks = append(autoLinks, symbolID)
	}

	entry := persistence.NoteEntry{
		SessionID:   SessionIDFromContext(ctx),
		ClientName:  sessionClientName(s, ctx),
		Body:        body,
		SymbolID:    symbolID,
		FilePath:    filePath,
		RepoPrefix:  repoPrefix,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Tags:        tags,
		AutoLinks:   autoLinks,
		Pinned:      pinned,
	}

	newID, err := s.notes.Save(entry)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save note: %v", err)), nil
	}
	entry.ID = newID
	saved, _ := s.notes.Get(newID)
	return s.respondJSONOrTOON(ctx, req, noteEntryToWire(saved))
}

// handleQueryNotes — multi-filter listing. Honours the session
// workspace boundary: every result is required to live inside the
// session's workspace (or carry no workspace when the session is
// unbound).
func (s *Server) handleQueryNotes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notes == nil {
		return mcp.NewToolResultError("notes storage not initialised"), nil
	}

	limit := req.GetInt("limit", 50)
	filter := NoteQueryFilter{
		SymbolID:   strings.TrimSpace(req.GetString("symbol_id", "")),
		FilePath:   strings.TrimSpace(req.GetString("file_path", "")),
		Tag:        strings.TrimSpace(req.GetString("tag", "")),
		TextSearch: req.GetString("text", ""),
		Limit:      limit,
	}

	sess := strings.TrimSpace(req.GetString("session_id", ""))
	switch sess {
	case "":
		filter.SessionID = SessionIDFromContext(ctx)
	case "all", "*":
		filter.SessionID = ""
	default:
		filter.SessionID = sess
	}

	if since := strings.TrimSpace(req.GetString("since", "")); since != "" {
		if ts, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = ts
		} else {
			return mcp.NewToolResultError(fmt.Sprintf("invalid `since` timestamp: %v", err)), nil
		}
	}
	if req.GetBool("pinned_only", false) {
		yes := true
		filter.Pinned = &yes
	}

	if workspaceID, workspaceIn, bound := s.sessionSideStoreScope(ctx); bound {
		filter.WorkspaceID = workspaceID
		filter.WorkspaceIn = workspaceIn
	}

	notes := s.notes.Query(filter)
	wire := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		wire = append(wire, noteEntryToWire(n))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"notes": wire,
		"total": len(wire),
	})
}

// handleDistillSession — assemble the per-session digest.
func (s *Server) handleDistillSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notes == nil {
		return mcp.NewToolResultError("notes storage not initialised"), nil
	}

	opts := defaultDistillOptions()
	if v := req.GetInt("max_symbols", 0); v > 0 {
		opts.MaxSymbols = v
	}
	if v := req.GetInt("max_files", 0); v > 0 {
		opts.MaxFiles = v
	}
	if v := req.GetInt("max_tags", 0); v > 0 {
		opts.MaxTags = v
	}
	if v := req.GetInt("max_recent", 0); v > 0 {
		opts.MaxRecent = v
	}
	if v := req.GetInt("excerpt_chars", 0); v > 0 {
		opts.ExcerptCap = v
	}

	sessionID := ""
	switch strings.TrimSpace(req.GetString("session_id", "")) {
	case "":
		sessionID = SessionIDFromContext(ctx)
	case "all", "*":
		sessionID = ""
	default:
		sessionID = req.GetString("session_id", "")
	}

	var workspaceID, projectID string
	if ws, proj, bound := s.sessionScope(ctx); bound {
		workspaceID = ws
		projectID = proj
	}

	reader := s.readerFor(ctx)
	res := s.notes.DistillSession(sessionID, workspaceID, projectID, opts, func(id string) *graph.Node {
		if reader == nil {
			return nil
		}
		return reader.GetNode(id)
	})
	return s.respondJSONOrTOON(ctx, req, res)
}

// noteEntryToWire shapes a stored note for inclusion in JSON / GCX /
// TOON responses.
func noteEntryToWire(e persistence.NoteEntry) map[string]any {
	m := map[string]any{
		"id":         e.ID,
		"timestamp":  e.Timestamp.UTC().Format(time.RFC3339),
		"updated_at": e.UpdatedAt.UTC().Format(time.RFC3339),
		"body":       e.Body,
	}
	if e.SessionID != "" {
		m["session_id"] = e.SessionID
	}
	if e.ClientName != "" {
		m["client_name"] = e.ClientName
	}
	if e.SymbolID != "" {
		m["symbol_id"] = e.SymbolID
	}
	if e.FilePath != "" {
		m["file_path"] = e.FilePath
	}
	if e.WorkspaceID != "" {
		m["workspace_id"] = e.WorkspaceID
	}
	if e.ProjectID != "" {
		m["project_id"] = e.ProjectID
	}
	if e.RepoPrefix != "" {
		m["repo_prefix"] = e.RepoPrefix
	}
	if len(e.Tags) > 0 {
		m["tags"] = e.Tags
	}
	if len(e.AutoLinks) > 0 {
		m["links"] = e.AutoLinks
	}
	if e.Pinned {
		m["pinned"] = true
	}
	return m
}

// sessionWorkspaceIDOrEmpty returns the session's workspace ID,
// falling back to "" for unbound sessions. Helper because the
// autoLinkBody signature takes a single string.
// sessionSideStoreScope returns the workspace filter for the per-workspace
// side stores (notes, memories). ws is the session's single workspace slug
// when it has one; in is the set of slugs a session bound to the repos its
// cwd contains spans, which has no single slug to filter on. Exactly one of
// the two is populated for a bound session — an empty ws with an empty in
// would read as "no filter" and hand that session every workspace's
// entries.
func (s *Server) sessionSideStoreScope(ctx context.Context) (ws string, in map[string]bool, bound bool) {
	slug, _, isBound := s.sessionScope(ctx)
	if !isBound {
		return "", nil, false
	}
	if slug == "" {
		return "", s.sessionScopeWorkspaces(ctx), true
	}
	return slug, nil, true
}

func sessionWorkspaceIDOrEmpty(s *Server, ctx context.Context) string {
	if s == nil {
		return ""
	}
	ws, _, bound := s.sessionScope(ctx)
	if !bound {
		return ""
	}
	return ws
}

// sessionClientName pulls the captured MCP clientInfo.name for the
// current session. Empty until the dispatcher snoops `initialize`.
func sessionClientName(s *Server, ctx context.Context) string {
	if s == nil {
		return ""
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return ""
	}
	return sess.snapshotClientName()
}
