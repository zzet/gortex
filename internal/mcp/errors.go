package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// Structured MCP error responses.
//
// MCP tool handlers historically returned errors as text content with
// no machine-readable code, leaving agents to regex-parse English
// strings to decide whether to retry, reauth, or surface to the user.
// Iteration 1 introduces a small set of stable error codes for the
// failure modes the workspace boundary checks introduce, plus the
// multi-server routing that can fail in distinct ways the agent
// should react to differently.
//
// The codes live here (one place to grep) so cross-package code that
// surfaces MCP errors can refer to a single canonical list. The
// payload is JSON-encoded into a TextContent block so existing
// transports keep working — the `is_error` flag on CallToolResult
// signals the structured shape, and a leading `{"error_code": ...}`
// is what a smart client decodes.

// ErrorCode is the stable identifier surfaced under
// `error_code` in tool error payloads. Adding new codes is OK; renaming
// or removing an existing one is a wire-contract break.
type ErrorCode string

const (
	// ErrCodeWorkspaceUnknown — the requested workspace doesn't
	// exist on this server. Routing guidance: the daemon should fall
	// back to the "default" server, surface a hint to the user, or
	// fail.
	ErrCodeWorkspaceUnknown ErrorCode = "workspace_unknown"

	// ErrCodeProjectUnknown — workspace exists but the requested
	// project slug doesn't (e.g. monorepo missing the named project).
	ErrCodeProjectUnknown ErrorCode = "project_unknown"

	// ErrCodeRepoNotTracked — the cwd wasn't found in any tracked
	// repo's root tree. Used by the daemon's MCP front-door.
	ErrCodeRepoNotTracked ErrorCode = "repo_not_tracked"

	// ErrCodeRepoLoading — the cwd belongs to a configured repository the
	// daemon has not finished loading since it started. Retryable: tools
	// start working in place once the repository registers, with no
	// reconnect and no change to tracking configuration.
	ErrCodeRepoLoading ErrorCode = "repo_loading"

	// ErrCodeRepoUnavailable — the cwd belongs to a configured repository
	// whose load after a start ended without registering it (a failed
	// index, a taken prefix) or whose path is gone. Not retryable as is;
	// the error's reason names what to repair.
	ErrCodeRepoUnavailable ErrorCode = "repo_unavailable"
	// ErrCodePrefixCollision names an already-owned repository namespace.
	ErrCodePrefixCollision ErrorCode = "prefix_collision"

	// ErrCodeSymbolNotFound — the requested symbol id is not in the
	// index. Recoverable: the agent can search for it instead.
	ErrCodeSymbolNotFound ErrorCode = "symbol_not_found"

	// ErrCodeSymbolNotIndexed — the symbol's file exists and is in scope but
	// carries no indexed symbols, so semantic rename cannot see it.
	ErrCodeSymbolNotIndexed ErrorCode = "symbol_not_indexed"

	// ErrCodeFileNotIndexed — no symbols are indexed for the requested
	// file (new / ignored / unsupported language). Recoverable.
	ErrCodeFileNotIndexed ErrorCode = "file_not_indexed"

	// ErrCodeRouteUnresolved — the multi-server router couldn't
	// pick a server for the request (no servers.toml entry covers
	// the workspace, no roster claims it, no default). Retriable
	// after the user adjusts servers.toml.
	ErrCodeRouteUnresolved ErrorCode = "route_unresolved"

	// ErrCodeProxyUpstream — the router proxied to a remote server
	// and the upstream returned a non-2xx. error.data carries
	// `upstream_status` and `upstream_body` for debugging.
	ErrCodeProxyUpstream ErrorCode = "proxy_upstream"

	// ErrCodeOverlayDrift — an overlay push carried a BaseSHA that
	// disagrees with the file's current on-disk SHA. The client
	// should re-read the file and resubmit a fresh overlay.
	ErrCodeOverlayDrift ErrorCode = "overlay_drift"

	// ErrCodeOverlaySessionUnknown — the overlay session ID isn't
	// registered (expired, dropped, or never existed).
	ErrCodeOverlaySessionUnknown ErrorCode = "overlay_session_unknown"

	// ErrCodeInvalidArgument — the tool args failed validation.
	// Generic catch-all so per-tool handlers don't have to invent
	// their own code for "you passed nonsense".
	ErrCodeInvalidArgument ErrorCode = "invalid_argument"

	// ErrCodeToolBlockedByMode — the call was refused because the
	// session's runtime mode forbids it (an editing tool while the
	// session is in planning mode). Switch with set_planning_mode.
	ErrCodeToolBlockedByMode ErrorCode = "tool_blocked_by_mode"

	// ErrCodeToolOutOfPhase — an active workflow does not allow this
	// tool in the current phase. The error data carries the current
	// phase and the allowed-tool list.
	ErrCodeToolOutOfPhase ErrorCode = "tool_out_of_phase"

	// ErrCodeLocalizationComplete — an explicit localization-only request
	// already produced its terminal evidence. The caller must respond, or make
	// the one exact read named by data.completion when that state is returned.
	ErrCodeLocalizationComplete ErrorCode = "localization_complete"

	// ErrCodeLocalizationTerminal — a localization contract is already in its
	// terminal answer_ready state. The original successful response contains
	// the evidence; retrying navigation cannot add evidence and is not
	// retriable. This is distinct from LocalizationComplete, which also covers
	// non-terminal refinement and in-flight states.
	ErrCodeLocalizationTerminal ErrorCode = "localization_terminal"
)

// StructuredError is the JSON shape encoded into the TextContent
// block of an MCP error. Smart clients parse the leading `{` to read
// it; older clients see the human-readable Message field as a
// fallback.
type StructuredError struct {
	ErrorCode ErrorCode      `json:"error_code"`
	Message   string         `json:"message"`
	Retriable bool           `json:"retriable,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// NewStructuredErrorResult returns a CallToolResult whose IsError is
// true and whose single TextContent block is the JSON form of err.
// Use this from any MCP handler that wants to surface a typed
// failure instead of a free-form string.
func NewStructuredErrorResult(err StructuredError) *mcp.CallToolResult {
	return newStructuredErrorResult(err, false)
}

// newStructuredErrorResult preserves the exported constructor's historical
// wire shape while allowing protocol contracts that require an explicit
// retriable boolean to opt in locally.
func newStructuredErrorResult(err StructuredError, explicitRetriable bool) *mcp.CallToolResult {
	if err.ErrorCode == "" {
		err.ErrorCode = ErrCodeInvalidArgument
	}
	if err.Message == "" {
		err.Message = string(err.ErrorCode)
	}
	var (
		body []byte
		mErr error
	)
	if explicitRetriable {
		body, mErr = json.Marshal(struct {
			ErrorCode ErrorCode      `json:"error_code"`
			Message   string         `json:"message"`
			Retriable bool           `json:"retriable"`
			Data      map[string]any `json:"data,omitempty"`
		}{
			ErrorCode: err.ErrorCode,
			Message:   err.Message,
			Retriable: err.Retriable,
			Data:      err.Data,
		})
	} else {
		body, mErr = json.Marshal(err)
	}
	if mErr != nil {
		// Fallback to plain text — never let a marshal failure mask
		// the actual error.
		return mcp.NewToolResultError(fmt.Sprintf("%s: %s", err.ErrorCode, err.Message))
	}
	res := mcp.NewToolResultError(string(body))
	res.IsError = true
	return res
}
