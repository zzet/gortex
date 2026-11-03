package antigravity

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// A clean global install is announced by Plan and removed completely, and a
// dry run of either direction writes nothing.
func TestAntigravityRemoveGlobalCleanInstall(t *testing.T) {
	env := globalEnv(t)
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{DryRun: true}); err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".gemini")); !os.IsNotExist(err) {
		t.Fatalf("dry-run apply wrote to disk (err=%v)", err)
	}

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	installed := GlobalArtifacts(env.Home)
	want := append(mcpConfigPaths(env.Home), HookConfigPath(env.Home),
		GlobalPluginManifestPath(env.Home), GlobalInstructionsPath(env.Home))
	for _, p := range want {
		if !slices.Contains(installed, p) {
			t.Errorf("GlobalArtifacts missing %s: %v", p, installed)
		}
	}
	plan, err := a.Plan(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range installed {
		if !slices.ContainsFunc(plan.Files, func(f agents.FileAction) bool { return f.Path == p }) {
			t.Errorf("Plan does not announce %s", p)
		}
	}

	if n, failures := a.RemoveGlobal(env, agents.ApplyOpts{DryRun: true}); n == 0 || len(failures) > 0 {
		t.Fatalf("dry-run RemoveGlobal = %d, %v", n, failures)
	}
	if got := GlobalArtifacts(env.Home); !slices.Equal(got, installed) {
		t.Fatalf("dry-run RemoveGlobal changed disk: %v", got)
	}

	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) > 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts after RemoveGlobal = %v, want none", got)
	}
	for _, p := range []string{
		HookConfigPath(env.Home), // held only the gortex spec
		filepath.Dir(GlobalPluginManifestPath(env.Home)),
		curatedSkillsRoot(env.Home),
		filepath.Join(env.Home, ".gemini", "antigravity", "knowledge"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone (err=%v)", p, err)
		}
	}
}

// Uninstall removes only Gortex's pieces. What it keeps (other servers and
// hook specs, user prose, customised files, a renamed copy of the gortex
// spec) is exactly what GlobalArtifacts still reports, and it reads
// commented (JSONC) files.
func TestAntigravityRemoveGlobalKeepsUserContent(t *testing.T) {
	env := globalEnv(t)
	a := New()
	hooksPath := HookConfigPath(env.Home)
	mcpPath := filepath.Join(env.Home, ".gemini", "config", "mcp_config.json")
	writeTestFile(t, mcpPath, `{"mcpServers": {"other": {"command": "other-bin"}}}`)
	writeTestFile(t, hooksPath, `{"user": {"SessionStart": [{"type": "command", "command": "echo user"}]}}`)
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// User edits after install.
	root := readJSONC(t, HookConfigPath(env.Home))
	root["mine"] = root[hookPluginName]
	agentstest.WriteJSON(t, hooksPath, root)
	for _, p := range []string{hooksPath, mcpPath} {
		data, _ := os.ReadFile(p)
		writeTestFile(t, p, "// note\n"+string(data))
	}
	rules := GlobalInstructionsPath(env.Home)
	data, _ := os.ReadFile(rules)
	writeTestFile(t, rules, "# My policy\n\n"+string(data))
	metaPath := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow", "metadata.json")
	writeTestFile(t, metaPath, `{"custom":true}`)
	skillPath := curatedSkillPath(env.Home, "gortex-debug")
	writeTestFile(t, skillPath, "# my debug playbook\n")

	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) > 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}

	hooks := readJSONC(t, hooksPath)
	if hooks[hookPluginName] != nil || hooks["user"] == nil || hooks["mine"] == nil {
		t.Errorf("hooks.json should keep user and mine and drop gortex: %v", hooks)
	}
	servers, _ := readJSONC(t, mcpPath)["mcpServers"].(map[string]any)
	if servers["gortex"] != nil || servers["other"] == nil {
		t.Errorf("mcp_config.json should keep other and drop gortex: %v", servers)
	}
	if data, _ := os.ReadFile(rules); !strings.Contains(string(data), "# My policy") ||
		strings.Contains(string(data), agents.GlobalRulesStartMarker) {
		t.Errorf("rules file should keep user prose without the Gortex block:\n%s", data)
	}
	for p, want := range map[string]string{metaPath: `{"custom":true}`, skillPath: "# my debug playbook\n"} {
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("customised %s not kept: %q", p, got)
		}
	}

	if got := GlobalArtifacts(env.Home); !slices.Equal(got, []string{hooksPath}) {
		t.Errorf("GlobalArtifacts after uninstall = %v, want only the renamed hook spec in %s", got, hooksPath)
	}
}

// Files Gortex cannot parse are not claimed as Gortex's, and uninstall leaves
// them untouched.
func TestAntigravityGlobalArtifactsIgnoresUnparsableFiles(t *testing.T) {
	env := globalEnv(t)
	paths := append(mcpConfigPaths(env.Home), HookConfigPath(env.Home), GlobalPluginManifestPath(env.Home))
	for _, p := range paths {
		writeTestFile(t, p, `{"gortex": {not json`)
	}
	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts claimed unparsable files: %v", got)
	}
	if _, failures := New().RemoveGlobal(env, agents.ApplyOpts{}); len(failures) > 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	for _, p := range paths {
		if data, _ := os.ReadFile(p); string(data) != `{"gortex": {not json` {
			t.Errorf("uninstall rewrote unparsable %s: %q", p, data)
		}
	}
}
