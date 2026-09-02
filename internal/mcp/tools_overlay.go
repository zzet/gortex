package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// registerOverlayTools wires the editor-overlay management family.
// They let an MCP-native IDE extension push, list, and tear down
// editor buffers without reaching for the parallel `/v1/overlay/*`
// HTTP surface. Once a session has overlays attached, every
// subsequent tools/call from the same MCP session sees the overlaid
// view (see overlay.go::wrapToolHandler).
//
// The functions are intentionally routed through addControlTool rather than
// addTool: the
// overlay middleware MUST NOT apply overlays before an overlay_push
// runs (or the new buffer would be re-evicted by the revert pass
// before it ever reached the user-facing query). So they register
// without bypassing safety gates, capture, or telemetry.
func (s *Server) registerOverlayTools() {
	s.addControlTool(
		mcp.NewTool("overlay_register",
			mcp.WithDescription("Register an editor-overlay session bound to the current MCP session. Subsequent overlay_push calls attach in-flight editor buffers; every tools/call from this session then sees the overlay merged on top of the saved-buffer graph view. Idempotent — calling twice with the same workspace is a no-op."),
			mcp.WithString("workspace_id", mcp.Description("Workspace slug the overlay belongs to. Optional; defaults to the session's bound workspace.")),
		),
		s.handleOverlayRegister,
	)
	s.addControlTool(
		mcp.NewTool("overlay_push",
			mcp.WithDescription("Push (or update) a single file overlay onto the current MCP session's overlay. The file at `path` is treated as if it contained `content` for the duration of every subsequent tools/call. Set `deleted: true` to mark a tombstone (queries see the file as missing). Drift detection: when `base_sha` is set, the daemon compares it to the on-disk git blob SHA at apply time and returns an overlay-drift error if they disagree."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute file path.")),
			mcp.WithString("content", mcp.Description("Editor-buffer content. Empty + deleted=false is allowed and means \"an empty file\".")),
			mcp.WithString("base_sha", mcp.Description("Git blob SHA the editor opened the file at. Empty disables drift detection.")),
			mcp.WithBoolean("deleted", mcp.Description("Mark the path as deleted (queries see no file).")),
		),
		s.handleOverlayPush,
	)
	s.addControlTool(
		mcp.NewTool("overlay_list",
			mcp.WithDescription("List every overlay file currently attached to the calling MCP session. Returns workspace_id, the count of files, and each overlay's path / content length / deleted flag / base_sha. The path and metadata are returned; content bytes are not, to keep the response small."),
		),
		s.handleOverlayList,
	)
	s.addControlTool(
		mcp.NewTool("overlay_delete",
			mcp.WithDescription("Remove a single overlay file from the calling MCP session's overlay. The next tools/call will see the saved-buffer view for that path."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute path of the overlay to remove.")),
		),
		s.handleOverlayDelete,
	)
	s.addControlTool(
		mcp.NewTool("overlay_drop",
			mcp.WithDescription("Tear down the calling MCP session's overlay entirely. Equivalent to calling overlay_delete on every attached path. The session remains live; a subsequent overlay_register / overlay_push starts a fresh overlay."),
		),
		s.handleOverlayDrop,
	)
	s.addControlTool(
		mcp.NewTool("overlay_keepalive",
			mcp.WithDescription("Refresh the calling MCP session's overlay idle timer without changing any overlay content. Cheaper than re-pushing buffer content when the editor needs to extend the lease (e.g. the user is debugging or paused on a breakpoint and won't push for a while). Returns the resulting expires_at / idle_seconds so the editor can schedule the next keepalive. The MCP session disconnect path already drops the overlay synchronously, so keepalive is only needed for genuine idle gaps below the disconnect-detection threshold."),
		),
		s.handleOverlayKeepalive,
	)

	// compare_with_overlay runs a query against both base and the
	// session's overlay view and returns the delta — the core
	// payoff of the shadow-graph design.
	s.registerOverlayDiffTool()

	// Branching surface: fork / branches / switch / merge / drop_branch
	// / compare_branches. Layers on top of the single-stream overlay
	// model — every existing tool continues to operate against the
	// session's active branch, so callers that never touch the
	// branching tools see the legacy behaviour unchanged.
	s.registerOverlayBranchTools()
}

// overlaySessionID returns the calling request's overlay cohort id —
// SessionIDFromContext by default, or the X-Gortex-Overlay-Session
// override when one was set (see WithOverlayCohortID) — or a
// structured MCP error result when neither is present. Used by every
// overlay_* handler.
func (s *Server) overlaySessionID(ctx context.Context) (string, *mcp.CallToolResult) {
	id := OverlayCohortIDFromContext(ctx)
	if id == "" {
		return "", mcp.NewToolResultError("overlay tools require an MCP session — connect via the daemon or set X-Mcp-Session-Id")
	}
	if s.overlays == nil {
		return "", mcp.NewToolResultError("overlay support is not enabled on this server")
	}
	return id, nil
}

func (s *Server) handleOverlayRegister(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	workspace := req.GetString("workspace_id", "")
	if workspace == "" {
		// Default to the session's bound workspace if any. The bind
		// is filled by the workspace-handshake flow; sessions that
		// haven't been handshaked are bound to the empty workspace.
		if ws, _, _ := s.sessionScope(ctx); ws != "" {
			workspace = ws
		}
	}
	if err := s.overlays.RegisterWithID(id, workspace); err != nil {
		if errors.Is(err, daemon.ErrSessionExists) {
			return mcp.NewToolResultError("overlay session is already registered for a different workspace; call overlay_drop first"), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"session_id":   id,
		"workspace_id": workspace,
	})), nil
}

func (s *Server) handleOverlayPush(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	overlay := daemon.OverlayFile{
		Path:    path,
		Content: req.GetString("content", ""),
		BaseSHA: req.GetString("base_sha", ""),
		Deleted: req.GetBool("deleted", false),
	}
	// Idempotent register-on-push: an extension that calls
	// overlay_push without a prior overlay_register lands on the
	// session's default workspace. This matches the HTTP path's
	// implicit-register behaviour (POST /v1/overlay/sessions with
	// an explicit session_id is the canonical bind path; the
	// fallback exists so test harnesses don't need both calls).
	if !s.overlays.Has(id) {
		workspace, _, _ := s.sessionScope(ctx)
		_ = s.overlays.RegisterWithID(id, workspace)
	}
	// Drift check runs on apply (see overlay.go::overlaySHAMatches);
	// passing nil here lets the manager accept the push and defers
	// the check to the tool call that needs the overlay. Pushing a
	// known-stale overlay is allowed because the editor may push
	// the latest buffer before processing a sibling tool's edit;
	// the apply pass surfaces the drift then.
	if err := s.overlays.Push(id, overlay, nil); err != nil {
		switch {
		case errors.Is(err, daemon.ErrSessionNotFound):
			return mcp.NewToolResultError("overlay session has been dropped — call overlay_register before pushing"), nil
		case errors.Is(err, daemon.ErrOverlayDrift):
			return mcp.NewToolResultError(err.Error()), nil
		default:
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	// Invalidate the cached parsed-overlay layer for this session so
	// the next tools/call re-parses with the fresh buffer state.
	s.overlayCacheInvalidate(id)
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"path":         overlay.Path,
		"content_size": len(overlay.Content),
		"deleted":      overlay.Deleted,
		"base_sha":     overlay.BaseSHA,
	})), nil
}

func (s *Server) handleOverlayList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	// StatusFor + Files (rather than SnapshotFor) so listing the
	// overlay doesn't accidentally bump LastUsed. Polling
	// overlay_list every second should NOT extend the lease;
	// otherwise a misconfigured editor could keep a dropped MCP
	// session's overlay alive forever just by polling.
	status, statusErr := s.overlays.StatusFor(id)
	if statusErr != nil {
		if errors.Is(statusErr, daemon.ErrSessionNotFound) {
			return mcp.NewToolResultText(jsonOK(map[string]any{
				"session_id":   id,
				"workspace_id": "",
				"count":        0,
				"files":        []any{},
				"expired":      true,
			})), nil
		}
		return mcp.NewToolResultError(statusErr.Error()), nil
	}
	rawFiles, _ := s.overlays.Files(id)
	files := make([]daemon.OverlayFile, 0, len(rawFiles))
	for _, f := range rawFiles {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{
			"path":         f.Path,
			"content_size": len(f.Content),
			"deleted":      f.Deleted,
			"base_sha":     f.BaseSHA,
		})
	}
	resp := map[string]any{
		"session_id":       id,
		"workspace_id":     status.WorkspaceID,
		"count":            len(out),
		"files":            out,
		"created_at":       status.Created.UTC().Format(time.RFC3339),
		"last_used_at":     status.LastUsed.UTC().Format(time.RFC3339),
		"idle_seconds":     status.IdleSeconds,
		"idle_ttl_seconds": status.IdleTTLSeconds,
	}
	if !status.ExpiresAt.IsZero() {
		resp["expires_at"] = status.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return mcp.NewToolResultText(jsonOK(resp)), nil
}

func (s *Server) handleOverlayDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.overlays.Delete(id, path); err != nil {
		if errors.Is(err, daemon.ErrSessionNotFound) {
			return mcp.NewToolResultError("overlay session has been dropped"), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	s.overlayCacheInvalidate(id)
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"path": path,
		"ok":   true,
	})), nil
}

func (s *Server) handleOverlayDrop(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	s.overlays.Drop(id)
	s.overlayCacheInvalidate(id)
	return mcp.NewToolResultText(jsonOK(map[string]any{
		"session_id": id,
		"ok":         true,
	})), nil
}

// handleOverlayKeepalive refreshes the session's idle timer and
// returns fresh expires_at / idle_seconds metadata. Returns an
// explicit "session expired" error when the daemon already reaped
// the session (or never knew about it) — the editor must
// overlay_register + re-push to recover.
func (s *Server) handleOverlayKeepalive(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := s.overlaySessionID(ctx)
	if errRes != nil {
		return errRes, nil
	}
	if err := s.overlays.Touch(id); err != nil {
		if errors.Is(err, daemon.ErrSessionNotFound) {
			return mcp.NewToolResultError("overlay session has been dropped or never registered — call overlay_register before pushing"), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	status, statusErr := s.overlays.StatusFor(id)
	if statusErr != nil {
		// Race: session was dropped between Touch and StatusFor —
		// extremely unlikely, but surface honestly.
		return mcp.NewToolResultError(statusErr.Error()), nil
	}
	resp := map[string]any{
		"session_id":       id,
		"workspace_id":     status.WorkspaceID,
		"last_used_at":     status.LastUsed.UTC().Format(time.RFC3339),
		"idle_seconds":     status.IdleSeconds,
		"idle_ttl_seconds": status.IdleTTLSeconds,
	}
	if !status.ExpiresAt.IsZero() {
		resp["expires_at"] = status.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return mcp.NewToolResultText(jsonOK(resp)), nil
}

// jsonOK marshals v to a compact JSON string. Tool handlers use it to
// build a text body that's both machine-readable and gcx-friendly.
func jsonOK(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
	}
	return string(b)
}
