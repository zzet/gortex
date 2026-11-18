package codex

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/testenv"
)

// Codex hooks an older binary installed are reported, never rewritten, and
// only when install would actually change them.
func TestCodexStaleHooksAreReportedNotRewritten(t *testing.T) {
	home := testenv.Sandbox(t).Home
	_, path := UserHookFiles(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(cmd string) agents.Env {
		return agents.Env{Home: home, Mode: agents.ModeGlobal, InstallHooks: true, HookCommand: cmd}
	}
	install := func(mode string) []byte {
		t.Helper()
		t.Setenv(codexHookModeEnvVar, mode)
		if _, err := InstallHooksOnly(io.Discard, path, env("/old/gortex hook"), agents.ApplyOpts{}); err != nil {
			t.Fatal(err)
		}
		t.Setenv(codexHookModeEnvVar, "")
		body, _ := os.ReadFile(path)
		return body
	}

	before := install("deny")
	got := New().StaleConfigs(env("/new/gortex hook"))
	if len(got) != 1 || got[0].Path != path || got[0].Note != codexHookTrustNotice {
		t.Fatalf("hooks of an older binary: %+v, want config.toml with the trust notice", got)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("the stale check rewrote config.toml")
	}
	// Same binary: current, even though this process has no
	// GORTEX_CODEX_HOOK_MODE and install ran with deny.
	if got := New().StaleConfigs(env("/old/gortex hook")); len(got) != 0 {
		t.Fatalf("current hooks reported stale: %+v", got)
	}
	// No Gortex hooks at all (--no-hooks): nothing to report.
	if err := os.WriteFile(path, []byte("[features]\nhooks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := New().StaleConfigs(env("/new/gortex hook")); len(got) != 0 {
		t.Fatalf("config without Gortex hooks reported: %+v", got)
	}
}
