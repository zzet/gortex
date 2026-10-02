package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
)

// updateAgentRender regenerates the committed agent-render goldens.
// Run: go test ./cmd/gortex -run TestAgentsRenderGolden -update-agent-render
var updateAgentRender = flag.Bool("update-agent-render", false, "regenerate the agent-render drift-fence goldens")

const agentRenderDir = "testdata/agent-render"

// TestAgentsRenderGolden is the all-platform skill-render drift fence:
// it renders every registered adapter and byte-compares the result
// against a committed golden. Any change to an adapter's generated MCP
// config, instructions, hooks, or routing blocks must be accompanied by
// a regenerated golden, so cross-platform drift is reviewable in the
// diff — not just for the Claude bundle.
func TestAgentsRenderGolden(t *testing.T) {
	manifests, err := agents.RenderManifest(buildRegistry().All())
	if err != nil {
		t.Fatalf("render adapters: %v", err)
	}

	if *updateAgentRender {
		if err := os.MkdirAll(agentRenderDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, got := range manifests {
			if err := os.WriteFile(filepath.Join(agentRenderDir, name+".txt"), []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("regenerated %d agent-render goldens", len(manifests))
		return
	}

	for name, got := range manifests {
		golden := filepath.Join(agentRenderDir, name+".txt")
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Errorf("%s: missing golden %s — regenerate with `go test ./cmd/gortex -run TestAgentsRenderGolden -update-agent-render`", name, golden)
			continue
		}
		if string(want) != got {
			t.Errorf("%s: rendered output drifted from %s.\nReview the change; if intended, regenerate goldens:\n  go test ./cmd/gortex -run TestAgentsRenderGolden -update-agent-render", name, golden)
		}
	}

	// A golden with no corresponding adapter means an adapter was
	// removed without dropping its golden — flag it.
	entries, err := os.ReadDir(agentRenderDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".txt")
		if name == e.Name() {
			continue // not a .txt golden
		}
		if _, ok := manifests[name]; !ok {
			t.Errorf("stale golden %s has no matching adapter — delete it or restore the adapter", e.Name())
		}
	}
}

// realHomeBeforeSandbox is the developer's actual home directory,
// captured during package-variable initialisation — which runs before
// TestMain installs the process-wide testenv sandbox that replaces
// $HOME. The render hermeticity guard needs the real path: after
// SandboxProcess, os.UserHomeDir() reports the sandbox and could not
// tell a genuine escape from a contained write.
var realHomeBeforeSandbox = func() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}()

// neutraliseClaudeConfigDirOverride clears the process-wide Claude
// config-root seam for the duration of a test. The seam outranks
// $CLAUDE_CONFIG_DIR, so a value left behind by an earlier test (or by
// a future caller of `gortex install --claude-config-dir` in the same
// process) would send the ModeGlobal render somewhere the sandbox
// cannot see — the manifest's home/ section would silently render
// empty. The render itself cannot clear it: claudecode imports
// internal/agents, so internal/agents cannot import claudecode.
func neutraliseClaudeConfigDirOverride(t *testing.T) {
	t.Helper()
	claudecode.SetConfigDirOverride("")
	t.Cleanup(func() { claudecode.SetConfigDirOverride("") })
}

// renderAllAdapters is the one-line render every guard below shares.
func renderAllAdapters(t *testing.T) map[string]string {
	t.Helper()
	manifests, err := agents.RenderManifest(buildRegistry().All())
	if err != nil {
		t.Fatalf("render adapters: %v", err)
	}
	return manifests
}

// manifestKeys extracts the `=== <key> ===` headers from a manifest.
func manifestKeys(manifest string) []string {
	var keys []string
	for _, line := range strings.Split(manifest, "\n") {
		if strings.HasPrefix(line, "=== ") && strings.HasSuffix(line, " ===") {
			keys = append(keys, strings.TrimSuffix(strings.TrimPrefix(line, "=== "), " ==="))
		}
	}
	return keys
}

// TestAgentsRenderIsHermetic proves the drift fence cannot write
// outside its sandbox. It matters because the fence now renders
// ModeGlobal, whose whole job is writing user-level files: an escape
// would corrupt the developer's real home AND hide itself, because the
// bytes that left the sandbox are exactly the ones missing from the
// manifest's home/ section.
//
// The environment below is hostile on purpose. Every variable is one a
// developer may legitimately have exported, and each one used to be a
// live escape hatch or a determinism hole.
func TestAgentsRenderIsHermetic(t *testing.T) {
	neutraliseClaudeConfigDirOverride(t)

	decoy := t.TempDir()
	claudeDecoy := filepath.Join(decoy, "claude-config-dir")
	kimiDecoy := filepath.Join(decoy, "kimi-code-home")
	piDecoy := filepath.Join(decoy, "pi-coding-agent-dir")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDecoy)
	t.Setenv("KIMI_CODE_HOME", kimiDecoy)
	t.Setenv("PI_CODING_AGENT_DIR", piDecoy)
	t.Setenv("GORTEX_CODEX_HOOK_MODE", "suppress")
	t.Setenv("GORTEX_INSTRUCTIONS_PROFILE", "full")

	manifests := renderAllAdapters(t)

	// 1. The decoy config roots must be untouched: if either had won,
	//    the adapter's user-level artifacts would have landed there.
	for _, dir := range []string{claudeDecoy, kimiDecoy, piDecoy} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("render escaped the sandbox into %s (stat err: %v) — the config-dir env pin is not holding", dir, err)
		}
	}

	// 2. No manifest may name the developer's real home. A leaked
	//    absolute path means something resolved a path outside the
	//    sandbox, whether or not it managed to write there.
	if realHomeBeforeSandbox != "" {
		for name, m := range manifests {
			if strings.Contains(m, realHomeBeforeSandbox) {
				t.Errorf("%s manifest references the real home %q", name, realHomeBeforeSandbox)
			}
		}
	}

	// 3. Nothing under the real home that the render *could* have
	//    written may have been created by this run. Every user-level
	//    manifest key is checked against its real-home counterpart, so
	//    the guard extends itself automatically as adapters grow new
	//    user-level surfaces.
	if realHomeBeforeSandbox != "" {
		checked := map[string]bool{}
		for name, m := range manifests {
			for _, key := range manifestKeys(m) {
				rel, ok := homeRelativeManifestKey(key)
				if !ok || checked[rel] {
					continue
				}
				checked[rel] = true
				path := filepath.Join(realHomeBeforeSandbox, filepath.FromSlash(rel))
				info, err := os.Lstat(path)
				if err != nil {
					continue // absent is the expected state
				}
				if info.ModTime().After(renderGuardStart) {
					t.Errorf("%s: %s was modified during the render — the sandbox leaked into the real home\n"+
						"(false positive only if another process wrote that path during this test)", name, path)
				}
			}
		}
	}

	// 4. The pinned variables are restored exactly as the test set them.
	for k, want := range map[string]string{
		"CLAUDE_CONFIG_DIR":           claudeDecoy,
		"KIMI_CODE_HOME":              kimiDecoy,
		"GORTEX_CODEX_HOOK_MODE":      "suppress",
		"GORTEX_INSTRUCTIONS_PROFILE": "full",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q after the render, want it restored to %q", k, got, want)
		}
	}
}

// renderGuardStart is captured before any test runs, so the
// hermeticity guard can distinguish "this file was already here" from
// "this render just wrote it".
var renderGuardStart = time.Now().Add(-time.Second)

// homeRelativeManifestKey maps a `<mode>/home/<rel>` manifest key to
// <rel>, and reports false for repo-root keys (which have no real-home
// counterpart).
func homeRelativeManifestKey(key string) (string, bool) {
	for _, prefix := range []string{"project/home/", "global/home/"} {
		if strings.HasPrefix(key, prefix) {
			return strings.TrimPrefix(key, prefix), true
		}
	}
	return "", false
}

// TestAgentsRenderIsDeterministic pins the fence against the ambient
// environment. Both variables below are read at render time — codex
// bakes $GORTEX_CODEX_HOOK_MODE into its hook command, and
// profiles.ActiveName honours $GORTEX_INSTRUCTIONS_PROFILE, which
// selects the user-level skill set — so without the render's env pins
// the goldens would encode whichever posture the regenerating
// developer happened to export, and CI would then fail on a diff
// nobody can explain.
func TestAgentsRenderIsDeterministic(t *testing.T) {
	neutraliseClaudeConfigDirOverride(t)

	t.Setenv("GORTEX_CODEX_HOOK_MODE", "suppress")
	t.Setenv("GORTEX_INSTRUCTIONS_PROFILE", "full")
	first := renderAllAdapters(t)

	t.Setenv("GORTEX_CODEX_HOOK_MODE", "rewrite")
	t.Setenv("GORTEX_INSTRUCTIONS_PROFILE", "localization")
	second := renderAllAdapters(t)

	for name, got := range second {
		if want, ok := first[name]; !ok {
			t.Errorf("%s rendered only once across two runs", name)
		} else if got != want {
			t.Errorf("%s manifest depends on the ambient environment: it changed when "+
				"$GORTEX_CODEX_HOOK_MODE / $GORTEX_INSTRUCTIONS_PROFILE changed", name)
		}
	}

	// And the codex hook command must carry the shipped default posture,
	// not the "suppress"/"rewrite" the environment asked for.
	codex, ok := first["codex"]
	if !ok {
		t.Fatal("no codex manifest")
	}
	if !strings.Contains(codex, "--agent=codex --mode=enrich") {
		t.Errorf("codex hook command does not carry the fixed default posture --mode=enrich")
	}
	for _, leaked := range []string{"--mode=suppress", "--mode=rewrite", "--mode=deny"} {
		if strings.Contains(codex, leaked) {
			t.Errorf("codex manifest carries %q from the ambient environment", leaked)
		}
	}
}

// TestAgentsRenderFenceCoverage is the positive counterpart to the
// goldens: it asserts the fence still *sees* the surfaces it was
// widened to cover. Without it, a change that quietly narrows the
// sandbox Env again — dropping InstallHooks, the generated-skill
// fixture, or the ModeGlobal pass — would sail through as a golden
// regeneration, which is exactly how the previous blind spot appeared.
func TestAgentsRenderFenceCoverage(t *testing.T) {
	neutraliseClaudeConfigDirOverride(t)
	manifests := renderAllAdapters(t)

	claude, ok := manifests["claude-code"]
	if !ok {
		t.Fatal("no claude-code manifest")
	}
	for _, want := range []string{
		// ModeGlobal user-level artifacts.
		"=== global/home/.claude/skills/",
		"=== global/home/.claude/CLAUDE.md ===",
		// Lifecycle hooks, both scopes.
		"=== project/root/.claude/settings.local.json ===",
		"=== global/home/.claude/settings.json ===",
		// Generated per-community skills.
		"=== project/root/.claude/skills/generated/gortex-example-one/SKILL.md ===",
		"=== project/root/.claude/skills/generated/gortex-example-two/SKILL.md ===",
	} {
		if !strings.Contains(claude, want) {
			t.Errorf("claude-code manifest lost fence coverage for %q", want)
		}
	}

	// Every adapter must render something in both modes; a mode that
	// renders nothing at all is the signature of a narrowed fence.
	var projectOnly []string
	for name, m := range manifests {
		if !strings.Contains(m, "=== project/") {
			t.Errorf("%s rendered no project-mode artifacts", name)
		}
		if !strings.Contains(m, "=== global/") {
			projectOnly = append(projectOnly, name)
		}
	}
	// Some adapters legitimately have no user-level surface, so this is
	// reported rather than failed — but every adapter that has one must
	// keep it visible.
	if len(projectOnly) == len(manifests) {
		t.Error("no adapter rendered a global-mode artifact — the ModeGlobal pass is not reaching adapters")
	}
	t.Logf("adapters with no ModeGlobal artifacts: %v", projectOnly)
}
