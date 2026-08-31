package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

// TestTrack_OfflineSucceeds asserts the offline-safe guarantee: with
// auto-start off and no daemon socket, `gortex track <repo>` still writes
// the repo to the global config (before any daemon contact) and returns
// success with the config-only summary — never an error.
func TestTrack_OfflineSucceeds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_AUTOSTART", "off")
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "nonexistent.sock"))

	repo := t.TempDir()
	trackName = ""
	trackAsWorktree = false
	t.Cleanup(func() { trackName = "" })

	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	if err := runTrack(cmd, []string{repo}); err != nil {
		t.Fatalf("offline track must succeed, got error: %v", err)
	}

	gc, err := config.LoadGlobal()
	if err != nil {
		t.Fatalf("reload global config: %v", err)
	}
	found := false
	canonicalRepo := pathkey.CanonicalExistingRoot(repo)
	for _, r := range gc.Repos {
		if pathkey.EqualPaths(r.Path, canonicalRepo) {
			found = true
		}
	}
	if !found {
		t.Fatalf("repo must be persisted to config even with no daemon; repos=%v", gc.Repos)
	}
}

// TestTrack_OfflineConfigWrittenBeforeDaemon asserts the config write
// happens regardless of daemon state — a second offline track of the same
// repo is idempotent and still succeeds.
func TestTrack_OfflineIdempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_AUTOSTART", "off")
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "nonexistent.sock"))
	repo := t.TempDir()
	trackName = ""
	trackAsWorktree = false

	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	if err := runTrack(cmd, []string{repo}); err != nil {
		t.Fatalf("first track: %v", err)
	}
	if err := runTrack(cmd, []string{repo}); err != nil {
		t.Fatalf("re-tracking an already-tracked repo must be a no-op success, got %v", err)
	}
	gc, _ := config.LoadGlobal()
	count := 0
	canonicalRepo := pathkey.CanonicalExistingRoot(repo)
	for _, r := range gc.Repos {
		if pathkey.EqualPaths(r.Path, canonicalRepo) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("a repo must appear exactly once after a double track, got %d", count)
	}
}
