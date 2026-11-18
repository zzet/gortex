package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/testenv"
)

// TestMain ensures hook tests never write telemetry to the user's real
// ~/.cache/gortex/hook-decisions.jsonl. Tests that want to inspect the
// log redirect it again via t.Setenv to a per-test tmp file.
func TestMain(m *testing.M) {
	// Sandbox the whole process first so every daemon touchpoint — daemon.Dial,
	// daemon.IsRunning, and the production probes behind the seams below —
	// resolves GORTEX_DAEMON_SOCKET to a scratch path nobody listens on.
	// Without it a developer's running daemon answered any probe a test forgot
	// to stub, flipping verdicts and replaying real index content in failure
	// output (#595). Tests that want a daemon start a fake one and t.Setenv the
	// socket, which unwinds to this sandbox rather than to the real one.
	restore, err := testenv.SandboxProcess()
	if err != nil {
		fmt.Fprintf(os.Stderr, "internal/hooks: cannot sandbox the test environment: %v\n", err)
		os.Exit(1)
	}
	// Pin the instruction profile so a developer machine that ran
	// `gortex instructions switch` cannot change hook-tier behavior
	// in unrelated tests; tier tests stub activeHookTier directly.
	_ = os.Setenv(profiles.ActiveEnv, profiles.DefaultName)
	dir, err := os.MkdirTemp("", "gortex-hooks-test")
	if err == nil {
		_ = os.Setenv("GORTEX_HOOK_LOG", filepath.Join(dir, "hook-decisions.jsonl"))
		_ = os.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", filepath.Join(dir, "hook-effectiveness.jsonl"))
	}
	// Per-call enforcement is gated on daemon reachability (#486). Default
	// it to "reachable" so enforcement tests stay deterministic on machines
	// without a daemon (and never dial a real socket); daemon-outage tests
	// stub it false via withDaemonReachable.
	daemonReachableFn = func() bool { return true }
	// Default the file-indexed / file-summary probes to "tracked, indexable,
	// not indexed yet" so no test dials a real daemon. That verdict rather
	// than the zero value: an abstention silences the read doors, and a test
	// that has not opted into one should still see the advisory. Tests
	// needing another verdict stub fileIndexScopeFn / fileSummaryFn
	// (stubFileIndexScope / fakeIndexedBridge / newIndexedBridge /
	// stubBridge) and restore these defaults on cleanup.
	fileIndexScopeFn = func(_, _ string, _ time.Duration) fileIndexStatus {
		return fileIndexStatus{Tracked: true, ProbeOK: true}
	}
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) { return nil, false }
	callServerToolDaemonFn = func(string, string, map[string]any) string { return "" }
	// Same reason: attribution resolves the diffed repo through the daemon's
	// tracked-repo list. Tests that want a resolved repo stub this.
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return nil }
	// The symbol probes default to "no daemon" explicitly as well, so the
	// soft fallbacks they feed never depend on the sandbox alone. Tests that
	// want hits stub them (stubProbe and friends) and restore on cleanup.
	noDaemon := func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return nil, errDaemonUnreachable
	}
	grepProbe = noDaemon
	userPromptProbe = noDaemon

	// os.Exit skips deferred calls, so clean up explicitly.
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	restore()
	os.Exit(code)
}

// TestMainSandboxesTheDaemonSocket pins the guard above: whatever daemon the
// developer has running, an unstubbed probe from this package must read as
// "no daemon" rather than reach it.
func TestMainSandboxesTheDaemonSocket(t *testing.T) {
	if daemon.IsRunning() {
		t.Fatalf("hooks tests reached a live daemon at %s; TestMain must sandbox GORTEX_DAEMON_SOCKET", daemon.SocketPath())
	}
	if _, err := probeViaDaemon("TestMainSandboxesTheDaemonSocket", "", 500*time.Millisecond); err != errDaemonUnreachable {
		t.Fatalf("probeViaDaemon err = %v, want errDaemonUnreachable", err)
	}
}
