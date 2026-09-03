package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
)

// Hermes (NousResearch hermes-agent) speaks a different hook wire
// protocol than Claude Code, so it gets its own dispatcher rather than
// riding Run. Three things differ:
//
//   - Event names are snake_case (`pre_tool_call`, `pre_llm_call`)
//     instead of Claude's PascalCase (`PreToolUse`, `UserPromptSubmit`).
//   - The tool surface is narrower: file reads are a single `read_file`
//     tool and every shell command (grep / rg / find / cat) rides inside
//     one `terminal` tool — there is no separate Grep / Glob / Bash.
//   - The stdout decision shape is `{"action":"block","message":...}`
//     (canonical) or `{"context":...}` (pre_llm_call injection), not
//     Claude's `hookSpecificOutput` envelope.
//
// The leaf enrichment logic (enrichRead / enrichBash, the daemon probe,
// the session-state postures, the prompt-injection builder) is shared
// with the Claude Code handlers — only the input decoding and output
// encoding are Hermes-specific.

// Hermes hook event names, as documented in hermes-agent's
// website/docs/user-guide/features/hooks.md.
const (
	hermesEventPreToolCall = "pre_tool_call"
	hermesEventPreLLMCall  = "pre_llm_call"
)

// Hermes tool names the pre_tool_call matcher targets. read_file is the
// file-read tool; terminal wraps every shell invocation (the grep / find
// / cat redirect inspects its `command` argument). These are the strings
// the installed matcher regex ("read_file|terminal") must agree with.
const (
	hermesReadFileTool = "read_file"
	hermesTerminalTool = "terminal"
)

// hermesDecision is the Hermes-canonical hook response. A blocking
// decision sets Action="block" + Message; a pre_llm_call injection sets
// Context. An all-empty struct marshals to `{}` — a no-op Hermes
// ignores. We emit the canonical action/message shape (Hermes also
// accepts the Claude-compatible decision/reason shape, but the native
// one is the stable contract across versions).
type hermesDecision struct {
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
	Context string `json:"context,omitempty"`
}

// hermesPreToolInput decodes the pre_tool_call / post_tool_call payload.
// The field names match Claude's HookInput by design — Hermes pipes the
// same snake_case keys — so we keep a dedicated struct only to document
// the Hermes shape and stay decoupled from Claude's permission_mode.
type hermesPreToolInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	SessionID     string         `json:"session_id"`
	CWD           string         `json:"cwd"`
}

// hermesPreLLMInput decodes the pre_llm_call payload. Hermes carries the
// event-specific fields (user_message, is_first_turn, conversation
// history) inside an `extra` dict, but some versions also surface them
// at the top level — we read both and coalesce so the handler works
// regardless of which layout a given Hermes build sends.
type hermesPreLLMInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	UserMessage   string `json:"user_message"`
	IsFirstTurn   bool   `json:"is_first_turn"`
	Extra         struct {
		UserMessage string `json:"user_message"`
		IsFirstTurn bool   `json:"is_first_turn"`
		CWD         string `json:"cwd"`
	} `json:"extra"`
}

// RunHermes reads a single Hermes hook payload from stdin, peeks at
// hook_event_name, and dispatches. Like Run, any read / parse failure is
// a silent no-op — a hook must never abort the agent's flow.
func RunHermes(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var peek struct {
		HookEventName string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}

	switch peek.HookEventName {
	case hermesEventPreToolCall:
		runHermesPreToolCall(data, port, mode)
	case hermesEventPreLLMCall:
		runHermesPreLLMCall(data)
	}
}

// emitHermes marshals a Hermes decision to stdout. An empty decision is
// still emitted as `{}` so a hook that decided "no-op" produces valid
// JSON rather than empty stdout. A marshal failure is swallowed.
func emitHermes(d hermesDecision) {
	out, err := json.Marshal(d)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// runHermesPreToolCall maps a Hermes tool call onto the shared
// enrichment logic and, when the call should be blocked, emits a
// Hermes block decision. The four postures mirror the Claude Code
// PreToolUse modes — with one structural difference: pre_tool_call has
// no soft-context channel (it can only block or pass), so ModeEnrich
// degrades to "never block" and the agent's guidance is delivered via
// the pre_llm_call injection instead.
func runHermesPreToolCall(data []byte, port int, mode Mode) {
	var input hermesPreToolInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != hermesEventPreToolCall {
		return
	}

	// pre_tool_call can only block or pass — it has no soft-context
	// channel like Claude's additionalContext. ModeEnrich never blocks,
	// so it is a pure no-op here; the graph nudge rides on the
	// pre_llm_call injection instead. Returning before the daemon probe
	// keeps an enrich install cheap.
	if mode == ModeEnrich {
		return
	}

	isGortexTool := isHermesGortexTool(input.ToolName)

	// Consult-unlock handshake: the first Gortex graph tool call this
	// session flips the marker that downgrades subsequent denies. The
	// call itself is a pass-through.
	if mode == ModeConsultUnlock && isGortexTool {
		markGraphConsulted(input.SessionID)
		return
	}

	// Daemon outage: never block (#486). pre_tool_call's only channel is a
	// block, and a block that redirects to graph tools the daemon cannot
	// serve deadlocks the agent. The consult-unlock marker above is local
	// state and deliberately stays live.
	if !daemonReachableFn() {
		return
	}

	result := hermesEnrich(input, port)

	switch mode {
	case ModeConsultUnlock:
		if result.deny {
			if loadSessionState(input.SessionID).GraphConsulted {
				return
			}
			result.reason = consultUnlockReason(result.reason)
		}
	case ModeAdaptiveNudge:
		result = hermesAdaptiveNudge(input, isGortexTool, result)
	}

	if !result.deny {
		return
	}
	emitHermes(hermesDecision{Action: "block", Message: result.reason})
}

// hermesEnrich routes a Hermes tool call to the matching Claude-side
// enrichment. read_file reuses enrichRead (after normalising the path
// key, which Hermes names `path` rather than Claude's `file_path`);
// terminal reuses enrichBash, whose `command` key Hermes already
// matches. Any other tool is a no-op.
//
// It reaches those enrichments directly rather than through enrich, so it
// applies the same tracked-repo gate itself — a Hermes session in an unindexed
// checkout has no more to redirect to than a Claude one.
func hermesEnrich(input hermesPreToolInput, _ int) enrichResult {
	switch input.ToolName {
	case hermesReadFileTool:
		toolInput := hermesNormalizeReadInput(input.ToolInput)
		if !hookCallTargetsTrackedRepo("Read", toolInput, input.CWD) {
			return enrichResult{}
		}
		return enrichRead(toolInput, input.CWD)
	case hermesTerminalTool:
		if !hookCallTargetsTrackedRepo("Bash", input.ToolInput, input.CWD) {
			return enrichResult{}
		}
		return enrichBash(input.ToolInput, input.CWD)
	default:
		return enrichResult{}
	}
}

// hermesNormalizeReadInput returns a tool-input map enrichRead
// understands: it copies the original (so offset/limit narrow-read
// hints survive) and ensures a `file_path` key, sourced from whichever
// path key Hermes used. Hermes' read_file tool names its argument
// `path`; we also accept the Claude-style `file_path` and a couple of
// common aliases so the redirect fires regardless of the exact schema.
func hermesNormalizeReadInput(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	maps.Copy(out, in)
	if _, ok := out["file_path"].(string); ok {
		return out
	}
	for _, key := range []string{"path", "filename", "file"} {
		if p, ok := in[key].(string); ok && p != "" {
			out["file_path"] = p
			break
		}
	}
	return out
}

// isHermesGortexTool reports whether a Hermes tool name is one of
// Gortex's own MCP graph tools (as opposed to read_file / terminal).
// Used by the consult-unlock and nudge postures to detect that the
// agent consulted the graph. Hermes namespaces an MCP server's tools
// with the server name ("gortex"), so a substring match is the robust
// test across Hermes' naming variants (gortex.search_symbols,
// gortex__search_symbols, …) without hard-coding the full tool list.
func isHermesGortexTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == hermesReadFileTool || n == hermesTerminalTool {
		return false
	}
	return strings.Contains(n, "gortex")
}

// hermesAdaptiveNudge is the Hermes counterpart of adaptiveNudge: it
// tracks a per-session streak of non-symbolic fallback calls and
// soft-denies once when the streak crosses nudgeThreshold, then resets.
// A Gortex graph tool call (or any call enrich didn't flag) resets the
// streak. Kept separate from adaptiveNudge because that helper is keyed
// to Claude's enrichResult flow; the posture logic is identical.
func hermesAdaptiveNudge(input hermesPreToolInput, isGortexTool bool, result enrichResult) enrichResult {
	if isGortexTool || !result.deny {
		st := loadSessionState(input.SessionID)
		if st.NonSymbolicStreak != 0 {
			st.NonSymbolicStreak = 0
			saveSessionState(input.SessionID, st)
		}
		return enrichResult{}
	}

	st := loadSessionState(input.SessionID)
	st.NonSymbolicStreak++
	if st.NonSymbolicStreak >= nudgeThreshold {
		st.NonSymbolicStreak = 0
		saveSessionState(input.SessionID, st)
		logHookDecision(input.ToolName, "", DecisionNudged, 0, 0)
		return enrichResult{deny: true, reason: nudgeReason(downgradeReason(result))}
	}
	saveSessionState(input.SessionID, st)
	return enrichResult{}
}

// runHermesPreLLMCall handles the pre_llm_call event — Hermes' single
// context-injection point, which it joins into the user message
// (preserving the prompt cache). It does double duty, covering two
// Claude Code surfaces at once:
//
//   - On the first turn (is_first_turn) it injects the Gortex session
//     orientation briefing — the equivalent of Claude's SessionStart
//     hook, which has no Hermes counterpart (on_session_start is
//     observer-only and cannot inject).
//   - On every later turn it surfaces graph symbols relevant to the
//     user's message — the equivalent of Claude's UserPromptSubmit.
//
// Best-effort throughout: a daemon miss, an empty result, or a trivial
// prompt is a silent no-op so a turn is never blocked or padded.
func runHermesPreLLMCall(data []byte) {
	var input hermesPreLLMInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != hermesEventPreLLMCall {
		return
	}

	userMessage := firstNonEmpty(input.UserMessage, input.Extra.UserMessage)
	isFirstTurn := input.IsFirstTurn || input.Extra.IsFirstTurn
	cwd := firstNonEmpty(input.CWD, input.Extra.CWD)

	var ctx string
	if isFirstTurn {
		ctx = buildSessionStartBriefing(cwd)
	} else {
		query := promptQuery(userMessage)
		if query == "" {
			return
		}
		hits, err := probeViaDaemon(query, cwd, userPromptProbeTimeout)
		if err != nil || len(hits) == 0 {
			return
		}
		ctx = buildPromptInjection(hits)
	}
	if ctx == "" {
		return
	}
	emitHermes(hermesDecision{Context: ctx})
}

// firstNonEmpty returns the first non-empty string of its arguments, or
// "" when all are empty. Used to coalesce the top-level vs `extra`
// layouts of the pre_llm_call payload.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
