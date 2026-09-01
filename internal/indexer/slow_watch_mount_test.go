package indexer

import (
	"runtime"
	"testing"
)

// TestSlowWatchMount_NormalMountNotDegraded guards the safe default: a
// normal local filesystem must never be flagged as a slow mount, so
// fsnotify is only disabled on a genuine slow-mount type. Runs on every
// platform, unlike TestIsSlowMountFSType (Linux-only, in
// slow_mount_test.go) which pins the statfs magic-number check itself
// against known constants and is the sturdier of the two checks.
//
// On Linux, slowWatchMount does a live statfs of the given path, so
// probing t.TempDir() here depends on TMPDIR actually sitting on local
// disk — not guaranteed on every CI runner. TestIsSlowMountFSType
// already covers the Linux logic against a known-local magic number
// (0xEF53), so skip the live probe there and keep it only on platforms
// where slowWatchMount is a static stub (always false, can't flake).
func TestSlowWatchMount_NormalMountNotDegraded(t *testing.T) {
	if slowWatchMount("") {
		t.Error("an empty path must not be flagged slow")
	}
	if runtime.GOOS == "linux" {
		t.Skip("Linux magic-number logic is pinned by TestIsSlowMountFSType against known constants, not a live TMPDIR probe")
	}
	if slowWatchMount(t.TempDir()) {
		t.Error("a normal local mount must not be flagged slow (fsnotify must stay enabled)")
	}
}
