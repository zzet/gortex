package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// registerOverlayBranchTools wires the branching family of overlay
// management tools onto the MCP server. Called from
// registerOverlayTools so the tools only exist when overlay support
// is enabled. Tools registered here are intentionally NOT routed
// through s.addTool: the overlay middleware must not interfere with
// branch management primitives (e.g. overlay_switch must take effect
// before the next tools/call's view is built, not be re-evaluated
// against the branch about to be switched away).
func (s *Server) registerOverlayBranchTools() {
	s.addControlTool(
		mcp.NewTool("overlay_fork",
			mcp.WithDescription("Clone the current session's overlay state into a named branch. Branches let an agent hold N simultaneous speculative states off one baseline so it can apply different edit strategies to each, compare outcomes via compare_branches, and promote the winner with overlay_merge. The implicit base branch is `main`; every new fork inherits its parent's file map by deep copy. Pass activate:true to make the new branch the session's active branch immediately."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Branch name (alphanumeric + dash/underscore, max 64 chars).")),
			mcp.WithString("from", mcp.Description("Source branch to fork from. Defaults to the session's currently active branch.")),
			mcp.WithBoolean("activate", mcp.Description("Switch the session to the new branch immediately (default false).")),
		),
		s.handleOverlayFork,
	)
	s.addControlTool(
		mcp.NewTool("overlay_branches",
			mcp.WithDescription("List every overlay branch the calling MCP session holds, including the implicit `main` branch. Each entry carries the branch name, whether it is currently active, the number of files attached, the number of files that carry a base_sha drift anchor, the parent branch name (empty for `main`), and the creation timestamp."),
		),
		s.handleOverlayBranches,
	)
	s.addControlTool(
		mcp.NewTool("overlay_switch",
			mcp.WithDescription("Make the named branch the active overlay for the calling MCP session. All existing overlay tools (overlay_push, overlay_list, overlay_delete, overlay_drop, compare_with_overlay, preview_edit, simulate_chain) operate against the active branch after the switch."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Branch name to activate.")),
		),
		s.handleOverlaySwitch,
	)
	s.addControlTool(
		mcp.NewTool("overlay_merge",
			mcp.WithDescription("Merge a branch's edits into another branch or write them to disk. With to_disk:false (default) the from branch's files are folded into the into branch (default: main); with to_disk:true the from branch's overlay files are written to the filesystem using the same atomic-write + base_sha drift-guard machinery as edit_file, then the from branch is dropped from the session. Conflict policy: a path that exists in both branches with different content is a conflict; without force:true the merge aborts and the response carries a conflicts list. force:true resolves conflicts last-writer-wins (from wins) and notes the resolution in the response."),
			mcp.WithString("from", mcp.Required(), mcp.Description("Source branch to merge.")),
			mcp.WithString("into", mcp.Description("Destination branch (default `main`). Ignored when to_disk:true.")),
			mcp.WithBoolean("to_disk", mcp.Description("Write the from branch's overlay files to disk instead of folding them into another branch (default false). Honours each overlay's base_sha for drift detection.")),
			mcp.WithBoolean("force", mcp.Description("Override conflicts last-writer-wins (default false).")),
		),
		s.handleOverlayMerge,
	)
	s.addControlTool(
		mcp.NewTool("overlay_drop_branch",
			mcp.WithDescription("Delete a named overlay branch from the calling session. Refuses to drop the implicit `main` branch (drop the whole session instead with overlay_drop) and refuses to drop the currently active branch (call overlay_switch first)."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Branch name to delete.")),
		),
		s.handleOverlayDropBranch,
	)
	s.addControlTool(
		mcp.NewTool("compare_branches",
			mcp.WithDescription("Run a graph query against two overlay branches in the calling session and report the delta. Each branch is materialised as its own shadow-graph view (no shared state). Useful when an agent has applied different edit strategies to two branches and wants to know which strategy actually changes the dependency graph in the desired direction. Supported `kind` values: find_usages, get_callers, get_call_chain, get_dependencies, get_dependents."),
			mcp.WithString("a", mcp.Required(), mcp.Description("First branch name.")),
			mcp.WithString("b", mcp.Required(), mcp.Description("Second branch name.")),
			mcp.WithString("kind", mcp.Required(), mcp.Description("Query kind to run. One of: find_usages, get_callers, get_call_chain, get_dependencies, get_dependents.")),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID to query (e.g. \"target.go::Target\").")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth for chain / dependency queries (default 2).")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of nodes to return per side (default 50).")),
		),
		s.handleCompareBranches,
	)
}

func (s *Server) handleOverlayFork(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := daemon.ValidateBranchName(name); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	from := req.GetString("from", "")
	if from != "" {
		if err := daemon.ValidateBranchName(from); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	activate := req.GetBool("activate", false)
	// Idempotent register-on-fork: the branching surface is also
	// available before any overlay_register, so an extension can
	// fork to a named branch as its very first overlay call.
	if !s.overlays.Has(id) {
		workspace, _, _ := s.sessionScope(ctx)
		_ = s.overlays.RegisterWithID(id, workspace)
	}
	res, err := s.overlays.Fork(id, daemon.ForkOptions{Name: name, From: from, Activate: activate})
	if err != nil {
		switch {
		case errors.Is(err, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register before forking"), nil
		case errors.Is(err, daemon.ErrBranchExists):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q already exists", name)), nil
		case errors.Is(err, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q not found", from)), nil
		case errors.Is(err, daemon.ErrInvalidBranchName):
			return mcp.NewToolResultError(err.Error()), nil
		default:
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	if activate {
		// Switching active branch must invalidate the cached
		// parsed-overlay layer; the cache is keyed by session, not
		// by branch, so a switch is functionally equivalent to a
		// push from the cache's point of view.
		s.overlayCacheInvalidate(id)
	}
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"branch":     res.Branch,
		"parent":     res.Parent,
		"files":      res.FileCount,
		"active":     res.Active,
		"session_id": id,
	})), nil
}

func (s *Server) handleOverlayBranches(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	if !s.overlays.Has(id) {
		// A session with no overlay history sees one implicit
		// `main` branch by convention so the response is uniform
		// regardless of whether the client has called
		// overlay_register yet.
		workspace, _, _ := s.sessionScope(ctx)
		_ = s.overlays.RegisterWithID(id, workspace)
	}
	branches, err := s.overlays.Branches(id)
	if err != nil {
		if errors.Is(err, daemon.ErrSessionNotFound) {
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(branches))
	for _, b := range branches {
		out = append(out, map[string]any{
			"name":           b.Name,
			"active":         b.Active,
			"file_count":     b.FileCount,
			"base_sha_count": b.BaseSHACount,
			"parent":         b.Parent,
			"created_at":     b.Created.UTC().Format(time.RFC3339),
		})
	}
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"session_id": id,
		"count":      len(out),
		"branches":   out,
	})), nil
}

func (s *Server) handleOverlaySwitch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := daemon.ValidateBranchName(name); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.overlays.SwitchBranch(id, name); err != nil {
		switch {
		case errors.Is(err, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		case errors.Is(err, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q not found", name)), nil
		case errors.Is(err, daemon.ErrInvalidBranchName):
			return mcp.NewToolResultError(err.Error()), nil
		default:
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	// Cached parsed-overlay layer is keyed by session, not branch;
	// after a switch the next tools/call must re-parse from the
	// new active branch's contents.
	s.overlayCacheInvalidate(id)
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"session_id":    id,
		"active_branch": name,
	})), nil
}

func (s *Server) handleOverlayDropBranch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.overlays.DropBranch(id, name); err != nil {
		switch {
		case errors.Is(err, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		case errors.Is(err, daemon.ErrCannotDropMainBranch):
			return mcp.NewToolResultError(err.Error()), nil
		case errors.Is(err, daemon.ErrCannotDropActiveBranch):
			return mcp.NewToolResultError(err.Error()), nil
		case errors.Is(err, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q not found", name)), nil
		case errors.Is(err, daemon.ErrInvalidBranchName):
			return mcp.NewToolResultError(err.Error()), nil
		default:
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"session_id": id,
		"branch":     name,
		"ok":         true,
	})), nil
}

func (s *Server) handleOverlayMerge(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	from, err := req.RequireString("from")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	into := req.GetString("into", "")
	toDisk := req.GetBool("to_disk", false)
	force := req.GetBool("force", false)

	opts := daemon.MergeOptions{
		From:   from,
		Into:   into,
		ToDisk: toDisk,
		Force:  force,
	}

	var writer daemon.DiskWriteFn
	if toDisk {
		// Capture the resolver closure once; each per-file write
		// re-resolves, re-checks drift, and atomically writes.
		writer = s.diskWriteForMerge(ctx)
	}

	res, mergeErr := s.overlays.MergeBranches(id, opts, writer)
	if mergeErr != nil {
		switch {
		case errors.Is(mergeErr, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		case errors.Is(mergeErr, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q or destination %q not found", from, opts.Into)), nil
		case errors.Is(mergeErr, daemon.ErrInvalidBranchName):
			return mcp.NewToolResultError(mergeErr.Error()), nil
		case errors.Is(mergeErr, daemon.ErrMergeConflict):
			// Surface the conflicts list so the caller can choose
			// to retry with force:true after inspecting them.
			return mcp.NewToolResultText(jsonOK(map[string]any{
				"session_id": id,
				"from":       from,
				"into":       opts.Into,
				"to_disk":    toDisk,
				"merged":     res.Merged,
				"conflicts":  res.Conflicts,
				"error":      "merge conflict — pass force:true to override (last-writer-wins)",
			})), nil
		case errors.Is(mergeErr, daemon.ErrOverlayDrift):
			return mcp.NewToolResultError(mergeErr.Error()), nil
		default:
			return mcp.NewToolResultError(mergeErr.Error()), nil
		}
	}
	if toDisk || res.ResolvedByForce {
		// Either the on-disk files changed (next overlay view
		// build must re-read them) or the branch contents shifted
		// for the destination; invalidate to be safe.
		s.overlayCacheInvalidate(id)
	}
	resp := map[string]any{
		"session_id":     id,
		"from":           from,
		"into":           opts.Into,
		"to_disk":        toDisk,
		"merged":         res.Merged,
		"conflicts":      res.Conflicts,
		"dropped_branch": res.DroppedBranch,
	}
	if res.ResolvedByForce {
		resp["resolution"] = "last-writer-wins"
	}
	return mcp.NewToolResultText(jsonOK(resp)), nil
}

// diskWriteForMerge returns the daemon.DiskWriteFn that
// MergeBranches invokes once per file when to_disk is true. It
// reuses the existing gitBlobSHA / atomic-write machinery from the
// edit_file tool so disk writes go through identical safety guards
// (path resolution, base_sha drift, temp+rename) — without that
// reuse, drift detection would be duplicated and could silently
// diverge across surfaces.
func (s *Server) diskWriteForMerge(ctx context.Context) daemon.DiskWriteFn {
	return func(f daemon.OverlayFile) error {
		absPath, _, resolveErr := s.resolveFilePath(ctx, f.Path)
		if resolveErr != nil {
			return fmt.Errorf("merge to_disk: %s: %w", f.Path, resolveErr)
		}
		if f.Deleted {
			// Tombstone-on-disk: remove the file. Honour the
			// drift guard before removal so a concurrent edit
			// can't be silently overwritten by an unlink.
			if f.BaseSHA != "" {
				current, readErr := os.ReadFile(absPath)
				if readErr != nil {
					// File already gone is not drift; surface
					// the error only when something other than
					// "not found" is at play.
					if errors.Is(readErr, os.ErrNotExist) {
						return nil
					}
					return readErr
				}
				if gitBlobSHA(current) != normalizeExpectedSHA(f.BaseSHA) {
					return fmt.Errorf("%s: %w", f.Path, daemon.ErrOverlayDrift)
				}
			}
			if rmErr := os.Remove(absPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("merge to_disk: remove %s: %w", absPath, rmErr)
			}
			s.reindexFile(absPath)
			return nil
		}
		if f.BaseSHA != "" {
			current, readErr := os.ReadFile(absPath)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
			if readErr == nil && gitBlobSHA(current) != normalizeExpectedSHA(f.BaseSHA) {
				return fmt.Errorf("%s: %w", f.Path, daemon.ErrOverlayDrift)
			}
			if readErr != nil && f.BaseSHA != "" {
				// File expected on disk but missing — treat as
				// drift so the caller re-reads + resubmits.
				return fmt.Errorf("%s: %w", f.Path, daemon.ErrOverlayDrift)
			}
		}
		perm := os.FileMode(0o644)
		if info, statErr := os.Stat(absPath); statErr == nil {
			perm = info.Mode().Perm()
		}
		if err := agents.AtomicWriteFile(absPath, []byte(f.Content), perm); err != nil {
			return fmt.Errorf("merge to_disk: write %s: %w", absPath, err)
		}
		s.reindexFile(absPath)
		return nil
	}
}

// handleCompareBranches runs the supplied query kind against two
// branches in the calling session and returns the delta. It mirrors
// handleCompareWithOverlay (base vs overlay) but the comparison is
// branch-vs-branch — both sides are shadow views layered on the
// shared immutable base graph.
func (s *Server) handleCompareBranches(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	a, err := req.RequireString("a")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	b, err := req.RequireString("b")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := daemon.ValidateBranchName(a); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := daemon.ValidateBranchName(b); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	kind, err := req.RequireString("kind")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	symID, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	depth := int(req.GetFloat("depth", 2))
	limit := int(req.GetFloat("limit", 50))
	opts := query.QueryOptions{Depth: depth, Limit: limit, Detail: "brief"}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aFiles, errA := s.overlays.FilesForBranchBounded(
		id,
		a,
		overlayRequestSnapshotMaxFiles,
		overlayRequestSnapshotMaxBytes,
	)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errA != nil {
		switch {
		case errors.Is(errA, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		case errors.Is(errA, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q not found", a)), nil
		default:
			return mcp.NewToolResultError(errA.Error()), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bFiles, errB := s.overlays.FilesForBranchBounded(
		id,
		b,
		overlayRequestSnapshotMaxFiles,
		overlayRequestSnapshotMaxBytes,
	)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errB != nil {
		switch {
		case errors.Is(errB, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register first"), nil
		case errors.Is(errB, daemon.ErrBranchNotFound):
			return mcp.NewToolResultError(fmt.Sprintf("overlay branch %q not found", b)), nil
		default:
			return mcp.NewToolResultError(errB.Error()), nil
		}
	}

	aIDs, aPaths, err := s.runBranchQuery(ctx, aFiles, kind, symID, opts)
	if err != nil {
		if ctxErr := requestContextError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(fmt.Sprintf("branch %q: %v", a, err)), nil
	}
	bIDs, bPaths, err := s.runBranchQuery(ctx, bFiles, kind, symID, opts)
	if err != nil {
		if ctxErr := requestContextError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(fmt.Sprintf("branch %q: %v", b, err)), nil
	}

	// Reuse the base/overlay diff primitive: diffIDSets(base, overlay)
	// returns (added=overlay-base, removed=base-overlay, common). With
	// (base=a, overlay=b): added=only_in_b, removed=only_in_a.
	onlyInB, onlyInA, common := diffIDSets(aIDs, bIDs)

	delta := map[string]any{
		"only_in_a":    onlyInA,
		"only_in_b":    onlyInB,
		"common":       common,
		"a_count":      len(aIDs),
		"b_count":      len(bIDs),
		"common_count": len(common),
	}
	resp := map[string]any{
		"session_id":      id,
		"a":               a,
		"b":               b,
		"kind":            kind,
		"id":              symID,
		"depth":           depth,
		"limit":           limit,
		"a_overlay_paths": aPaths,
		"b_overlay_paths": bPaths,
		"a_result":        aIDs,
		"b_result":        bIDs,
		"delta":           delta,
	}
	return mcp.NewToolResultText(jsonOK(resp)), nil
}

// runBranchQuery materialises an overlay view from the supplied
// branch file set and runs the query kind against it. Returns the
// stable-sorted result IDs and the overlay path coverage. Empty
// branches fall through to the base engine.
func (s *Server) runBranchQuery(ctx context.Context, files []daemon.OverlayFile, kind, symID string, opts query.QueryOptions) ([]string, []string, error) {
	eng := s.engine
	paths := []string{}
	if len(files) > 0 {
		layer, p, err := s.constructOverlayLayer(ctx, files)
		if err != nil {
			return nil, nil, err
		}
		if p != nil {
			paths = p
		}
		if layer != nil {
			view := graph.NewOverlaidView(s.graph, layer)
			if view != nil {
				eng = s.engine.WithReader(view)
			}
		}
	}
	ids := runQueryKind(eng, kind, symID, opts)
	return ids, paths, nil
}
