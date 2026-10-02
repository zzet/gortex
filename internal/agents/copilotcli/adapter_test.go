package copilotcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/agents/vscode"
)

// newCopilotEnv returns a sandbox Env the adapter detects: a temp home
// with a ~/.copilot directory. COPILOT_HOME is cleared so a developer
// who exports it cannot redirect these writes onto their real config.
func newCopilotEnv(t *testing.T) (agents.Env, *bytes.Buffer) {
	t.Helper()
	t.Setenv(copilotHomeEnvVar, "")
	env, buf := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Home, copilotConfigDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	return env, buf
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func gortexServer(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcpServers object: %v", cfg)
	}
	entry, ok := servers[serverName].(map[string]any)
	if !ok {
		t.Fatalf("missing gortex entry: %v", servers)
	}
	return entry
}

// TestApplyWritesMcpServersWithLocalTransport pins the two schema facts
// the VS Code adapter would mislead a maintainer about: the top-level
// key is "mcpServers" (not "servers"), and the entry carries an explicit
// "type": "local" the CLI needs to launch it.
func TestApplyWritesMcpServersWithLocalTransport(t *testing.T) {
	env, _ := newCopilotEnv(t)

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected || !res.Configured {
		t.Fatalf("expected detected+configured, got %+v", res)
	}
	if res.DocsURL != DocsURL {
		t.Fatalf("DocsURL = %q, want %q", res.DocsURL, DocsURL)
	}

	cfg := agentstest.ReadJSON(t, MCPConfigPath(env))
	if _, wrong := cfg["servers"]; wrong {
		t.Fatalf("wrote VS Code's \"servers\" key; Copilot CLI reads \"mcpServers\": %v", cfg)
	}
	entry := gortexServer(t, cfg)
	if entry["type"] != localTransport {
		t.Fatalf("entry type = %v, want %q — the CLI does not infer stdio", entry["type"], localTransport)
	}
	if entry["command"] != "gortex" {
		t.Fatalf("command = %v, want gortex", entry["command"])
	}
	args, _ := entry["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Fatalf("args = %v, want [mcp]", entry["args"])
	}
	if _, hasEnv := entry["env"]; hasEnv {
		t.Fatalf("env = %v, want none — the parse worker count is the daemon's default", entry["env"])
	}
}

// TestDetectFalseInEmptySandbox: no copilot binary, no config home.
func TestDetectFalseInEmptySandbox(t *testing.T) {
	t.Setenv(copilotHomeEnvVar, "")
	original := lookCopilotBinary
	lookCopilotBinary = func() (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookCopilotBinary = original })

	env, _ := agentstest.NewEnv(t)
	detected, err := New().Detect(env)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if detected {
		t.Fatal("Detect must be false with no copilot binary and no ~/.copilot")
	}

	// And a .vscode directory must not flip it — that is the vscode
	// adapter's host, not ours.
	if err := os.MkdirAll(filepath.Join(env.Root, ".vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if detected, _ := New().Detect(env); detected {
		t.Fatal("Detect must not key off .vscode")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	env, _ := newCopilotEnv(t)
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	mcpBefore := readFile(t, MCPConfigPath(env))
	insBefore := readFile(t, filepath.Join(env.Root, ".github", instructionsFileName))

	agentstest.AssertIdempotent(t, a, env)

	if got := readFile(t, MCPConfigPath(env)); got != mcpBefore {
		t.Fatalf("mcp-config.json changed on re-run:\n%s\n---\n%s", mcpBefore, got)
	}
	if got := readFile(t, filepath.Join(env.Root, ".github", instructionsFileName)); got != insBefore {
		t.Fatal("copilot-instructions.md changed on re-run")
	}
}

// TestUserAuthoredEntriesSurvive: a server we did not author is the
// user's, whatever it is named.
func TestUserAuthoredEntriesSurvive(t *testing.T) {
	env, _ := newCopilotEnv(t)
	seed := map[string]any{
		"mcpServers": map[string]any{
			// Named "gortex" but launched by something else — a user who
			// wired their own wrapper. Never rewrite it.
			serverName: map[string]any{
				"type":    "local",
				"command": "node",
				"args":    []any{"./my-gortex-wrapper.js"},
			},
			"acme": map[string]any{"command": "acme-server", "args": []any{"--stdio"}},
		},
	}
	agentstest.WriteJSON(t, MCPConfigPath(env), seed)
	before := readFile(t, MCPConfigPath(env))

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, MCPConfigPath(env)); got != before {
		t.Fatalf("user-authored entries were rewritten:\n%s\n---\n%s", before, got)
	}
	for _, f := range res.Files {
		if f.Path == MCPConfigPath(env) && f.Action != agents.ActionSkip {
			t.Fatalf("expected a skip for a user-owned config, got %s", f.Action)
		}
	}
}

// TestInstructionsConvergeWithVSCodeAdapter is the co-existence fence.
// The vscode adapter writes the same community-routing block into the
// same .github/copilot-instructions.md, so a repo running both must end
// up with exactly one marker-fenced block regardless of apply order.
// The agent-render golden cannot catch this: it renders each adapter
// into its own sandbox, so the two never meet there.
func TestInstructionsConvergeWithVSCodeAdapter(t *testing.T) {
	cases := []struct {
		name  string
		order []agents.Adapter
	}{
		{"vscode-first", []agents.Adapter{vscode.New(), New()}},
		{"copilot-cli-first", []agents.Adapter{New(), vscode.New()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := newCopilotEnv(t)
			// ForceDetect so the assertion does not depend on whether VS
			// Code happens to be installed on the machine running the test.
			opts := agents.ApplyOpts{ForceDetect: true}
			apply := func() {
				for _, a := range tc.order {
					if _, err := a.Apply(env, opts); err != nil {
						t.Fatalf("%s apply: %v", a.Name(), err)
					}
				}
			}

			apply()
			path := filepath.Join(env.Root, ".github", instructionsFileName)
			first := readFile(t, path)
			if n := strings.Count(first, agents.CommunitiesStartMarker); n != 1 {
				t.Fatalf("expected exactly 1 communities block, got %d:\n%s", n, first)
			}
			if n := strings.Count(first, agents.CommunitiesEndMarker); n != 1 {
				t.Fatalf("expected exactly 1 communities end marker, got %d:\n%s", n, first)
			}

			apply()
			if second := readFile(t, path); second != first {
				t.Fatalf("re-running both adapters was not byte-stable:\n%s\n---\n%s", first, second)
			}
		})
	}
}

// claudecodeMCPJSON is the repo .mcp.json the claudecode adapter writes,
// plus an unrelated sibling server. Its gortex entry has no "type" and
// an env value in shell-parameter form — neither of which the CLI
// understands, and the CLI prefers this file over ~/.copilot's.
const claudecodeMCPJSON = `{
  "mcpServers": {
    "acme": {
      "command": "acme-server",
      "args": [
        "--stdio"
      ]
    },
    "gortex": {
      "command": "gortex",
      "args": [
        "mcp"
      ],
      "env": {
        "GORTEX_INDEX_WORKERS": "${GORTEX_WORKERS:-8}"
      }
    }
  }
}
`

func TestReconcilesRepoMCPJSONThatShadowsUserConfig(t *testing.T) {
	env, _ := newCopilotEnv(t)
	path := filepath.Join(env.Root, ".mcp.json")
	if err := os.WriteFile(path, []byte(claudecodeMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	var seeded map[string]any
	if err := json.Unmarshal([]byte(claudecodeMCPJSON), &seeded); err != nil {
		t.Fatal(err)
	}
	wantSibling, _ := json.Marshal(seeded["mcpServers"].(map[string]any)["acme"])

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var touched bool
	for _, f := range res.Files {
		if f.Path == path {
			touched = true
			if f.Action != agents.ActionMerge {
				t.Fatalf(".mcp.json action = %s, want merge", f.Action)
			}
		}
	}
	if !touched {
		t.Fatal("Apply did not report the .mcp.json reconciliation")
	}

	cfg := agentstest.ReadJSON(t, path)
	entry := gortexServer(t, cfg)
	if entry["type"] != localTransport {
		t.Fatalf("type = %v, want %q added to the shadowing entry", entry["type"], localTransport)
	}
	envMap, _ := entry["env"].(map[string]any)
	if envMap["GORTEX_INDEX_WORKERS"] != "8" {
		t.Fatalf("env GORTEX_INDEX_WORKERS = %v, want the literal \"8\"", envMap["GORTEX_INDEX_WORKERS"])
	}

	gotSibling, _ := json.Marshal(cfg["mcpServers"].(map[string]any)["acme"])
	if string(gotSibling) != string(wantSibling) {
		t.Fatalf("non-gortex server changed:\n%s\n---\n%s", wantSibling, gotSibling)
	}

	// Second run must be a no-op on the file we repaired.
	before := readFile(t, path)
	agentstest.AssertIdempotent(t, New(), env)
	if after := readFile(t, path); after != before {
		t.Fatalf(".mcp.json rewritten on re-run:\n%s\n---\n%s", before, after)
	}
}

// TestNeverCreatesRepoMCPJSON: an absent .mcp.json belongs to the
// claudecode adapter; we must not conjure one.
func TestNeverCreatesRepoMCPJSON(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".mcp.json must not be created by this adapter (stat err = %v)", err)
	}
}

// TestLeavesForeignRepoMCPJSONAlone: a .mcp.json with no Gortex-authored
// entry is entirely somebody else's file.
func TestLeavesForeignRepoMCPJSONAlone(t *testing.T) {
	env, _ := newCopilotEnv(t)
	path := filepath.Join(env.Root, ".mcp.json")
	agentstest.WriteJSON(t, path, map[string]any{
		"mcpServers": map[string]any{"acme": map[string]any{"command": "acme-server", "args": []any{"--stdio"}}},
	})
	before := readFile(t, path)

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if after := readFile(t, path); after != before {
		t.Fatalf("foreign .mcp.json was rewritten:\n%s\n---\n%s", before, after)
	}
}

// TestWritesNothingUnderClaudeOrPrompts pins two surfaces that look
// plausible and are dead: the CLI stopped reading ~/.claude/* in
// v1.0.36, and it has never supported .github/prompts prompt files.
func TestWritesNothingUnderClaudeOrPrompts(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(env.Home, ".claude"),
		filepath.Join(env.Root, ".claude"),
		filepath.Join(env.Root, ".github", "prompts"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s must not be written by the Copilot CLI adapter (stat err = %v)", dir, err)
		}
	}
}

func TestGlobalModeWritesUserInstructions(t *testing.T) {
	env, _ := newCopilotEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body := readFile(t, GlobalInstructionsPath(env))
	if !strings.Contains(body, agents.GlobalRulesStartMarker) || !strings.Contains(body, agents.GlobalRulesEndMarker) {
		t.Fatalf("rule block missing from %s:\n%s", GlobalInstructionsPath(env), body)
	}
	if strings.Contains(body, agents.CommunitiesStartMarker) {
		t.Fatal("global instructions must carry the rules block, not the communities block")
	}
	// The CLI reads plain markdown; a Claude-style @-include would be
	// prose to it, so the profile body has to be inlined.
	if strings.Contains(body, "@"+agents.InstructionsDir(env)) {
		t.Fatal("global instructions must inline the profile body, not @-include it")
	}
	agentstest.AssertIdempotent(t, a, env)
}

// TestPlanEnumeratesEveryAppliedFile keeps `gortex doctor` /
// --print-config honest: they read Plan, not Apply.
func TestPlanEnumeratesEveryAppliedFile(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if err := os.WriteFile(filepath.Join(env.Root, ".mcp.json"), []byte(claudecodeMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planned := make(map[string]agents.ActionKind, len(plan.Files))
	for _, f := range plan.Files {
		planned[f.Path] = f.Action
	}

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, f := range res.Files {
		if _, ok := planned[f.Path]; !ok {
			t.Errorf("Apply touched %s but Plan did not list it", f.Path)
		}
	}
	if _, ok := planned[filepath.Join(env.Root, ".mcp.json")]; !ok {
		t.Error("Plan must list the repo .mcp.json it reconciles")
	}
}

func TestCopilotConfigHomeHonoursOverrideOnlyForRealHome(t *testing.T) {
	override := t.TempDir()
	t.Setenv(copilotHomeEnvVar, override)

	real, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no resolvable user home on this machine")
	}
	if got := copilotConfigHome(agents.Env{Home: real}); got != override {
		t.Fatalf("COPILOT_HOME ignored for the real home: got %q, want %q", got, override)
	}

	sandbox := t.TempDir()
	want := filepath.Join(sandbox, copilotConfigDirName)
	if got := copilotConfigHome(agents.Env{Home: sandbox}); got != want {
		t.Fatalf("sandbox Env escaped its root: got %q, want %q", got, want)
	}
	if got := copilotConfigHome(agents.Env{}); got != "" {
		t.Fatalf("empty Home must yield no path, got %q", got)
	}
}

func TestShellDefault(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"${GORTEX_WORKERS:-8}", "8", true},
		{"8", "", false},
		{"${GORTEX_WORKERS}", "", false},
		{"${GORTEX_WORKERS:-${NESTED}}", "", false},
		{"prefix${A:-1}", "", false},
	}
	for _, tc := range cases {
		got, ok := shellDefault(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("shellDefault(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}
