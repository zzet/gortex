// Package hooks provides Claude Code hook handlers for Gortex.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// RunPreToolUse reads a PreToolUse hook payload from stdin and handles it
// in the legacy deny posture. Kept as a public entry point for
// backward compatibility; new callers should use Run which dispatches
// based on hook_event_name and respects the configured Mode.
func RunPreToolUse(gortexPort int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runPreToolUse(data, gortexPort, ModeDeny)
}

// gortexMCPToolPrefix is the namespace Claude Code gives Gortex's own
// MCP tools (server name "gortex"). A tool call whose name starts with
// this prefix is a graph query — the in-process hook sees it like any
// other tool call, which is what lets the consult-unlock handshake and
// the adaptive-nudge streak reset work without an external signal.
const gortexMCPToolPrefix = "mcp__gortex__"

// runPreToolUse is the bytes-accepting helper used by both RunPreToolUse and
// the generic Run dispatcher. In ModeEnrich the deny branch is downgraded
// to an additionalContext message — the agent is informed about the graph
// alternative but the original call still runs and PostToolUse can layer
// graph context on the actual output.
func runPreToolUse(data []byte, gortexPort int, mode Mode) {
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
	terminalIdentity, terminalTurnReady := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if terminalTurnReady && hasLocalizationTerminal(terminalIdentity) {
		// Let only Gortex navigation reach the engine's immutable successful
		// replay. Native Read/Grep/Glob and every other tool remain denied here,
		// so the host cannot turn terminal localization into unrestricted work.
		if localizationNavigationTool(input.ToolName) {
			return
		}
		emitPreToolUse(HookOutput{HookSpecificOutput: &HookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: localizationTerminalDenyReason,
		}})
		localizationTerminalTelemetry("denied", true, started)
		return
	}
	if terminalTurnReady {
		// Correlate the current turn with this exact tool invocation. PostToolUse
		// consumes the snapshot, so a delayed result from a previous turn cannot
		// arm the new turn's marker.
		_ = snapshotLocalizationToolUse(input, terminalIdentity)
	}

	// The installed matcher is deliberately broad so terminal state can stop
	// any tool. With no marker, tools outside the historical access-policy
	// matcher must be an immediate no-op: no daemon probe, classification,
	// enrichment, or telemetry I/O.
	if !preToolUsePolicyTool(input.ToolName) {
		return
	}

	emitted := false
	defer func() {
		logHookEffectiveness("PreToolUse", emitted, daemonReachableFn(), hookAlternationSegmentCount(input), time.Since(started))
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
		if adv := gortexReadNudge(input.ToolName, input.ToolInput); adv != "" {
			hso.AdditionalContext = adv
		}
		emitted = hso.AdditionalContext != "" || hso.PermissionDecisionReason != ""
		emitPreToolUse(HookOutput{HookSpecificOutput: hso})
		return
	}

	// Consult-unlock handshake: any Gortex MCP tool call records that
	// the agent has consulted the graph this session. The hook sees the
	// MCP call in-process, so the marker is fully self-contained. The
	// call itself is a no-op pass-through — nothing to enrich.
	if mode == ModeConsultUnlock && isGortexMCP {
		markGraphConsulted(input.SessionID)
		return
	}

	result := applyMode(input, isGortexMCP, mode, enrich(input, gortexPort))

	if result.context == "" && !result.deny {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName: "PreToolUse",
		},
	}

	if result.deny {
		output.HookSpecificOutput.PermissionDecision = "deny"
		output.HookSpecificOutput.PermissionDecisionReason = result.reason
	} else {
		output.HookSpecificOutput.AdditionalContext = result.context
	}

	emitted = true
	emitPreToolUse(output)
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

// emitPreToolUse marshals a PreToolUse HookOutput to stdout. A marshal
// failure is swallowed — a hook must never block Claude Code's flow.
func emitPreToolUse(output HookOutput) {
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// downgradeReason picks the human text to surface when a deny is
// softened to additionalContext: the deny reason if present, else the
// advisory context. Shared by ModeEnrich and ModeConsultUnlock.
func downgradeReason(result enrichResult) string {
	if result.reason != "" {
		return result.reason
	}
	return result.context
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

	fileIndexed, symbolCount := queryFileIndexed(cwd, filePath)

	// If the file is indexed, BLOCK the read and provide graph alternatives.
	if fileIndexed {
		var reason strings.Builder
		fmt.Fprintf(&reason, "[Gortex] BLOCKED: Read of %s (%d symbols indexed). Call `explore` first, then use `read` instead:\n", filePath, symbolCount)
		reason.WriteString("  - `read(target:{symbol:\"<id>\"})` — one symbol\n")
		reason.WriteString("  - `read(target:{symbols:[\"<id>\"]})` — several symbols\n")
		reason.WriteString("  - `read(operation:\"editing_context\", target:{file:\"<path>\"})` — full editing context\n")
		reason.WriteString(gcxTip)
		reason.WriteString(toolref.MCPRequiredLine())

		return enrichResult{
			deny:   true,
			reason: reason.String(),
		}
	}

	// File not indexed — allow with advisory.
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
type grepProbeFn func(pattern string, timeout time.Duration) ([]grepSymbolHit, error)

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
	if hookSearchScopeIndexed(cwd, toolInput) {
		return enrichResult{
			deny:   true,
			reason: formatTrackedSearchDeny("Grep", pattern),
		}
	}
	return probeSymbolPattern("Grep", pattern, defaultGrepGuidance())
}

func hookSearchScopeIndexed(cwd string, toolInput map[string]any) bool {
	scope, _ := toolInput["path"].(string)
	scope = strings.TrimSpace(scope)
	if scope != "" {
		abs := scope
		if !filepath.IsAbs(abs) && cwd != "" {
			abs = filepath.Join(cwd, abs)
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			if !looksLikeSourceFile(scope) {
				return false
			}
			indexed, _ := queryFileIndexed(cwd, scope)
			return indexed
		}
		if filepath.Ext(scope) != "" {
			if !looksLikeSourceFile(scope) {
				return false
			}
			indexed, _ := queryFileIndexed(cwd, scope)
			return indexed
		}
	}
	return scopeTrackedFn(cwd, scope)
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
func probeSymbolPattern(tool, pattern, guidance string) enrichResult {
	if pattern == "" {
		return enrichResult{}
	}

	segments := splitAlternation(pattern)
	if len(segments) == 1 {
		return probeSinglePattern(tool, segments[0], guidance)
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
	hits, reached := probeSegments(symbolSegs)
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
func probeSinglePattern(tool, pattern, guidance string) enrichResult {
	if classifyGrepPattern(pattern) != GrepPatternSymbol {
		if len(pattern) > 2 {
			logHookDecision(tool, pattern, DecisionSkippedNonSymbol, 0, 0)
			return enrichResult{context: guidance}
		}
		return enrichResult{}
	}

	start := time.Now()
	hits, err := grepProbe(pattern, grepProbeTimeout)
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
func probeSegments(segs []string) (hits []grepSymbolHit, reached bool) {
	seen := make(map[string]bool)
	for _, s := range segs {
		found, err := grepProbe(s, grepProbeTimeout)
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
	const maxShown = 5
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: \"%s\" matches %d indexed symbol(s). Use `search(operation:\"symbols\")` or `relations(operation:\"usages\")`:\n\n", pattern, len(hits))
	shown := min(len(hits), maxShown)
	for i := range shown {
		h := hits[i]
		kind := h.Kind
		if kind == "" {
			kind = "symbol"
		}
		fmt.Fprintf(&b, "  %s — %s:%d (%s)\n", h.Name, h.FilePath, h.Line, kind)
	}
	if len(hits) > maxShown {
		fmt.Fprintf(&b, "  ... and %d more\n", len(hits)-maxShown)
	}
	b.WriteString("\n")
	b.WriteString("Localizing a task rather than one symbol? `explore` returns the ranked neighborhood (symbols + source + call paths) in one call.\n")
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	b.WriteString("To force text search, add a regex metachar (e.g. \\b) or quote the pattern.")
	return b.String()
}

// queryFileIndexed reports whether the file at filePath is indexed by the
// daemon, with the symbol count when it is. cwd scopes the probe to the
// right workspace (and absolutises a relative filePath). A zero return
// (false, 0) is the "no signal" case — daemon unreachable, malformed
// response, or file genuinely not indexed; callers treat all three the
// same (fall through to soft guidance).
//
// fileIndexedFn is the seam tests stub; production routes through the
// daemon's MCP socket (the old HTTP :8765 /api/graph/file endpoint this
// used to hit was removed when the web API migrated to the daemon, which
// is why the hard deny silently stopped firing for every agent).
func queryFileIndexed(cwd, filePath string) (bool, int) {
	return fileIndexedFn(cwd, filePath)
}

// fileIndexedTimeout bounds the daemon probe so a wedged daemon never
// stalls the PreToolUse critical path.
const fileIndexedTimeout = 2 * time.Second

var fileIndexedFn = fileIndexedViaDaemon

// fileIndexedViaDaemon asks the daemon's get_file_summary tool how many
// definition symbols the file carries, over the AF_UNIX MCP channel. The
// graph keys files by their repo-relative path, so the absolute file path
// is resolved against its tracked-repo root and the root-relative path is
// what gets queried (with the handshake CWD set to that root for scoping).
//
// TODO(hook-local perf): each probe opens a fresh MCP connection, so a wide
// postGlob still pays one dial per file even though repoRootForFile's status
// fetch is now memoised. Reusing a single connection across the batch — or a
// count-only probe that skips get_file_summary's ensureFresh re-index on the
// hot path — is the next optimisation. Deferred so the test seam
// (fileIndexedFn) stays a simple per-file func; left to the maintainer.
func fileIndexedViaDaemon(cwd, filePath string) (bool, int) {
	resp, ok := daemonFileSummaryRaw(cwd, filePath)
	if !ok {
		return false, 0
	}
	return parseFileSummaryIndexed(resp)
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
	status, err := cachedDaemonStatus()
	if err != nil || status == nil {
		return ""
	}
	var best string
	for _, r := range status.TrackedRepos {
		if r.Path == "" {
			continue
		}
		if abs == r.Path || strings.HasPrefix(abs, r.Path+string(filepath.Separator)) {
			if len(r.Path) > len(best) {
				best = r.Path
			}
		}
	}
	return best
}

// parseFileSummaryIndexed unwraps a get_file_summary tools/call response —
// JSON-RPC envelope → first content block (the JSON payload as text) →
// total_nodes. get_file_summary strips the file/import nodes, so
// total_nodes is the definition-symbol count; a not-indexed file comes back
// as a tool error / guidance text, which fails the parse → (false, 0).
func parseFileSummaryIndexed(resp []byte) (bool, int) {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return false, 0
	}
	if rpc.Result.IsError || len(rpc.Result.Content) == 0 {
		return false, 0
	}
	var summary struct {
		TotalNodes int `json:"total_nodes"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &summary); err != nil {
		return false, 0
	}
	if summary.TotalNodes <= 0 {
		return false, 0
	}
	return true, summary.TotalNodes
}

// enrichBash classifies the Bash command and routes codebase-search shapes
// through the same graph probes the Grep and Read enrichments use. Anything
// not recognised as a codebase search passes through silently — false-deny
// is more disruptive than a miss, so the classifier only flags primary
// grep/rg/find-name/cat-source invocations.
func enrichBash(toolInput map[string]any, cwd string) enrichResult {
	cmd, _ := toolInput["command"].(string)
	if cmd == "" {
		return enrichResult{}
	}

	c := classifyBashCommand(cmd)
	switch c.Action {
	case BashActionGrepLike:
		return probeSymbolPattern("Bash", c.Pattern, defaultGrepGuidance())

	case BashActionFindName:
		// find -name values often include `*` globs; the classifier has
		// already stripped wildcards, but the residue may still be
		// non-symbol-shaped (e.g. ".go" from `-name "*.go"`) — let
		// probeSymbolPattern decide.
		return probeSymbolPattern("Bash", c.Pattern, defaultGrepGuidance())

	case BashActionReadSource:
		indexed, symbolCount := queryFileIndexed(cwd, c.Path)
		if indexed {
			var reason strings.Builder
			fmt.Fprintf(&reason,
				"[Gortex] BLOCKED: Bash `%s %s` reads indexed source (%d symbols). Use graph tools instead:\n",
				c.Primary, c.Path, symbolCount)
			reason.WriteString("  - one symbol: `read(target:{symbol:\"<id>\"})`\n")
			reason.WriteString("  - file overview: `read(operation:\"summary\", target:{file:\"<path>\"})`\n")
			reason.WriteString("  - before editing: `read(operation:\"editing_context\", target:{file:\"<path>\"})`\n")
			reason.WriteString(gcxTip)
			reason.WriteString(toolref.MCPRequiredLine())
			return enrichResult{deny: true, reason: reason.String()}
		}
		// Not indexed — soft guidance so Bash proceeds.
		var g strings.Builder
		g.WriteString("[Gortex] Use `read` instead of Bash cat/head/tail for indexed source:\n")
		g.WriteString("  - `read(target:{symbol:\"<id>\"})` for one symbol; use operation `summary` for an overview or `editing_context` before editing\n")
		g.WriteString(gcxTip)
		g.WriteString(toolref.MCPRequiredLine())
		return enrichResult{context: g.String()}
	}

	return enrichResult{}
}

// daemonReachableFn is the seam tests use to fake daemon availability
// without a real socket. Production reads daemon.IsRunning.
var daemonReachableFn = daemon.IsRunning

// scopeTrackedFn proves that a Grep/Glob scope contains at least one indexed
// source file. Tests replace it so fallback cases stay deterministic.
var scopeTrackedFn = scopeTrackedViaDaemon

func scopeTrackedViaDaemon(cwd, scope string) bool {
	if !daemonReachableFn() {
		return false
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = cwd
	}
	if scope == "" {
		return false
	}
	if !filepath.IsAbs(scope) {
		if cwd == "" {
			return false
		}
		scope = filepath.Join(cwd, scope)
	}
	scope = filepath.Clean(scope)
	info, err := os.Stat(scope)
	if err != nil || !info.IsDir() {
		return false
	}
	root := repoRootForFile(scope)
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, scope)
	if err != nil {
		return false
	}

	client, err := daemon.Dial(hookMCPHandshake(root))
	if err != nil {
		return false
	}
	defer client.Close()
	_ = client.Conn.SetDeadline(time.Now().Add(fileIndexedTimeout))

	arguments := map[string]any{"glob": "**/*", "limit": 1, "format": "json"}
	if rel != "." {
		arguments["path"] = filepath.ToSlash(rel)
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "find_files",
			"arguments": arguments,
		},
	})
	if err != nil || client.WriteMCPFrame(frame) != nil {
		return false
	}
	resp, err := client.ReadMCPFrame()
	return err == nil && parseFindFilesHasSource(resp)
}

func parseFindFilesHasSource(resp []byte) bool {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &rpc) != nil || rpc.Result.IsError || len(rpc.Result.Content) == 0 {
		return false
	}
	var files struct {
		Count int `json:"count"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &files) != nil {
		return false
	}
	return files.Count > 0 && len(files.Files) > 0
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

	if hookSearchScopeIndexed(cwd, toolInput) {
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

// editBlockingEnvVar gates Edit/Write enforcement. We ship behind a
// flag because Edit/Write redirects are higher-blast-radius than
// Read/Grep — false positives stop the agent from making any
// progress at all. Once we have field telemetry showing the
// classifier is reliable, the gate can flip default-on or be
// removed.
const editBlockingEnvVar = "GORTEX_HOOK_BLOCK_EDIT"

// editBlockingEnabled reports whether the env-gated Edit/Write
// redirect is on. Anything besides empty/"0"/"false"/"no"/"off"
// enables.
func editBlockingEnabled() bool {
	return envGateEnabled(editBlockingEnvVar)
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
// Gortex MCP edit tools. Behind GORTEX_HOOK_BLOCK_EDIT until the
// classifier is proven; without it the hook is a no-op so Edit
// behaves exactly as it did pre-feature.
func enrichEdit(toolInput map[string]any, cwd string) enrichResult {
	if !editBlockingEnabled() {
		return enrichResult{}
	}
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}
	indexed, _ := queryFileIndexed(cwd, filePath)
	if !indexed {
		return enrichResult{}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: Edit of %s (indexed source). Use Gortex MCP edit tools — they don't require a prior Read and update the graph atomically:\n", filePath)
	b.WriteString("  - choose `edit` operation `symbol`, `file`, or `batch`; for example `edit(operation:\"file\", target:{file:\"<path>\"})`\n")
	b.WriteString("  - `refactor(operation:\"rename\")` for a coordinated rename\n\n")
	b.WriteString(toolref.MCPRequiredLine())
	b.WriteString("To bypass this redirect: unset GORTEX_HOOK_BLOCK_EDIT, or target a file outside the tracked repos.\n")
	return enrichResult{deny: true, reason: b.String()}
}

// enrichWrite mirrors enrichEdit for whole-file Write. New files
// (not yet indexed) pass through; rewrites of existing indexed
// files are redirected to `edit_file` / `write_file`.
func enrichWrite(toolInput map[string]any, cwd string) enrichResult {
	if !editBlockingEnabled() {
		return enrichResult{}
	}
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}
	indexed, _ := queryFileIndexed(cwd, filePath)
	if !indexed {
		return enrichResult{}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: Write of %s (indexed source — would overwrite existing tracked file). Use:\n", filePath)
	b.WriteString("  - `edit(operation:\"write\")` for a whole-file write\n")
	b.WriteString("  - `edit(operation:\"file\")` for a guarded replacement\n\n")
	b.WriteString(toolref.MCPRequiredLine())
	b.WriteString("To bypass: unset GORTEX_HOOK_BLOCK_EDIT, or target a path outside tracked repos.\n")
	return enrichResult{deny: true, reason: b.String()}
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
