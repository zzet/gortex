package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/toolref"
)

// TestMain pins Detect's binary probe to "agy is installed" so the tests do
// not depend on whether the host has agy on PATH. TestAntigravityDetect
// overrides it per case.
func TestMain(m *testing.M) {
	lookAntigravityBinary = func() (string, error) { return "/usr/bin/agy", nil }
	os.Exit(m.Run())
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globalEnv(t *testing.T) agents.Env {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = true
	env.InstallGlobalInstructions = true
	return env
}

// TestAntigravityRegistersMCPAndWritesKI is the acceptance test for
// project mode: both native MCP configs, the Knowledge Item, the AGENTS.md
// community routing block and generated skills, but no user-level hooks.
func TestAntigravityRegistersMCPAndWritesKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// 1. Native MCP stanzas under ~/.gemini/antigravity/mcp_config.json (IDE)
	//    and ~/.gemini/config/mcp_config.json (agy CLI / Antigravity 2.0).
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		if _, err := os.Stat(mcpPath); err != nil {
			t.Fatalf("mcp_config.json missing at %s: %v", mcpPath, err)
		}
		cfg := agentstest.ReadJSON(t, mcpPath)
		servers, ok := cfg["mcpServers"].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers missing in %s: %v", mcpPath, cfg)
		}
		if _, ok := servers["gortex"]; !ok {
			t.Fatalf("gortex server missing from mcpServers in %s: %v", mcpPath, servers)
		}
	}

	// 2. Knowledge Item artifacts.
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	for _, p := range []string{
		filepath.Join(kiBase, "metadata.json"),
		filepath.Join(kiBase, "artifacts", "gortex-instructions.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("KI artifact missing: %s (%v)", p, err)
		}
	}
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatalf("read instructions: %v", err)
	}
	text := string(instructions)
	for _, want := range []string{
		agents.InstructionsSentinel,
		"Call `explore` first",
		"change(operation:\"impact\")",
		toolref.TransportClause(),
		toolref.DegradedModeLine(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Antigravity instructions missing %q", want)
		}
	}
	for _, forbidden := range []string{"./gortex query", "facade-v1", "tools_search", "gortex call ", "daemon start"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Antigravity instructions contain obsolete agent vocabulary %q", forbidden)
		}
	}

	// 3. Project mode: routing block and generated skills, no user-level hooks.
	for _, p := range []string{
		filepath.Join(env.Root, "AGENTS.md"),
		generatedSkillPath(env.Root, "gortex-stub"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("project-mode artifact missing: %v", err)
		}
	}
	if _, err := os.Stat(HookConfigPath(env.Home)); err == nil {
		t.Error("hooks.json must not be installed in project mode")
	}
	agentstest.AssertIdempotent(t, a, env)
}

func TestAntigravityMigratesExactV060KI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	for path, content := range map[string]string{
		metadataPath:     v060Metadata,
		instructionsPath: v060Instructions,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range map[string]string{
		metadataPath:     Metadata,
		instructionsPath: Instructions,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("v0.60.0 artifact %s was not migrated", path)
		}
	}
}

func TestAntigravityPreservesCustomizedKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	custom := map[string]string{
		metadataPath:     `{"summary":"my policy"}`,
		instructionsPath: "# Keep my custom workflow\n",
	}
	for path, content := range custom {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range custom {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("customized artifact %s was overwritten", path)
		}
	}
}

// readJSONC parses a file agy reads as JSONC (comments allowed).
func readJSONC(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(agents.StripJSONComments(data), &root); err != nil {
		t.Fatalf("%s does not parse: %v\n%s", path, err, data)
	}
	return root
}

// A global install writes one gortex spec covering all five events, each
// carrying its on-disk event name and the --hook-mode flag.
func TestAntigravityHooksJSONHonoursHookMode(t *testing.T) {
	for mode, flag := range map[string]string{
		"": "", "deny": "", "enrich": " --mode=enrich",
		"consult-unlock": " --mode=consult-unlock", "nudge": " --mode=nudge",
	} {
		t.Run("mode="+mode, func(t *testing.T) {
			env := globalEnv(t)
			env.HookMode = mode
			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			type handler struct{ Command string }
			var root map[string]map[string][]struct {
				handler
				Matcher string
				Hooks   []handler
			}
			data, _ := os.ReadFile(HookConfigPath(env.Home))
			if err := json.Unmarshal(data, &root); err != nil {
				t.Fatalf("hooks.json: %v\n%s", err, data)
			}
			for _, ev := range HookEvents {
				onDisk := antigravityHookEventNames[ev]
				entries := root[hookPluginName][onDisk]
				if len(entries) != 1 {
					t.Fatalf("%s: want one entry, got %+v", onDisk, entries)
				}
				e := entries[0]
				if len(e.Hooks) == 1 {
					e.handler = e.Hooks[0]
				}
				if want := "/tmp/test-gortex hook --agent=antigravity --event=" + onDisk + flag; e.Command != want {
					t.Errorf("%s command = %q, want %q", onDisk, e.Command, want)
				}
				wantMatcher := map[string]string{"PreToolUse": "*", "PostToolUse": postToolUseMatcher}[onDisk]
				if e.Matcher != wantMatcher {
					t.Errorf("%s matcher = %q, want %q", onDisk, e.Matcher, wantMatcher)
				}
			}
		})
	}
}

// The PostToolUse matcher must catch every way agy names an MCP call and
// skip native tools, whether agy anchors the regex or not.
func TestAntigravityPostToolUseMatcherSelectsMCPCalls(t *testing.T) {
	substring := regexp.MustCompile(postToolUseMatcher)
	anchored := regexp.MustCompile("^(?:" + postToolUseMatcher + ")$")
	for name, want := range map[string]bool{
		"call_mcp_tool": true, "mcp_gortex_search": true, "mcp__gortex__explore": true,
		"view_file": false, "run_command": false, "grep_search": false, "replace_file_content": false,
	} {
		if substring.MatchString(name) != want || anchored.MatchString(name) != want {
			t.Errorf("PostToolUse matcher on %q: want match=%v", name, want)
		}
	}
}

// agy runs hooks via `sh -c` on Unix and `cmd /c` on Windows; cmd has no
// single quotes and strips quotes from a line that starts with one.
func TestAntigravityHookCommandLineQuotesForEachShell(t *testing.T) {
	prev := hookShellIsCmd
	t.Cleanup(func() { hookShellIsCmd = prev })

	const args = " hook --agent=antigravity --event=PreToolUse"
	for _, tc := range []struct {
		cmd       bool
		bin, want string
	}{
		{false, "/usr/local/bin/gortex", "/usr/local/bin/gortex"},
		{false, "/home/First Last/go/bin/gortex", "'/home/First Last/go/bin/gortex'"},
		{true, "C:/Users/me/go/bin/gortex.exe", `C:\Users\me\go\bin\gortex.exe`},
		{true, `C:/Program Files (x86)/gortex/gortex.exe`, `call "C:\Program Files (x86)\gortex\gortex.exe"`},
	} {
		hookShellIsCmd = tc.cmd
		got := hookCommandLine(tc.bin, args)
		if got != tc.want+args {
			t.Errorf("hookCommandLine(%q, cmd=%v) = %s, want %s", tc.bin, tc.cmd, got, tc.want+args)
		}
		if !isGortexAntigravityCommand(got) {
			t.Errorf("doctor/uninstall would not recognise %s as a Gortex hook", got)
		}
	}
}

// A global install merges into the JSONC files agy reads (keeping other
// users' entries and the user's "enabled": false kill-switch, even under
// --force) and writes the rule block and curated skills.
func TestAntigravityGlobalInstallKeepsUserState(t *testing.T) {
	env := globalEnv(t)
	a := New()
	hooksPath := HookConfigPath(env.Home)
	mcpPath := filepath.Join(env.Home, ".gemini", "config", "mcp_config.json")
	writeTestFile(t, hooksPath, `{
  // destructive command guard
  "dcg": {"PreToolUse": [{"matcher": "run_command", "hooks": [{"type": "command", "command": "dcg check"}]}],},
  "gortex": {"enabled": false},
}`)
	writeTestFile(t, mcpPath, `{
  /* shared servers */
  "mcpServers": {"other": {"command": "other-server"}},
}`)
	if _, err := a.Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, p := range []string{hooksPath, mcpPath} {
		if _, err := os.Stat(p + ".bak"); err == nil {
			t.Errorf("%s was treated as malformed (a .bak was written)", p)
		}
	}
	root := readJSONC(t, hooksPath)
	spec, _ := root[hookPluginName].(map[string]any)
	if root["dcg"] == nil || spec["enabled"] != false || spec["PreToolUse"] == nil {
		t.Errorf("want dcg kept and gortex hooks written with enabled=false kept: %v", root)
	}
	servers, _ := readJSONC(t, mcpPath)["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["gortex"] == nil {
		t.Errorf("expected both other and gortex servers, got %v", servers)
	}
	if _, err := os.Stat(GlobalPluginManifestPath(env.Home)); err != nil {
		t.Errorf("plugin.json missing: %v", err)
	}
	for _, id := range CuratedSkillNames() {
		if _, err := os.Stat(curatedSkillPath(env.Home, id)); err != nil {
			t.Errorf("curated skill %s missing: %v", id, err)
		}
	}

	// A customised skill survives a forced re-install and a profile switch
	// that drops it; an unedited skill outside the profile is pruned.
	custom := curatedSkillPath(env.Home, "gortex-debug")
	writeTestFile(t, custom, "# my debug playbook\n")
	if _, err := a.Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if _, err := SyncSkills(env.Stderr, env.Home, []string{"gortex-explore"}, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	if got, _ := os.ReadFile(custom); string(got) != "# my debug playbook\n" {
		t.Errorf("customised skill was overwritten or pruned: %q", got)
	}
	if _, err := os.Stat(curatedSkillPath(env.Home, "gortex-explore")); err != nil {
		t.Errorf("gortex-explore should stay: %v", err)
	}
	if _, err := os.Stat(curatedSkillPath(env.Home, "gortex-refactor")); !os.IsNotExist(err) {
		t.Errorf("unedited gortex-refactor should be pruned, err=%v", err)
	}
}

// Detect must not treat what earlier Gortex releases wrote on every machine
// (the KI and the MCP stanzas) as proof that Antigravity is installed.
func TestAntigravityDetect(t *testing.T) {
	footprint := func(extra ...string) []string {
		return append([]string{
			".gemini/antigravity/mcp_config.json",
			".gemini/config/mcp_config.json",
			".gemini/antigravity/knowledge/gortex-workflow/metadata.json",
		}, extra...)
	}
	for _, tc := range []struct {
		name   string
		onPath bool
		files  []string
		want   bool
	}{
		{"empty home", false, nil, false},
		{"gortex footprint only", false, footprint(), false},
		{"agy on PATH", true, nil, true},
		{"agy CLI home", false, footprint(".gemini/antigravity-cli/installation_id"), true},
		{"agy config", false, footprint(".gemini/config/config.json"), true},
		{"IDE conversations", false, footprint(".gemini/antigravity/brain/conv/transcript.jsonl"), true},
		{"user's own KI", false, footprint(".gemini/antigravity/knowledge/my-notes/metadata.json"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := lookAntigravityBinary
			t.Cleanup(func() { lookAntigravityBinary = prev })
			lookAntigravityBinary = func() (string, error) {
				if tc.onPath {
					return "/usr/bin/agy", nil
				}
				return "", os.ErrNotExist
			}
			env, _ := agentstest.NewEnv(t)
			for _, rel := range tc.files {
				writeTestFile(t, filepath.Join(env.Home, rel), "{}")
			}
			if got, err := New().Detect(env); err != nil || got != tc.want {
				t.Errorf("Detect = %v (err=%v), want %v", got, err, tc.want)
			}
		})
	}
}

// The repo routing block goes to AGENTS.md; an existing GEMINI.md is used
// only while AGENTS.md is absent, matching agy's fallback order.
func TestAntigravityCommunitiesRoutingPrefersAgentsMD(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()
	if !a.WritesCommunitiesRouting() || !a.WritesSkillFiles() || a.Name() != Name || a.DocsURL() != DocsURL {
		t.Error("adapter must advertise routing and skill writes")
	}
	agentsMD, geminiMD := filepath.Join(env.Root, "AGENTS.md"), filepath.Join(env.Root, "GEMINI.md")
	if p, _ := a.Plan(agents.Env{}); len(p.Files) != 0 {
		t.Errorf("Plan without a home should be empty: %+v", p.Files)
	}
	if p := a.CommunitiesRoutingPath(env); p != agentsMD {
		t.Errorf("no instruction file: got %s, want AGENTS.md", p)
	}

	writeTestFile(t, geminiMD, "# Gemini\n")
	plan, err := a.Plan(env)
	if err != nil || !slices.ContainsFunc(plan.Files, func(f agents.FileAction) bool { return f.Path == geminiMD }) {
		t.Errorf("plan should merge into GEMINI.md: %+v (err=%v)", plan, err)
	}
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if data, _ := os.ReadFile(geminiMD); !strings.Contains(string(data), agentstest.StubSkillsRouting) {
		t.Errorf("GEMINI.md missing routing block:\n%s", data)
	}
	if _, err := os.Stat(agentsMD); err == nil {
		t.Error("AGENTS.md was created while GEMINI.md was present")
	}

	writeTestFile(t, agentsMD, "# Agents\n")
	if p := a.CommunitiesRoutingPath(env); p != agentsMD {
		t.Errorf("both files: got %s, want AGENTS.md", p)
	}
}
