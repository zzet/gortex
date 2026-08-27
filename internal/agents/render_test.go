package agents

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderStubAdapter is a minimal Adapter that records every Env the
// render sandbox hands it and drops one file into each sandbox dir, so
// tests can assert on both the Env the fence supplies and the manifest
// key scheme it produces without depending on a real adapter (which
// this package cannot import — every adapter imports it).
type renderStubAdapter struct {
	envs    []Env
	inApply func(env Env)
}

func (a *renderStubAdapter) Name() string    { return "render-stub" }
func (a *renderStubAdapter) DocsURL() string { return "https://example.invalid/render-stub" }

func (a *renderStubAdapter) Detect(Env) (bool, error) { return false, nil }

func (a *renderStubAdapter) Plan(Env) (*Plan, error) { return &Plan{}, nil }

func (a *renderStubAdapter) Apply(env Env, opts ApplyOpts) (*Result, error) {
	a.envs = append(a.envs, env)
	if a.inApply != nil {
		a.inApply(env)
	}
	if err := os.WriteFile(filepath.Join(env.Root, "stub-root.txt"), []byte("root\n"), 0o644); err != nil {
		return nil, err
	}
	homeDir := filepath.Join(env.Home, ".stub")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(homeDir, "stub-home.txt"), []byte("home\n"), 0o644); err != nil {
		return nil, err
	}
	return &Result{Name: a.Name(), Detected: true, Configured: true}, nil
}

// TestRenderCoversBothModes pins the widened fence contract: every
// adapter is rendered once per install mode, with hooks on and a
// generated-skill fixture supplied, so hook stanzas, community skills
// and the ModeGlobal branch all end up in the golden.
func TestRenderCoversBothModes(t *testing.T) {
	stub := &renderStubAdapter{}
	if _, err := renderOne(stub); err != nil {
		t.Fatalf("renderOne: %v", err)
	}

	if len(stub.envs) != 2 {
		t.Fatalf("expected one Apply per install mode, got %d", len(stub.envs))
	}
	project, global := stub.envs[0], stub.envs[1]
	if project.Mode != ModeProject {
		t.Errorf("first pass should be ModeProject, got %v", project.Mode)
	}
	if global.Mode != ModeGlobal {
		t.Errorf("second pass should be ModeGlobal, got %v", global.Mode)
	}

	for i, env := range stub.envs {
		if !env.InstallHooks {
			t.Errorf("pass %d: InstallHooks must be true or no adapter renders its hook stanza", i)
		}
		if len(env.GeneratedSkills) != 2 {
			t.Errorf("pass %d: expected the two-entry generated-skill fixture, got %d", i, len(env.GeneratedSkills))
		}
		if env.SkillsRouting == "" {
			t.Errorf("pass %d: SkillsRouting must be seeded", i)
		}
		// InstructionsDir must live inside the sandbox home; empty would
		// fall back to profiles.DefaultDir() (the real ~/.gortex).
		if env.InstructionsDir == "" {
			t.Errorf("pass %d: InstructionsDir must be pinned into the sandbox", i)
		} else if !strings.HasPrefix(env.InstructionsDir, env.Home+string(os.PathSeparator)) {
			t.Errorf("pass %d: InstructionsDir %q escapes sandbox home %q", i, env.InstructionsDir, env.Home)
		}
	}

	if project.InstallGlobalInstructions {
		t.Error("InstallGlobalInstructions is a ModeGlobal-only switch; it must be off in the project pass")
	}
	if !global.InstallGlobalInstructions {
		t.Error("the global pass must set InstallGlobalInstructions or the user-level rule block never renders")
	}
	// Env.HookMode is honoured only by the claudecode adapter; the fence
	// leaves it empty so no golden can be mistaken for pinning a
	// non-Claude hook posture.
	if global.HookMode != "" || project.HookMode != "" {
		t.Error("HookMode must stay empty: only claudecode reads it, so setting it would pin nothing for other adapters")
	}

	// Each pass must get its own sandbox, otherwise the project pass's
	// files show up again in the global pass's manifest section.
	if project.Home == global.Home || project.Root == global.Root {
		t.Error("each mode pass needs its own sandbox home/root")
	}
}

// TestRenderGeneratedSkillFixtureIsAValidSlug guards the property the
// OpenCode and Copilot adapters depend on: they refuse to materialise a
// skill whose directory name is not a lowercase kebab slug, so the
// fixture must be one or the fence proves nothing about them.
func TestRenderGeneratedSkillFixtureIsAValidSlug(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	seen := map[string]bool{}
	skills := renderGeneratedSkills()
	if len(skills) < 2 {
		t.Fatalf("fixture should carry at least two skills, got %d", len(skills))
	}
	for _, s := range skills {
		if !slug.MatchString(s.DirName) {
			t.Errorf("DirName %q is not a valid skill slug", s.DirName)
		}
		if s.Content == "" || s.CommunityID == "" || s.Label == "" {
			t.Errorf("skill %q has an empty field: %+v", s.DirName, s)
		}
		if seen[s.DirName] {
			t.Errorf("duplicate DirName %q", s.DirName)
		}
		seen[s.DirName] = true
	}
}

// TestRenderManifestKeysAreModePrefixed pins the key scheme: every key
// is `<mode>/<home|root>/<path>`, and the project block is emitted
// before the global one so a change confined to one mode produces a
// diff hunk confined to that block.
func TestRenderManifestKeysAreModePrefixed(t *testing.T) {
	got, err := renderOne(&renderStubAdapter{})
	if err != nil {
		t.Fatalf("renderOne: %v", err)
	}

	want := []string{
		"=== project/root/stub-root.txt ===",
		"=== project/home/.stub/stub-home.txt ===",
		"=== global/root/stub-root.txt ===",
		"=== global/home/.stub/stub-home.txt ===",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("manifest missing %q\n---\n%s", w, got)
		}
	}
	if p, g := strings.Index(got, "=== project/"), strings.Index(got, "=== global/"); p < 0 || g < 0 || p > g {
		t.Errorf("project block must be emitted before the global block (project=%d global=%d)", p, g)
	}
}

// TestRenderPinsHostileEnvironment is the hermeticity + determinism
// fence for the fence itself. Every variable below is one a developer
// may legitimately have exported; each one, unpinned, either relocates
// a ModeGlobal write out of the sandbox (so the manifest's home/
// section renders empty and the escape hides itself) or bakes a local
// posture into the goldens.
func TestRenderPinsHostileEnvironment(t *testing.T) {
	hostile := t.TempDir()
	t.Setenv("HOME", hostile)
	t.Setenv("USERPROFILE", hostile)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(hostile, "escaped-claude"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(hostile, "escaped-kimi"))
	t.Setenv("GORTEX_CODEX_HOOK_MODE", "suppress")
	t.Setenv("GORTEX_INSTRUCTIONS_PROFILE", "full")
	t.Setenv("XDG_DATA_HOME", filepath.Join(hostile, "escaped-xdg"))

	stub := &renderStubAdapter{}
	stub.inApply = func(env Env) {
		if got := os.Getenv("HOME"); got != env.Home {
			t.Errorf("HOME = %q during render, want the sandbox home %q", got, env.Home)
		}
		if got, err := os.UserHomeDir(); err != nil || got != env.Home {
			t.Errorf("os.UserHomeDir() = %q (err %v) during render, want %q", got, err, env.Home)
		}
		for _, k := range []string{
			// Cleared rather than repointed: both also relocate files
			// (CLAUDE_CONFIG_DIR moves ~/.claude.json), so unsetting is
			// what puts every adapter on its real default path inside
			// the sandbox home.
			"CLAUDE_CONFIG_DIR",
			"KIMI_CODE_HOME",
			"GORTEX_CODEX_HOOK_MODE",
			"GORTEX_INSTRUCTIONS_PROFILE",
			"XDG_DATA_HOME",
		} {
			if v, ok := os.LookupEnv(k); ok {
				t.Errorf("%s must be unset during render, got %q", k, v)
			}
		}
	}
	if _, err := renderOne(stub); err != nil {
		t.Fatalf("renderOne: %v", err)
	}

	// Restored exactly, including the sandbox HOME t.Setenv installed.
	for k, want := range map[string]string{
		"HOME":                        hostile,
		"CLAUDE_CONFIG_DIR":           filepath.Join(hostile, "escaped-claude"),
		"KIMI_CODE_HOME":              filepath.Join(hostile, "escaped-kimi"),
		"GORTEX_CODEX_HOOK_MODE":      "suppress",
		"GORTEX_INSTRUCTIONS_PROFILE": "full",
		"XDG_DATA_HOME":               filepath.Join(hostile, "escaped-xdg"),
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q after render, want it restored to %q", k, got, want)
		}
	}

	// Nothing may have been created in the hostile home beyond the
	// variables' own parent dir, which we never wrote to.
	entries, err := os.ReadDir(hostile)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("render escaped the sandbox and wrote into the ambient home: %v", names)
	}
}

// TestRenderRestoresUnsetEnvironment covers the other half of the
// save/restore contract: a variable that was not set before the render
// must not exist afterwards.
func TestRenderRestoresUnsetEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}
	if _, err := renderOne(&renderStubAdapter{}); err != nil {
		t.Fatalf("renderOne: %v", err)
	}
	if v, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok {
		t.Errorf("CLAUDE_CONFIG_DIR leaked out of the render as %q; it was unset before", v)
	}
}

// TestNormalizeRenderScrubsTheSlashSpelledPath binds the second
// replacement in normalizeRender, which nothing else can.
//
// A rendered manifest carries '/'-spelled paths even on Windows —
// shellSafeHookBinary normalises unconditionally, and an @-include is
// document content rather than a filesystem call — while the values handed
// to normalizeRender are native. Scrubbing only the native spelling
// therefore leaves a machine-specific absolute in the manifest, and
// TestAgentsRenderGolden catches it only in cmd/gortex, which the Windows
// job does not test.
//
// On linux/macos both spellings are the same string and this passes with
// either implementation; on Windows it fails without the ToSlash pass.
// That asymmetry is the point, and it is why the assertion lives in
// internal/agents, which the Windows job already runs.
func TestNormalizeRenderScrubsTheSlashSpelledPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator)+"sandbox", "home")
	root := filepath.Join(string(filepath.Separator)+"sandbox", "repo")
	exe := filepath.Join(string(filepath.Separator)+"tools", "bin", "gortex")

	// The binary path is the third thing normalizeRender scrubs, and the
	// one an adapter is most likely to have written through
	// shellSafeHookBinary. Pinning gortexBinaryPaths keeps the assertion
	// about the scrub rather than about whatever gortex happens to be
	// installed on the machine running the test. No test in this package
	// calls t.Parallel, so the swap cannot race one.
	restore := gortexBinaryPaths
	t.Cleanup(func() { gortexBinaryPaths = restore })
	gortexBinaryPaths = func() []string { return []string{exe} }

	// The body is what a renderer emits: forward slashes, whatever the OS.
	body := "@" + filepath.ToSlash(home) + "/.gortex/instructions/active.md\n" +
		"root=" + filepath.ToSlash(root) + "\n" +
		`"command": "` + filepath.ToSlash(exe) + ` hook"` + "\n"

	got := normalizeRender(body, home, root)

	if !strings.Contains(got, "@$HOME/.gortex/instructions/active.md") {
		t.Errorf("the slash-spelled home survived normalizeRender:\n%s", got)
	}
	if !strings.Contains(got, "root=$ROOT") {
		t.Errorf("the slash-spelled root survived normalizeRender:\n%s", got)
	}
	if !strings.Contains(got, `"command": "gortex hook"`) {
		t.Errorf("the slash-spelled executable survived normalizeRender:\n%s", got)
	}
	for _, leaked := range []string{
		filepath.ToSlash(home), filepath.ToSlash(root), filepath.ToSlash(exe),
	} {
		if strings.Contains(got, leaked) {
			t.Errorf("machine-specific absolute %q is still in the manifest:\n%s", leaked, got)
		}
	}
}
