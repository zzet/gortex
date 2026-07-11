package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// RunCodex handles the Codex hook wire shape. Codex defaults to ModeEnrich,
// but callers may explicitly opt into a stricter posture.
func RunCodex(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runCodexWithMode(data, port, mode)
}

func runCodex(data []byte, port int) {
	runCodexWithMode(data, port, ModeEnrich)
}

func runCodexWithMode(data []byte, port int, mode Mode) {
	var peek struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		CWD           string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}
	setHookCWD(peek.CWD)
	defer setHookCWD("")

	started := time.Now()
	emitted, reachability, alternations := false, "not_checked", 0
	switch {
	case peek.HookEventName == "PreToolUse" && peek.ToolName == "Bash":
		emitted = runPreToolUse(data, port, mode)
		reachability = codexDaemonReachability()
		alternations = codexAlternationSegments(data)
	case peek.HookEventName == "PreToolUse" && codexMCPReadPreToolUseTool(peek.ToolName):
		emitted = runCodexMCPReadPreToolUse(data, mode)
	case peek.HookEventName == "PostToolUse" && peek.ToolName == "Bash":
		emitted = runCodexPostToolUse(data)
		reachability = codexDaemonReachability()
	case peek.HookEventName == "PostToolUse" && peek.ToolName == "apply_patch":
		emitted = runCodexMutationPostToolUse(data, port)
		reachability = codexDaemonReachability()
	case peek.HookEventName == "UserPromptSubmit":
		// Re-surface graph symbols relevant to the prompt on every turn.
		// Codex forgets MCP tools as context grows, so a SessionStart
		// orientation alone fades; this lands a fresh, prompt-specific
		// nudge at the top of each turn (the wire shape is shared with
		// Claude Code — hookSpecificOutput.additionalContext).
		emitted = runUserPromptSubmit(data)
		reachability = codexDaemonReachability()
	default:
		return
	}
	logCodexHookEffect(peek.HookEventName, peek.ToolName, emitted, reachability, alternations, time.Since(started))
}

// runCodexMutationPostToolUse runs the same graph diagnostics as a terminal
// post-task check after Codex's native apply_patch tool succeeds. It is
// advisory-only: a failed/empty daemon response is silent and never changes
// the applied patch or asks Codex to repeat it.
func runCodexMutationPostToolUse(data []byte, port int) bool {
	var input postHookInput
	if json.Unmarshal(data, &input) != nil || input.HookEventName != "PostToolUse" || input.ToolName != "apply_patch" {
		return false
	}
	briefing := buildPostTaskBriefing(port)
	if briefing == "" {
		return false
	}
	out, err := json.Marshal(HookOutput{HookSpecificOutput: &HookSpecificOutput{
		HookEventName: "PostToolUse", AdditionalContext: briefing,
	}})
	if err != nil {
		return false
	}
	fmt.Print(string(out))
	return true
}

func codexDaemonReachability() string {
	if daemon.IsRunning() {
		return "reachable"
	}
	return "unreachable"
}

func codexMCPReadPreToolUseTool(toolName string) bool {
	switch toolName {
	case gortexReadFileTool, gortexEditingContextTool:
		return true
	default:
		return false
	}
}

func runCodexMCPReadPreToolUse(data []byte, mode Mode) bool {
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return false
	}
	if input.HookEventName != "PreToolUse" || !codexMCPReadPreToolUseTool(input.ToolName) {
		return false
	}

	ctx := gortexReadNudge(input.ToolName, input.ToolInput)
	if ctx == "" {
		return false
	}
	hso := &HookSpecificOutput{HookEventName: "PreToolUse", AdditionalContext: ctx}
	if mode == ModeDeny {
		hso.PermissionDecision = "deny"
		hso.PermissionDecisionReason = "[Gortex] BLOCKED: use the already-available Gortex MCP read tools with compress_bodies:true instead of a full-body source read."
	}
	return emitPreToolUse(HookOutput{HookSpecificOutput: hso})
}

func runCodexPostToolUse(data []byte) bool {
	var input postHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return false
	}
	if input.HookEventName != "PostToolUse" || input.ToolName != "Bash" {
		return false
	}

	cmd, _ := input.ToolInput["command"].(string)
	classification := classifyBashCommand(cmd)
	switch classification.Action {
	case BashActionGrepLike:
		// Codex wraps grep/rg/ag in Bash. Re-label that narrow shape as Grep so
		// the existing PostToolUse enrichment can parse path:line output and do
		// the graph lookup without changing Claude Code behavior.
		input.ToolName = "Grep"
	case BashActionFindName:
		input.ToolName = "Glob"
	case BashActionReadSource:
		if classification.Path == "" {
			return false
		}
		if input.ToolInput == nil {
			input.ToolInput = make(map[string]any)
		}
		input.ToolName = "Read"
		input.ToolInput["file_path"] = classification.Path
	case BashActionFileList:
		input.ToolName = "Glob"
	default:
		return false
	}

	normalized, err := json.Marshal(input)
	if err != nil {
		return false
	}
	return runPostToolUse(normalized)
}

func codexAlternationSegments(data []byte) int {
	var input HookInput
	if json.Unmarshal(data, &input) != nil {
		return 0
	}
	command, _ := input.ToolInput["command"].(string)
	c := classifyBashCommand(command)
	if c.Action != BashActionGrepLike {
		return 0
	}
	segments := splitAlternation(c.Pattern)
	if len(segments) <= 1 {
		return 0
	}
	return len(segments)
}
