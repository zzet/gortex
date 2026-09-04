// Package hooks provides Claude Code hook handlers for Gortex.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/toolref"
)

// HookInput is the JSON structure Claude Code sends to PreToolUse hooks via stdin.
type HookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
	CWD           string         `json:"cwd"`
	// SessionID identifies the Claude Code session. Used to key the
	// per-session state store (consult-unlock marker, nudge streak).
	SessionID string `json:"session_id"`
	// PromptID is accepted when a host supplies it. Current Claude Code hook
	// schemas guarantee tool_use_id instead, so terminal state primarily uses
	// the per-turn token correlated through that field.
	PromptID string `json:"prompt_id"`
	// AgentID is present inside subagents and isolates their terminal state from
	// the parent agent sharing the same Claude session.
	AgentID string `json:"agent_id"`
	// PermissionMode is the host's active permission posture
	// ("default" / "acceptEdits" / "plan" / "bypassPermissions" / "auto").
	// Drives the auto-approve branch for Gortex's own MCP tools.
	PermissionMode string `json:"permission_mode"`
}

// HookOutput is the JSON structure the hook writes to stdout.
type HookOutput struct {
	Decision           string              `json:"decision,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	Continue           *bool               `json:"continue,omitempty"`
	StopReason         string              `json:"stopReason,omitempty"`
	SystemMessage      string              `json:"systemMessage,omitempty"`
	HookSpecificOutput *HookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// HookSpecificOutput carries the permission decision and/or additional context.
type HookSpecificOutput struct {
	HookEventName            string         `json:"hookEventName"`
	AdditionalContext        string         `json:"additionalContext,omitempty"`
	PermissionDecision       string         `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
}

// enrichResult carries both the context text and whether the call should be blocked.
type enrichResult struct {
	context string
	deny    bool
	reason  string
}

// gortexMCPToolPrefix is the namespace Claude Code gives Gortex's own
// MCP tools (server name "gortex"). A tool call whose name starts with
// this prefix is a graph query — the in-process hook sees it like any
// other tool call, which is what lets the consult-unlock handshake and
// the adaptive-nudge streak reset work without an external signal.
const gortexMCPToolPrefix = "mcp__gortex__"

// runPreToolUse is the bytes-accepting helper the generic Run dispatcher
// and the Codex bridge share. In ModeEnrich the deny branch is downgraded
// to an additionalContext message — the agent is informed about the graph
// alternative but the original call still runs and PostToolUse can layer
// graph context on the actual output.
func runPreToolUse(data []byte, gortexPort int, mode Mode) {
	runPreToolUseForHost(data, gortexPort, mode, preToolUseClaude)
}

func runPreToolUseForHost(data []byte, gortexPort int, mode Mode, host preToolUseHost) {
	started := time.Now()
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}

	if input.HookEventName != "PreToolUse" {
		return
	}

	// Terminal enforcement is deliberately the first policy branch. It is a
	// local marker lookup, so it neither waits for the daemon nor gets bypassed
	// by permissive permission modes. A new user prompt clears the marker.
	if enforceLocalizationTerminalPreToolUse(input, started) {
		return
	}
	terminalTurn, terminalTurnReady := currentLocalizationTurnState(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	terminalIdentity := terminalTurn.Identity
	localizationAuthToken := ""
	if terminalTurnReady {
		// Correlate the current turn with this exact tool invocation. The nonce is
		// injected into the MCP request, then the server publishes a one-shot
		// answer_ready receipt under it immediately before returning. PostToolUse
		// consumes both the snapshot and receipt, so stripped response metadata,
		// delayed events, and visible JSON cannot arm terminal state.
		if authToken, snapshotReady := snapshotLocalizationToolUseWithAuth(input, terminalIdentity); snapshotReady {
			localizationAuthToken = authToken
		}
	}
	updatedInput := localizationPreToolUpdatedInput(input, localizationAuthToken, terminalTurn.ProblemStatement)

	// Record what this call is about to rewrite, so a Stop-hook briefing can
	// tell this session's edits apart from a sibling session's on a shared
	// checkout. Placed here deliberately: after the terminal deny above (a
	// refused call is never credited) but ahead of both gates below, because
	// MultiEdit / NotebookEdit are outside preToolUsePolicyTools and every
	// Gortex MCP write short-circuits at the auto-approve branch under a
	// permissive permission mode. For a tool that writes nothing this returns
	// before touching disk, so the no-op contract below still holds.
	recordSessionWriteTargets(input)

	// The installed matcher is deliberately broad so terminal state can stop
	// any tool. With no marker, tools outside the historical access-policy
	// matcher must be an immediate no-op: no daemon probe, classification,
	// enrichment, or telemetry I/O beyond the write-target record above.
	if !preToolUsePolicyTool(input.ToolName) {
		return
	}

	daemonUp := daemonReachableFn()
	emitted := false
	defer func() {
		logHookEffectiveness("PreToolUse", emitted, daemonUp, hookAlternationSegmentCount(input), time.Since(started))
	}()

	isGortexMCP := isGortexMCPToolName(input.ToolName)

	// Auto-approve: under a permissive permission mode the host has
	// already granted blanket approval, so Gortex's own MCP tools
	// should ride along with it. This branch is independent of Mode —
	// it fires in any posture — and runs before the per-tool enrich
	// switch and the other modes' logic so a gortex tool is never
	// processed further.
	if isGortexMCP && isPermissivePermissionMode(input.PermissionMode) {
		hso := &HookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "allow",
			PermissionDecisionReason: "[Gortex] auto-approved: Gortex MCP graph tool under a permissive permission mode.",
		}
		// The read-discipline nudge still rides along as soft context —
		// a permissive permission mode means low friction, not "stop
		// reminding me my full-body read is expensive". Never a hard
		// deny here: auto-approve has already promised to allow the call.
		// Skipped during a daemon outage: coaching the shape of a call
		// that is about to fail on transport is noise (#486).
		if daemonUp {
			if adv := gortexReadNudge(input.ToolName, input.ToolInput); adv != "" {
				hso.AdditionalContext = adv
			}
		}
		if updatedInput != nil {
			hso.UpdatedInput = updatedInput
		}
		emitted = hso.AdditionalContext != "" || hso.PermissionDecisionReason != ""
		emitPreToolUseForHost(host, input.PermissionMode, HookOutput{HookSpecificOutput: hso})
		return
	}

	// Consult-unlock handshake: any Gortex MCP tool call records that
	// the agent has consulted the graph this session. The hook sees the
	// MCP call in-process, so the marker is fully self-contained. The
	// call itself is a no-op pass-through — nothing to enrich.
	if mode == ModeConsultUnlock && isGortexMCP {
		markGraphConsulted(input.SessionID)
		if updatedInput != nil {
			emitPreToolUseForHost(host, input.PermissionMode, HookOutput{HookSpecificOutput: &HookSpecificOutput{
				HookEventName: "PreToolUse",
				UpdatedInput:  updatedInput,
			}})
		}
		return
	}

	// Daemon outage: per-call enforcement stands down (#486). Every deny and
	// advisory the enrichment below can produce mandates Gortex MCP tools;
	// while the daemon cannot serve them that guidance deadlocks the agent —
	// instructed to use tools that cannot answer and flagged for using the
	// tools that can. Degrade the way the SessionStart briefing does
	// (rule-only enforcement): stay quiet per call, surface one notice per
	// session so a mid-session outage is explained, and keep the
	// localization input passthrough intact. The terminal deny above stays
	// live on purpose — it is daemon-independent and carries its own answer.
	if !daemonUp {
		hso := &HookSpecificOutput{HookEventName: "PreToolUse"}
		if updatedInput != nil {
			hso.UpdatedInput = updatedInput
		}
		if hookCallTargetsTrackedRepo(input.ToolName, input.ToolInput, input.CWD) {
			hso.AdditionalContext = daemonDownNoticeOnce(input.SessionID)
		}
		if hso.AdditionalContext == "" && hso.UpdatedInput == nil {
			return
		}
		emitted = hso.AdditionalContext != ""
		emitPreToolUseForHost(host, input.PermissionMode, HookOutput{HookSpecificOutput: hso})
		return
	}

	result := applyMode(input, isGortexMCP, mode, enrich(input, gortexPort))

	if result.context == "" && !result.deny {
		if updatedInput != nil {
			emitPreToolUseForHost(host, input.PermissionMode, HookOutput{HookSpecificOutput: &HookSpecificOutput{
				HookEventName: "PreToolUse",
				UpdatedInput:  updatedInput,
			}})
		}
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName: "PreToolUse",
		},
	}
	if updatedInput != nil {
		output.HookSpecificOutput.UpdatedInput = updatedInput
	}

	if result.deny {
		output.HookSpecificOutput.PermissionDecision = "deny"
		output.HookSpecificOutput.PermissionDecisionReason = result.reason
	} else {
		output.HookSpecificOutput.AdditionalContext = result.context
	}

	emitted = true
	emitPreToolUseForHost(host, input.PermissionMode, output)
}

// enforceLocalizationTerminalPreToolUse applies only the local terminal
// contract and reports whether it emitted a deny. Keeping this seam separate
// lets hosts with specialized per-tool behavior (notably Codex) enforce the
// same all-tool terminal policy before dispatching to those handlers.
func enforceLocalizationTerminalPreToolUse(input HookInput, started time.Time) bool {
	terminalTurn, ready := currentLocalizationTurnState(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ready {
		return false
	}
	marker, marked := localizationTerminalMarkerFor(terminalTurn.Identity)
	if !marked {
		return false
	}

	reason := ""
	switch {
	case !marker.Advisory:
		reason = localizationTerminalDenyReason
	case localizationNavigationTool(input.ToolName):
		reason = localizationAdvisoryDenyReason
	case localizationRedirectedHostTool(input.ToolName):
		// Left to the access policy this deny becomes "call a Gortex graph
		// tool instead", and the navigation branch then refuses that call.
		reason = localizationAdvisoryDenyReason
	}
	if reason == "" {
		return false
	}
	if answer := strings.TrimSpace(marker.FinalResponse); answer != "" {
		reason += "\n\n" + answer
	}
	emitPreToolUse(HookOutput{HookSpecificOutput: &HookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	}})
	localizationTerminalTelemetry("denied", true, started)
	return true
}

func hookAlternationSegmentCount(input HookInput) int {
	var pattern string
	switch input.ToolName {
	case "Grep":
		pattern, _ = input.ToolInput["pattern"].(string)
	case "Bash":
		command, _ := input.ToolInput["command"].(string)
		classification := classifyBashCommand(command)
		if classification.Action == BashActionGrepLike || classification.Action == BashActionFindName {
			pattern = classification.Pattern
		}
	}
	if pattern == "" || !strings.Contains(pattern, "|") {
		return 0
	}
	return len(splitAlternation(pattern))
}

// applyMode adjusts a raw enrich result according to the active posture.
// Shared by the Claude Code PreToolUse handler (runPreToolUse) and the Pi
// bridge (RunPi) so the deny / enrich / consult-unlock / nudge semantics
// stay identical across agents. Modes are mutually exclusive — only one
// branch ever fires for a given mode.
//
//   - ModeDeny (default): result passes through unchanged — a deny stays a
//     hard deny.
//   - ModeEnrich: a deny is downgraded to soft additionalContext so the
//     original call still runs.
//   - ModeConsultUnlock: a deny stays hard until the agent has queried the
//     graph once this session, then downgrades to soft context.
//   - ModeAdaptiveNudge: per-call denial is replaced with a single
//     soft-deny per burst of non-symbolic fallback calls.
func applyMode(input HookInput, isGortexMCP bool, mode Mode, result enrichResult) enrichResult {
	switch mode {
	case ModeEnrich:
		if result.deny {
			return enrichResult{context: downgradeReason(result)}
		}
	case ModeConsultUnlock:
		if result.deny {
			if loadSessionState(input.SessionID).GraphConsulted {
				return enrichResult{context: downgradeReason(result)}
			}
			result.reason = consultUnlockReason(result.reason)
		}
	case ModeAdaptiveNudge:
		return adaptiveNudge(input, isGortexMCP, result)
	}
	return result
}

// markGraphConsulted flips the per-session consult-unlock marker idempotently; a blank session id is a no-op.
func markGraphConsulted(sessionID string) {
	if sessionID == "" {
		return
	}
	st := loadSessionState(sessionID)
	if !st.GraphConsulted {
		st.GraphConsulted = true
		saveSessionState(sessionID, st)
	}
}

type preToolUseHost uint8

const (
	preToolUseClaude preToolUseHost = iota
	preToolUseCodex
)

// normalizePreToolUseOutput applies the host-specific rewrite contract without
// broadening the user's permission policy. Claude Code can ask while carrying
// updatedInput; Codex requires allow for every rewrite and does not support ask.
func normalizePreToolUseOutput(host preToolUseHost, permissionMode string, output HookOutput) HookOutput {
	hso := output.HookSpecificOutput
	if hso == nil || hso.HookEventName != "PreToolUse" {
		return output
	}

	normalized := *hso
	if normalized.UpdatedInput == nil {
		// Codex rejects allow when there is no replacement input. Removing it
		// restores the host's normal permission flow instead of broadening it.
		if host == preToolUseCodex && normalized.PermissionDecision == "allow" {
			normalized.PermissionDecision = ""
			normalized.PermissionDecisionReason = ""
		}
		output.HookSpecificOutput = &normalized
		return output
	}

	switch normalized.PermissionDecision {
	case "":
		if host == preToolUseCodex || isPermissivePermissionMode(permissionMode) {
			normalized.PermissionDecision = "allow"
		} else {
			normalized.PermissionDecision = "ask"
		}
	case "allow":
		// Valid for both hosts when paired with updatedInput.
	case "ask":
		if host == preToolUseCodex {
			normalized.PermissionDecision = "deny"
			normalized.PermissionDecisionReason = "[Gortex] Codex cannot safely apply an ask rewrite."
			normalized.UpdatedInput = nil
		}
	default:
		// A deny, defer, or future non-rewrite decision keeps its policy but
		// cannot carry replacement input.
		normalized.UpdatedInput = nil
	}
	output.HookSpecificOutput = &normalized
	return output
}

func emitPreToolUseForHost(host preToolUseHost, permissionMode string, output HookOutput) {
	emitPreToolUse(normalizePreToolUseOutput(host, permissionMode, output))
}

// emitPreToolUse marshals a PreToolUse HookOutput to stdout. A marshal
// failure is swallowed — a hook must never block the host agent's flow.
func emitPreToolUse(output HookOutput) {
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// downgradeReason picks the human text to surface when a deny is
// softened to additionalContext. An advisory rendering of the same
// message wins when the enrichment supplied one: the call is about to
// proceed, so telling the agent it was BLOCKED and how to unset a flag
// would be two false statements in one line. Shared by ModeEnrich and
// ModeConsultUnlock.
func downgradeReason(result enrichResult) string {
	if result.context != "" {
		return result.context
	}
	return result.reason
}

// consultUnlockReason augments a hard deny reason with the one-line
// instruction for unlocking fallback file reads under ModeConsultUnlock.
func consultUnlockReason(reason string) string {
	const unlock = "\n[Gortex] consult-unlock: query the Gortex graph once (any mcp__gortex__ tool) to unlock fallback file reads for the rest of this session."
	if reason == "" {
		return strings.TrimPrefix(unlock, "\n")
	}
	return reason + unlock
}

// isPermissivePermissionMode reports whether the host's permission mode
// is one under which Gortex's own MCP tools should be auto-approved.
//
// Implemented as an allowlist — only "acceptEdits" and "auto" return
// true. Everything else (including "bypassPermissions", "default",
// "plan", and the empty string) returns false, so an unknown future
// permission mode is never auto-approved by default.
func isPermissivePermissionMode(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "acceptEdits", "auto":
		return true
	default:
		return false
	}
}

// nudgeThreshold is the number of consecutive non-symbolic fallback
// tool calls that triggers a single soft-deny under ModeAdaptiveNudge.
const nudgeThreshold = 3

// adaptiveNudge implements the ModeAdaptiveNudge posture. Rather than
// denying every Read / Grep / Glob fallback, it tracks a per-session
// streak of non-symbolic calls and soft-denies exactly once when the
// streak reaches nudgeThreshold, then resets the streak so the very
// next call proceeds. A symbolic or Gortex MCP call resets the streak.
//
// result is the outcome of the per-tool enrich switch. result.deny is
// the signal that the current call is a non-symbolic fallback the
// classifiers flagged; anything else is treated as symbolic.
func adaptiveNudge(input HookInput, isGortexMCP bool, result enrichResult) enrichResult {
	// A Gortex MCP call (or any call enrich didn't flag as a deny) is
	// symbolic enough — reset the streak and let it proceed. Any
	// advisory context still rides along.
	if isGortexMCP || !result.deny {
		st := loadSessionState(input.SessionID)
		if st.NonSymbolicStreak != 0 {
			st.NonSymbolicStreak = 0
			saveSessionState(input.SessionID, st)
		}
		return enrichResult{context: result.context}
	}

	// Non-symbolic fallback call: extend the streak.
	st := loadSessionState(input.SessionID)
	st.NonSymbolicStreak++

	if st.NonSymbolicStreak >= nudgeThreshold {
		// Fire the reminder once, then reset so the next identical
		// call is allowed through — the nudge is per-burst, not
		// per-call.
		st.NonSymbolicStreak = 0
		saveSessionState(input.SessionID, st)
		logHookDecision(input.ToolName, "", DecisionNudged, 0, 0)
		return enrichResult{
			deny:   true,
			reason: nudgeReason(downgradeReason(result)),
		}
	}

	// Below threshold — record the streak and let the call through
	// with whatever advisory context enrich produced.
	saveSessionState(input.SessionID, st)
	return enrichResult{context: result.context}
}

// nudgeReason builds the soft-deny message shown when the adaptive
// nudge fires. It keeps the underlying graph-tool guidance and adds the
// one-shot notice so the agent knows the next call will proceed.
func nudgeReason(guidance string) string {
	var b strings.Builder
	b.WriteString("[Gortex] You've made several raw file-search calls in a row. ")
	b.WriteString("Call `explore` for the task, then use `search`, `read`, or `relations`; do not continue the raw search/read loop.\n")
	b.WriteString(toolref.MCPRequiredLine())
	if guidance != "" {
		b.WriteString(guidance)
		if !strings.HasSuffix(guidance, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("This reminder fires once — the next call will proceed.")
	return b.String()
}

func enrich(input HookInput, port int) enrichResult {
	// A call that touches no tracked repo has no graph to redirect to, so the
	// access policy has nothing true to say about it. Checked once here rather
	// than per-enrichment so every host tool the policy covers is gated the
	// same way, and so a Gortex MCP call — which is not repo-scoped — is not.
	if !isGortexMCPToolName(input.ToolName) &&
		!hookCallTargetsTrackedRepo(input.ToolName, input.ToolInput, input.CWD) {
		return enrichResult{}
	}
	switch input.ToolName {
	case "Read":
		return enrichRead(input.ToolInput, input.CWD)
	case "Grep":
		return enrichGrep(input.ToolInput, port, input.CWD)
	case "Glob":
		return enrichGlob(input.ToolInput, input.CWD)
	case "Task":
		return enrichTask(input.ToolInput, port)
	case "Bash":
		return enrichBash(input.ToolInput, input.CWD)
	case "Edit":
		return enrichEdit(input.ToolInput, input.CWD)
	case "Write":
		return enrichWrite(input.ToolInput, input.CWD)
	case gortexReadFileTool, gortexEditingContextTool, gortexCompactReadTool:
		return enrichGortexRead(input.ToolName, input.ToolInput)
	default:
		return enrichResult{}
	}
}

// enrichRead blocks reads of indexed source files and suggests graph tools.
// Ranged reads are included: indexed source must stay on the graph-aware read path.
func enrichRead(toolInput map[string]any, cwd string) enrichResult {
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}

	// Skip non-source files — allow reading .md, .yaml, .json, etc.
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}

	st := queryFileIndexScope(cwd, filePath)

	// If the file is indexed, BLOCK the read and provide graph alternatives.
	if st.Indexed {
		var reason strings.Builder
		fmt.Fprintf(&reason, "[Gortex] BLOCKED: Read of %s (%d symbols indexed). Call `explore` first, then use `read` instead:\n", filePath, st.Count)
		reason.WriteString("  - `read(target:{symbol:\"<id>\"})` — one symbol\n")
		reason.WriteString("  - `read(target:{symbols:[\"<id>\"]})` — several symbols\n")
		reason.WriteString("  - `read(operation:\"editing_context\", target:{file:\"<path>\"})` — full editing context\n")
		reason.WriteString(gcxTip)
		reason.WriteString(toolref.MCPRequiredLine())
		// Naming the replacement tool without showing what the graph holds
		// makes the caller spend its next turn asking for what the hook can
		// already see. The outline is additive: with no answer the deny reads
		// exactly as it did before.
		reason.WriteString(readOutlineEvidence(cwd, filePath))

		return enrichResult{
			deny:   true,
			reason: reason.String(),
		}
	}

	// Stay silent when the graph has no answer to redirect to: the file is
	// unindexable by design, it is held but defines no symbols, or no usable
	// verdict came back at all.
	if st.noGraphAnswer() {
		return enrichResult{}
	}

	// Tracked and indexable, just not indexed yet — allow with advisory.
	var guidance strings.Builder
	guidance.WriteString("[Gortex] Use `explore` first, then `read` for indexed source:\n")
	guidance.WriteString("  - one symbol: `read(target:{symbol:\"<id>\"})`\n")
	guidance.WriteString("  - before editing: `read(operation:\"editing_context\", target:{file:\"<path>\"})`\n")
	guidance.WriteString("  - file overview: `read(operation:\"summary\", target:{file:\"<path>\"})`\n")
	guidance.WriteString(gcxTip)
	guidance.WriteString(toolref.MCPRequiredLine())

	return enrichResult{context: guidance.String()}
}

// gcxTip is appended to every Read/Grep/Glob redirect so agents see the
// compact-output option at the exact moment they are picking a tool call.
// Public tools nest response shaping under output.
const gcxTip = "  - For compact output, pass `output:{format:\"gcx\"}`.\n"

// isNarrowRead returns true if the Read has offset+limit targeting a small range,
// indicating the agent is reading a specific section for editing.
func isNarrowRead(toolInput map[string]any) bool {
	_, hasOffset := toolInput["offset"]
	_, hasLimit := toolInput["limit"]

	if hasOffset && hasLimit {
		// Any offset+limit read is considered narrow (the agent knows what it wants).
		return true
	}

	if hasOffset {
		// Offset alone means "read from this line" — likely targeted.
		return true
	}

	if hasLimit {
		// Limit alone — check if it's a small read.
		if limitVal, ok := toFloat64(toolInput["limit"]); ok && limitVal <= 50 {
			return true
		}
	}

	return false
}

// grepProbeTimeout caps the search_symbols probe so hooks never slow Grep.
const grepProbeTimeout = 200 * time.Millisecond

// grepSymbolHit mirrors daemon.SymbolHit but lives in this package so the
// probe interface can be swapped for tests without dragging the full
// daemon-protocol types into hook unit tests.
type grepSymbolHit struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
}

// errProbeTimeout is the sentinel returned by the probe when the daemon
// didn't reply within grepProbeTimeout. Differentiates from "daemon
// unreachable" / "no hits" so telemetry can record it correctly.
var errProbeTimeout = errors.New("probe timeout")

// errDaemonUnreachable is returned when the daemon socket can't be dialed
// (no daemon, wrong path, permissions). Treated as "no signal" — fall
// through to soft guidance, do not log telemetry.
var errDaemonUnreachable = errors.New("daemon unreachable")

// grepProbeFn is the function the Grep enrichment uses to query the
// graph for symbol matches. Defaults to the daemon-socket implementation;
// tests swap it for a stub.
//
// scope is where the search was issued from. It rides along so the daemon can
// answer out of the graph that location reads through — an automatic worktree
// is served by its own composed view, and matching the pattern against the
// family primary's corpus would cite another working copy's code as evidence.
type grepProbeFn func(pattern, scope string, timeout time.Duration) ([]grepSymbolHit, error)

// grepProbe is the indirection point. Production reads probeViaDaemon;
// tests reassign this var via a t.Cleanup-restored helper.
var grepProbe grepProbeFn = probeViaDaemon

// enrichGrep denies searches within a proven tracked/indexed scope, regardless
// of pattern shape. When scope ownership cannot be established, the existing
// symbol probe and soft fallback preserve the historical posture.
func enrichGrep(toolInput map[string]any, _ int, cwdArg ...string) enrichResult {
	cwd := ""
	if len(cwdArg) > 0 {
		cwd = cwdArg[0]
	}
	pattern, _ := toolInput["pattern"].(string)
	if pattern == "" {
		return enrichResult{}
	}
	switch hookSearchScope(cwd, toolInput) {
	case searchScopeNonSource:
		// The search is confined to docs or config. Neither the deny nor the
		// "do not Grep indexed source" advisory is true of it, and the graph
		// surface both point at has no answer for it either.
		return enrichResult{}
	case searchScopeIndexed:
		return enrichResult{
			deny:   true,
			reason: formatTrackedSearchDeny("Grep", pattern),
		}
	}
	return probeSymbolPattern("Grep", pattern, cwd, defaultGrepGuidance())
}

// searchScopeVerdict is what a Grep/Glob scope resolves to. The three states
// are deliberately distinct: only searchScopeIndexed is proof the policy
// applies, and only searchScopeNonSource is proof it does not. Unproven keeps
// the historical posture — probe the pattern, fall back to soft guidance.
type searchScopeVerdict int

const (
	searchScopeUnproven searchScopeVerdict = iota
	searchScopeIndexed
	searchScopeNonSource
)

// hookSearchScope classifies what a Grep/Glob call actually searches, from its
// `path` scope plus (for Grep) the `type` / `glob` filters that narrow it.
func hookSearchScope(cwd string, toolInput map[string]any) searchScopeVerdict {
	if searchRestrictedToNonSource(toolInput) {
		return searchScopeNonSource
	}

	scope, _ := toolInput["path"].(string)
	scope = strings.TrimSpace(scope)
	if scope != "" && scopeNamesFile(cwd, scope) {
		// A search scoped to one non-source file is out of policy whatever the
		// graph holds for it: grepping a README is a text search, not a
		// symbol lookup wearing a filename.
		if !looksLikeSourceFile(scope) {
			return searchScopeNonSource
		}
		st := queryFileIndexScope(cwd, scope)
		switch {
		case st.Indexed:
			return searchScopeIndexed
		// Before Symbolless, which it can accompany: a size- or gate-skipped
		// file earns a synthetic node, so the graph holds it while its bytes
		// were never read. Denying would redirect a text search to an index
		// that has nothing of it.
		case st.NeverIndexable:
			return searchScopeNonSource
		// Symbolless denies here but not on the read doors: search(text),
		// search(files) and explore(outline) all have rows for a file the
		// graph holds, symbol-free or not.
		case st.Symbolless:
			return searchScopeIndexed
		}
		// A failed probe is not evidence. NonSource here would make narrowing
		// `path` to one file a way to switch enforcement off on a hiccup.
		return searchScopeUnproven
	}

	// A directory scope. scopeTrackedFn reports whether it holds indexed
	// source, and separately whether it could tell.
	hasSource, probeOK := scopeTrackedFn(cwd, scope)
	switch {
	case hasSource:
		return searchScopeIndexed
	case probeOK:
		return searchScopeNonSource
	}
	return searchScopeUnproven
}

// scopeNamesFile reports whether a `path` scope names one file rather than a
// directory to search under. An existing path answers directly; one that is
// not on disk (a scope the agent typed from memory) is judged by whether it
// carries an extension.
func scopeNamesFile(cwd, scope string) bool {
	abs := scope
	if !filepath.IsAbs(abs) && cwd != "" {
		abs = filepath.Join(cwd, abs)
	}
	if info, err := os.Stat(abs); err == nil {
		return !info.IsDir()
	}
	return filepath.Ext(scope) != ""
}

func formatTrackedSearchDeny(tool, query string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: %s `%s` targets indexed source. Use the indexed search surface instead:\n", tool, query)
	b.WriteString("  - `search(operation:\"text\", query:\"<literal>\")` — literal or regex-related source lookup\n")
	b.WriteString("  - `search(operation:\"symbols\", query:\"<name>\")` — symbol lookup\n")
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}

// maxAlternationProbes caps how many identifier-shaped alternatives of a
// multi-keyword grep pattern (grep 'a|b|c') the hook probes, so a long
// alternation can't fan out into an unbounded number of daemon round-trips.
const maxAlternationProbes = 5

// probeSymbolPattern is the shared body of enrichGrep and the grep-/find-like
// branches of enrichBash. Given a pattern, it gates on symbol-shape, probes
// the daemon, and returns deny-with-hits or soft guidance. Telemetry is
// attributed to the `tool` label so Grep- vs Bash-sourced probes stay
// distinguishable in `hook-decisions.jsonl`.
//
// Alternation patterns (grep 'a|b|c') get split first: agents that wrap grep
// in Bash — Codex especially — routinely batch several keywords behind `|`,
// so the whole pattern is never a bare identifier even when the individual
// alternatives are. Each identifier-shaped alternative is probed and the hits
// aggregated; a pure-text alternation (phrases, hyphenated words) falls
// through to guidance that points at search_text.
func probeSymbolPattern(tool, pattern, scope, guidance string) enrichResult {
	if pattern == "" {
		return enrichResult{}
	}

	segments := splitAlternation(pattern)
	if len(segments) == 1 {
		return probeSinglePattern(tool, segments[0], scope, guidance)
	}

	var symbolSegs []string
	for _, s := range segments {
		if classifyGrepPattern(s) == GrepPatternSymbol {
			symbolSegs = append(symbolSegs, s)
			if len(symbolSegs) >= maxAlternationProbes {
				break
			}
		}
	}
	if len(symbolSegs) == 0 {
		// Pure text search — phrases, hyphenated words, numeric literals.
		// search_text (surfaced in the guidance) is the graph equivalent.
		if len(pattern) > 2 {
			logHookDecision(tool, pattern, DecisionSkippedNonSymbol, 0, 0)
			return enrichResult{context: guidance}
		}
		return enrichResult{}
	}

	start := time.Now()
	hits, reached := probeSegments(symbolSegs, scope)
	dur := time.Since(start)
	if len(hits) == 0 {
		// Only record a miss when the daemon actually answered — a fully
		// unreachable daemon is "no signal", not a miss (matches the
		// single-pattern path).
		if reached {
			logHookDecision(tool, pattern, DecisionProbedMiss, 0, dur)
		}
		return enrichResult{context: guidance}
	}
	logHookDecision(tool, pattern, DecisionProbedHit, len(hits), dur)
	return enrichResult{
		deny:   true,
		reason: formatGrepDeny(pattern, hits),
	}
}

// probeSinglePattern gates a single (non-alternation) pattern on symbol-shape
// and probes the daemon's search_symbols endpoint, returning deny-with-hits on
// a match or soft guidance on miss/timeout/non-symbol.
func probeSinglePattern(tool, pattern, scope, guidance string) enrichResult {
	if classifyGrepPattern(pattern) != GrepPatternSymbol {
		if len(pattern) > 2 {
			logHookDecision(tool, pattern, DecisionSkippedNonSymbol, 0, 0)
			return enrichResult{context: guidance}
		}
		return enrichResult{}
	}

	start := time.Now()
	hits, err := grepProbe(pattern, scope, grepProbeTimeout)
	dur := time.Since(start)
	switch {
	case errors.Is(err, errProbeTimeout):
		logHookDecision(tool, pattern, DecisionTimedOut, 0, dur)
		return enrichResult{context: guidance}
	case errors.Is(err, errDaemonUnreachable):
		// No daemon = no signal. Don't pollute telemetry with infra noise.
		return enrichResult{context: guidance}
	case err != nil:
		// Other transport/decode failure — treat as miss so we have a record.
		logHookDecision(tool, pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	if len(hits) == 0 {
		logHookDecision(tool, pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	logHookDecision(tool, pattern, DecisionProbedHit, len(hits), dur)
	return enrichResult{
		deny:   true,
		reason: formatGrepDeny(pattern, hits),
	}
}

// probeSegments probes each alternation segment and returns the deduplicated
// union of hits plus whether the daemon answered at least once. A per-segment
// error (timeout, decode) drops that segment silently — one bad alternative
// shouldn't sink the whole redirect — and an unreachable daemon leaves
// reached=false so the caller can stay quiet instead of logging a false miss.
func probeSegments(segs []string, scope string) (hits []grepSymbolHit, reached bool) {
	seen := make(map[string]bool)
	for _, s := range segs {
		found, err := grepProbe(s, scope, grepProbeTimeout)
		if errors.Is(err, errDaemonUnreachable) {
			continue
		}
		reached = true
		if err != nil {
			continue
		}
		for _, h := range found {
			key := fmt.Sprintf("%s:%d:%s", h.FilePath, h.Line, h.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, h)
		}
	}
	return hits, reached
}

// splitAlternation splits a grep pattern on top-level '|' alternation so a
// multi-keyword search like "place_edges|location_edge|normalize" can be
// probed as individual identifiers. A backslash-escaped `\|` is kept literal
// and does not split. Empty segments are dropped; a pattern with no usable
// '|' returns a single-element slice (the fast path).
func splitAlternation(pattern string) []string {
	if !strings.Contains(pattern, "|") {
		return []string{pattern}
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			cur.WriteByte(c)
			cur.WriteByte(pattern[i+1])
			i++
			continue
		}
		if c == '|' {
			if seg := strings.TrimSpace(cur.String()); seg != "" {
				out = append(out, seg)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if seg := strings.TrimSpace(cur.String()); seg != "" {
		out = append(out, seg)
	}
	if len(out) == 0 {
		return []string{pattern}
	}
	return out
}

func defaultGrepGuidance() string {
	var b strings.Builder
	b.WriteString("[Gortex] Do not Grep indexed source. Call `explore` for a task, then use:\n")
	b.WriteString("  - symbol name: `search(operation:\"symbols\", query:\"...\")`; literal text: use operation `text`\n")
	b.WriteString("  - references: `relations(operation:\"usages\", target:{symbol:\"<id>\"})`; choose `callers` or `implementations` when that is the required relation\n")
	b.WriteString("  - TODOs or contracts: `analyze(kind:\"todos\")` or `analyze(kind:\"contracts\")`\n")
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}

func formatGrepDeny(pattern string, hits []grepSymbolHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: \"%s\" matches %d indexed symbol(s). Use `search(operation:\"symbols\")` or `relations(operation:\"usages\")`:\n\n", pattern, len(hits))
	b.WriteString(grepHitEvidence(hits))
	b.WriteString("\n")
	b.WriteString("Localizing a task rather than one symbol? `explore` returns the ranked neighborhood (symbols + source + call paths) in one call.\n")
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	b.WriteString("To force text search, add a regex metachar (e.g. \\b) or quote the pattern.")
	return b.String()
}

// A deny that only names the replacement tool costs the caller a turn to
// re-derive what the hook already asked the graph. These bounds decide how
// much of that answer rides along: enough rows to act on, never enough to
// bury the redirect itself.
const (
	denyEvidenceMaxLines = 5
	denyEvidenceMaxBytes = 600
)

// denyEvidenceRow is one graph result rendered into a deny: what was found,
// where it lives, and an optional parenthesised note (its kind).
type denyEvidenceRow struct {
	Label string
	File  string
	Line  int
	Note  string
}

// denyEvidenceBlock renders rows as "  label — file:line (note)" under an
// optional header, capped at denyEvidenceMaxLines rows and denyEvidenceMaxBytes
// for the whole block including the header and the trailing "... and N more".
// An empty return means nothing fit, and the caller's message stays as it was.
func denyEvidenceBlock(header string, rows []denyEvidenceRow) string {
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, denyEvidenceMaxLines)
	used := len(header)
	for _, row := range rows {
		if len(lines) == denyEvidenceMaxLines {
			break
		}
		line := formatDenyEvidenceRow(row)
		// Reserve the tail before taking the line. The dropped count only
		// shrinks as more rows are taken, so budgeting for the tail this row
		// would leave behind keeps every stopping point inside the cap.
		tail := 0
		if dropped := len(rows) - len(lines) - 1; dropped > 0 {
			tail = len(denyEvidenceMoreLine(dropped))
		}
		if used+len(line)+tail > denyEvidenceMaxBytes {
			break
		}
		lines = append(lines, line)
		used += len(line)
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for _, line := range lines {
		b.WriteString(line)
	}
	if dropped := len(rows) - len(lines); dropped > 0 {
		b.WriteString(denyEvidenceMoreLine(dropped))
	}
	return b.String()
}

func formatDenyEvidenceRow(row denyEvidenceRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s — %s:%d", row.Label, row.File, row.Line)
	if row.Note != "" {
		fmt.Fprintf(&b, " (%s)", row.Note)
	}
	b.WriteString("\n")
	return b.String()
}

func denyEvidenceMoreLine(dropped int) string {
	return fmt.Sprintf("  ... and %d more\n", dropped)
}

// grepHitEvidence renders the probe hits the deny was decided on. The probe
// has already run by the time this is called, so the evidence costs nothing
// beyond formatting.
func grepHitEvidence(hits []grepSymbolHit) string {
	rows := make([]denyEvidenceRow, 0, len(hits))
	for _, h := range hits {
		kind := h.Kind
		if kind == "" {
			kind = "symbol"
		}
		rows = append(rows, denyEvidenceRow{Label: h.Name, File: h.FilePath, Line: h.Line, Note: kind})
	}
	return denyEvidenceBlock("", rows)
}

// readOutlineTimeout bounds the outline lookup behind a Read deny. It is much
// tighter than fileIndexedTimeout because the deny is already decided: the
// outline only enriches it, so a slow daemon must cost the refusal nothing.
const readOutlineTimeout = 250 * time.Millisecond

const readOutlineHeader = "\nSymbols indexed in this file:\n"

// readOutlineEvidence returns the head of filePath's indexed symbol outline,
// or "" when the daemon cannot answer inside readOutlineTimeout. Degrading to
// "" rather than to a half-formatted section keeps the daemon-less deny byte
// for byte what it has always been.
func readOutlineEvidence(cwd, filePath string) string {
	summary, ok := fileOutlineWithin(cwd, filePath, readOutlineTimeout)
	if !ok || summary == nil {
		return ""
	}
	rows := make([]denyEvidenceRow, 0, len(summary.Symbols))
	for _, sym := range summary.Symbols {
		if sym.Name == "" || sym.StartLine <= 0 {
			continue
		}
		rows = append(rows, denyEvidenceRow{Label: sym.Name, File: filePath, Line: sym.StartLine, Note: sym.Kind})
	}
	return denyEvidenceBlock(readOutlineHeader, rows)
}

// fileOutlineWithin runs the shared get_file_summary probe under a deadline of
// its own. daemonFileSummaryRaw's socket deadline is sized for the indexed
// check that gates the deny; this call happens after that verdict, so it gets
// a budget the caller never waits on.
func fileOutlineWithin(cwd, filePath string, timeout time.Duration) (*hookFileSummary, bool) {
	type outlineResult struct {
		summary *hookFileSummary
		ok      bool
	}
	// Read the seam here, not in the goroutine: an abandoned probe must not
	// race a test restoring it.
	probe := fileSummaryFn
	done := make(chan outlineResult, 1)
	go func() {
		summary, ok := probe(cwd, filePath)
		done <- outlineResult{summary: summary, ok: ok}
	}()
	select {
	case res := <-done:
		return res.summary, res.ok
	case <-time.After(timeout):
		return nil, false
	}
}

// fileIndexStatus is the daemon's per-file verdict from one file_coverage
// probe. The flags are independent facts, not a ranking. ProbeOK false is an
// abstention, never a negative verdict, and Unreached (nothing came back) is
// kept apart from it — collapsing either into "no repo owns this" is the
// bypass this type exists to prevent.
type fileIndexStatus struct {
	Indexed        bool // the graph holds Count definition symbols; the only deny
	Symbolless     bool // held, but defines nothing
	NeverIndexable bool // the walk would reject it
	Tracked        bool // a registered checkout owns the path
	ProbeOK        bool // the daemon resolved the path to a graph and read it
	Unreached      bool // the probe never came back
	Count          int
}

// noGraphAnswer reports whether a redirect to graph tools has nothing true to
// say. READ-shaped doors only (Read, Bash cat/head/tail) — they redirect to
// symbol lookups; the search doors answer for symbol-free files too.
func (st fileIndexStatus) noGraphAnswer() bool {
	switch {
	case st.Indexed:
		// Before the Tracked tests below: an older daemon reports coverage
		// without the tracked flag, and they would silence it.
		return false
	case st.NeverIndexable || st.Symbolless:
		return true
	case st.ProbeOK:
		// Silence only for a path the daemon placed outside every checkout.
		return !st.Tracked
	case !daemonReachableFn():
		// The advisory would name tools the agent cannot reach.
		return true
	case st.Unreached:
		// A failed probe proves nothing, so enforcement stays on.
		return false
	default:
		return !st.Tracked
	}
}

// queryFileIndexed is the WRITE doors' shape: "does the graph hold symbols".
// Read-shaped callers need queryFileIndexScope — this collapses "excluded",
// "no verdict" and "not indexed yet" to (false, 0).
func queryFileIndexed(cwd, filePath string) (bool, int) {
	st := queryFileIndexScope(cwd, filePath)
	return st.Indexed, st.Count
}

// queryFileIndexScope returns the full per-file verdict under the standard
// probe budget.
func queryFileIndexScope(cwd, filePath string) fileIndexStatus {
	return fileIndexScopeFn(cwd, filePath, fileIndexedTimeout)
}

// fileIndexedTimeout bounds the daemon probe so a wedged daemon never
// stalls the PreToolUse critical path.
const fileIndexedTimeout = 2 * time.Second

// fileIndexScopeFn is the seam tests stub. The timeout is a parameter rather
// than a constant read inside because the witness walk raises several probes
// under one shared budget.
var fileIndexScopeFn = fileIndexScopeViaDaemon

// fileIndexScopeViaDaemon asks the daemon's file_coverage control verb what
// the graph serving this path holds for it, and what the index walk would do
// with it if it holds nothing.
//
// The path is sent absolute and resolved daemon-side. That is the whole point
// of the verb: which graph answers for a path is a catalog question — an
// automatic worktree reads a composed view, a dedicated checkout reads its own
// corpus — and a hook resolving the path against its own tracked-repo list
// cannot see any of it.
//
// The answer's view block is recorded, not acted on: a fallback answer still
// decides the deny the same way an exact one does, and the flag exists so the
// posture is visible before it is given weight.
func fileIndexScopeViaDaemon(cwd, filePath string, timeout time.Duration) fileIndexStatus {
	abs := filePath
	if !filepath.IsAbs(abs) {
		if cwd == "" {
			// Nothing to resolve against, so the path is placed nowhere. That
			// is an answer, not a failed probe.
			return fileIndexStatus{}
		}
		abs = filepath.Join(cwd, abs)
	}
	result, ok := fileCoverageViaDaemon(abs, timeout)
	if !ok {
		return fileIndexStatus{Unreached: true}
	}
	logProbeViewFallback(daemon.ControlFileCoverage, result.View)
	// Covered counts as an answer whatever the daemon's vintage. A daemon
	// predating Answered still reports coverage truthfully, and gating on the
	// missing field would drop a real deny into silence for the whole life of
	// that process — daemons outlive the binary upgrade that starts them.
	// The advisory tier still degrades to silence there, because such a daemon
	// genuinely cannot say whether it tracks an uncovered path.
	st := fileIndexStatus{
		Tracked: result.Tracked,
		ProbeOK: result.Answered || result.Covered,
	}
	if !st.ProbeOK {
		return st
	}
	st.Indexed = result.Covered
	st.Count = result.Symbols
	st.Symbolless = result.Held && !result.Covered
	st.NeverIndexable = result.Excluded || result.Unindexable
	return st
}

// daemonFileSummaryRaw resolves filePath to its tracked-repo root, asks the
// daemon's get_file_summary tool over the AF_UNIX MCP channel, and returns
// the raw tools/call response frame. ok=false on any failure (relative path
// with no cwd, outside every tracked repo, daemon unreachable, socket error).
// The graph keys files by their repo-relative path, so the absolute path is
// resolved against its tracked-repo root and the root-relative path is what
// gets queried (with the handshake CWD set to that root for scoping).
//
// Shared by the PreToolUse file-indexed probe (fileIndexedViaDaemon) and the
// PostToolUse enrichment (fileSummaryViaDaemon) so both stay on the daemon
// socket — the HTTP :8765 /api/graph/* API they used to hit was removed when
// the web surface migrated to the daemon (#241).
func daemonFileSummaryRaw(cwd, filePath string) ([]byte, bool) {
	abs := filePath
	if !filepath.IsAbs(abs) {
		if cwd == "" {
			return nil, false
		}
		abs = filepath.Join(cwd, abs)
	}

	root := repoRootForFile(abs)
	if root == "" {
		return nil, false // outside every tracked repo → not indexed
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return nil, false
	}

	client, err := daemon.Dial(hookMCPHandshake(root))
	if err != nil {
		return nil, false
	}
	defer client.Close()
	_ = client.Conn.SetDeadline(time.Now().Add(fileIndexedTimeout))

	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_file_summary",
			"arguments": map[string]any{"path": rel, "format": "json"},
		},
	})
	if err != nil {
		return nil, false
	}
	if err := client.WriteMCPFrame(frame); err != nil {
		return nil, false
	}
	resp, err := client.ReadMCPFrame()
	if err != nil {
		return nil, false
	}
	return resp, true
}

// daemonStatusCacheTTL bounds how long a fetched tracked-repo list is reused
// within one hook process. The set is effectively static for the lifetime of
// a short-lived hook invocation, so a few seconds collapses a wide postGlob
// (which probes every matched path) from one control-status round-trip per
// file to ~one for the whole batch.
const daemonStatusCacheTTL = 5 * time.Second

var (
	statusCacheMu  sync.Mutex
	statusCacheVal *daemon.StatusResponse
	statusCacheErr error
	statusCacheAt  time.Time
)

// cachedDaemonStatus memoises fetchDaemonStatus for daemonStatusCacheTTL so
// repoRootForFile — called once per probed file — does not re-dial the daemon
// and re-marshal the entire tracked-repo list on every file in a postGlob
// loop. The error is cached too: within one short hook process the daemon does
// not realistically flip reachable↔unreachable, and a cached "down" short-
// circuits N failed dials straight to soft-fallback.
func cachedDaemonStatus() (*daemon.StatusResponse, error) {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()
	if !statusCacheAt.IsZero() && time.Since(statusCacheAt) < daemonStatusCacheTTL {
		return statusCacheVal, statusCacheErr
	}
	statusCacheVal, statusCacheErr = fetchDaemonStatus()
	statusCacheAt = time.Now()
	return statusCacheVal, statusCacheErr
}

// repoRootForFile returns the tracked-repo root that contains abs (longest
// match wins for nested repos), or "" when no tracked repo owns it.
//
// TODO(hook-local altitude): this hand-rolls a subset of the MCP server's
// resolveFilePath (abs → repo-relative key) because get_file_summary does no
// path resolution of its own — it looks the path up verbatim in the graph's
// by-file index. It is also symlink-naive (no EvalSymlinks) where
// resolveFilePath enforces the SECURITY.md repo-confinement guard, and yields
// a bare repo-relative path that can diverge from the graph key in multi-repo
// mode. The deeper fix is to route get_file_summary's path through
// resolveFilePath server-side and have the hook forward {cwd, file_path}
// verbatim — left to the maintainer as it touches a shared handler.
func repoRootForFile(abs string) string {
	repo, ok := trackedRepoForPath(abs)
	if !ok {
		return ""
	}
	return repo.Path
}

// enrichBash classifies the Bash command and routes codebase-search shapes
// through the same graph probes the Grep and Read enrichments use, and shell
// mutations of indexed source through the same answer Edit and Write get.
// Anything not recognised passes through silently — false-deny is more
// disruptive than a miss, so the classifier only flags primary
// grep/rg/find-name/cat-source invocations and recognised write shapes.
func enrichBash(toolInput map[string]any, cwd string) enrichResult {
	cmd, _ := toolInput["command"].(string)
	if cmd == "" {
		return enrichResult{}
	}

	c := classifyBashCommand(cmd)
	if c.Action == BashActionWriteSource {
		if result, answered := enrichBashWrite(c, cwd); answered {
			return result
		}
		// The write targets are all outside the graph. Whatever the command
		// also reads still deserves the read answer, so re-read it without
		// the write pre-pass instead of returning silence.
		c = classifyBashReadCommand(cmd)
	}
	switch c.Action {
	case BashActionGrepLike:
		return probeSymbolPattern("Bash", c.Pattern, cwd, defaultGrepGuidance())

	case BashActionFindName:
		// find -name values often include `*` globs; the classifier has
		// already stripped wildcards, but the residue may still be
		// non-symbol-shaped (e.g. ".go" from `-name "*.go"`) — let
		// probeSymbolPattern decide.
		return probeSymbolPattern("Bash", c.Pattern, cwd, defaultGrepGuidance())

	case BashActionReadSource:
		// Bash is the door an agent falls back to the moment Read denies, so
		// it has to reach the same verdict Read does — including the silences.
		st := queryFileIndexScope(cwd, c.Path)
		if st.Indexed {
			var reason strings.Builder
			fmt.Fprintf(&reason,
				"[Gortex] BLOCKED: Bash `%s %s` reads indexed source (%d symbols). Use graph tools instead:\n",
				c.Primary, c.Path, st.Count)
			reason.WriteString("  - one symbol: `read(target:{symbol:\"<id>\"})`\n")
			reason.WriteString("  - file overview: `read(operation:\"summary\", target:{file:\"<path>\"})`\n")
			reason.WriteString("  - before editing: `read(operation:\"editing_context\", target:{file:\"<path>\"})`\n")
			reason.WriteString(gcxTip)
			reason.WriteString(toolref.MCPRequiredLine())
			return enrichResult{deny: true, reason: reason.String()}
		}
		if st.noGraphAnswer() {
			return enrichResult{}
		}
		// Tracked, indexable, not indexed yet — soft guidance so Bash proceeds.
		var g strings.Builder
		g.WriteString("[Gortex] Use `read` instead of Bash cat/head/tail for indexed source:\n")
		g.WriteString("  - `read(target:{symbol:\"<id>\"})` for one symbol; use operation `summary` for an overview or `editing_context` before editing\n")
		g.WriteString(gcxTip)
		g.WriteString(toolref.MCPRequiredLine())
		return enrichResult{context: g.String()}
	}

	return enrichResult{}
}

// enrichBashWrite answers a shell command that would mutate indexed source.
// The shell is the write door that skips both the graph-aware edit tools and
// the pre-write parse gate, and it used to be the one door that said nothing
// at all — so an agent that fell back to `sed -i` once had no reason to ever
// come back. It now gets the same answer Edit and Write get, at the same
// strength.
//
// A path the daemon does not know is left alone, mirroring enrichWrite:
// writing a new file is not a graph operation yet. The second return reports
// whether the write shape was answered at all, so the caller can fall back to
// the read classification when it was not.
func enrichBashWrite(c BashClassification, cwd string) (enrichResult, bool) {
	started := time.Now()
	write, symbolCount, indexed := firstIndexedWriteTarget(c.Writes, cwd)
	if !indexed {
		return enrichResult{}, false
	}
	logHookDecision("Bash", "", DecisionRedirectedWrite, symbolCount, time.Since(started))
	return writeRedirect(
		fmt.Sprintf(
			"Bash `%s` writes %s (indexed source, %d symbols). A shell write goes around the graph, which only catches up afterwards, and around the parse gate `edit` runs before it touches disk. Use:",
			write.Shape, write.Path, symbolCount),
		"`edit(operation:\"symbol\", target:{symbol:\"<id>\"})` for one symbol",
		"`edit(operation:\"file\", target:{file:\"<path>\"})` for a guarded replacement — this one parse-gates",
		"`edit(operation:\"write\", target:{file:\"<path>\"})` for a whole-file write — this one too",
		"`refactor(operation:\"rename\")` for a coordinated rename",
	), true
}

// firstIndexedWriteTarget returns the first recognised target the daemon
// confirms is indexed. Candidates are already capped at bashWriteProbeLimit,
// so a compound command costs a bounded number of probes rather than one per
// redirect.
func firstIndexedWriteTarget(writes []BashWrite, cwd string) (BashWrite, int, bool) {
	for _, write := range writes {
		if indexed, symbolCount := queryFileIndexed(cwd, write.Path); indexed {
			return write, symbolCount, true
		}
	}
	return BashWrite{}, 0, false
}

// daemonReachableFn is the seam tests use to fake daemon availability
// without a real socket. Production reads daemon.IsRunning.
var daemonReachableFn = daemon.IsRunning

// scopeTrackedFn asks whether a Grep/Glob directory scope contains at least
// one indexed source file. The second return separates "asked, and the answer
// is no" from "could not ask" — without it a daemon hiccup is indistinguishable
// from a proven-empty vendored tree. Tests replace it so fallback cases stay
// deterministic.
var scopeTrackedFn = scopeTrackedViaDaemon

func scopeTrackedViaDaemon(cwd, scope string) (hasSource, probeOK bool) {
	if !daemonReachableFn() {
		return false, false
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = cwd
	}
	if scope == "" {
		return false, false
	}
	if !filepath.IsAbs(scope) {
		if cwd == "" {
			return false, false
		}
		scope = filepath.Join(cwd, scope)
	}
	scope = filepath.Clean(scope)
	info, err := os.Stat(scope)
	if err != nil || !info.IsDir() {
		return false, false
	}
	result, ok := dirCoverageFn(scope, fileIndexedTimeout)
	if !ok {
		return false, false
	}
	logProbeViewFallback(daemon.ControlDirCoverage, result.View)
	return scopeTrackedFromCoverage(result)
}

// dirCoverageFn asks the daemon what a directory scope holds. Tests replace it
// to drive scopeTrackedViaDaemon's verdict without a socket.
var dirCoverageFn = dirCoverageViaDaemon

// scopeTrackedFromCoverage turns the daemon's scope answer into the hook's
// verdict, split from the transport so it is testable without a daemon.
//
// Only a completed walk that claimed nothing proves a scope holds no source.
// From the graph alone a scope mid-walk looks exactly like an excluded one.
func scopeTrackedFromCoverage(result daemon.DirCoverageResult) (hasSource, probeOK bool) {
	if !result.Answered {
		return false, false
	}
	if result.HasSource {
		return true, true
	}
	return false, result.Walked && !result.Indexable
}

// enrichGlob denies source enumeration within a proven tracked/indexed scope.
// Pattern shape no longer creates a bypass; unproven or unavailable scopes
// retain soft guidance so the hook never blocks code it cannot identify.
func enrichGlob(toolInput map[string]any, cwdArg ...string) enrichResult {
	cwd := ""
	if len(cwdArg) > 0 {
		cwd = cwdArg[0]
	}
	pattern, ok := toolInput["pattern"].(string)
	if !ok || pattern == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(pattern) {
		return enrichResult{}
	}

	switch hookSearchScope(cwd, toolInput) {
	case searchScopeNonSource:
		// The enumeration is scoped to something that is not indexed source,
		// so neither the deny nor the soft guidance below describes it.
		return enrichResult{}
	case searchScopeIndexed:
		var b strings.Builder
		fmt.Fprintf(&b, "[Gortex] BLOCKED: Glob `%s` targets indexed source. Use:\n", pattern)
		b.WriteString("  - `explore(operation:\"outline\")` — repository/file outline\n")
		b.WriteString("  - `search(operation:\"files\", query:\"<name>\")` — filename lookup\n")
		b.WriteString("  - `search(operation:\"symbols\", query:\"<name>\")` — symbols with file paths\n")
		b.WriteString("  - `read(operation:\"summary\", target:{file:\"<path>\"})` — a specific file overview\n")
		b.WriteString(gcxTip)
		b.WriteString(toolref.MCPRequiredLine())
		return enrichResult{deny: true, reason: b.String()}
	}

	return enrichResult{context: defaultGlobGuidance()}
}

// defaultGlobGuidance is the soft-guidance message returned when a
// Glob pattern targets source files but isn't a greedy "all of this
// extension" pattern, or when the daemon is unreachable.
func defaultGlobGuidance() string {
	return "[Gortex] PREFER graph tools over Glob for source files:\n" +
		"  - symbol/file lookup: `search(operation:\"symbols\")`\n" +
		"  - file structure: `read(operation:\"summary\", target:{file:\"<path>\"})`\n" +
		"  - task-level discovery: `explore`\n" +
		"  - For migration / SQL globs (`db/migrations/*.sql`, `**/*.sql`): use `analyze kind=orphan_tables` and `kind=unreferenced_tables` to find queried-but-undeclared and provided-but-unused tables\n" +
		gcxTip +
		toolref.MCPRequiredLine()
}

// isGreedySourceGlob returns true when the pattern is a bare
// extension wildcard like `*.go`, `**/*.ts`, `src/**/*.tsx`. The
// classifier looks at the segment between the last `/` and the
// extension: if it's just `*` (or `**` collapsed), the agent is
// asking for "every source file of this kind" — exactly the shape
// `get_repo_outline` answers. Anything else (a literal filename, a
// substring wildcard like `*test*.go`) is treated as name-based
// search and not denied.
func isGreedySourceGlob(pattern string) bool {
	last := pattern
	if idx := strings.LastIndex(pattern, "/"); idx >= 0 {
		last = pattern[idx+1:]
	}
	dot := strings.LastIndex(last, ".")
	if dot <= 0 {
		return false
	}
	stem := last[:dot]
	// Bare wildcard stems indicate "all files of this extension".
	return stem == "*" || stem == "**"
}

// editBlockingEnvVar gates how hard a write redirect lands, not whether
// it happens. A false positive on a write stops the agent from making
// progress at all, so blocking stays opt-in; but a write door that says
// nothing is how the shell quietly became the default one, so every door
// speaks in every posture and only the strength is gated.
const editBlockingEnvVar = "GORTEX_HOOK_BLOCK_EDIT"

// editBlockingEnabled reports whether a write redirect blocks rather
// than advises. Anything besides empty/"0"/"false"/"no"/"off" enables.
func editBlockingEnabled() bool {
	return envGateEnabled(editBlockingEnvVar)
}

// writeRedirect renders the one answer every write door gives for a
// mutation of indexed source: what was written, why the graph path is
// different, and which operation to use instead. Edit, Write, and a shell
// write share it so no door can drift into being the quiet one — the whole
// failure this guards against is an agent finding the door that says nothing.
func writeRedirect(subject string, alternatives ...string) enrichResult {
	var body strings.Builder
	body.WriteString(subject)
	body.WriteString("\n")
	for _, alternative := range alternatives {
		fmt.Fprintf(&body, "  - %s\n", alternative)
	}
	body.WriteString("\n")
	body.WriteString(toolref.MCPRequiredLine())

	advisory := "[Gortex] " + body.String()
	if !editBlockingEnabled() {
		return enrichResult{context: advisory}
	}
	// The advisory text rides along with the deny. A posture that downgrades
	// a deny reads `context`, and handing it an empty string is how a blocking
	// door ends up quieter than an advisory one.
	return enrichResult{
		deny: true,
		reason: "[Gortex] BLOCKED: " + body.String() +
			"To bypass this redirect: unset GORTEX_HOOK_BLOCK_EDIT, or target a path outside the tracked repos.\n",
		context: advisory,
	}
}

// envGateEnabled reports whether a boolean env-var gate is on. Empty,
// "0", "false", "no", and "off" are off; anything else is on. Shared by
// the Edit/Write and force-compress gates so they read identically.
func envGateEnabled(name string) bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(name))) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// enrichEdit redirects whole-file edits of indexed source to the
// Gortex MCP edit tools, which don't require a prior Read and update the
// graph atomically. Advisory by default and a hard block under
// GORTEX_HOOK_BLOCK_EDIT.
func enrichEdit(toolInput map[string]any, cwd string) enrichResult {
	filePath, indexed := indexedSourceTarget(toolInput, cwd)
	if !indexed {
		return enrichResult{}
	}
	return writeRedirect(
		fmt.Sprintf("Edit of %s (indexed source). Use Gortex MCP edit tools — they don't require a prior Read and update the graph atomically:", filePath),
		"choose `edit` operation `symbol`, `file`, or `batch`; for example `edit(operation:\"file\", target:{file:\"<path>\"})`",
		"`refactor(operation:\"rename\")` for a coordinated rename",
	)
}

// indexedSourceTarget resolves a tool input's file_path and reports whether
// the daemon knows it as source. A path outside the graph is a new file, which
// the graph has no opinion about yet.
func indexedSourceTarget(toolInput map[string]any, cwd string) (string, bool) {
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" || !looksLikeSourceFile(filePath) {
		return "", false
	}
	indexed, _ := queryFileIndexed(cwd, filePath)
	return filePath, indexed
}

// enrichWrite mirrors enrichEdit for whole-file Write. New files
// (not yet indexed) pass through; rewrites of existing indexed
// files are redirected to `edit_file` / `write_file`.
func enrichWrite(toolInput map[string]any, cwd string) enrichResult {
	filePath, indexed := indexedSourceTarget(toolInput, cwd)
	if !indexed {
		return enrichResult{}
	}
	return writeRedirect(
		fmt.Sprintf("Write of %s (indexed source — would overwrite existing tracked file). Use:", filePath),
		"`edit(operation:\"write\")` for a whole-file write",
		"`edit(operation:\"file\")` for a guarded replacement",
	)
}

func looksLikeSourceFile(path string) bool {
	sourceExts := []string{
		".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java",
		".kt", ".scala", ".swift", ".php", ".rb", ".ex", ".exs",
		".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".cs",
		".sql", ".proto", ".sh", ".bash",
	}
	lower := strings.ToLower(path)
	for _, ext := range sourceExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// toFloat64 attempts to convert an any value to float64.
// JSON numbers are decoded as float64 by encoding/json.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
