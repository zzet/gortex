package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/testenv"
)

// A brew-style binary swap: the OpenCode bridge an older release installed
// with --hook-mode=enrich is rewritten once by the new binary, posture kept,
// and a second start with the same binary touches nothing.
func TestAgentRefreshOnVersionChange(t *testing.T) {
	dirs := testenv.Sandbox(t) // HOME alone is a no-op on Windows
	home := dirs.Home
	plugin := opencode.PluginPath(home)
	manifest := agentRefreshManifestPath()

	// oldInstall leaves what an older release left: its bridge, and either
	// no manifest or one it stamped with its own version.
	oldInstall := func(record bool) {
		t.Helper()
		env := agents.Env{Home: home, Mode: agents.ModeGlobal, InstallHooks: true, HookMode: "enrich",
			HookCommand: "/old/bin/gortex hook"}
		if _, err := opencode.New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(manifest)
		if record {
			body, _ := os.ReadFile(plugin)
			writeJSON(t, manifest, agents.GeneratedManifest{Version: "v0.0.1",
				Files: map[string]string{plugin: agents.GeneratedHash(body)}})
		}
	}
	run := func() (rewritten int) {
		t.Helper()
		refreshAgentFilesIfVersionChanged(agentRefreshReport{
			entry: func(e agents.RefreshEntry) {
				if e.Status == agents.RefreshRewritten {
					rewritten++
				}
			},
			fail: func(err error) { t.Fatal(err) },
		})
		return rewritten
	}
	refreshed := func() bool {
		body, _ := os.ReadFile(plugin)
		if !strings.Contains(string(body), `"--mode=enrich"`) {
			t.Fatalf("bridge lost enrich:\n%s", body)
		}
		return !strings.Contains(string(body), "/old/bin/gortex")
	}

	oldInstall(false)
	if got := run(); got != 1 || !refreshed() {
		t.Fatalf("first run rewrote %d file(s), want the bridge once", got)
	}
	before, _ := os.Stat(plugin)
	if got := run(); got != 0 {
		t.Fatalf("second run with the same binary rewrote %d file(s)", got)
	}
	if after, _ := os.Stat(plugin); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("second run touched the bridge")
	}

	// A partial install by the new binary (here: nothing it owns) must not
	// stamp the version over a bridge an older release recorded.
	oldInstall(true)
	if err := recordAgentFiles(agents.Env{Home: home, Mode: agents.ModeGlobal, BinaryVersion: canonicalVersion()}); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != 1 || !refreshed() {
		t.Fatalf("after a partial install the stale bridge was not refreshed (%d)", got)
	}

	// A corrupt manifest counts as none; the shipped bridge is still refreshed.
	oldInstall(false)
	if err := os.WriteFile(manifest, []byte(`{"version":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != 1 || !refreshed() {
		t.Fatalf("corrupt manifest blocked the refresh (%d)", got)
	}

	// Opt-out: the file stays as the old binary wrote it.
	oldInstall(true)
	cfg := filepath.Join(dirs.Config, "gortex", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("agents:\n  auto_refresh: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != 0 || refreshed() {
		t.Fatalf("auto_refresh: false still rewrote %d file(s)", got)
	}

	// --no-hooks: no bridge on disk, and the refresh does not create one.
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	oldInstall(true)
	if err := os.Remove(plugin); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := os.Stat(plugin); !os.IsNotExist(err) {
		t.Fatalf("refresh created a bridge the user opted out of: %v", err)
	}
}

// Codex hooks from an older binary are reported by the refresh and by
// doctor, with the re-approval notice, and the config is left as it was.
func TestAgentRefreshReportsStaleCodexHooks(t *testing.T) {
	home := testenv.Sandbox(t).Home
	_, cfg := codex.UserHookFiles(home)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agents.Env{Home: home, Mode: agents.ModeGlobal, InstallHooks: true, HookCommand: "/old/bin/gortex hook"}
	if _, err := codex.InstallHooksOnly(io.Discard, cfg, env, agents.ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfg)

	var reported []agents.RefreshEntry
	refreshAgentFilesIfVersionChanged(agentRefreshReport{
		entry: func(e agents.RefreshEntry) { reported = append(reported, e) },
		fail:  func(err error) { t.Fatal(err) },
	})
	if len(reported) != 1 || reported[0].Agent != "codex" || reported[0].Status != agents.RefreshStale || reported[0].Note == "" {
		t.Fatalf("refresh reported %+v, want stale Codex hooks with the trust notice", reported)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Fatal("the refresh rewrote Codex's config.toml")
	}
	found := false
	for _, f := range doctorGeneratedFiles() {
		found = found || (f.Agent == "codex" && f.Note != "" && f.Refresh == "gortex install --agents=codex")
	}
	if !found {
		t.Fatalf("doctor does not report the stale Codex hooks: %+v", doctorGeneratedFiles())
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
