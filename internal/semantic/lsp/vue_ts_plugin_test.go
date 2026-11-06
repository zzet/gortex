package lsp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func stubVueServerOnPath(t *testing.T, bin string) {
	t.Helper()
	prev := lookPath
	lookPath = func(cmd string) (string, error) {
		if cmd == "vue-language-server" && bin != "" {
			return bin, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = prev })
}

func withPlugin(t *testing.T, location string) {
	t.Helper()
	touch(t, location, "node_modules", "@vue", "typescript-plugin", "package.json")
}

func withProjectTS(t *testing.T, root string) string {
	t.Helper()
	return touch(t, root, "node_modules", "typescript", "lib", "tsserver.js")
}

func TestVueTSPluginLocation(t *testing.T) {
	t.Run("project install wins", func(t *testing.T) {
		root, global := t.TempDir(), t.TempDir()
		withPlugin(t, root)
		withPlugin(t, global)
		stubVueServerOnPath(t, touch(t, global, "bin", "vue-language-server.js"))
		if got := vueTSPluginLocation(root); got != root {
			t.Fatalf("location = %q, want the repo root %q", got, root)
		}
	})
	t.Run("nested under the project language server", func(t *testing.T) {
		root := t.TempDir()
		want := filepath.Join(root, "node_modules", "@vue", "language-server")
		withPlugin(t, want)
		stubVueServerOnPath(t, "")
		if got := vueTSPluginLocation(root); got != want {
			t.Fatalf("location = %q, want %q", got, want)
		}
	})
	t.Run("next to the server on PATH, through its symlink", func(t *testing.T) {
		root, pkgDir, binDir := t.TempDir(), t.TempDir(), t.TempDir()
		withPlugin(t, pkgDir)
		link := filepath.Join(binDir, "vue-language-server")
		if err := os.Symlink(touch(t, pkgDir, "bin", "vue-language-server.js"), link); err != nil {
			t.Fatal(err)
		}
		stubVueServerOnPath(t, link)
		want, err := filepath.EvalSymlinks(pkgDir)
		if err != nil {
			t.Fatal(err)
		}
		if got := vueTSPluginLocation(root); got != want {
			t.Fatalf("location = %q, want the package root %q", got, want)
		}
	})
	t.Run("nowhere", func(t *testing.T) {
		stubVueServerOnPath(t, "")
		if got := vueTSPluginLocation(t.TempDir()); got != "" {
			t.Fatalf("location = %q, want empty", got)
		}
	})
}

func TestVueWorkspaceGate(t *testing.T) {
	stubVueServerOnPath(t, "")
	t.Run("no plugin names the plugin", func(t *testing.T) {
		root := t.TempDir()
		withProjectTS(t, root)
		err := vueWorkspaceGate(root)
		if err == nil || !strings.Contains(err.Error(), "@vue/typescript-plugin") {
			t.Fatalf("err = %v, want one naming the missing plugin", err)
		}
	})
	t.Run("no project typescript says so", func(t *testing.T) {
		root := t.TempDir()
		withPlugin(t, root)
		err := vueWorkspaceGate(root)
		if err == nil || !strings.Contains(err.Error(), "tsserver") {
			t.Fatalf("err = %v, want one naming the missing tsserver", err)
		}
	})
	t.Run("ready", func(t *testing.T) {
		root := t.TempDir()
		withPlugin(t, root)
		withProjectTS(t, root)
		if err := vueWorkspaceGate(root); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
}

func TestVueInitOptions(t *testing.T) {
	stubVueServerOnPath(t, "")
	root := t.TempDir()
	if got := vueInitOptions(root); got != nil {
		t.Fatalf("options without a plugin = %s, want nil", got)
	}
	withPlugin(t, root)
	tsserver := withProjectTS(t, root)

	var got struct {
		Plugins []struct {
			Name      string   `json:"name"`
			Location  string   `json:"location"`
			Languages []string `json:"languages"`
		} `json:"plugins"`
		TSServer struct {
			Path string `json:"path"`
		} `json:"tsserver"`
	}
	if err := json.Unmarshal(vueInitOptions(root), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "@vue/typescript-plugin" ||
		got.Plugins[0].Location != root || len(got.Plugins[0].Languages) != 1 || got.Plugins[0].Languages[0] != "vue" {
		t.Fatalf("plugins = %+v", got.Plugins)
	}
	if got.TSServer.Path != tsserver {
		t.Fatalf("tsserver path = %q, want %q", got.TSServer.Path, tsserver)
	}
}

// The spec is on-demand: an internal language would pull every .vue file into
// the eager enrichment sweep, and the .ts routing must not move.
func TestVueSpecIsOnDemandAndLeavesTSRoutingAlone(t *testing.T) {
	spec := SpecByName("vue-typescript")
	if spec == nil {
		t.Fatal("vue-typescript spec is not registered")
	}
	if len(spec.Languages) != 0 {
		t.Fatalf("Languages = %v, want none", spec.Languages)
	}
	if spec.WorkspaceGate == nil || spec.InitOptionsFunc == nil {
		t.Fatal("spec needs its workspace gate and init options")
	}
	if got := SpecsForExtension(".vue"); len(got) != 1 || got[0] != spec {
		t.Fatalf(".vue routes to %v", got)
	}
	if id := LanguageIDForPath("a/b.vue"); id != "vue" {
		t.Fatalf("languageId = %q, want vue", id)
	}
	for _, s := range SpecsForExtension(".ts") {
		if s == spec {
			t.Fatal(".ts must not route to the Vue spec")
		}
	}
}

func TestEffectiveInitOptionsConsultsSpecFunc(t *testing.T) {
	spec := &ServerSpec{
		InitializationOptions: json.RawMessage(`{"static":true}`),
		InitOptionsFunc: func(root string) json.RawMessage {
			return json.RawMessage(`{"root":"` + root + `"}`)
		},
	}
	p := &Provider{spec: spec}
	if got := string(p.effectiveInitOptions("/w")); got != `{"root":"/w"}` {
		t.Fatalf("options = %s", got)
	}
	spec.InitOptionsFunc = func(string) json.RawMessage { return nil }
	if got := string(p.effectiveInitOptions("/w")); got != `{"static":true}` {
		t.Fatalf("empty func result must fall back, got %s", got)
	}
}
