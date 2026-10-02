package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestInstallPermissionsMigratesWildcardButPreservesCustomPolicy(t *testing.T) {
	t.Run("legacy wildcard", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["mcp__gortex__*"]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := installPermissions(io.Discard, path, agents.ApplyOpts{}); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, "mcp__gortex__*") || !strings.Contains(text, "mcp__gortex__read") || strings.Contains(text, "mcp__gortex__edit") {
			t.Fatalf("unsafe wildcard migration: %s", text)
		}
	})

	t.Run("custom narrow list", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["mcp__gortex__read"]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := installPermissions(io.Discard, path, agents.ApplyOpts{}); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(body), "mcp__gortex__") != 1 || strings.Contains(string(body), "mcp__gortex__search") {
			t.Fatalf("custom permission list was widened: %s", body)
		}
	})
}

// TestClaudeCodeProjectModeCreatesCanonicalArtifacts is the
// acceptance test for the most important adapter. It asserts that
// a fresh project gets:
//   - .mcp.json with our server stanza
//   - .claude/settings.json with MCP permissions
//   - .claude/settings.local.json with the lifecycle and tool hook events
//   - AGENTS.md with the marker-guarded communities block
//   - CLAUDE.md importing the canonical AGENTS.md instructions
//   - .claude/skills/generated/<DirName>/SKILL.md (one per
//     GeneratedSkill)
//
// Slash commands and the curated GlobalSkills are NOT written in
// project mode anymore — they're user-level artifacts installed by
// `gortex install`. TestClaudeCodeGlobalModeWritesUserFiles covers
// those.
//
// Re-running must be a no-op (idempotent contract).
func TestClaudeCodeProjectModeCreatesCanonicalArtifacts(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	expected := []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, ".claude", "settings.json"),
		filepath.Join(env.Root, ".claude", "settings.local.json"),
		filepath.Join(env.Root, "AGENTS.md"),
		filepath.Join(env.Root, "CLAUDE.md"),
	}
	for _, s := range env.GeneratedSkills {
		expected = append(expected, filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md"))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing artifact %s: %v", p, err)
		}
	}

	// Project-mode must NOT touch the user-level slash commands or
	// curated skills — those live in install mode now.
	for name := range SlashCommands {
		if _, err := os.Stat(filepath.Join(env.Home, ".claude", "commands", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote user-level slash command %s", name)
		}
		if _, err := os.Stat(filepath.Join(env.Root, ".claude", "commands", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote project-level slash command %s", name)
		}
	}
	for name := range GlobalSkills {
		if _, err := os.Stat(filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md")); err == nil {
			t.Errorf("project mode unexpectedly wrote user-level skill %s", name)
		}
	}
	for name := range SubAgents {
		if _, err := os.Stat(filepath.Join(env.Home, ".claude", "agents", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote user-level sub-agent %s", name)
		}
		if _, err := os.Stat(filepath.Join(env.Root, ".claude", "agents", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote project-level sub-agent %s", name)
		}
	}

	// Community routing has one canonical copy in AGENTS.md. CLAUDE.md
	// imports it rather than carrying a second generated block.
	agentsMd, _ := os.ReadFile(filepath.Join(env.Root, "AGENTS.md"))
	if !strings.Contains(string(agentsMd), agents.CommunitiesStartMarker) {
		t.Fatalf("AGENTS.md missing communities start marker: %s", agentsMd)
	}
	claudeMd, _ := os.ReadFile(filepath.Join(env.Root, "CLAUDE.md"))
	if strings.Count(string(claudeMd), projectAgentsImport) != 1 {
		t.Fatalf("CLAUDE.md must import AGENTS.md exactly once: %s", claudeMd)
	}
	if strings.Contains(string(claudeMd), agents.CommunitiesStartMarker) {
		t.Fatalf("CLAUDE.md unexpectedly duplicates communities block: %s", claudeMd)
	}

	// Hooks file must reference our test hook command.
	hooksFile, _ := os.ReadFile(filepath.Join(env.Root, ".claude", "settings.local.json"))
	if !strings.Contains(string(hooksFile), "PreToolUse") {
		t.Fatalf("settings.local.json missing PreToolUse: %s", hooksFile)
	}

	// Idempotent re-run: every file should report skip.
	res2, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, f := range res2.Files {
		if f.Action != agents.ActionSkip {
			t.Errorf("expected skip on re-run for %s, got %s", f.Path, f.Action)
		}
	}
}

// TestClaudeCodeGlobalModeWritesUserFiles verifies that global mode
// (entered via `gortex install`) writes to ~/.claude.json, user-level
// hooks, and the user-level slash-commands + curated skills trees,
// while leaving the per-repo tree alone.
func TestClaudeCodeGlobalModeWritesUserFiles(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true in global mode")
	}

	// User-level files exist.
	expected := []string{
		filepath.Join(env.Home, ".claude.json"),
		filepath.Join(env.Home, ".claude", "settings.json"),
	}
	for name := range SlashCommands {
		expected = append(expected, filepath.Join(env.Home, ".claude", "commands", name))
	}
	for name := range GlobalSkills {
		expected = append(expected, filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md"))
	}
	for name := range SubAgents {
		expected = append(expected, filepath.Join(env.Home, ".claude", "agents", name))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing user-level artifact %s: %v", p, err)
		}
	}

	// settings.json auto-approves only the safe compact read surface; mutation,
	// session control, and external writes still require host approval.
	settingsPath := filepath.Join(env.Home, ".claude", "settings.json")
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", settingsPath, err)
	}
	if !strings.Contains(string(body), "mcp__gortex__read") || strings.Contains(string(body), "mcp__gortex__*") || strings.Contains(string(body), "mcp__gortex__edit") {
		t.Errorf("expected the safe compact permission set in %s, got:\n%s", settingsPath, body)
	}

	// CLAUDE.md must contain the marker-fenced rule block when
	// InstallGlobalInstructions is true.
	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	mdBody, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMd, err)
	}
	if !strings.Contains(string(mdBody), agents.GlobalRulesStartMarker) ||
		!strings.Contains(string(mdBody), agents.GlobalRulesEndMarker) {
		t.Errorf("expected gortex marker block in %s, got:\n%s", claudeMd, mdBody)
	}
	if !strings.Contains(string(mdBody), "MANDATORY: Use Gortex MCP tools") {
		t.Errorf("expected rule body in %s, got:\n%s", claudeMd, mdBody)
	}

	// Per-repo files should *not* exist under global mode.
	for _, p := range []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, "CLAUDE.md"),
		filepath.Join(env.Root, ".claude", "settings.local.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("global mode unexpectedly wrote per-repo file %s", p)
		}
	}
}

func TestClaudeCodeGlobalModeHonorsClaudeConfigDir(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true
	configDir := filepath.Join(t.TempDir(), "work-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true in global mode")
	}

	expected := []string{
		filepath.Join(configDir, ".claude.json"),
		filepath.Join(configDir, "settings.json"),
		filepath.Join(configDir, "CLAUDE.md"),
	}
	for name := range SlashCommands {
		expected = append(expected, filepath.Join(configDir, "commands", name))
	}
	for name := range GlobalSkills {
		expected = append(expected, filepath.Join(configDir, "skills", name, "SKILL.md"))
	}
	for name := range SubAgents {
		expected = append(expected, filepath.Join(configDir, "agents", name))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing CLAUDE_CONFIG_DIR artifact %s: %v", p, err)
		}
	}

	for _, p := range []string{
		filepath.Join(env.Home, ".claude.json"),
		filepath.Join(env.Home, ".claude"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("global mode unexpectedly wrote HOME artifact %s", p)
		}
	}

	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		if !strings.HasPrefix(f.Path, configDir+string(os.PathSeparator)) {
			t.Errorf("planned path %s did not use CLAUDE_CONFIG_DIR %s", f.Path, configDir)
		}
	}
}

// TestClaudeCodeConfigDirOverrideBeatsEnv verifies the explicit
// override (set by `gortex install --claude-config-dir`) wins over
// $CLAUDE_CONFIG_DIR, and that neither the env dir nor HOME is touched
// when the override is active.
func TestClaudeCodeConfigDirOverrideBeatsEnv(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	envDir := filepath.Join(t.TempDir(), "env-profile")
	overrideDir := filepath.Join(t.TempDir(), "flag-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", envDir)
	SetConfigDirOverride(overrideDir)
	t.Cleanup(func() { SetConfigDirOverride("") })

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, p := range []string{
		filepath.Join(overrideDir, ".claude.json"),
		filepath.Join(overrideDir, "settings.json"),
		filepath.Join(overrideDir, "CLAUDE.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing override artifact %s: %v", p, err)
		}
	}
	if _, err := os.Stat(envDir); err == nil {
		t.Errorf("env-profile dir should be untouched when the override is set")
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".claude")); err == nil {
		t.Errorf("HOME .claude should be untouched when the override is set")
	}
}

// TestClaudeCodeRemoveGlobal is the round-trip contract for `gortex
// uninstall --global`: after an install, RemoveGlobal must delete the
// owned skills/commands/agents and strip the Gortex portion of every
// merged file while preserving the user's own content (other MCP
// servers, personal CLAUDE.md prose). GlobalArtifacts must report an
// empty footprint afterward.
func TestClaudeCodeRemoveGlobal(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	configDir := filepath.Join(env.Home, ".claude")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-gortex MCP server and personal CLAUDE.md prose must survive.
	if err := os.WriteFile(filepath.Join(env.Home, ".claude.json"),
		[]byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "CLAUDE.md"),
		[]byte("# My rules\n\nBe terse.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Fatal("expected a non-empty footprint after install")
	}

	removed, failures := a.RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("expected RemoveGlobal to remove at least one artifact")
	}

	// Owned files deleted.
	for name := range GlobalSkills {
		if _, err := os.Stat(filepath.Join(configDir, "skills", name)); err == nil {
			t.Errorf("skill %s not removed", name)
		}
	}
	for name := range SlashCommands {
		if _, err := os.Stat(filepath.Join(configDir, "commands", name)); err == nil {
			t.Errorf("command %s not removed", name)
		}
	}
	for name := range SubAgents {
		if _, err := os.Stat(filepath.Join(configDir, "agents", name)); err == nil {
			t.Errorf("sub-agent %s not removed", name)
		}
	}

	// CLAUDE.md: gortex block stripped, personal prose preserved.
	md, _ := os.ReadFile(filepath.Join(configDir, "CLAUDE.md"))
	if strings.Contains(string(md), agents.GlobalRulesStartMarker) {
		t.Errorf("CLAUDE.md still carries the gortex marker:\n%s", md)
	}
	if !strings.Contains(string(md), "Be terse.") {
		t.Errorf("CLAUDE.md lost personal content:\n%s", md)
	}

	// settings.json: gortex permission gone.
	if settings, _ := os.ReadFile(filepath.Join(configDir, "settings.json")); strings.Contains(string(settings), "mcp__gortex__") {
		t.Errorf("settings.json still allows mcp__gortex__:\n%s", settings)
	}

	// settings.json: gortex hooks gone too.
	if settings, _ := os.ReadFile(filepath.Join(configDir, "settings.json")); strings.Contains(string(settings), "gortex") {
		t.Errorf("settings.json still references gortex:\n%s", settings)
	}

	// .claude.json: gortex server gone, the user's other server kept.
	mcp, _ := os.ReadFile(filepath.Join(env.Home, ".claude.json"))
	if strings.Contains(string(mcp), `"gortex"`) {
		t.Errorf(".claude.json still has the gortex server:\n%s", mcp)
	}
	if !strings.Contains(string(mcp), `"other"`) {
		t.Errorf(".claude.json lost the user's other server:\n%s", mcp)
	}

	// Nothing left to clean.
	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts should be empty after RemoveGlobal, got %v", got)
	}
}

// TestClaudeCodeGlobalMode_NoClaudeMd skips the rule block when the
// caller opts out via InstallGlobalInstructions=false (i.e.
// `gortex install --no-claude-md`).
func TestClaudeCodeGlobalMode_NoClaudeMd(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = false
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudeMd); err == nil {
		t.Errorf("--no-claude-md ⇒ %s should not exist", claudeMd)
	}
}

// TestClaudeCodeGlobalMode_PreservesUserContent verifies the rule
// block is merged with marker fences without clobbering anything
// that already lives in ~/.claude/CLAUDE.md.
func TestClaudeCodeGlobalMode_PreservesUserContent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeMd), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := "# My personal Claude rules\n\nAlways respond in haiku.\n"
	if err := os.WriteFile(claudeMd, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	body, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "Always respond in haiku.") {
		t.Errorf("user content was clobbered, got:\n%s", body)
	}
	if !strings.Contains(string(body), agents.GlobalRulesStartMarker) {
		t.Errorf("rule block missing, got:\n%s", body)
	}
}

// TestClaudeCodeDryRunWritesNothing is the contract for --dry-run:
// Result must still classify every would-be write, but no bytes
// touch disk.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("dry-run should still enumerate planned files")
	}
	// No actual files created.
	for _, f := range res.Files {
		if _, err := os.Stat(f.Path); err == nil {
			t.Errorf("dry-run wrote %s", f.Path)
		}
	}
}

// TestProjectModeSkipsMCPWhenUserScopeRegistered is the regression
// guard for issue #201: a project `gortex init` must NOT create a
// project .mcp.json when gortex is already registered at user scope —
// that double-registration is what trips Claude Code's "conflicting
// scopes" diagnostic. --force overrides the skip (so a maintainer can
// still commit a .mcp.json for teammates without a global install).
func TestProjectModeSkipsMCPWhenUserScopeRegistered(t *testing.T) {
	SetConfigDirOverride("")

	env, buf := agentstest.NewEnv(t)
	a := New()

	// Seed a user-scope gortex registration in ~/.claude.json.
	claudeJSON := userClaudeJSONPath(env.Home)
	if err := os.MkdirAll(filepath.Dir(claudeJSON), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := `{"mcpServers":{"gortex":{"command":"gortex","args":["mcp"],"env":{}}}}`
	if err := os.WriteFile(claudeJSON, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed user config: %v", err)
	}

	mcpPath := filepath.Join(env.Root, ".mcp.json")

	// Without --force: the project .mcp.json must be skipped and the
	// user warned.
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(mcpPath); err == nil {
		t.Errorf("project .mcp.json was written despite a user-scope gortex registration")
	}
	if !strings.Contains(buf.String(), "already registered at user scope") {
		t.Errorf("expected a conflicting-scopes warning, got: %q", buf.String())
	}

	// With --force: the project .mcp.json is written anyway.
	if _, err := a.Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply --force: %v", err)
	}
	if _, err := os.Stat(mcpPath); err != nil {
		t.Errorf("--force should still write project .mcp.json: %v", err)
	}
}

// TestProjectModeWritesMCPWhenNoUserScope confirms the common path is
// unchanged: with no user-scope gortex registration, the project
// .mcp.json is created as before.
func TestProjectModeWritesMCPWhenNoUserScope(t *testing.T) {
	SetConfigDirOverride("")

	env, _ := agentstest.NewEnv(t)
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".mcp.json")); err != nil {
		t.Errorf("project .mcp.json should be written when no user-scope entry exists: %v", err)
	}
}

// TestClaudeCodeGlobalHooksLandInUserSettings pins where the user-level hooks
// go. Claude Code loads user-scope settings from ~/.claude/settings.json;
// settings.local.json is only read as a project file, so hooks written to
// ~/.claude/settings.local.json never run (#840).
func TestClaudeCodeGlobalHooksLandInUserSettings(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	userSettings, _ := os.ReadFile(filepath.Join(env.Home, ".claude", "settings.json"))
	if !strings.Contains(string(userSettings), `"hooks"`) || !strings.Contains(string(userSettings), "gortex") {
		t.Errorf("user-level hooks missing from ~/.claude/settings.json:\n%s", userSettings)
	}
	if local, err := os.ReadFile(filepath.Join(env.Home, ".claude", "settings.local.json")); err == nil &&
		strings.Contains(string(local), `"hooks"`) {
		t.Errorf("user-level hooks written to ~/.claude/settings.local.json, which is never loaded as user settings:\n%s", local)
	}
}

// TestInspectReadsHooksFromUserSettings keeps doctor on the file the host
// loads: hooks only in settings.local.json must not read as configured.
func TestInspectReadsHooksFromUserSettings(t *testing.T) {
	body := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/gortex hook"}]}]}}`
	write := func(t *testing.T, home, name string) {
		t.Helper()
		dir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("settings.json is counted", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		write(t, env.Home, "settings.json")
		if got := Inspect(env.Home).Hooks["SessionStart"]; got != 1 {
			t.Errorf("SessionStart hooks in settings.json = %d, want 1", got)
		}
	})
	t.Run("settings.local.json is not counted", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		write(t, env.Home, "settings.local.json")
		if got := Inspect(env.Home).Hooks["SessionStart"]; got != 0 {
			t.Errorf("SessionStart hooks in settings.local.json = %d, want 0 (never loaded as user settings)", got)
		}
	})
}

// TestRemoveGlobalCleansLegacyLocalHooks: installs from before #840 left hooks
// in ~/.claude/settings.local.json; uninstall must still remove them.
func TestRemoveGlobalCleansLegacyLocalHooks(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dir := filepath.Join(env.Home, ".claude")
	legacy := filepath.Join(dir, "settings.local.json")
	if err := os.WriteFile(legacy, []byte(legacyLocalHooks), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := GlobalArtifacts(env.Home); !slices.Contains(got, legacy) {
		t.Errorf("GlobalArtifacts should list the legacy file, got %v", got)
	}
	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); strings.Contains(string(b), "gortex hook") {
			t.Errorf("%s still carries a gortex hook:\n%s", name, b)
		}
	}
}

const legacyLocalHooks = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/old/bin/gortex hook"}]}]}}`

// TestApplyGlobalMigratesLegacyLocalHooks: re-running install after upgrading
// moves the hooks out of settings.local.json so a session rooted at $HOME
// does not load them twice.
func TestApplyGlobalMigratesLegacyLocalHooks(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	dir := filepath.Join(env.Home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), []byte(legacyLocalHooks), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.local.json")); strings.Contains(string(b), "gortex") {
		t.Errorf("legacy hooks left in settings.local.json:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.json")); !strings.Contains(string(b), "gortex") {
		t.Errorf("hooks missing from settings.json:\n%s", b)
	}
}

// TestRemoveGlobalCountsSettingsOnce: permissions and hooks share
// settings.json, which must count as one removed artifact, and a user-owned
// entry that merely mentions gortex is neither listed nor removed.
func TestRemoveGlobalCountsSettingsOnce(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	dir := filepath.Join(env.Home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"permissions":{"allow":["Bash(gortex status)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := GlobalArtifacts(env.Home); slices.Contains(got, settings) {
		t.Errorf("user-owned entry should not be listed, got %v", got)
	}
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	artifacts := GlobalArtifacts(env.Home)
	removed, failures := a.RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed != len(artifacts) {
		t.Errorf("RemoveGlobal removed %d, GlobalArtifacts listed %d: %v", removed, len(artifacts), artifacts)
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "Bash(gortex status)") {
		t.Errorf("user-owned entry was removed:\n%s", b)
	}
}
