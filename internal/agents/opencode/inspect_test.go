package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestInspectEmptyHome: doctor describes a broken machine, it does not
// fail on one, so an absent install reports zeroes rather than an error.
func TestInspectEmptyHome(t *testing.T) {
	state := Inspect("")
	if state.ConfigPresent || state.MCPServer || state.PluginPresent {
		t.Fatalf("empty home reported an install: %+v", state)
	}
	for _, event := range HookEvents {
		if _, ok := state.Hooks[event]; !ok {
			t.Fatalf("Hooks is missing the declared event %q; doctor would print nothing for it", event)
		}
	}

	state = Inspect(t.TempDir())
	if state.ConfigPresent || state.PluginPresent || state.Skills != 0 || state.Commands != 0 {
		t.Fatalf("empty home dir reported an install: %+v", state)
	}
}

// TestInspectAfterGlobalInstall is the round trip: what Apply wrote is
// what Inspect finds.
func TestInspectAfterGlobalInstall(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	state := Inspect(env.Home)
	if !state.PluginPresent {
		t.Fatalf("installed bridge not detected at %s", state.PluginPath)
	}
	if state.PluginPath != PluginPath(env.Home) {
		t.Fatalf("PluginPath disagrees with the writer: %s", state.PluginPath)
	}
	if state.Skills != len(agents.GlobalSkills) {
		t.Fatalf("found %d skills, want %d", state.Skills, len(agents.GlobalSkills))
	}
	if state.Commands != len(agents.SlashCommands) {
		t.Fatalf("found %d commands, want %d", state.Commands, len(agents.SlashCommands))
	}
	// The bridge is the whole hook surface: present means every declared
	// event is wired.
	for _, event := range HookEvents {
		if state.Hooks[event] != 1 {
			t.Fatalf("event %q not reported as configured: %v", event, state.Hooks)
		}
	}
}

// TestInspectIgnoresForeignPlugin: a plugin somebody else called
// gortex.js is not ours, and counting it would tell doctor enforcement is
// wired when nothing is.
func TestInspectIgnoresForeignPlugin(t *testing.T) {
	home := t.TempDir()
	path := PluginPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const NotGortex = async () => ({});\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := Inspect(home)
	if state.PluginPresent {
		t.Fatalf("a foreign gortex.js was counted as the Gortex bridge")
	}
	for _, event := range HookEvents {
		if state.Hooks[event] != 0 {
			t.Fatalf("event %q reported configured off a foreign plugin", event)
		}
	}
}

// TestInspectReadsGlobalMCPStanza covers the user-level opencode.json.
// The adapter does not write it (the MCP server goes in the repo's own
// config), but a user may have added one by hand and doctor should say
// so.
func TestInspectReadsGlobalMCPStanza(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(globalConfigDir(home), "opencode.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcp":{"gortex":{"type":"local"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	state := Inspect(home)
	if !state.ConfigPresent || !state.MCPServer {
		t.Fatalf("global MCP stanza not detected: %+v", state)
	}
}

// TestHookEventsAreSubstantiatedByThePlugin is the fence the doctor
// wiring depends on. Doctor divides recorded runs by HookEvents and
// reports any declared event with zero runs as a blocker, so declaring an
// event nothing can emit is a permanent false alarm.
//
// The mapping is: a bridge phase the plugin sends -> the Claude event
// name internal/hooks/pi.go logs for it.
func TestHookEventsAreSubstantiatedByThePlugin(t *testing.T) {
	emitters := map[string][]string{
		"SessionStart":     {`event: "session"`},
		"UserPromptSubmit": {`event: "chat.message"`},
		"PreToolUse":       {`event: "tool.execute.before"`, `event: "permission.ask"`},
	}
	if len(emitters) != len(HookEvents) {
		t.Fatalf("HookEvents has %d entries but this test knows about %d", len(HookEvents), len(emitters))
	}
	for _, event := range HookEvents {
		phases, ok := emitters[event]
		if !ok {
			t.Fatalf("HookEvents declares %q with no plugin phase behind it", event)
		}
		for _, phase := range phases {
			if !strings.Contains(pluginSource, phase) {
				t.Fatalf("HookEvents declares %q but the plugin no longer sends %s", event, phase)
			}
		}
	}
	// PostToolUse must stay out: tool.execute.after never shells the
	// bridge, so it can never log a run.
	for _, event := range HookEvents {
		if event == "PostToolUse" {
			t.Fatalf("PostToolUse cannot be substantiated — the after-hook does not call the bridge")
		}
	}
}
