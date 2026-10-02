package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// defaultPostureEnv returns an env whose every posture is the package's
// own default, so the adapter has nothing to write into a sidecar.
// NewEnv's HookCommand names an off-PATH binary on purpose, which is a
// non-default; the tests that want no sidecar clear it here.
func defaultPostureEnv(t *testing.T) agents.Env {
	t.Helper()
	env, _ := agentstest.NewEnv(t)
	env.HookCommand = defaultBin + " hook"
	mkPiDir(t, env.Root)
	return env
}

func mkPiDir(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// packagesOf reads the `packages` array out of a settings file.
func packagesOf(t *testing.T, path string) []any {
	t.Helper()
	root := agentstest.ReadJSON(t, path)
	list, ok := root["packages"].([]any)
	if !ok {
		t.Fatalf("%s: packages is %T, want an array", path, root["packages"])
	}
	return list
}

func TestPiDetect(t *testing.T) {
	t.Run("project .pi dir", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		// NewEnv's Home is a fresh temp dir with no .pi, so detection
		// hinges on the project marker (PATH may or may not have pi).
		mkPiDir(t, env.Root)
		ok, err := New().Detect(env)
		if err != nil || !ok {
			t.Fatalf("expected detect=true with .pi/, got %v (err %v)", ok, err)
		}
	})

	t.Run("home .pi dir", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		if err := os.MkdirAll(filepath.Join(env.Home, ".pi"), 0o755); err != nil {
			t.Fatal(err)
		}
		ok, err := New().Detect(env)
		if err != nil || !ok {
			t.Fatalf("expected detect=true with ~/.pi/, got %v (err %v)", ok, err)
		}
	})
}

func TestPiApplyDeclaresPackageAndRouting(t *testing.T) {
	env, _ := agentstest.NewEnv(t) // NewEnv seeds SkillsRouting → routing block written.
	mkPiDir(t, env.Root)

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected || !res.Configured {
		t.Fatalf("expected detected+configured, got %+v", res)
	}

	list := packagesOf(t, filepath.Join(env.Root, ".pi", "settings.json"))
	if len(list) != 1 || list[0] != PackageSpec {
		t.Fatalf("packages = %v, want [%s]", list, PackageSpec)
	}

	// AGENTS.md carries the community-routing block, but NOT the
	// read-discipline rules: the package injects those at runtime.
	agentsMd, err := os.ReadFile(filepath.Join(env.Root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not written: %v", err)
	}
	if !strings.Contains(string(agentsMd), agents.CommunitiesStartMarker) {
		t.Errorf("AGENTS.md missing communities block")
	}
	if strings.Contains(string(agentsMd), agents.InstructionsSentinel) {
		t.Errorf("AGENTS.md must NOT carry the read-discipline rules block; the package injects them at runtime")
	}

	agentstest.AssertIdempotent(t, a, env)
}

// Without skills routing the adapter never touches AGENTS.md: the
// read-discipline rules ride the package, so there is nothing to persist
// to an instructions file.
func TestPiApplyNoSkillsLeavesAgentsMdUntouched(t *testing.T) {
	env := defaultPostureEnv(t)
	env.SkillsRouting = ""
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); err == nil {
		t.Errorf("AGENTS.md should not be written when skills routing is empty")
	}
}

// An install that changes nothing leaves no sidecar: the package applies
// the same defaults, so a file restating them is noise in a repo teams
// commit.
func TestPiDefaultPostureWritesNoSidecar(t *testing.T) {
	env := defaultPostureEnv(t)
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	sidecar := filepath.Join(env.Root, ".pi", "gortex.json")
	if _, err := os.Stat(sidecar); err == nil {
		t.Errorf("expected no sidecar for an all-default install, found %s", sidecar)
	}
	for _, f := range res.Files {
		if f.Path == sidecar {
			t.Errorf("all-default install reported an action on the sidecar: %s", f.Action)
		}
	}
}

func TestPiSidecarCarriesNonDefaults(t *testing.T) {
	env := defaultPostureEnv(t)
	env.HookMode = "enrich"
	env.InstallHooks = false

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, ".pi", "gortex.json"))
	if cfg["hook_mode"] != "enrich" {
		t.Errorf("hook_mode = %v, want enrich", cfg["hook_mode"])
	}
	if cfg["enforce"] != false {
		t.Errorf("enforce = %v, want false", cfg["enforce"])
	}
	// The deny posture rides as an absent key, and a PATH-resolvable
	// binary needs no pin.
	if _, ok := cfg["bin"]; ok {
		t.Errorf("bin should be absent when a bare %q resolves to the same binary, got %v", defaultBin, cfg["bin"])
	}
}

// An off-PATH binary is pinned, because the agent's PATH is frequently
// not the shell's.
func TestPiSidecarPinsOffPathBinary(t *testing.T) {
	env := defaultPostureEnv(t)
	env.HookCommand = filepath.Join(t.TempDir(), "gortex") + " hook"

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, ".pi", "gortex.json"))
	if cfg["bin"] != strings.Fields(env.HookCommand)[0] {
		t.Errorf("bin = %v, want %s", cfg["bin"], strings.Fields(env.HookCommand)[0])
	}
}

// Gortex owns the sidecar, so a plain re-run resets the posture rather
// than leaving a stale value behind.
func TestPiDefaultRerunRemovesStaleSidecar(t *testing.T) {
	env := defaultPostureEnv(t)
	env.HookMode = "enrich"
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	sidecar := filepath.Join(env.Root, ".pi", "gortex.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("expected a sidecar after the enrich install: %v", err)
	}

	env.HookMode = ""
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("expected the stale sidecar to be removed, stat err = %v", err)
	}
	var sawDelete bool
	for _, f := range res.Files {
		if f.Path == sidecar && f.Action == agents.ActionDelete {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Errorf("expected a delete action for %s, got %+v", sidecar, res.Files)
	}
}

// Every spelling Pi accepts for the package is recognised, and a
// deliberate one (a pin, a filter object, a local checkout) is left
// exactly as the user wrote it.
func TestPiExistingPackageEntryIsLeftAlone(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "package.json"),
		[]byte(`{"name":"pi-gortex","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		entry any
	}{
		{"bare name", packageName},
		{"npm prefix", PackageSpec},
		{"pinned version", PackageSpec + "@1.2.3"},
		{"filter object", map[string]any{"source": PackageSpec, "skills": []any{}}},
		{"local checkout", checkout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := defaultPostureEnv(t)
			settings := filepath.Join(env.Root, ".pi", "settings.json")
			agentstest.WriteJSON(t, settings, map[string]any{
				"packages": []any{tc.entry},
			})

			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			list := packagesOf(t, settings)
			if len(list) != 1 {
				t.Fatalf("packages = %v, want the single existing entry preserved", list)
			}
		})
	}
}

// A packages array that does not mention us still gets the entry, and
// every other entry survives.
func TestPiAppendsBesideOtherPackages(t *testing.T) {
	env := defaultPostureEnv(t)
	settings := filepath.Join(env.Root, ".pi", "settings.json")
	agentstest.WriteJSON(t, settings, map[string]any{
		"packages":   []any{"npm:pi-skills", "git:github.com/someone/repo@v1"},
		"npmCommand": []any{"mise", "exec", "--", "npm"},
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	list := packagesOf(t, settings)
	if len(list) != 3 || list[2] != PackageSpec {
		t.Fatalf("packages = %v, want the two originals plus %s", list, PackageSpec)
	}
	if _, ok := agentstest.ReadJSON(t, settings)["npmCommand"]; !ok {
		t.Errorf("unrelated settings keys must survive the merge")
	}
}

func TestPiGlobalMode(t *testing.T) {
	env := defaultPostureEnv(t)
	env.Mode = agents.ModeGlobal
	env.HookMode = "enrich"
	if err := os.MkdirAll(filepath.Join(env.Home, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected configured in global mode")
	}

	agent := filepath.Join(env.Home, ".pi", "agent")
	list := packagesOf(t, filepath.Join(agent, "settings.json"))
	if len(list) != 1 || list[0] != PackageSpec {
		t.Fatalf("packages = %v, want [%s]", list, PackageSpec)
	}
	// The global sidecar sits inside the agent dir's extensions folder,
	// not beside its settings.
	cfg := agentstest.ReadJSON(t, filepath.Join(agent, "extensions", "gortex.json"))
	if cfg["hook_mode"] != "enrich" {
		t.Errorf("hook_mode = %v, want enrich", cfg["hook_mode"])
	}
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); err == nil {
		t.Errorf("global mode should not write repo AGENTS.md")
	}

	agentstest.AssertIdempotent(t, a, env)
}

// The installer has to resolve the agent dir exactly as the package's
// config.ts does, or the sidecar lands where nothing reads it.
func TestPiHonoursAgentDirOverride(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", dir)
		if got := AgentDir("/home/someone"); got != dir {
			t.Errorf("AgentDir = %q, want %q", got, dir)
		}
	})

	t.Run("tilde prefix", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", "~/custom/agent")
		want := filepath.Join("/home/someone", "custom", "agent")
		if got := AgentDir("/home/someone"); got != want {
			t.Errorf("AgentDir = %q, want %q", got, want)
		}
	})

	t.Run("bare tilde", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", "~")
		if got := AgentDir("/home/someone"); got != "/home/someone" {
			t.Errorf("AgentDir = %q, want the home directory", got)
		}
	})

	t.Run("unset", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", "")
		want := filepath.Join("/home/someone", ".pi", "agent")
		if got := AgentDir("/home/someone"); got != want {
			t.Errorf("AgentDir = %q, want %q", got, want)
		}
	})

	t.Run("sidecar follows the override", func(t *testing.T) {
		env := defaultPostureEnv(t)
		env.Mode = agents.ModeGlobal
		env.HookMode = "nudge"
		dir := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", dir)

		if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
			t.Errorf("settings not written under the override: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "extensions", "gortex.json")); err != nil {
			t.Errorf("sidecar not written under the override: %v", err)
		}
	})
}

func TestPiApplyDryRun(t *testing.T) {
	env := defaultPostureEnv(t)
	env.HookMode = "enrich"
	legacy := writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"),
		legacyMarker+"\nexport default {};\n")

	res, err := New().Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply dry-run: %v", err)
	}
	for _, path := range []string{
		filepath.Join(env.Root, ".pi", "settings.json"),
		filepath.Join(env.Root, ".pi", "gortex.json"),
	} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("dry-run must not write %s", path)
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("dry-run must not delete %s: %v", legacy, err)
	}
	if len(res.Files) == 0 {
		t.Errorf("dry-run should still report planned actions")
	}
}

// Plan is what --dry-run and `gortex init doctor` render, so it has to
// predict the same set of paths Apply touches.
func TestPiPlanMatchesApply(t *testing.T) {
	env := defaultPostureEnv(t)
	env.HookMode = "enrich"
	writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"),
		legacyMarker+"\nexport default {};\n")

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
			t.Errorf("Apply touched %s, which Plan never named", f.Path)
		}
	}
	for path := range planned {
		var seen bool
		for _, f := range res.Files {
			if f.Path == path {
				seen = true
			}
		}
		if !seen {
			t.Errorf("Plan named %s, which Apply never touched", path)
		}
	}
}

// MergeJSON's fallback for a file encoding/json rejects is to back it up
// and write ours over the original. This one is the user's, and Pi may
// accept syntax this parser does not, so it is left exactly as it is.
func TestPiKeepsUnparseableSettings(t *testing.T) {
	env := defaultPostureEnv(t)
	path := settingsPath(env)
	body := "{\n  // a comment Pi's own parser may allow\n  \"packages\": []\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Files[0].Action != agents.ActionSkip {
		t.Errorf("plan must not promise a write Apply will decline: %+v", plan.Files[0])
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("settings were rewritten:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("nothing should have been backed up, stat err = %v", err)
	}
}

func TestPackageEntryName(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "package.json"),
		[]byte(`{"name":"pi-gortex"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		entry any
		want  string
	}{
		{"npm:pi-gortex", "pi-gortex"},
		{"pi-gortex", "pi-gortex"},
		{"npm:pi-gortex@1.2.3", "pi-gortex"},
		{"npm:@scope/pkg@2.0.0", "@scope/pkg"},
		{"@scope/pkg", "@scope/pkg"},
		{map[string]any{"source": "npm:pi-gortex"}, "pi-gortex"},
		{map[string]any{"source": "pi-gortex", "skills": []any{}}, "pi-gortex"},
		{checkout, "pi-gortex"},
		{"git:github.com/user/repo@v1", ""},
		{"https://github.com/user/repo", ""},
		{"/nonexistent/checkout", ""},
		{map[string]any{"extensions": []any{}}, ""},
		{"", ""},
		{42, ""},
	} {
		if got := packageEntryName(tc.entry, checkout); got != tc.want {
			t.Errorf("packageEntryName(%#v) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

func TestNormalizeMode(t *testing.T) {
	for in, want := range map[string]string{
		"enrich":          "enrich",
		"ENRICH":          "enrich",
		"consult-unlock":  "consult-unlock",
		"nudge":           "nudge",
		"adaptive-nudge":  "nudge",
		"deny":            "deny",
		"":                "deny",
		"something-else":  "deny",
		"  enrich       ": "enrich",
	} {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}
