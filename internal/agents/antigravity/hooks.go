package antigravity

import (
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

const (
	hookPluginName     = "gortex"
	hookTimeoutSeconds = 10
	hookAgentFlag      = "antigravity"

	// postToolUseMatcher limits PostToolUse to MCP calls, the only ones
	// its handler acts on (localization receipts from Gortex navigation
	// tools), so native tool calls stop spawning a gortex process. agy's
	// bundled hooks docs define matchers as regexes over tool names
	// ("run_command|view_file", "browser_.*"). Lazy MCP calls arrive as
	// call_mcp_tool and eager ones as mcp_gortex_<op>; the leading and
	// trailing .* match both whether or not agy anchors the regex.
	postToolUseMatcher = ".*mcp.*|.*gortex.*"
)

// hookShellIsCmd reports whether agy runs hook commands through `cmd /c`
// (Windows) rather than `sh -c`, as agy's bundled hooks docs state. A
// variable so tests can render both forms.
var hookShellIsCmd = runtime.GOOS == "windows"

// HookEvents are the canonical PascalCase lifecycle events this adapter
// installs, in the order they matter for adoption and `gortex doctor`.
//
// Antigravity's per-turn prompt hook is named "PreInvocation" on disk;
// antigravityHookEventNames translates between Gortex's canonical
// "UserPromptSubmit" telemetry bucket and the on-disk key so doctor's
// tally and internal/hooks' effectiveness allowlist stay unified.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

var antigravityHookEventNames = map[string]string{
	"SessionStart":     "SessionStart",
	"UserPromptSubmit": "PreInvocation",
	"PreToolUse":       "PreToolUse",
	"PostToolUse":      "PostToolUse",
	"Stop":             "Stop",
}

// HookConfigPath returns the user-level hooks.json path read by
// Antigravity CLI (agy) and Antigravity 2.0 (~/.gemini/config/hooks.json).
func HookConfigPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "config", "hooks.json")
}

func hookBinary(env agents.Env) string {
	if base := strings.TrimSpace(env.HookCommand); base != "" {
		if bin, ok := strings.CutSuffix(base, " hook"); ok {
			return strings.TrimSpace(bin)
		}
		return base
	}
	return agents.ResolveGortexHookBinary()
}

func posixQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hookCommandForEvent(env agents.Env, onDiskEvent string) string {
	args := " hook --agent=" + hookAgentFlag + " --event=" + onDiskEvent
	switch strings.ToLower(strings.TrimSpace(env.HookMode)) {
	case "enrich":
		args += " --mode=enrich"
	case "consult-unlock":
		args += " --mode=consult-unlock"
	case "nudge", "adaptive-nudge":
		args += " --mode=nudge"
	}
	return hookCommandLine(hookBinary(env), args)
}

// hookCommandLine joins the binary and its arguments for the shell agy runs
// hooks through: `sh -c` on Unix, `cmd /c` on Windows.
//
// cmd has no single-quote syntax, so a Windows path that needs quoting is
// double-quoted with its native backslashes kept, behind `call` so the line
// never starts with a quote. A line that does start with one loses its
// first and last quote to cmd unless it has exactly two quotes with none of
// & < > ( ) @ ^ | between them (see `cmd /?`), which breaks
// "C:\Program Files (x86)\..." under `cmd /c` and any quoted path under
// `cmd /s /c`. That holds when agy hands cmd the line as written; if it
// re-escapes the line as one argument, a path that needs quoting fails
// either way.
func hookCommandLine(bin, args string) string {
	if !hookShellIsCmd {
		return posixQuote(strings.ReplaceAll(bin, `\`, "/")) + args
	}
	bin = strings.ReplaceAll(bin, "/", `\`)
	if bin != "" && !strings.ContainsAny(bin, " \t&|<>^(),;=") {
		return bin + args
	}
	return `call "` + bin + `"` + args
}

func desiredHandler(env agents.Env, onDiskEvent string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": hookCommandForEvent(env, onDiskEvent),
		"timeout": hookTimeoutSeconds,
	}
}

func desiredEventValue(env agents.Env, onDiskEvent string) any {
	handler := desiredHandler(env, onDiskEvent)
	switch onDiskEvent {
	case "PreToolUse":
		// Every tool: the policy covers native file and shell tools too.
		// "*" is agy's match-all value, not a regex.
		return []any{map[string]any{"matcher": "*", "hooks": []any{handler}}}
	case "PostToolUse":
		return []any{map[string]any{"matcher": postToolUseMatcher, "hooks": []any{handler}}}
	}
	return []any{handler}
}

// upsertAntigravityHooks merges the "gortex" hook spec into root
// (~/.gemini/config/hooks.json). Unrelated top-level hook specs are
// preserved untouched, and an existing `"enabled": false` kill-switch
// inside the "gortex" spec is left in place, even under opts.Force.
func upsertAntigravityHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	changed := false
	spec, ok := root[hookPluginName].(map[string]any)
	if !ok {
		if _, exists := root[hookPluginName]; exists && !opts.Force {
			return false
		}
		spec = make(map[string]any)
		changed = true
	}

	for _, canonicalEvent := range HookEvents {
		onDiskEvent := antigravityHookEventNames[canonicalEvent]
		want := desiredEventValue(env, onDiskEvent)
		existing, exists := spec[onDiskEvent]
		if !exists || opts.Force || !agents.MCPEntriesEqual(existing, want) {
			spec[onDiskEvent] = want
			changed = true
		}
	}

	if changed {
		root[hookPluginName] = spec
	}
	return changed
}

func isGortexAntigravityCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "gortex") || !strings.Contains(lower, "hook") {
		return false
	}
	return strings.Contains(lower, "--agent="+hookAgentFlag) || strings.Contains(lower, "--agent "+hookAgentFlag)
}

func countGortexEventHandlers(onDiskEvent string, raw any) int {
	list, ok := antigravityHookSlice(raw)
	if !ok {
		return 0
	}
	count := 0
	if onDiskEvent == "PreToolUse" || onDiskEvent == "PostToolUse" {
		for _, item := range list {
			group, ok := item.(map[string]any)
			if !ok {
				continue
			}
			handlers, ok := antigravityHookSlice(group["hooks"])
			if !ok {
				continue
			}
			for _, h := range handlers {
				if hm, ok := h.(map[string]any); ok {
					if cmd, _ := hm["command"].(string); isGortexAntigravityCommand(cmd) {
						count++
					}
				}
			}
		}
		return count
	}
	for _, item := range list {
		if hm, ok := item.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); isGortexAntigravityCommand(cmd) {
				count++
			}
		}
	}
	return count
}

func antigravityHookSlice(v any) ([]any, bool) {
	if v == nil {
		return nil, true
	}
	switch list := v.(type) {
	case []any:
		return list, true
	case []map[string]any:
		out := make([]any, 0, len(list))
		for _, item := range list {
			out = append(out, item)
		}
		return out, true
	default:
		return nil, false
	}
}
