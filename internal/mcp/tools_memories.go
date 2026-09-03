package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// registerMemoriesTools wires the cross-session development-memory
// triplet:
//
//   - store_memory     — create or update a memory (auto-linked to
//     symbols mentioned in the body)
//   - query_memories   — list / search memories by symbol / file /
//     tag / kind / source / author / text
//   - surface_memories — proactively rank memories for a given
//     working set (anchor symbols / files + task
//     description); the memory analogue of
//     smart_context
//
// Memories are workspace-wide and have no session boundary, so
// every result is filtered through the session's workspace scope
// (mirroring notes), but a memory created in session A is visible
// to session B as soon as it lands on disk.
func (s *Server) registerMemoriesTools() {
	s.addTool(
		mcp.NewTool("store_memory",
			mcp.WithDescription("Persist a cross-session development memory: an invariant, gotcha, decision, convention, or reference fact that future agents in this workspace should know. Memories are anchored to symbols and/or files, survive across sessions, and are surfaced automatically when their anchors enter an agent's working set. Pass `id` to update an existing memory."),
			mcp.WithString("body", mcp.Description("Free-form memory text. Symbol IDs (file/path.go::Name) and bare identifier names are auto-linked when they resolve in the graph.")),
			mcp.WithString("title", mcp.Description("Short caption (one-liner) used as the headline in surface_memories output.")),
			mcp.WithString("symbol_ids", mcp.Description("Comma-separated primary symbol anchors (e.g. pkg/foo.go::Bar,pkg/foo.go::Baz). At least one of symbol_ids / file_paths is recommended for high-quality surfacing.")),
			mcp.WithString("file_paths", mcp.Description("Comma-separated primary file anchors.")),
			mcp.WithString("tags", mcp.Description("Comma-separated labels — e.g. 'invariant,gotcha,decision'.")),
			mcp.WithString("kind", mcp.Description("Memory kind: invariant | constraint | convention | gotcha | decision | incident | reference (default: reference).")),
			mcp.WithString("source", mcp.Description("Where this came from: manual | distilled | incident | review (default: manual).")),
			mcp.WithNumber("importance", mcp.Description("1..5 — operator-assigned weight; surfaced higher in ranking (default: 3).")),
			mcp.WithNumber("confidence", mcp.Description("0.0..1.0 — how sure we are this still holds (default: 1.0). Decays the surfacing score.")),
			mcp.WithBoolean("pinned", mcp.Description("Pinned memories are never evicted by the store cap and float to the top of surfacing.")),
			mcp.WithString("supersedes", mcp.Description("Comma-separated memory IDs that this new memory replaces. Each older entry is marked SupersededBy=<new id> and hidden from surface_memories by default.")),
			mcp.WithString("superseded_by", mcp.Description("(Update-only) explicitly mark this memory as superseded by another memory ID — hides it from surface_memories.")),
			mcp.WithString("id", mcp.Description("Existing memory ID — passing it switches the call from create to update.")),
			mcp.WithBoolean("no_autolink", mcp.Description("Skip the body→symbol auto-linker.")),
			mcp.WithString("scope", mcp.Description("Memory scope: workspace (default — per-repo) or global (~/.gortex/memories — shared across every workspace this user touches).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleStoreMemory,
	)

	s.addTool(
		mcp.NewTool("query_memories",
			mcp.WithDescription("Search the cross-session memory store by symbol, file, tag, kind, source, author, free-text, or recency. Returns matches sorted pinned-first, then importance-DESC, then UpdatedAt-DESC. Unlike query_notes, memories are workspace-wide and have no session boundary."),
			mcp.WithString("symbol_id", mcp.Description("Return memories anchored or auto-linked to this symbol.")),
			mcp.WithString("file_path", mcp.Description("Return memories anchored to this file path.")),
			mcp.WithString("tag", mcp.Description("Return memories carrying this tag (case-insensitive).")),
			mcp.WithString("kind", mcp.Description("Filter by memory kind: invariant | constraint | convention | gotcha | decision | incident | reference.")),
			mcp.WithString("source", mcp.Description("Filter by memory source: manual | distilled | incident | review.")),
			mcp.WithString("author", mcp.Description("Filter by author agent (MCP clientInfo.name).")),
			mcp.WithString("text", mcp.Description("Case-insensitive substring filter on body or title.")),
			mcp.WithString("since", mcp.Description("Only return memories updated at or after this RFC-3339 timestamp.")),
			mcp.WithNumber("min_importance", mcp.Description("Only return memories with importance >= this value (1..5).")),
			mcp.WithBoolean("pinned_only", mcp.Description("Return only pinned memories.")),
			mcp.WithBoolean("include_superseded", mcp.Description("Include superseded memories (default false).")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 50).")),
			mcp.WithString("scope", mcp.Description("Memory scope: workspace (default), global, or both.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleQueryMemories,
	)

	s.addTool(
		mcp.NewTool("surface_memories",
			mcp.WithDescription("Proactively surface relevant development memories for a task or working set. Given anchor symbols / files / a task description, returns memories ranked by symbol overlap, file overlap, keyword hits, importance, pinning, recency, and confidence — deterministic scoring, no LLM. Use at task start (the memory analogue of smart_context) or before editing a symbol with a known history."),
			mcp.WithString("task", mcp.Description("Natural-language task description — keywords matched against memory body / title.")),
			mcp.WithString("symbol_ids", mcp.Description("Comma-separated anchor symbols (the working set).")),
			mcp.WithString("file_paths", mcp.Description("Comma-separated anchor files.")),
			mcp.WithNumber("limit", mcp.Description("Cap the surfaced set (default: 10).")),
			mcp.WithNumber("excerpt_chars", mcp.Description("Body excerpt cap in bytes (default: 320).")),
			mcp.WithNumber("min_score", mcp.Description("Drop hits below this score (default: 0).")),
			mcp.WithBoolean("include_superseded", mcp.Description("Include superseded memories (default false).")),
			mcp.WithBoolean("mark_accessed", mcp.Description("Increment AccessCount + LastAccessed on returned memories (default true).")),
			mcp.WithString("scope", mcp.Description("Memory scope: workspace (default), global, or both.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleSurfaceMemories,
	)

	s.registerMemoryHierarchyTools()

	s.addTool(
		mcp.NewTool("check_onboarding_performed",
			mcp.WithDescription("Light-weight readiness probe for the gortex-onboarding skill. Returns {performed, total_memories, counts_by_kind, missing_kinds}. `performed` is true iff every essential memory kind has at least `min_per_kind` entries in the session's workspace; `missing_kinds` lists the categories that fall short. Agents call this before deciding to run the onboarding playbook — if performed=false, run the skill and store the durable knowledge as memories."),
			mcp.WithString("essential_kinds", mcp.Description("Comma-separated kinds that must each have at least `min_per_kind` memories (default: invariant,convention,decision — the three durable-knowledge categories from CLAUDE.md). Pass an empty string to disable kind checking; then `performed` is true iff total_memories >= min_total.")),
			mcp.WithNumber("min_per_kind", mcp.Description("Minimum memories required per essential kind (default: 1).")),
			mcp.WithNumber("min_total", mcp.Description("Minimum total memories required (default: 0 — no total threshold).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleCheckOnboardingPerformed,
	)
}

// handleStoreMemory — create-or-update entry point. Update mode is
// selected by passing an existing `id`; everything else is a create.
func (s *Server) handleStoreMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := strings.TrimSpace(req.GetString("scope", ""))
	store := s.resolveMemoryStore(scope)
	if store == nil {
		if scope == "global" {
			return mcp.NewToolResultError("global memory store not available (could not resolve ~/.gortex)"), nil
		}
		return mcp.NewToolResultError("memories storage not initialised"), nil
	}

	id := strings.TrimSpace(req.GetString("id", ""))
	body := req.GetString("body", "")
	title := strings.TrimSpace(req.GetString("title", ""))
	symbolIDs := splitCSV(req.GetString("symbol_ids", ""))
	filePaths := splitCSV(req.GetString("file_paths", ""))
	tags := splitCSV(req.GetString("tags", ""))
	kind := strings.TrimSpace(req.GetString("kind", ""))
	source := strings.TrimSpace(req.GetString("source", ""))
	importance := req.GetInt("importance", 0)
	confidence := float32(req.GetFloat("confidence", 0))
	pinned := req.GetBool("pinned", false)
	pinnedSet := requestHasArg(req, "pinned")
	supersedes := splitCSV(req.GetString("supersedes", ""))
	supersededBy := strings.TrimSpace(req.GetString("superseded_by", ""))
	noAutolink := req.GetBool("no_autolink", false)

	if id == "" && body == "" && len(symbolIDs) == 0 && len(filePaths) == 0 {
		return mcp.NewToolResultError("store_memory requires at least one of: body, symbol_ids, file_paths"), nil
	}

	// Update path.
	if id != "" {
		patch := MemoryPatch{}
		if body != "" {
			patch.Body = &body
		}
		if title != "" {
			patch.Title = &title
		}
		if kind != "" {
			patch.Kind = &kind
		}
		if source != "" {
			patch.Source = &source
		}
		if importance != 0 {
			patch.Importance = &importance
		}
		if confidence > 0 {
			patch.Confidence = &confidence
		}
		if tags != nil {
			patch.Tags = tags
		}
		if symbolIDs != nil {
			patch.SymbolIDs = symbolIDs
		}
		if filePaths != nil {
			patch.FilePaths = filePaths
		}
		if pinnedSet {
			patch.Pinned = &pinned
		}
		if supersededBy != "" {
			patch.SupersededBy = &supersededBy
		}
		if !noAutolink && body != "" {
			patch.AddLinks = autoLinkBody(body, s.readerFor(ctx), sessionWorkspaceIDOrEmpty(s, ctx), defaultAutoLinkOptions())
		}
		updated, err := store.Update(id, patch)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("update memory: %v", err)), nil
		}
		// Backfill: when the update names `supersedes`, retro-mark
		// the older entries even on an update call.
		for _, oldID := range supersedes {
			if oldID == id {
				continue
			}
			if _, ok := store.Get(oldID); !ok {
				continue
			}
			newID := id
			_, _ = store.Update(oldID, MemoryPatch{SupersededBy: &newID})
		}
		s.reconcileRationale(scope)
		return s.respondJSONOrTOON(ctx, req, memoryEntryToWire(updated))
	}

	// Create path. Stamp scope from session + (when available) the
	// first attached symbol's node, so memories inherit the workspace
	// boundary without forcing the caller to think about it.
	workspaceID, projectID, _ := s.sessionScope(ctx)
	repoPrefix, _ := s.sessionLocality(ctx)

	if reader := s.readerFor(ctx); len(symbolIDs) > 0 && reader != nil {
		if node := reader.GetNode(symbolIDs[0]); node != nil {
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

	var autoLinks []string
	if !noAutolink && body != "" {
		autoLinks = autoLinkBody(body, s.readerFor(ctx), workspaceID, defaultAutoLinkOptions())
	}

	entry := persistence.MemoryEntry{
		Body:        body,
		Title:       title,
		Kind:        kind,
		Source:      source,
		Importance:  importance,
		Confidence:  confidence,
		AuthorAgent: sessionClientName(s, ctx),
		SymbolIDs:   symbolIDs,
		FilePaths:   filePaths,
		AutoLinks:   autoLinks,
		Tags:        tags,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		RepoPrefix:  repoPrefix,
		Pinned:      pinned,
	}

	newID, err := store.Save(entry)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save memory: %v", err)), nil
	}

	// Stamp each named older memory as superseded by this new one.
	for _, oldID := range supersedes {
		if oldID == newID {
			continue
		}
		if _, ok := store.Get(oldID); !ok {
			continue
		}
		nid := newID
		_, _ = store.Update(oldID, MemoryPatch{SupersededBy: &nid})
	}

	saved, _ := store.Get(newID)
	s.reconcileRationale(scope)
	return s.respondJSONOrTOON(ctx, req, memoryEntryToWire(saved))
}

// handleQueryMemories — multi-filter listing. Honours the session
// workspace boundary: every result lives inside the session's
// workspace (or carries no workspace when the session is unbound).
func (s *Server) handleQueryMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := strings.ToLower(strings.TrimSpace(req.GetString("scope", "")))
	stores := s.resolveMemoryStores(scope)
	if len(stores) == 0 {
		return mcp.NewToolResultError("memories storage not initialised"), nil
	}

	limit := req.GetInt("limit", 50)
	filter := MemoryQueryFilter{
		SymbolID:          strings.TrimSpace(req.GetString("symbol_id", "")),
		FilePath:          strings.TrimSpace(req.GetString("file_path", "")),
		Tag:               strings.TrimSpace(req.GetString("tag", "")),
		Kind:              strings.TrimSpace(req.GetString("kind", "")),
		Source:            strings.TrimSpace(req.GetString("source", "")),
		AuthorAgent:       strings.TrimSpace(req.GetString("author", "")),
		TextSearch:        req.GetString("text", ""),
		MinImportance:     req.GetInt("min_importance", 0),
		IncludeSuperseded: req.GetBool("include_superseded", false),
		Limit:             limit,
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

	wire := make([]map[string]any, 0)
	for _, store := range stores {
		// Per-store WorkspaceID filter: only applies to the workspace
		// store, not global. Global entries have no WorkspaceID by
		// construction, so the filter would silently drop them.
		perStoreFilter := filter
		if store != s.globalMemories {
			if workspaceID, workspaceIn, bound := s.sessionSideStoreScope(ctx); bound {
				perStoreFilter.WorkspaceID = workspaceID
				perStoreFilter.WorkspaceIn = workspaceIn
			}
		} else {
			perStoreFilter.WorkspaceID = ""
			perStoreFilter.WorkspaceIn = nil
		}
		for _, m := range store.Query(perStoreFilter) {
			row := memoryEntryToWire(m)
			if store == s.globalMemories {
				row["scope"] = "global"
			} else {
				row["scope"] = "workspace"
			}
			wire = append(wire, row)
		}
	}
	// When both stores were consulted, the combined list may exceed
	// limit; trim at the call boundary so the contract holds.
	if limit > 0 && len(wire) > limit {
		wire = wire[:limit]
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"memories": wire,
		"total":    len(wire),
	})
}

// handleSurfaceMemories — proactively retrieve memories for a
// working set. Defaults to mark_accessed=true so the access stats
// stay accurate.
func (s *Server) handleSurfaceMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := strings.ToLower(strings.TrimSpace(req.GetString("scope", "")))
	store := s.resolveMemoryStore(scope)
	if store == nil {
		if scope == "global" {
			return mcp.NewToolResultError("global memory store not available"), nil
		}
		return mcp.NewToolResultError("memories storage not initialised"), nil
	}

	opts := SurfaceOptions{
		Task:              strings.TrimSpace(req.GetString("task", "")),
		SymbolIDs:         splitCSV(req.GetString("symbol_ids", "")),
		FilePaths:         splitCSV(req.GetString("file_paths", "")),
		Limit:             req.GetInt("limit", 10),
		ExcerptCap:        req.GetInt("excerpt_chars", 320),
		MinScore:          float32(req.GetFloat("min_score", 0)),
		IncludeSuperseded: req.GetBool("include_superseded", false),
		MarkAccessed:      requestBoolDefault(req, "mark_accessed", true),
	}
	// Workspace scoping only applies to the workspace store. Global
	// entries don't carry a WorkspaceID, so leaving the opts default
	// is correct.
	if store != s.globalMemories {
		if _, projectID, bound := s.sessionScope(ctx); bound {
			workspaceID, workspaceIn, _ := s.sessionSideStoreScope(ctx)
			opts.WorkspaceID = workspaceID
			opts.WorkspaceIn = workspaceIn
			opts.ProjectID = projectID
		}
	}

	reader := s.readerFor(ctx)
	res := store.Surface(opts, func(id string) *graph.Node {
		if reader == nil {
			return nil
		}
		return reader.GetNode(id)
	})
	return s.respondJSONOrTOON(ctx, req, res)
}

// memoryEntryToWire shapes a stored memory for inclusion in
// JSON / GCX / TOON responses.
func memoryEntryToWire(e persistence.MemoryEntry) map[string]any {
	m := map[string]any{
		"id":         e.ID,
		"timestamp":  e.Timestamp.UTC().Format(time.RFC3339),
		"updated_at": e.UpdatedAt.UTC().Format(time.RFC3339),
		"body":       e.Body,
	}
	if e.Title != "" {
		m["title"] = e.Title
	}
	if e.Kind != "" {
		m["kind"] = e.Kind
	}
	if e.Source != "" {
		m["source"] = e.Source
	}
	if e.Confidence > 0 {
		m["confidence"] = e.Confidence
	}
	if e.Importance > 0 {
		m["importance"] = e.Importance
	}
	if e.AuthorAgent != "" {
		m["author_agent"] = e.AuthorAgent
	}
	if len(e.SymbolIDs) > 0 {
		m["symbol_ids"] = e.SymbolIDs
	}
	if len(e.FilePaths) > 0 {
		m["file_paths"] = e.FilePaths
	}
	if len(e.AutoLinks) > 0 {
		m["links"] = e.AutoLinks
	}
	if len(e.Tags) > 0 {
		m["tags"] = e.Tags
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
	if e.Pinned {
		m["pinned"] = true
	}
	if e.SupersededBy != "" {
		m["superseded_by"] = e.SupersededBy
	}
	if e.AccessCount > 0 {
		m["access_count"] = e.AccessCount
	}
	if !e.LastAccessed.IsZero() {
		m["last_accessed"] = e.LastAccessed.UTC().Format(time.RFC3339)
	}
	return m
}

// requestHasArg reports whether the caller explicitly supplied a
// named argument. Needed for tri-state booleans (pinned: set vs.
// unset vs. false) on update.
func requestHasArg(req mcp.CallToolRequest, key string) bool {
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return false
	}
	_, present := args[key]
	return present
}

// requestBoolDefault returns the bool at key, or def when the
// caller didn't supply the argument at all. (GetBool returns the
// zero value for "not present", which conflates absent with false.)
func requestBoolDefault(req mcp.CallToolRequest, key string, def bool) bool {
	if !requestHasArg(req, key) {
		return def
	}
	return req.GetBool(key, def)
}

// defaultEssentialKinds is the kind list check_onboarding_performed
// uses when the caller doesn't override it — the three durable
// knowledge categories CLAUDE.md tells agents to record. Onboarding
// is "done" when each has at least one anchored memory in the
// workspace.
var defaultEssentialKinds = []string{"invariant", "convention", "decision"}

// handleCheckOnboardingPerformed is the onboarding-readiness probe.
// Returns whether each essential memory kind has min_per_kind
// memories anchored in the session's workspace, plus the per-kind
// breakdown so the caller can tell exactly which categories are
// missing before deciding to run the onboarding playbook.
func (s *Server) handleCheckOnboardingPerformed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.memories == nil {
		return mcp.NewToolResultError("memories storage not initialised"), nil
	}

	var essentialKinds []string
	if requestHasArg(req, "essential_kinds") {
		essentialKinds = splitCSV(req.GetString("essential_kinds", ""))
	} else {
		essentialKinds = append(essentialKinds, defaultEssentialKinds...)
	}

	minPerKind := max(req.GetInt("min_per_kind", 1), 1)
	minTotal := max(req.GetInt("min_total", 0), 0)

	workspaceID, workspaceIn, _ := s.sessionSideStoreScope(ctx)

	// One Query per kind. The store is in-memory so this is cheap;
	// the readability win over a single Query+group-by is worth
	// the extra calls.
	countsByKind := make(map[string]int, len(essentialKinds))
	missing := make([]string, 0)
	for _, k := range essentialKinds {
		entries := s.memories.Query(MemoryQueryFilter{
			Kind:        k,
			WorkspaceID: workspaceID,
			WorkspaceIn: workspaceIn,
		})
		countsByKind[k] = len(entries)
		if len(entries) < minPerKind {
			missing = append(missing, k)
		}
	}

	// total counts every workspace-scoped memory regardless of kind.
	total := len(s.memories.Query(MemoryQueryFilter{WorkspaceID: workspaceID, WorkspaceIn: workspaceIn}))

	performed := len(missing) == 0 && total >= minTotal
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"performed":       performed,
		"total_memories":  total,
		"essential_kinds": essentialKinds,
		"min_per_kind":    minPerKind,
		"min_total":       minTotal,
		"counts_by_kind":  countsByKind,
		"missing_kinds":   missing,
	})
}

// registerMemoryHierarchyTools wires edit_memory + rename_memory —
// the two operations that extend the existing memory triplet with
// scope migration (workspace ↔ global) and in-place body edits
// (regex or literal find/replace). Closes serena's 6-tool surface
// gap (we previously had 3 + check_onboarding_performed; now 6).
func (s *Server) registerMemoryHierarchyTools() {
	s.addTool(
		mcp.NewTool("edit_memory",
			mcp.WithDescription("In-place find/replace inside a memory's body. Use when a memory is mostly right but one detail drifted — beats supersede-and-recreate. Returns the updated entry. mode=regex (default) treats `pattern` as a Go regexp; mode=literal escapes it. Updates UpdatedAt; preserves Importance / Confidence / Pinned / SymbolIDs / FilePaths."),
			mcp.WithString("id", mcp.Description("Memory ID to edit.")),
			mcp.WithString("pattern", mcp.Description("Find pattern. Treated as a regexp under mode=regex; as a literal substring under mode=literal.")),
			mcp.WithString("replacement", mcp.Description("Replacement text. Regex groups (`$1`, `$2`) work under mode=regex.")),
			mcp.WithString("mode", mcp.Description("regex (default) or literal.")),
			mcp.WithString("scope", mcp.Description("workspace (default) or global.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleEditMemory,
	)

	s.addTool(
		mcp.NewTool("rename_memory",
			mcp.WithDescription("Move a memory between workspace and global scope. Preserves Body / Title / Kind / Tags / SymbolIDs / FilePaths / Importance / Confidence / Pinned; clears WorkspaceID + ProjectID when promoting to global, otherwise stamps them from the session. Returns the entry as it landed in the new scope. The original entry is deleted on success."),
			mcp.WithString("id", mcp.Description("Memory ID to move.")),
			mcp.WithString("to_scope", mcp.Description("Target scope: workspace or global.")),
			mcp.WithString("from_scope", mcp.Description("Source scope (default: workspace).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleRenameMemory,
	)
}

// handleEditMemory applies an in-place find/replace to the memory's
// body and persists the result. Regex mode is the default — pass
// mode=literal for a verbatim substring replace.
func (s *Server) handleEditMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	pattern, err := req.RequireString("pattern")
	if err != nil {
		return mcp.NewToolResultError("pattern is required"), nil
	}
	replacement := req.GetString("replacement", "")
	mode := strings.ToLower(strings.TrimSpace(req.GetString("mode", "regex")))
	scope := strings.TrimSpace(req.GetString("scope", ""))

	store := s.resolveMemoryStore(scope)
	if store == nil {
		return mcp.NewToolResultError("memories storage not initialised"), nil
	}
	current, ok := store.Get(id)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("memory %q not found in %s scope", id, defaultIfEmpty(scope, "workspace"))), nil
	}

	var newBody string
	switch mode {
	case "literal":
		newBody = strings.ReplaceAll(current.Body, pattern, replacement)
	case "regex", "":
		re, rerr := regexp.Compile(pattern)
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid regex: %v", rerr)), nil
		}
		newBody = re.ReplaceAllString(current.Body, replacement)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown mode %q (use regex or literal)", mode)), nil
	}

	if newBody == current.Body {
		// No change — return current entry so callers can detect
		// idempotent application without an extra round-trip.
		return s.respondJSONOrTOON(ctx, req, memoryEntryToWire(current))
	}

	updated, uerr := store.Update(id, MemoryPatch{Body: &newBody})
	if uerr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update memory: %v", uerr)), nil
	}
	return s.respondJSONOrTOON(ctx, req, memoryEntryToWire(updated))
}

// handleRenameMemory moves a memory between workspace and global
// scope. Preserves every field except WorkspaceID / ProjectID which
// are cleared on the promote-to-global path and re-stamped from
// the session on the demote-to-workspace path. The original entry
// is deleted on success.
func (s *Server) handleRenameMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	toScope := strings.ToLower(strings.TrimSpace(req.GetString("to_scope", "")))
	if toScope != "workspace" && toScope != "global" {
		return mcp.NewToolResultError("to_scope must be 'workspace' or 'global'"), nil
	}
	fromScope := strings.ToLower(strings.TrimSpace(req.GetString("from_scope", "workspace")))
	if fromScope == toScope {
		return mcp.NewToolResultError("from_scope and to_scope are identical — nothing to do"), nil
	}

	fromStore := s.resolveMemoryStore(fromScope)
	toStore := s.resolveMemoryStore(toScope)
	if fromStore == nil {
		return mcp.NewToolResultError(fmt.Sprintf("%s memory store not initialised", fromScope)), nil
	}
	if toStore == nil {
		return mcp.NewToolResultError(fmt.Sprintf("%s memory store not initialised", toScope)), nil
	}

	current, ok := fromStore.Get(id)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("memory %q not found in %s scope", id, fromScope)), nil
	}

	// Build the new entry. Drop the ID so the destination store
	// generates a fresh one — the move semantically creates a new
	// memory at the destination and removes the source.
	moved := current
	moved.ID = ""
	switch toScope {
	case "global":
		moved.WorkspaceID = ""
		moved.ProjectID = ""
		moved.RepoPrefix = ""
	case "workspace":
		if workspaceID, projectID, bound := s.sessionScope(ctx); bound {
			moved.WorkspaceID = workspaceID
			moved.ProjectID = projectID
		}
	}

	newID, serr := toStore.Save(moved)
	if serr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save to %s: %v", toScope, serr)), nil
	}
	if derr := fromStore.Delete(id); derr != nil {
		// Save succeeded but delete failed — leave both copies in
		// place rather than risk losing the entry. Surface the
		// inconsistency to the caller.
		return mcp.NewToolResultError(fmt.Sprintf("saved to %s but could not delete from %s: %v (memory now exists in both scopes)", toScope, fromScope, derr)), nil
	}

	saved, _ := toStore.Get(newID)
	wire := memoryEntryToWire(saved)
	wire["scope"] = toScope
	wire["previous_id"] = id
	return s.respondJSONOrTOON(ctx, req, wire)
}

// defaultIfEmpty returns def when s is the empty string. Tiny helper
// kept inline because the two-line alternative is everywhere.
func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
