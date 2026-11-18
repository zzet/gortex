package hooks

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

// opencode.go is the Go side of the OpenCode (v1.18.18+ and 2.x) integration.
//
// OpenCode has no lifecycle-hook configuration at all — there is no settings
// key that runs a command on session start or before a tool call. Its only
// extension point is a JS/TS plugin exposing `tool.execute.before` (throwing
// from it blocks the call), `tool.execute.after`, `permission.ask` (returns
// "ask" | "deny" | "allow"), `chat.message`, and a bus `event` hook. OpenCode
// 2 moves the same points onto domains (`tool` execute.before/after,
// `permission` evaluate, `session` prompt); the plugin serves both and keeps
// sending the 1.x event names below, so the router has one vocabulary. So
// OpenCode gets the same treatment Pi does: the Gortex-owned bridge envelope
// (pi.go), with a small JavaScript plugin that shells
// `gortex hook --agent=opencode`, writes a BridgeEvent to stdin, and applies
// the BridgeDecision it reads back — Block on `tool.execute.before` /
// `permission.ask`, Orientation on the session event, AdditionalContext on
// `chat.message`.
//
// Everything policy-shaped (the deny / enrich / consult-unlock / nudge
// postures, indexed-source classification, telemetry) therefore lives in the
// shared bridge core rather than in the plugin, where it would have to be
// re-implemented in TypeScript and would drift.

// openCodeToolNames maps OpenCode's built-in tool vocabulary onto the Claude
// names the shared enrichment leaves switch on.
//
// The plugin is expected to send names already normalized (that is the
// bridge contract), so this table is belt and braces — but a cheap one: if a
// plugin build ever forwards OpenCode's own lowercase names instead, the
// policy would otherwise classify nothing and the install would look healthy
// while enforcing nothing. Already-normalized names miss the table and pass
// through untouched, so applying it twice is a no-op.
var openCodeToolNames = map[string]string{
	"read":  "Read",
	"write": "Write",
	"edit":  "Edit",
	"bash":  "Bash",
	"grep":  "Grep",
	"glob":  "Glob",
	"task":  "Task",
	// OpenCode 2 renamed bash, task and the multi-file edit.
	"shell":    "Bash",
	"patch":    "Edit",
	"subagent": "Task",
}

// RunOpenCode reads a single bridge envelope from stdin, dispatches on the
// event phase, and writes a decision to stdout. Any read/parse error is a
// silent no-op (an empty decision) — a hook must never break the host
// agent's flow.
func RunOpenCode(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		emitBridgeDecision(BridgeDecision{})
		return
	}
	emitBridgeDecision(handleOpenCode(data, port, mode))
}

// handleOpenCode is the testable core: decode the envelope, normalize the
// one host-specific field, and hand it to the shared bridge router.
func handleOpenCode(data []byte, port int, mode Mode) BridgeDecision {
	var ev BridgeEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return BridgeDecision{}
	}
	ev.ToolName = openCodeToolName(ev.ToolName)
	return handleBridgeEvent(ev, port, mode)
}

// openCodeToolName maps one OpenCode tool name into the Claude vocabulary,
// leaving anything unrecognised untouched.
func openCodeToolName(name string) string {
	if mapped, ok := openCodeToolNames[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return strings.TrimSpace(name)
}
