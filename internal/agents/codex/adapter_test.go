package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

const testCodexHookCommand = "/tmp/test-gortex hook --agent=codex --mode=enrich"

// TestCodexWritesMcpServersTOMLTable verifies we produce the
// documented [mcp_servers.gortex] table — not a legacy
// [mcp.gortex] or [mcpServers.gortex].
func TestCodexWritesMcpServersTOMLTable(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Detection sentinel: ~/.codex/ exists.
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Two creates: ~/.codex/config.toml for MCP plus AGENTS.md, the
	// per-repo instructions file Codex CLI reads on every task.
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 2})

	data, err := os.ReadFile(filepath.Join(env.Home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "mcp_servers") {
		t.Fatalf("expected mcp_servers table: %s", got)
	}
	if !strings.Contains(got, "gortex") {
		t.Fatalf("expected gortex entry: %s", got)
	}
	cfg := readCodexConfig(t, env)
	servers := cfg["mcp_servers"].(map[string]any)
	gortex := servers["gortex"].(map[string]any)
	envVars := gortex["env"].(map[string]any)
	if envVars["GORTEX_TOOLS"] != codexToolsEnvValue {
		t.Fatalf("GORTEX_TOOLS=%v want %q", envVars["GORTEX_TOOLS"], codexToolsEnvValue)
	}
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("expected one Gortex SessionStart hook, got %d: %#v", count, cfg["hooks"])
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("expected one Gortex PostToolUse hook, got %d: %#v", count, cfg["hooks"])
	}

	agentstest.AssertIdempotent(t, a, env)
}

func TestCodexInstallsSessionStartHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := readCodexConfig(t, env)
	entries := sessionStartEntries(t, cfg)
	if len(entries) != 1 {
		t.Fatalf("SessionStart entries=%d want 1: %#v", len(entries), entries)
	}
	entry := entries[0].(map[string]any)
	if entry["matcher"] != codexSessionStartMatcher {
		t.Fatalf("matcher=%v want %q", entry["matcher"], codexSessionStartMatcher)
	}
	handlers, ok := codexHookList(entry["hooks"])
	if !ok || len(handlers) != 1 {
		t.Fatalf("handlers=%#v", entry["hooks"])
	}
	handler := handlers[0].(map[string]any)
	if handler["type"] != "command" {
		t.Errorf("hook type=%v want command", handler["type"])
	}
	if handler["command"] != codexSessionStartCommand {
		t.Errorf("command=%v want %q", handler["command"], codexSessionStartCommand)
	}
	if handler["command_windows"] != codexSessionStartWindowsCommand {
		t.Errorf("command_windows=%v want %q", handler["command_windows"], codexSessionStartWindowsCommand)
	}
	command := handler["command"].(string)
	if !strings.Contains(command, "IMPORTANT: Prefer Gortex MCP tools") {
		t.Errorf("command should emit the graph-tools orientation: %v", handler["command"])
	}
	if !strings.Contains(command, "edit_file") {
		t.Errorf("command should mention edit_file: %v", handler["command"])
	}
	if strings.Contains(command, "[Gortex]") {
		t.Errorf("command should not duplicate the Gortex label: %v", handler["command"])
	}
}

func TestCodexInstallsPreToolUseHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := readCodexConfig(t, env)
	entries := preToolUseEntries(t, cfg)
	if len(entries) != 2 {
		t.Fatalf("PreToolUse entries=%d want Bash+MCP read entries: %#v", len(entries), entries)
	}
	assertGortexPreToolUseHooks(t, cfg)

	bashHandler := requireHookEntry(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand)
	mcpHandler := requireHookEntry(t, cfg, "PreToolUse", codexMCPReadPreToolUseMatcher, testCodexHookCommand)
	for name, handler := range map[string]map[string]any{"Bash": bashHandler, "MCP read": mcpHandler} {
		if handler["type"] != "command" {
			t.Errorf("%s hook type=%v want command", name, handler["type"])
		}
		if handler["timeout"] != int64(codexHookTimeoutSeconds) {
			t.Errorf("%s timeout=%v want %d", name, handler["timeout"], codexHookTimeoutSeconds)
		}
	}
	if bashHandler["statusMessage"] != "Loading Gortex Bash guidance..." {
		t.Errorf("Bash statusMessage=%v", bashHandler["statusMessage"])
	}
	if mcpHandler["statusMessage"] != "Loading Gortex read guidance..." {
		t.Errorf("MCP read statusMessage=%v", mcpHandler["statusMessage"])
	}
}

func TestCodexInstallsPostToolUseHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := readCodexConfig(t, env)
	entries := postToolUseEntries(t, cfg)
	if len(entries) != 1 {
		t.Fatalf("PostToolUse entries=%d want 1: %#v", len(entries), entries)
	}
	entry := entries[0].(map[string]any)
	if entry["matcher"] != codexPostToolUseMatcher {
		t.Fatalf("matcher=%v want %q", entry["matcher"], codexPostToolUseMatcher)
	}
	handlers, ok := codexHookList(entry["hooks"])
	if !ok || len(handlers) != 1 {
		t.Fatalf("handlers=%#v", entry["hooks"])
	}
	handler := handlers[0].(map[string]any)
	if handler["type"] != "command" {
		t.Errorf("hook type=%v want command", handler["type"])
	}
	command := handler["command"].(string)
	if command != testCodexHookCommand {
		t.Errorf("command=%v want test hook command with --agent=codex --mode=enrich", command)
	}
	if handler["timeout"] != int64(codexHookTimeoutSeconds) {
		t.Errorf("timeout=%v want %d", handler["timeout"], codexHookTimeoutSeconds)
	}
}

func TestCodexInstallsUserPromptSubmitHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := readCodexConfig(t, env)
	entries := userPromptSubmitEntries(t, cfg)
	if len(entries) != 1 {
		t.Fatalf("UserPromptSubmit entries=%d want 1: %#v", len(entries), entries)
	}
	entry := entries[0].(map[string]any)
	// UserPromptSubmit takes no matcher — Codex can't filter it by tool name.
	if _, hasMatcher := entry["matcher"]; hasMatcher {
		t.Fatalf("UserPromptSubmit entry should carry no matcher: %#v", entry)
	}
	handlers, ok := codexHookList(entry["hooks"])
	if !ok || len(handlers) != 1 {
		t.Fatalf("handlers=%#v", entry["hooks"])
	}
	handler := handlers[0].(map[string]any)
	if handler["type"] != "command" {
		t.Errorf("hook type=%v want command", handler["type"])
	}
	if handler["command"] != testCodexHookCommand {
		t.Errorf("command=%v want %q", handler["command"], testCodexHookCommand)
	}
	if handler["timeout"] != int64(codexHookTimeoutSeconds) {
		t.Errorf("timeout=%v want %d", handler["timeout"], codexHookTimeoutSeconds)
	}
	if count := gortexUserPromptSubmitHookCount(t, cfg); count != 1 {
		t.Fatalf("Gortex UserPromptSubmit hooks=%d want 1", count)
	}
}

func TestCodexInstallHooksOnlyCreatesOnlyHooks(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)

	action, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("install hooks only: %v", err)
	}
	if action.Action != agents.ActionCreate {
		t.Fatalf("action=%s want create", action.Action)
	}
	if len(action.Keys) != 1 || action.Keys[0] != "hooks" {
		t.Fatalf("keys=%#v want hooks only", action.Keys)
	}

	cfg := readCodexConfig(t, env)
	if _, ok := cfg["mcp_servers"]; ok {
		t.Fatalf("hooks-only should not write mcp_servers: %#v", cfg["mcp_servers"])
	}
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("hooks-only should not write AGENTS.md, stat err=%v", err)
	}
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("SessionStart hooks=%d want 1", count)
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("PostToolUse hooks=%d want 1", count)
	}

	action, err = InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second install hooks only: %v", err)
	}
	if action.Action != agents.ActionSkip {
		t.Fatalf("second action=%s want skip", action.Action)
	}
}

func TestCodexInstallHooksOnlyPreservesExistingConfig(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	seed := `model = "gpt-5-codex"

[mcp_servers.gortex]
command = "custom-gortex"
args = ["mcp", "--custom"]

[mcp_servers.other]
command = "other"

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo user-pretooluse"
statusMessage = "User PreToolUse"

[[hooks.PostToolUse]]
matcher = "^Bash$"

[[hooks.PostToolUse.hooks]]
type = "command"
command = "echo user-posttooluse"
statusMessage = "User PostToolUse"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	action, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("install hooks only: %v", err)
	}
	if action.Action != agents.ActionMerge {
		t.Fatalf("action=%s want merge", action.Action)
	}
	if len(action.Keys) != 1 || action.Keys[0] != "hooks" {
		t.Fatalf("keys=%#v want hooks only", action.Keys)
	}

	cfg := readCodexConfig(t, env)
	if cfg["model"] != "gpt-5-codex" {
		t.Fatalf("unrelated top-level key was clobbered: %#v", cfg)
	}
	servers := cfg["mcp_servers"].(map[string]any)
	gortexServer := servers["gortex"].(map[string]any)
	if gortexServer["command"] != "custom-gortex" {
		t.Fatalf("hooks-only rewrote mcp_servers.gortex: %#v", gortexServer)
	}
	if _, ok := servers["other"]; !ok {
		t.Fatalf("existing MCP server was clobbered: %#v", servers)
	}
	if !hasHookCommand(t, cfg, "PreToolUse", "echo user-pretooluse") {
		t.Fatalf("user PreToolUse hook was not preserved: %#v", preToolUseEntries(t, cfg))
	}
	if !hasHookCommand(t, cfg, "PostToolUse", "echo user-posttooluse") {
		t.Fatalf("user PostToolUse hook was not preserved: %#v", postToolUseEntries(t, cfg))
	}
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("SessionStart hooks=%d want 1", count)
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("PostToolUse hooks=%d want 1", count)
	}
}

func TestCodexInstallHooksOnlyForceReplacesOnlyGortexHooks(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	seed := `[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo user-pretooluse"
statusMessage = "User PreToolUse"

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/tmp/old-gortex hook --agent=codex --mode=enrich"
statusMessage = "Old Gortex PreToolUse"

[[hooks.PreToolUse]]
matcher = "^mcp__gortex__(read_file|get_editing_context)$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/tmp/old-gortex hook --agent=codex --mode=enrich"
statusMessage = "Old Gortex MCP Read PreToolUse"

[[hooks.PostToolUse]]
matcher = "^Bash$"

[[hooks.PostToolUse.hooks]]
type = "command"
command = "echo user-posttooluse"
statusMessage = "User PostToolUse"

[[hooks.PostToolUse]]
matcher = "^Bash$"

[[hooks.PostToolUse.hooks]]
type = "command"
command = "/tmp/old-gortex hook --agent=codex --mode=enrich"
statusMessage = "Old Gortex PostToolUse"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("install hooks only: %v", err)
	}

	cfg := readCodexConfig(t, env)
	if !hasHookCommand(t, cfg, "PreToolUse", "echo user-pretooluse") {
		t.Fatalf("Force removed user PreToolUse hook: %#v", preToolUseEntries(t, cfg))
	}
	if !hasHookCommand(t, cfg, "PostToolUse", "echo user-posttooluse") {
		t.Fatalf("Force removed user PostToolUse hook: %#v", postToolUseEntries(t, cfg))
	}
	if hasHookCommand(t, cfg, "PreToolUse", "/tmp/old-gortex hook --agent=codex --mode=enrich") {
		t.Fatalf("Force kept stale Gortex PreToolUse hook: %#v", preToolUseEntries(t, cfg))
	}
	if hasHookCommand(t, cfg, "PostToolUse", "/tmp/old-gortex hook --agent=codex --mode=enrich") {
		t.Fatalf("Force kept stale Gortex PostToolUse hook: %#v", postToolUseEntries(t, cfg))
	}
	if !hasHookCommand(t, cfg, "PreToolUse", testCodexHookCommand) {
		t.Fatalf("Force did not install current Gortex PreToolUse hook: %#v", preToolUseEntries(t, cfg))
	}
	if !hasHookCommand(t, cfg, "PostToolUse", testCodexHookCommand) {
		t.Fatalf("Force did not install current Gortex PostToolUse hook: %#v", postToolUseEntries(t, cfg))
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("Gortex PostToolUse hooks=%d want 1", count)
	}
}

func TestCodexInstallHooksOnlyDryRunDoesNotWrite(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)

	action, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("install hooks only dry-run: %v", err)
	}
	if action.Action != agents.ActionWouldCreate {
		t.Fatalf("action=%s want would-create", action.Action)
	}
	if len(action.Keys) != 1 || action.Keys[0] != "hooks" {
		t.Fatalf("keys=%#v want hooks only", action.Keys)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not write config.toml, stat err=%v", err)
	}
}

func TestCodexPreToolUseCommandFallsBackToGortexHook(t *testing.T) {
	command := codexPreToolUseCommand(agents.Env{})
	if command != "gortex hook --agent=codex --mode=enrich" {
		t.Fatalf("fallback command=%q", command)
	}
}

func TestCodexHookModeDefaultsSoftAndHonoursExplicitDeny(t *testing.T) {
	env := codexGlobalEnv(t)
	if got := codexHookCommand(env); !strings.Contains(got, "--mode=enrich") {
		t.Fatalf("default Codex hook command=%q, want soft enrich", got)
	}
	env.CodexHookMode = "deny"
	if got := codexHookCommand(env); !strings.Contains(got, "--mode=deny") {
		t.Fatalf("explicit Codex deny command=%q", got)
	}
}

func TestCodexSessionStartHookIdempotent(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionSkip: 1})

	cfg := readCodexConfig(t, env)
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("re-run duplicated Gortex SessionStart hook: got %d", count)
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("re-run duplicated Gortex PostToolUse hook: got %d", count)
	}
}

func TestCodexSessionStartHookPreservesExistingConfig(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	seed := `model = "gpt-5-codex"

[mcp_servers.other]
command = "other"

[[hooks.SessionStart]]
matcher = "startup"

[[hooks.SessionStart.hooks]]
type = "command"
command = "echo user-session-start"
statusMessage = "User hook"

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo user-pretooluse"
statusMessage = "User PreToolUse"

[[hooks.PostToolUse]]
matcher = "^Bash$"

[[hooks.PostToolUse.hooks]]
type = "command"
command = "echo user-posttooluse"
statusMessage = "User PostToolUse"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1})

	cfg := readCodexConfig(t, env)
	if cfg["model"] != "gpt-5-codex" {
		t.Fatalf("unrelated top-level key was clobbered: %#v", cfg)
	}
	servers := cfg["mcp_servers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("existing MCP server was clobbered: %#v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex MCP server missing after merge: %#v", servers)
	}
	entries := sessionStartEntries(t, cfg)
	if len(entries) != 2 {
		t.Fatalf("SessionStart entries=%d want user+gortex entries: %#v", len(entries), entries)
	}
	if !hasSessionStartCommand(t, cfg, "echo user-session-start") {
		t.Fatalf("user SessionStart hook was not preserved: %#v", entries)
	}
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("Gortex SessionStart hooks=%d want 1", count)
	}
	preEntries := preToolUseEntries(t, cfg)
	if len(preEntries) != 3 {
		t.Fatalf("PreToolUse entries=%d want user+Bash+MCP read entries: %#v", len(preEntries), preEntries)
	}
	if !hasHookCommand(t, cfg, "PreToolUse", "echo user-pretooluse") {
		t.Fatalf("user PreToolUse hook was not preserved: %#v", preEntries)
	}
	assertGortexPreToolUseHooks(t, cfg)
	postEntries := postToolUseEntries(t, cfg)
	if len(postEntries) != 2 {
		t.Fatalf("PostToolUse entries=%d want user+gortex entries: %#v", len(postEntries), postEntries)
	}
	if !hasHookCommand(t, cfg, "PostToolUse", "echo user-posttooluse") {
		t.Fatalf("user PostToolUse hook was not preserved: %#v", postEntries)
	}
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("Gortex PostToolUse hooks=%d want 1", count)
	}
}

func TestCodexForceReplacesOnlyGortexPreToolUseHook(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	seed := `[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo user-pretooluse"
statusMessage = "User PreToolUse"

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/tmp/old-gortex hook --agent=codex --mode=enrich"
statusMessage = "Old Gortex PreToolUse"

[[hooks.PreToolUse]]
matcher = "^mcp__gortex__(read_file|get_editing_context)$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/tmp/old-gortex hook --agent=codex --mode=enrich"
statusMessage = "Old Gortex MCP Read PreToolUse"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{Force: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1})

	cfg := readCodexConfig(t, env)
	preEntries := preToolUseEntries(t, cfg)
	if len(preEntries) != 3 {
		t.Fatalf("PreToolUse entries=%d want user+Bash+MCP read entries: %#v", len(preEntries), preEntries)
	}
	if !hasHookCommand(t, cfg, "PreToolUse", "echo user-pretooluse") {
		t.Fatalf("Force removed user PreToolUse hook: %#v", preEntries)
	}
	if hasHookCommand(t, cfg, "PreToolUse", "/tmp/old-gortex hook --agent=codex --mode=enrich") {
		t.Fatalf("Force kept stale Gortex PreToolUse hook: %#v", preEntries)
	}
	if !hasHookCommand(t, cfg, "PreToolUse", testCodexHookCommand) {
		t.Fatalf("Force did not install current Gortex PreToolUse hook: %#v", preEntries)
	}
	assertGortexPreToolUseHooks(t, cfg)
}

func TestCodexNoHooksSkipsSessionStartHook(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	if _, ok := cfg["hooks"]; ok {
		t.Fatalf("--no-hooks should not write Codex hooks: %#v", cfg["hooks"])
	}
	if _, ok := cfg["mcp_servers"].(map[string]any)["gortex"]; !ok {
		t.Fatal("mcp_servers.gortex should still be written under --no-hooks")
	}

	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Files) != 1 {
		t.Fatalf("plan files=%d want 1", len(plan.Files))
	}
	for _, key := range plan.Files[0].Keys {
		if key == "hooks" {
			t.Fatalf("Plan should not report hooks under --no-hooks: %#v", plan.Files[0].Keys)
		}
	}
}

func codexGlobalEnv(t *testing.T) agents.Env {
	t.Helper()
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

func codexConfigPath(env agents.Env) string {
	return filepath.Join(env.Home, ".codex", "config.toml")
}

func readCodexConfig(t *testing.T, env agents.Env) map[string]any {
	t.Helper()
	data, err := os.ReadFile(codexConfigPath(env))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse config.toml: %v\n%s", err, data)
	}
	return out
}

func sessionStartEntries(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	return hookEntries(t, cfg, "SessionStart")
}

func preToolUseEntries(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	return hookEntries(t, cfg, "PreToolUse")
}

func postToolUseEntries(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	return hookEntries(t, cfg, "PostToolUse")
}

func userPromptSubmitEntries(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	return hookEntries(t, cfg, "UserPromptSubmit")
}

func hookEntries(t *testing.T, cfg map[string]any, event string) []any {
	t.Helper()
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks map: %#v", cfg)
	}
	entries, ok := codexHookList(hooks[event])
	if !ok {
		t.Fatalf("hooks.%s has unexpected shape: %#v", event, hooks[event])
	}
	return entries
}

func gortexSessionStartHookCount(t *testing.T, cfg map[string]any) int {
	t.Helper()
	count := 0
	for _, entry := range sessionStartEntries(t, cfg) {
		if codexHookEntryIsGortexSessionStart(entry) {
			count++
		}
	}
	return count
}

func gortexPreToolUseHookCount(t *testing.T, cfg map[string]any) int {
	t.Helper()
	count := 0
	for _, entry := range preToolUseEntries(t, cfg) {
		if codexHookEntryIsGortexPreToolUse(entry) {
			count++
		}
	}
	return count
}

func assertGortexPreToolUseHooks(t *testing.T, cfg map[string]any) {
	t.Helper()
	if count := gortexPreToolUseHookCount(t, cfg); count != 2 {
		t.Fatalf("Gortex PreToolUse hooks=%d want Bash+MCP read hooks: %#v", count, preToolUseEntries(t, cfg))
	}
	if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand); count != 1 {
		t.Fatalf("Bash PreToolUse hook count=%d want 1: %#v", count, preToolUseEntries(t, cfg))
	}
	if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexMCPReadPreToolUseMatcher, testCodexHookCommand); count != 1 {
		t.Fatalf("MCP read PreToolUse hook count=%d want 1: %#v", count, preToolUseEntries(t, cfg))
	}
}

func gortexPostToolUseHookCount(t *testing.T, cfg map[string]any) int {
	t.Helper()
	count := 0
	for _, entry := range postToolUseEntries(t, cfg) {
		if codexHookEntryIsGortexPostToolUse(entry) {
			count++
		}
	}
	return count
}

func gortexUserPromptSubmitHookCount(t *testing.T, cfg map[string]any) int {
	t.Helper()
	count := 0
	for _, entry := range userPromptSubmitEntries(t, cfg) {
		if codexHookEntryIsGortexUserPromptSubmit(entry) {
			count++
		}
	}
	return count
}

func requireHookEntry(t *testing.T, cfg map[string]any, event, matcher, command string) map[string]any {
	t.Helper()
	for _, entry := range hookEntries(t, cfg, event) {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		gotMatcher, _ := group["matcher"].(string)
		if gotMatcher != matcher {
			continue
		}
		handlers, ok := codexHookList(group["hooks"])
		if !ok {
			continue
		}
		for _, handler := range handlers {
			hm, ok := handler.(map[string]any)
			if !ok {
				continue
			}
			if got, _ := hm["command"].(string); got == command {
				return hm
			}
		}
	}
	t.Fatalf("missing %s hook matcher=%q command=%q in %#v", event, matcher, command, hookEntries(t, cfg, event))
	return nil
}

func hookMatcherCommandCount(t *testing.T, cfg map[string]any, event, matcher, command string) int {
	t.Helper()
	count := 0
	for _, entry := range hookEntries(t, cfg, event) {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		gotMatcher, _ := group["matcher"].(string)
		if gotMatcher != matcher {
			continue
		}
		handlers, ok := codexHookList(group["hooks"])
		if !ok {
			continue
		}
		for _, handler := range handlers {
			hm, ok := handler.(map[string]any)
			if !ok {
				continue
			}
			if got, _ := hm["command"].(string); got == command {
				count++
			}
		}
	}
	return count
}

func hasSessionStartCommand(t *testing.T, cfg map[string]any, command string) bool {
	t.Helper()
	return hasHookCommand(t, cfg, "SessionStart", command)
}

func hasHookCommand(t *testing.T, cfg map[string]any, event string, command string) bool {
	t.Helper()
	for _, entry := range hookEntries(t, cfg, event) {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := codexHookList(group["hooks"])
		if !ok {
			continue
		}
		for _, handler := range handlers {
			hm, ok := handler.(map[string]any)
			if !ok {
				continue
			}
			if got, _ := hm["command"].(string); got == command {
				return true
			}
		}
	}
	return false
}
