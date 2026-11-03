package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// InstallState is the observed state of the Antigravity integration,
// consumed by `gortex doctor`.
type InstallState struct {
	ConfigPath             string         `json:"config_path"`
	ConfigPresent          bool           `json:"config_present"`
	MCPServer              bool           `json:"mcp_server"`
	Hooks                  map[string]int `json:"hooks"`
	HooksDisabled          bool           `json:"hooks_disabled,omitempty"`
	PostToolUseDenyPosture bool           `json:"post_tool_use_deny_posture"`
	InstructionsPath       string         `json:"instructions_path,omitempty"`
	InstructionsWired      bool           `json:"instructions_wired"`
}

// HooksDisabledRemedy is how a user turns Gortex's Antigravity hooks back on.
const HooksDisabledRemedy = `remove "enabled": false from the "gortex" entry in ~/.gemini/config/hooks.json`

// Inspect reads the Antigravity configuration under home. Every failure
// degrades to "not configured" rather than an error so `gortex doctor`
// can report broken or partial setups cleanly.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	if home == "" {
		return state
	}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}

	state.ConfigPath = HookConfigPath(home)
	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		var root map[string]any
		if json.Unmarshal(agents.StripJSONComments(data), &root) == nil {
			countGortexAntigravityHooks(root, state.Hooks)
			state.HooksDisabled = antigravityHooksDisabled(root)
			if state.Hooks["PostToolUse"] > 0 {
				// Antigravity's PostToolUse response schema carries no
				// context-injection channel; the hook runs solely to
				// observe localization terminal receipts.
				state.PostToolUseDenyPosture = true
			}
		}
	}

	for _, mcpPath := range mcpConfigPaths(home) {
		data, err := os.ReadFile(mcpPath)
		if err != nil {
			continue
		}
		state.ConfigPresent = true
		var root struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(agents.StripJSONComments(data), &root) == nil {
			if _, ok := root.MCPServers["gortex"]; ok {
				state.MCPServer = true
			}
		}
	}

	state.InstructionsPath = GlobalInstructionsPath(home)
	for _, p := range []string{
		state.InstructionsPath,
		filepath.Join(home, ".gemini", "antigravity", "knowledge", "gortex-workflow", "artifacts", "gortex-instructions.md"),
	} {
		if body, err := os.ReadFile(p); err == nil {
			text := string(body)
			if strings.Contains(text, agents.GlobalRulesStartMarker) ||
				strings.Contains(text, agents.InstructionsSentinel) {
				state.InstructionsWired = true
				break
			}
		}
	}
	return state
}

func countGortexAntigravityHooks(root map[string]any, out map[string]int) {
	for _, specAny := range root {
		spec, ok := specAny.(map[string]any)
		if !ok {
			continue
		}
		for _, canonicalEvent := range HookEvents {
			onDiskEvent := antigravityHookEventNames[canonicalEvent]
			out[canonicalEvent] += countGortexEventHandlers(onDiskEvent, spec[onDiskEvent])
		}
	}
}

func antigravityHooksDisabled(root map[string]any) bool {
	spec, ok := root[hookPluginName].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := spec["enabled"].(bool)
	return ok && !enabled
}
