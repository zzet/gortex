package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

const testCodexHookCommand = "/tmp/test-gortex hook --agent=codex --mode=enrich"
const v060CodexMCPReadPreToolUseMatcher = "^mcp__gortex__(read_file|get_editing_context)$"

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
	// per-repo instructions file Codex CLI reads on every task. Plus one
	// SKILL.md per generated community skill under <root>/.agents/skills.
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 2 + len(env.GeneratedSkills)})

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
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("expected one Gortex SessionStart hook, got %d: %#v", count, cfg["hooks"])
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("expected one Gortex PostToolUse hook, got %d: %#v", count, cfg["hooks"])
	}

	agentstest.AssertIdempotent(t, a, env)
}

func TestCodexMakesGortexToolsDirectWithoutHardDependency(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	servers := cfg["mcp_servers"].(map[string]any)
	server := servers["gortex"].(map[string]any)
	// A required server Codex cannot start aborts the whole session, so a
	// Gortex outage would take the user's CLI down with it (#607).
	if _, exists := server["required"]; exists {
		t.Fatalf("Gortex must not declare itself a required Codex dependency: %#v", server)
	}
	if want := agents.ResolveGortexLaunchBinary(); server["command"] != want {
		t.Fatalf("mcp_servers.gortex.command=%v want %q", server["command"], want)
	}
	if server["startup_timeout_sec"] != int64(codexMCPStartupTimeoutSeconds) {
		t.Fatalf("mcp_servers.gortex.startup_timeout_sec=%v want %d", server["startup_timeout_sec"], codexMCPStartupTimeoutSeconds)
	}

	features := cfg["features"].(map[string]any)
	codeMode := features["code_mode"].(map[string]any)
	namespaces, ok := codexStringList(codeMode["direct_only_tool_namespaces"])
	if !ok || len(namespaces) != 2 || namespaces[0] != codexGortexToolNamespace || namespaces[1] != codexGortexNonPrefixedToolNamespace {
		t.Fatalf("direct_only_tool_namespaces=%#v want [%q %q]", codeMode["direct_only_tool_namespaces"], codexGortexToolNamespace, codexGortexNonPrefixedToolNamespace)
	}
	if _, exists := codeMode["enabled"]; exists {
		t.Fatalf("Gortex must not enable Codex's experimental code mode: %#v", codeMode)
	}
}

func TestCodexAvailabilityMergePreservesCustomConfig(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	path := codexConfigPath(env)
	seed := `model = "gpt-5.4"

[features.code_mode]
enabled = false
excluded_tool_namespaces = ["mcp__private"]
direct_only_tool_namespaces = ["mcp__history"]

[mcp_servers.gortex]
command = "gortex"
args = ["mcp", "--custom"]
required = false
startup_timeout_sec = 15
tool_timeout_sec = 321

[mcp_servers.gortex.env]
CUSTOM_GORTEX = "yes"

[mcp_servers.other]
command = "other"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	if cfg["model"] != "gpt-5.4" {
		t.Fatalf("unrelated top-level config changed: %#v", cfg)
	}
	servers := cfg["mcp_servers"].(map[string]any)
	server := servers["gortex"].(map[string]any)
	// The seed's bare "gortex" is what earlier releases wrote, not a user
	// choice, so it is upgraded to the absolute path Codex can always find.
	if want := agents.ResolveGortexLaunchBinary(); server["command"] != want {
		t.Fatalf("command=%v want the pinned launch binary %q", server["command"], want)
	}
	args, ok := codexStringList(server["args"])
	if !ok || len(args) != 2 || args[1] != "--custom" {
		t.Fatalf("custom args changed: %#v", server["args"])
	}
	if server["tool_timeout_sec"] != int64(321) {
		t.Fatalf("custom tool timeout changed: %#v", server)
	}
	// An explicit opt-out is already the safe posture; only `true` is pruned.
	if server["required"] != false {
		t.Fatalf("user's required=false was not preserved: %#v", server)
	}
	if server["startup_timeout_sec"] != int64(codexMCPStartupTimeoutSeconds) {
		t.Fatalf("startup timeout=%v want %d", server["startup_timeout_sec"], codexMCPStartupTimeoutSeconds)
	}
	envMap := server["env"].(map[string]any)
	if envMap["CUSTOM_GORTEX"] != "yes" {
		t.Fatalf("custom environment changed: %#v", envMap)
	}
	if _, exists := servers["other"]; !exists {
		t.Fatalf("unrelated MCP server removed: %#v", servers)
	}

	features := cfg["features"].(map[string]any)
	codeMode := features["code_mode"].(map[string]any)
	if codeMode["enabled"] != false {
		t.Fatalf("code_mode.enabled changed: %#v", codeMode)
	}
	excluded, ok := codexStringList(codeMode["excluded_tool_namespaces"])
	if !ok || len(excluded) != 1 || excluded[0] != "mcp__private" {
		t.Fatalf("excluded namespaces changed: %#v", codeMode)
	}
	direct, ok := codexStringList(codeMode["direct_only_tool_namespaces"])
	if !ok || len(direct) != 3 || direct[0] != "mcp__history" || direct[1] != codexGortexToolNamespace || direct[2] != codexGortexNonPrefixedToolNamespace {
		t.Fatalf("direct namespaces=%#v", direct)
	}

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionSkip: 1 + curatedSkillCount(t) + subAgentCount()})
}

func TestCodexDirectNamespaceUpgradesBooleanFeatureForm(t *testing.T) {
	root := map[string]any{
		"features": map[string]any{"code_mode": true},
	}
	if !upsertCodexDirectToolNamespaces(root) {
		t.Fatal("expected boolean code_mode form to be upgraded")
	}
	features := root["features"].(map[string]any)
	codeMode := features["code_mode"].(map[string]any)
	if codeMode["enabled"] != true {
		t.Fatalf("boolean feature value not preserved: %#v", codeMode)
	}
	namespaces, ok := codexStringList(codeMode["direct_only_tool_namespaces"])
	if !ok || len(namespaces) != 2 || namespaces[0] != codexGortexToolNamespace || namespaces[1] != codexGortexNonPrefixedToolNamespace {
		t.Fatalf("direct namespaces=%#v", namespaces)
	}
}

func TestCodexPreservesUserOwnedGortexServer(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	path := codexConfigPath(env)
	seed := `[mcp_servers.gortex]
command = "company-gortex-wrapper"
args = ["serve", "--company-policy"]
custom = "keep"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	server := cfg["mcp_servers"].(map[string]any)["gortex"].(map[string]any)
	if server["command"] != "company-gortex-wrapper" || server["custom"] != "keep" {
		t.Fatalf("user-owned server changed: %#v", server)
	}
	if _, exists := server["required"]; exists {
		t.Fatalf("user-owned server gained managed required policy: %#v", server)
	}
	if _, exists := server["startup_timeout_sec"]; exists {
		t.Fatalf("user-owned server gained managed startup timeout: %#v", server)
	}
	features := cfg["features"].(map[string]any)
	codeMode := features["code_mode"].(map[string]any)
	namespaces, ok := codexStringList(codeMode["direct_only_tool_namespaces"])
	if !ok || len(namespaces) != 2 {
		t.Fatalf("Gortex server namespace should still be direct: %#v", codeMode)
	}
}

func TestCodexMCPServerPreservesLongerStartupTimeout(t *testing.T) {
	root := map[string]any{
		"mcp_servers": map[string]any{
			"gortex": map[string]any{
				"command":             agents.ResolveGortexLaunchBinary(),
				"args":                []any{"mcp"},
				"startup_timeout_sec": int64(180),
			},
		},
	}
	if upsertCodexMCPServer(root, agents.ApplyOpts{}) {
		t.Fatal("a longer user-selected startup timeout should already satisfy the invariant")
	}
	server := root["mcp_servers"].(map[string]any)["gortex"].(map[string]any)
	if server["startup_timeout_sec"] != int64(180) {
		t.Fatalf("longer startup timeout changed: %#v", server)
	}
}

// TestCodexUpgradeRemovesRequiredFlag covers the migration for everyone who
// already ran an affected release: their config carries `required = true` on
// disk, so leaving it there would keep their Codex unable to start whenever
// the daemon is down (#607).
func TestCodexUpgradeRemovesRequiredFlag(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	path := codexConfigPath(env)
	seed := `[mcp_servers.gortex]
args = ['mcp']
command = 'gortex'
required = true
startup_timeout_sec = 90

[mcp_servers.gortex.env]
GORTEX_INDEX_WORKERS = '8'
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	server := readCodexConfig(t, env)["mcp_servers"].(map[string]any)["gortex"].(map[string]any)
	if _, exists := server["required"]; exists {
		t.Fatalf("upgrade left the session-blocking required flag behind: %#v", server)
	}
	if server["startup_timeout_sec"] != int64(codexMCPStartupTimeoutSeconds) {
		t.Fatalf("startup timeout lost during the migration: %#v", server)
	}
	if _, exists := server["env"]; exists {
		t.Fatalf("upgrade left the managed GORTEX_INDEX_WORKERS env behind: %#v", server)
	}
}

func TestCodexPruneManagedRequired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   map[string]any
		changed bool
		want    any
		present bool
	}{
		{name: "managed true is removed", entry: map[string]any{"required": true}, changed: true},
		{name: "explicit opt-out is preserved", entry: map[string]any{"required": false}, want: false, present: true},
		{name: "absent stays absent", entry: map[string]any{}},
		{
			// A non-boolean is a shape we did not write and cannot judge.
			name: "non-boolean is left alone", entry: map[string]any{"required": "yes"},
			want: "yes", present: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pruneManagedCodexRequired(tc.entry); got != tc.changed {
				t.Fatalf("changed=%v want %v", got, tc.changed)
			}
			got, present := tc.entry["required"]
			if present != tc.present || (present && got != tc.want) {
				t.Fatalf("required=(%#v, present=%v) want (%#v, present=%v)", got, present, tc.want, tc.present)
			}
		})
	}
}

func TestCodexPinsBareGortexCommandOnly(t *testing.T) {
	pinned := agents.ResolveGortexLaunchBinary()
	if pinned == "gortex" {
		t.Skip("no installed gortex binary to pin to on this machine")
	}
	entry := map[string]any{"command": "gortex"}
	if !pinCodexBareGortexCommand(entry) {
		t.Fatal("bare command should be pinned to the installed binary")
	}
	if entry["command"] != pinned {
		t.Fatalf("command=%v want %q", entry["command"], pinned)
	}
	// An absolute path is a deliberate choice — a user's, or an earlier run's.
	absolute := map[string]any{"command": "/opt/custom/build/gortex"}
	if pinCodexBareGortexCommand(absolute) {
		t.Fatalf("an absolute command must survive: %#v", absolute)
	}
}

func TestCodexDirectNamespaceVersionGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		supported bool
		version   string
	}{
		{name: "current", output: "codex-cli 0.144.1\n", supported: true, version: "0.144.1"},
		{name: "first supported minor", output: "codex-cli 0.142.0\n", supported: true, version: "0.142.0"},
		{name: "unsupported", output: "codex-cli 0.141.0\n", supported: false, version: "0.141.0"},
		{name: "future major", output: "codex-cli v1.0.0\n", supported: true, version: "1.0.0"},
		{name: "unparseable app or IDE version", output: "codex-cli dev\n", supported: true, version: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCodexVersion(t, tc.output)
			supported, detected := codexSupportsDirectToolNamespaces()
			if supported != tc.supported || detected != tc.version {
				t.Fatalf("support=(%v, %q) want (%v, %q)", supported, detected, tc.supported, tc.version)
			}
		})
	}
}

func TestCodexOldVersionSkipsUnsupportedDirectNamespaceConfig(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	stubCodexVersion(t, "codex-cli 0.141.0\n")

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	if _, exists := cfg["features"]; exists {
		t.Fatalf("Codex 0.141 must not receive unsupported features.code_mode fields: %#v", cfg["features"])
	}
	server := cfg["mcp_servers"].(map[string]any)["gortex"].(map[string]any)
	if server["startup_timeout_sec"] != int64(codexMCPStartupTimeoutSeconds) {
		t.Fatalf("version gate must not weaken the MCP startup timeout: %#v", server)
	}
	if _, exists := server["required"]; exists {
		t.Fatalf("old Codex must not receive the session-blocking required flag either: %#v", server)
	}
}

func TestCodexOldVersionMergesExistingDirectNamespaceField(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallHooks = false
	stubCodexVersion(t, "codex-cli 0.141.0\n")
	path := codexConfigPath(env)
	seed := `[features.code_mode]
direct_only_tool_namespaces = ["mcp__history"]
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	codeMode := cfg["features"].(map[string]any)["code_mode"].(map[string]any)
	direct, ok := codexStringList(codeMode["direct_only_tool_namespaces"])
	if !ok || len(direct) != 3 || direct[0] != "mcp__history" || direct[1] != codexGortexToolNamespace || direct[2] != codexGortexNonPrefixedToolNamespace {
		t.Fatalf("existing supported field was not merged: %#v", codeMode)
	}
}

func TestCodexInstallsSessionStartHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1 + curatedSkillCount(t) + subAgentCount()})

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
	if handler["command"] != testCodexHookCommand {
		t.Errorf("command=%v want %q", handler["command"], testCodexHookCommand)
	}
	if _, exists := handler["command_windows"]; exists {
		t.Errorf("SessionStart should use the same managed hook command on every platform: %#v", handler)
	}
	command := handler["command"].(string)
	if !codexCommandInvokesCodexHook(command) {
		t.Errorf("SessionStart must flow through the managed Codex hook for effectiveness telemetry: %v", handler["command"])
	}
}

func TestCodexInstallsPreToolUseHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1 + curatedSkillCount(t) + subAgentCount()})

	cfg := readCodexConfig(t, env)
	entries := preToolUseEntries(t, cfg)
	if len(entries) != 1 {
		t.Fatalf("PreToolUse entries=%d want one match-all entry: %#v", len(entries), entries)
	}
	assertGortexPreToolUseHooks(t, cfg)

	handler := requireHookEntry(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand)
	if handler["type"] != "command" {
		t.Errorf("hook type=%v want command", handler["type"])
	}
	if handler["timeout"] != int64(codexHookTimeoutSeconds) {
		t.Errorf("timeout=%v want %d", handler["timeout"], codexHookTimeoutSeconds)
	}
	if handler["statusMessage"] != "Loading Gortex tool guidance..." {
		t.Errorf("statusMessage=%v", handler["statusMessage"])
	}
	matcher := regexp.MustCompile(codexPreToolUseMatcher)
	for _, tool := range []string{"Bash", "apply_patch", "Read", "WebSearch", "mcp__gortex__search", "mcp__gortex__change"} {
		if !matcher.MatchString(tool) {
			t.Errorf("match-all PreToolUse matcher missed %q", tool)
		}
	}
}

func TestCodexInstallsPostToolUseHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1 + curatedSkillCount(t) + subAgentCount()})

	cfg := readCodexConfig(t, env)
	entries := postToolUseEntries(t, cfg)
	if len(entries) != 1 {
		t.Fatalf("PostToolUse entries=%d want 1: %#v", len(entries), entries)
	}
	entry := entries[0].(map[string]any)
	if entry["matcher"] != codexPostToolUseMatcher {
		t.Fatalf("matcher=%v want %q", entry["matcher"], codexPostToolUseMatcher)
	}
	if !strings.Contains(codexPostToolUseMatcher, "apply_patch") {
		t.Fatalf("PostToolUse matcher must cover mutation-aware apply_patch handling: %q", codexPostToolUseMatcher)
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

func TestCodexHookModeIsOptInAndMigratesInPlace(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	if got := codexHookMode(); got != "enrich" {
		t.Fatalf("default Codex posture=%q want enrich", got)
	}
	t.Setenv(codexHookModeEnvVar, "deny")
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	cfg := readCodexConfig(t, env)
	if !hasHookCommand(t, cfg, "PreToolUse", "/tmp/test-gortex hook --agent=codex --mode=deny") {
		t.Fatalf("deny posture not installed: %#v", preToolUseEntries(t, cfg))
	}
	if !hasSessionStartCommand(t, cfg, "/tmp/test-gortex hook --agent=codex --mode=deny") {
		t.Fatalf("SessionStart did not migrate to deny command: %#v", sessionStartEntries(t, cfg))
	}

	t.Setenv(codexHookModeEnvVar, "rewrite")
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	cfg = readCodexConfig(t, env)
	if !hasHookCommand(t, cfg, "PreToolUse", "/tmp/test-gortex hook --agent=codex --mode=rewrite") {
		t.Fatalf("rewrite posture not installed: %#v", preToolUseEntries(t, cfg))
	}
	if hasHookCommand(t, cfg, "PreToolUse", "/tmp/test-gortex hook --agent=codex --mode=deny") {
		t.Fatalf("stale deny hook survived posture migration: %#v", preToolUseEntries(t, cfg))
	}
	if count := gortexPreToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("posture migration duplicated match-all PreToolUse hook: %d", count)
	}
}

func TestCodexInstallsUserPromptSubmitHook(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1 + curatedSkillCount(t) + subAgentCount()})

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
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionSkip: 1 + curatedSkillCount(t) + subAgentCount()})

	cfg := readCodexConfig(t, env)
	if count := gortexSessionStartHookCount(t, cfg); count != 1 {
		t.Fatalf("re-run duplicated Gortex SessionStart hook: got %d", count)
	}
	assertGortexPreToolUseHooks(t, cfg)
	if count := gortexPostToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("re-run duplicated Gortex PostToolUse hook: got %d", count)
	}
}

func TestCodexUpgradesV060CompactSurfaceHooks(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	seed := `[[hooks.SessionStart]]
matcher = "startup|resume|clear|compact"

[[hooks.SessionStart.hooks]]
type = "command"
command = "` + strings.ReplaceAll(v060CodexSessionStartCommand, `\`, `\\`) + `"

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + testCodexHookCommand + `"

[[hooks.PreToolUse]]
matcher = "^mcp__gortex__(read_file|get_editing_context)$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + testCodexHookCommand + `"

[[hooks.PreToolUse]]
matcher = "` + codexLegacyMCPNavigationPreToolUseMatcher + `"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + testCodexHookCommand + `"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("upgrade hooks: %v", err)
	}
	cfg := readCodexConfig(t, env)
	if len(sessionStartEntries(t, cfg)) != 1 {
		t.Fatalf("v0.60.0 SessionStart should update in place: %#v", sessionStartEntries(t, cfg))
	}
	if !hasSessionStartCommand(t, cfg, testCodexHookCommand) {
		t.Fatalf("managed SessionStart hook missing after static-command migration: %#v", sessionStartEntries(t, cfg))
	}
	if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand); count != 1 {
		t.Fatalf("match-all matcher count=%d want 1: %#v", count, preToolUseEntries(t, cfg))
	}
	for _, stale := range []string{codexLegacyBashPreToolUseMatcher, codexLegacyMCPNavigationPreToolUseMatcher, v060CodexMCPReadPreToolUseMatcher} {
		if count := hookMatcherCommandCount(t, cfg, "PreToolUse", stale, testCodexHookCommand); count != 0 {
			t.Fatalf("stale matcher %q survived upgrade: %#v", stale, preToolUseEntries(t, cfg))
		}
	}
	if count := gortexPreToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("split hooks were not collapsed: got %d entries %#v", count, preToolUseEntries(t, cfg))
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
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1, agents.ActionCreate: curatedSkillCount(t) + subAgentCount()})

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
	if len(preEntries) != 2 {
		t.Fatalf("PreToolUse entries=%d want user+match-all entries: %#v", len(preEntries), preEntries)
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
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1, agents.ActionCreate: curatedSkillCount(t) + subAgentCount()})

	cfg := readCodexConfig(t, env)
	preEntries := preToolUseEntries(t, cfg)
	if len(preEntries) != 2 {
		t.Fatalf("PreToolUse entries=%d want user+match-all entries: %#v", len(preEntries), preEntries)
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
	// config.toml, one planned SKILL.md per curated skill, and one
	// planned .toml per sub-agent.
	if want := 1 + curatedSkillCount(t) + subAgentCount(); len(plan.Files) != want {
		t.Fatalf("plan files=%d want %d", len(plan.Files), want)
	}
	for _, key := range plan.Files[0].Keys {
		if key == "hooks" {
			t.Fatalf("Plan should not report hooks under --no-hooks: %#v", plan.Files[0].Keys)
		}
	}
}

func codexGlobalEnv(t *testing.T) agents.Env {
	t.Helper()
	t.Setenv(codexHookModeEnvVar, "")
	stubCodexVersion(t, "codex-cli 0.144.1\n")
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

func stubCodexVersion(t *testing.T, output string) {
	t.Helper()
	previous := codexVersionOutput
	codexVersionOutput = func() ([]byte, error) { return []byte(output), nil }
	t.Cleanup(func() { codexVersionOutput = previous })
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
	if count := gortexPreToolUseHookCount(t, cfg); count != 1 {
		t.Fatalf("Gortex PreToolUse hooks=%d want one match-all hook: %#v", count, preToolUseEntries(t, cfg))
	}
	if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand); count != 1 {
		t.Fatalf("match-all PreToolUse hook count=%d want 1: %#v", count, preToolUseEntries(t, cfg))
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

// TestCodexInstallWritesGlobalInstructions covers the surface that makes a
// Codex session reach for Gortex at all. Codex merges ~/.codex/AGENTS.md into
// every session; without a block there, the only Gortex guidance a session can
// get is the SessionStart hook — which Codex skips until the user trusts it.
func TestCodexInstallWritesGlobalInstructions(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	path := GlobalInstructionsPath(env.Home)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, agents.InstructionsSentinel) {
		t.Fatalf("expected the mandatory-rule sentinel in %s:\n%s", path, got)
	}
	if !strings.Contains(got, agents.GlobalRulesStartMarker) || !strings.Contains(got, agents.GlobalRulesEndMarker) {
		t.Fatalf("expected a marker-fenced block in %s:\n%s", path, got)
	}
	// Codex reads AGENTS.md as literal markdown — an @-include line is prose
	// to it, so the profile body must be inlined, not pointed at.
	if strings.Contains(got, "@"+filepath.Join(env.InstructionsDir, "active.md")) {
		t.Fatalf("expected an inlined body, got an @-include pointer:\n%s", got)
	}
	if !strings.Contains(got, "explore") {
		t.Fatalf("expected the active profile body to be inlined:\n%s", got)
	}

	var reported bool
	for _, f := range res.Files {
		if f.Path == path {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("apply did not report %s in its file actions: %#v", path, res.Files)
	}
}

// TestCodexGlobalInstructionsIdempotent asserts a re-run leaves exactly one
// block. `gortex install` is re-run after every upgrade, so an appending
// writer would grow the file Codex loads on every session without bound.
func TestCodexGlobalInstructionsIdempotent(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	a := New()

	for i := 0; i < 3; i++ {
		if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(GlobalInstructionsPath(env.Home))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), agents.GlobalRulesStartMarker); n != 1 {
		t.Fatalf("expected exactly one rule block after 3 applies, got %d", n)
	}
}

// TestCodexGlobalInstructionsPreservesUserContent asserts the block merges
// into a personal AGENTS.md instead of replacing it.
func TestCodexGlobalInstructionsPreservesUserContent(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	path := GlobalInstructionsPath(env.Home)
	if err := os.WriteFile(path, []byte("# My rules\n\nAlways run gofmt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "Always run gofmt.") {
		t.Fatalf("user content was clobbered:\n%s", got)
	}
	if !strings.Contains(got, agents.InstructionsSentinel) {
		t.Fatalf("rule block missing after merge:\n%s", got)
	}
}

// TestCodexGlobalInstructionsOptOutAndScope pins the two cases that must not
// write the file: --no-claude-md (InstallGlobalInstructions=false) and project
// mode, where `gortex init` has no business writing user-level rules.
func TestCodexGlobalInstructionsOptOutAndScope(t *testing.T) {
	t.Run("opt-out", func(t *testing.T) {
		env := codexGlobalEnv(t)
		env.InstallGlobalInstructions = false
		if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if _, err := os.Stat(GlobalInstructionsPath(env.Home)); !os.IsNotExist(err) {
			t.Fatalf("--no-claude-md should not write %s (stat err=%v)", GlobalInstructionsPath(env.Home), err)
		}
	})
	t.Run("project-mode", func(t *testing.T) {
		env := codexGlobalEnv(t)
		env.Mode = agents.ModeProject
		env.InstallGlobalInstructions = true
		if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if _, err := os.Stat(GlobalInstructionsPath(env.Home)); !os.IsNotExist(err) {
			t.Fatalf("project mode should not write user-level rules (stat err=%v)", err)
		}
	})
}

// TestCodexGlobalInstructionsDryRun asserts --dry-run plans the write without
// touching disk, and that Plan agrees with what Apply would do.
func TestCodexGlobalInstructionsDryRun(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	a := New()

	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var planned bool
	for _, f := range plan.Files {
		if f.Path == GlobalInstructionsPath(env.Home) {
			planned = true
		}
	}
	if !planned {
		t.Fatalf("plan omitted %s: %#v", GlobalInstructionsPath(env.Home), plan.Files)
	}

	if _, err := a.Apply(env, agents.ApplyOpts{DryRun: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(GlobalInstructionsPath(env.Home)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote %s (stat err=%v)", GlobalInstructionsPath(env.Home), err)
	}
}

// TestCodexHookInstallWarnsAboutTrust pins the notice that makes an otherwise
// silent failure visible: Codex hashes each non-managed hook and skips new or
// changed ones until they are trusted in `/hooks`, so writing the hook set is
// only half the job. The notice must not repeat once the hooks are unchanged.
func TestCodexHookInstallWarnsAboutTrust(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a hook-trust notice on first install")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "/hooks") {
			found = true
		}
	}
	if !found {
		t.Fatalf("hook-trust notice should name /hooks: %#v", res.Warnings)
	}

	// Second run changes nothing, so the trust hashes still match and there
	// is nothing for the user to re-approve.
	res2, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(res2.Warnings) != 0 {
		t.Fatalf("unchanged hooks should not re-warn: %#v", res2.Warnings)
	}
}

func TestCodexPruneManagedIndexWorkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   map[string]any
		changed bool
		want    map[string]any
	}{
		{
			name:    "managed value alone drops the env table",
			entry:   map[string]any{"env": map[string]any{"GORTEX_INDEX_WORKERS": "8"}},
			changed: true,
			want:    map[string]any{},
		},
		{
			name:    "managed value beside a user key keeps the user key",
			entry:   map[string]any{"env": map[string]any{"GORTEX_INDEX_WORKERS": "8", "GORTEX_LOG": "debug"}},
			changed: true,
			want:    map[string]any{"env": map[string]any{"GORTEX_LOG": "debug"}},
		},
		{
			name:  "a user-chosen value is preserved",
			entry: map[string]any{"env": map[string]any{"GORTEX_INDEX_WORKERS": "4"}},
			want:  map[string]any{"env": map[string]any{"GORTEX_INDEX_WORKERS": "4"}},
		},
		{
			name:  "no env stays untouched",
			entry: map[string]any{},
			want:  map[string]any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pruneManagedCodexIndexWorkers(tc.entry); got != tc.changed {
				t.Fatalf("changed = %v, want %v", got, tc.changed)
			}
			if !reflect.DeepEqual(tc.entry, tc.want) {
				t.Fatalf("entry = %#v, want %#v", tc.entry, tc.want)
			}
		})
	}
}
