package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// external_agent.go handles lifecycle hooks for the non-Claude agents
// that share Claude Code's hookSpecificOutput.additionalContext wire
// shape — the Gemini CLI and Google Antigravity. The gortex adapters
// install `gortex hook --agent <name>` SessionStart + AfterTool hooks;
// this handler answers them, injecting the same session orientation and
// stale-index hints Claude gets via its SessionStart hook.

// ExternalAgentInput is the stdin JSON the Gemini CLI / Antigravity send
// on a lifecycle hook. We read only the fields we use; the shapes
// overlap with Claude's hook payloads.
type ExternalAgentInput struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	CWD           string `json:"cwd"`
}

// RunExternalAgent reads a lifecycle-hook event from stdin and emits a
// hookSpecificOutput.additionalContext block for a non-Claude agent. It
// is the entry point for `gortex hook --agent <gemini|antigravity>`.
func RunExternalAgent() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	handleExternalAgent(data)
}

// handleExternalAgent is the testable core: parse the event, build the
// context, emit it. SessionStart gets the full orientation; a post-tool
// event gets a concise stale-index hint, emitted only when something is
// actionable (so the hook stays quiet on the happy path).
func handleExternalAgent(data []byte) {
	var input ExternalAgentInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}

	var ctx string
	switch normalizeExternalEvent(input.HookEventName) {
	case "sessionstart":
		ctx = buildSessionStartBriefing(input.CWD)
	case "aftertool":
		ctx = buildStaleIndexHint(input.CWD)
	default:
		return
	}
	if strings.TrimSpace(ctx) == "" {
		return
	}

	out, err := json.Marshal(HookOutput{HookSpecificOutput: &HookSpecificOutput{
		HookEventName:     input.HookEventName,
		AdditionalContext: ctx,
	}})
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// normalizeExternalEvent maps the agent's event name onto the two
// shapes we handle. Gemini/Antigravity use "SessionStart" and
// "AfterTool"; we also accept Claude's "PostToolUse" spelling so a
// shared config works either way.
func normalizeExternalEvent(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "sessionstart":
		return "sessionstart"
	case "aftertool", "posttooluse":
		return "aftertool"
	default:
		return ""
	}
}

// buildStaleIndexHint returns a one-line actionable hint when the graph
// is unavailable or the cwd is not covered by a tracked repo, and ""
// when everything is fresh — keeping the per-tool hook quiet on the
// happy path rather than spamming context every tool call.
func buildStaleIndexHint(cwd string) string {
	status, err := sessionStartStatusFn()
	switch {
	case errors.Is(err, errDaemonUnreachable):
		return "[Gortex] graph transport is unreachable — treat this as an MCP integration failure, stop indexed code operations, and report it; do not start a daemon manually or switch to a CLI fallback."
	case err != nil:
		return ""
	}
	if !status.Ready {
		return fmt.Sprintf("[Gortex] daemon is warming up (%s elapsed); graph answers may be partial until it is ready.", formatDuration(status.WarmupSeconds))
	}
	if cwd != "" {
		abs, e := filepath.Abs(cwd)
		if e != nil {
			abs = cwd
		}
		exact, contained := classifyCwd(abs, status.TrackedRepos)
		if exact == nil && len(contained) == 0 {
			if pendingAutomaticCheckout(abs, status.TrackedRepos) {
				return fmt.Sprintf("[Gortex] cwd `%s` is a Git checkout awaiting automatic discovery; do not run `gortex track`. Wait for reconciliation and reconnect.", abs)
			}
			return fmt.Sprintf("[Gortex] cwd `%s` is not tracked — run `gortex track %s` to enable graph context here.", abs, abs)
		}
	}
	return ""
}
