package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// legacyBody is the head of a rendered index.ts, captured verbatim from
// the template the pre-package adapter embedded. It must never be built
// out of legacyMarker: the marker is what gates every deletion, and a
// fixture derived from it passes for any value, including one that no
// longer matches the bytes that shipped.
func legacyBody(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "legacy-extension-head.ts"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The sweep is only as good as the marker, and nothing else in the tree
// holds the rendered header now that the template is gone.
func TestLegacyMarkerMatchesTheRenderedHeader(t *testing.T) {
	body := legacyBody(t)
	if !strings.Contains(body, legacyMarker) {
		t.Fatalf("legacyMarker %q is absent from the header the old adapter rendered:\n%s",
			legacyMarker, body)
	}
}

// writeLegacyExtension seeds a rendered index.ts at an extensions root
// and returns its path.
func writeLegacyExtension(t *testing.T, extensionsRoot, body string) string {
	t.Helper()
	path := LegacyExtensionPath(extensionsRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Pi auto-discovers `.pi/extensions/*/index.ts` in both scopes, so a
// rendered copy left behind would run a second bridge beside the package.
func TestPiRemovesLegacyExtensionInBothScopes(t *testing.T) {
	env := defaultPostureEnv(t)
	project := writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"), legacyBody(t))
	global := writeLegacyExtension(t, filepath.Join(AgentDir(env.Home), "extensions"), legacyBody(t))

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, path := range []string{project, global} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, stat err = %v", path, err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Errorf("expected the emptied %s pruned", filepath.Dir(path))
		}
	}

	deletes := 0
	for _, f := range res.Files {
		if f.Action == agents.ActionDelete {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("expected 2 delete actions, got %d (%+v)", deletes, res.Files)
	}
}

// A global-mode run sweeps the project copy too. That is the case a
// project-scoped sweep alone would miss in reverse: whichever mode runs,
// both scopes get clean.
func TestPiRemovesProjectLegacyExtensionInGlobalMode(t *testing.T) {
	env := defaultPostureEnv(t)
	env.Mode = agents.ModeGlobal
	project := writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"), legacyBody(t))

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(project); !os.IsNotExist(err) {
		t.Errorf("expected %s removed, stat err = %v", project, err)
	}
}

// Removal is gated on the rendered header, not the path, so an extension
// somebody else wrote under the same name survives.
func TestPiKeepsForeignExtensionAtTheSamePath(t *testing.T) {
	env := defaultPostureEnv(t)
	path := writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"),
		"// somebody else's gortex extension\nexport default {};\n")

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a foreign extension must be kept, stat err = %v", err)
	}
	if ownsLegacyExtension(path) {
		t.Errorf("ownsLegacyExtension must not claim a file without the header")
	}
}

// A directory holding something else survives losing our index.ts: the
// prune is an os.Remove, which refuses a non-empty directory.
func TestPiKeepsExtensionDirWithOtherContents(t *testing.T) {
	env := defaultPostureEnv(t)
	path := writeLegacyExtension(t, filepath.Join(env.Root, ".pi", "extensions"), legacyBody(t))
	sibling := filepath.Join(filepath.Dir(path), "notes.md")
	if err := os.WriteFile(sibling, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s removed, stat err = %v", path, err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("a file beside ours must keep its directory alive: %v", err)
	}
}

func TestLegacyExtensionPaths(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")

	t.Run("both scopes", func(t *testing.T) {
		paths := legacyExtensionPaths(agents.Env{Root: "/repo", Home: "/home/someone"})
		want := []string{
			filepath.Join("/repo", ".pi", "extensions", "gortex", "index.ts"),
			filepath.Join("/home/someone", ".pi", "agent", "extensions", "gortex", "index.ts"),
		}
		if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
			t.Errorf("paths = %v, want %v", paths, want)
		}
	})

	t.Run("no root", func(t *testing.T) {
		paths := legacyExtensionPaths(agents.Env{Home: "/home/someone"})
		if len(paths) != 1 {
			t.Errorf("paths = %v, want the global path alone", paths)
		}
	})

	t.Run("no home", func(t *testing.T) {
		paths := legacyExtensionPaths(agents.Env{Root: "/repo"})
		if len(paths) != 1 {
			t.Errorf("paths = %v, want the project path alone", paths)
		}
	})

	t.Run("neither", func(t *testing.T) {
		if paths := legacyExtensionPaths(agents.Env{}); paths != nil {
			t.Errorf("paths = %v, want none", paths)
		}
	})

	// The adapter that wrote these files joined <home>/.pi/agent
	// unconditionally, so an override has to widen the sweep rather than
	// move it.
	t.Run("agent dir override", func(t *testing.T) {
		t.Setenv("PI_CODING_AGENT_DIR", filepath.Join("/elsewhere", "pi"))
		paths := legacyExtensionPaths(agents.Env{Home: "/home/someone"})
		want := []string{
			filepath.Join("/elsewhere", "pi", "extensions", "gortex", "index.ts"),
			filepath.Join("/home/someone", ".pi", "agent", "extensions", "gortex", "index.ts"),
		}
		if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
			t.Errorf("paths = %v, want %v", paths, want)
		}
	})
}

// An override set after an install leaves the rendered copy under the
// default agent dir, which is the one place Pi's own resolution no
// longer looks and the sweep has to.
func TestPiSweepsLegacyExtensionUnderTheDefaultAgentDir(t *testing.T) {
	env := defaultPostureEnv(t)
	stale := writeLegacyExtension(t,
		filepath.Join(env.Home, ".pi", "agent", "extensions"), legacyBody(t))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(env.Home, "custom-agent-dir"))

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("expected %s removed, stat err = %v", stale, err)
	}
}
