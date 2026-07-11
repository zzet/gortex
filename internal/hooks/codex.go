package hooks

import (
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// RunCodex handles the Codex hook wire shape. Codex support is deliberately
// soft-only: PreToolUse is forced through ModeEnrich, PostToolUse only emits
// additionalContext, and UserPromptSubmit re-surfaces prompt-relevant graph
// symbols on every turn. No branch ever denies a tool call.
func RunCodex(port int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runCodex(data, port)
}

func runCodex(data []byte, port int) {
	var peek struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}

	started := time.Now()
	emitted, reachability, alternations := false, "not_checked", 0
	switch {
	case peek.HookEventName == "PreToolUse" && peek.ToolName == "Bash":
		emitted = runPreToolUse(data, port, ModeEnrich)
		reachability = codexDaemonReachability()
		alternations = codexAlternationSegments(data)
	case peek.HookEventName == "PreToolUse" && codexMCPReadPreToolUseTool(peek.ToolName):
		emitted = runCodexMCPReadPreToolUse(data)
	case peek.HookEventName == "PostToolUse" && peek.ToolName == "Bash":
		emitted = runCodexPostToolUse(data)
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

func runCodexMCPReadPreToolUse(data []byte) bool {
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
	return emitPreToolUse(HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "PreToolUse",
			AdditionalContext: ctx,
		},
	})
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
