package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/profiles"
)

// TestMain ensures hook tests never write telemetry to the user's real
// ~/.cache/gortex/hook-decisions.jsonl. Tests that want to inspect the
// log redirect it again via t.Setenv to a per-test tmp file.
func TestMain(m *testing.M) {
	// Pin the instruction profile so a developer machine that ran
	// `gortex instructions switch` cannot change hook-tier behavior
	// in unrelated tests; tier tests stub activeHookTier directly.
	_ = os.Setenv(profiles.ActiveEnv, profiles.DefaultName)
	dir, err := os.MkdirTemp("", "gortex-hooks-test")
	if err == nil {
		_ = os.Setenv("GORTEX_HOOK_LOG", filepath.Join(dir, "hook-decisions.jsonl"))
		_ = os.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", filepath.Join(dir, "hook-effectiveness.jsonl"))
		defer func() { _ = os.RemoveAll(dir) }()
	}
	// Per-call enforcement is gated on daemon reachability (#486). Default
	// it to "reachable" so enforcement tests stay deterministic on machines
	// without a daemon (and never dial a real socket); daemon-outage tests
	// stub it false via withDaemonReachable.
	daemonReachableFn = func() bool { return true }
	// Default the file-indexed / file-summary probes to "not indexed" so no
	// test dials a real daemon. Tests needing an indexed verdict stub
	// fileIndexedFn / fileSummaryFn (fakeIndexedBridge / newIndexedBridge /
	// stubBridge) and restore these defaults on cleanup.
	fileIndexedFn = func(_, _ string) (bool, int) { return false, 0 }
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) { return nil, false }
	callServerToolDaemonFn = func(string, string, map[string]any) string { return "" }
	// Same reason: attribution resolves the diffed repo through the daemon's
	// tracked-repo list. Tests that want a resolved repo stub this.
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return nil }
	os.Exit(m.Run())
}
