package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/testenv"
)

// TestDaemonRequiredErr_GuidesTracking asserts the daemon-required error
// tells callers in an unconfigured directory how to make the daemon able
// to answer, naming `gortex track <abs path>`.
func TestDaemonRequiredErr_GuidesTracking(t *testing.T) {
	dir := t.TempDir()
	err := daemonRequiredErr(dir)
	if err == nil {
		t.Fatal("daemonRequiredErr returned nil")
	}
	abs, _ := filepath.Abs(dir)
	msg := err.Error()
	if !strings.Contains(msg, "gortex track") {
		t.Errorf("error should suggest `gortex track`: %q", msg)
	}
	if !strings.Contains(msg, abs) {
		t.Errorf("error should name the absolute repo path %q: %q", abs, msg)
	}
}

func TestCall_NoListenerGuidanceUsesLocalRoster(t *testing.T) {
	base := testenv.ShortTempDir(t)
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(base, "s"))
	t.Setenv("GORTEX_AUTOSTART", "0")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))

	primary, linked := fakeLinkedWorktree(t, filepath.Join(base, "configured"))
	admin := filepath.Join(primary, ".git", "worktrees", "feature")
	backlink, err := filepath.Rel(admin, filepath.Join(linked, ".git"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(admin, "gitdir"), []byte(backlink+"\n"), 0o600))
	routerCWD := filepath.Join(primary, "router-cwd")
	adjacent := primary + "-unconfigured"
	copied := filepath.Join(base, "copied-gitdir")
	overrideOnly := filepath.Join(base, "configured-by-override")
	for _, path := range []string{routerCWD, adjacent, copied, overrideOnly} {
		require.NoError(t, os.Mkdir(path, 0o700))
	}
	forward, err := os.ReadFile(filepath.Join(linked, ".git"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(copied, ".git"), forward, 0o600))
	_, copiedLooksLinked := linkedWorktreeAt(copied)
	require.True(t, copiedLooksLinked, "the copied marker must fool the one-way classifier")

	roster := config.DefaultGlobalConfigPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(roster), 0o700))
	primaryRoster := fmt.Sprintf("repos:\n  - path: %q\n", primary)
	require.NoError(t, os.WriteFile(roster, []byte(primaryRoster), 0o600))
	oldConfig := cfgFile
	t.Cleanup(func() { cfgFile = oldConfig })
	cfgFile = filepath.Join(base, "command-local.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(fmt.Sprintf("repos:\n  - path: %q\n", overrideOnly)), 0o600))

	assertAdvice := func(t *testing.T, path string, configured bool) {
		t.Helper()
		cmd, output := newCallTestCmd(t)
		callIndex = path
		err := runCall(cmd, []string{"search_symbols"})
		require.ErrorContains(t, err, "no gortex daemon is running")
		require.ErrorContains(t, err, "gortex daemon start --detach")
		var coded *exitCodeError
		require.NotErrorAs(t, err, &coded, "keep the ordinary nonzero exit, never a loading exit")
		var loading *repoLoadingError
		require.NotErrorAs(t, err, &loading)
		require.NotContains(t, output.String()+err.Error(), "repo_loading")
		if configured {
			require.NotContains(t, err.Error(), "gortex track")
			require.NotContains(t, err.Error(), "gortex repos reconcile")
			require.ErrorContains(t, err, "configured repository")
		} else {
			require.ErrorContains(t, err, "gortex track")
			require.NotContains(t, err.Error(), "configured repository")
		}
	}
	for _, test := range []struct {
		name, path string
		configured bool
	}{
		{"primary", primary, true},
		{"router_subdirectory", routerCWD, true},
		{"reciprocal_linked_checkout", linked, true},
		{"component_prefix_only", adjacent, false},
		{"copied_configured_gitdir", copied, false},
		{"command_config_is_not_daemon_roster", overrideOnly, false},
	} {
		t.Run(test.name, func(t *testing.T) { assertAdvice(t, test.path, test.configured) })
	}
	t.Run("distinct_folded_component", func(t *testing.T) {
		declared := filepath.Join(base, "caf\u00e9")
		unconfigured := filepath.Join(base, "cafe\u0301")
		require.NoError(t, os.Mkdir(declared, 0o700))
		if err := os.Mkdir(unconfigured, 0o700); os.IsExist(err) {
			first, firstErr := os.Stat(declared)
			second, secondErr := os.Stat(unconfigured)
			require.NoError(t, firstErr)
			require.NoError(t, secondErr)
			require.True(t, os.SameFile(first, second))
			t.Skip("filesystem treats the two names as the same directory")
		} else {
			require.NoError(t, err)
		}
		nested := filepath.Join(unconfigured, "nested")
		require.NoError(t, os.Mkdir(nested, 0o700))
		require.True(t, pathkey.HasPathPrefix(nested, declared), "the names must match a folded lexical prefix")
		require.False(t, pathkey.SamePathIdentity(declared, unconfigured))
		require.NoError(t, os.WriteFile(roster, []byte(fmt.Sprintf("repos:\n  - path: %q\n", declared)), 0o600))
		t.Cleanup(func() { require.NoError(t, os.WriteFile(roster, []byte(primaryRoster), 0o600)) })
		assertAdvice(t, nested, false)
	})
	t.Run("malformed_roster_authority", func(t *testing.T) {
		require.NoError(t, os.WriteFile(roster, []byte("repos: [\n"), 0o600))
		assertAdvice(t, primary, false)
	})
}
