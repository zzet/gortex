package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/platform"
)

func TestDetachedDaemonEnvCarriesSQLiteTmpdir(t *testing.T) {
	dir := t.TempDir()
	parent := []string{"SQLITE_TMPDIR=/wrong", "KEEP=unchanged", "TMPDIR=/general-temp", "GORTEX_DAEMON_CHILD=0", "SQLITE_TMPDIR=/also-wrong"}
	before := append([]string(nil), parent...)
	for _, test := range []struct {
		name, path, want string
	}{
		{name: "explicit", path: filepath.Join(dir, "store volume", "custom.sqlite"), want: filepath.Join(dir, "store volume")},
		{name: "default", path: "", want: platform.StoreDir()},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, err := detachedDaemonEnv(parent, test.path)
			require.NoError(t, err)
			var sqlite []string
			for _, entry := range env {
				if strings.HasPrefix(entry, "SQLITE_TMPDIR=") {
					sqlite = append(sqlite, entry)
				}
			}
			require.Equal(t, []string{"SQLITE_TMPDIR=" + test.want}, sqlite)
			require.Contains(t, env, "GORTEX_DAEMON_CHILD=1")
			require.NotContains(t, env, "GORTEX_DAEMON_CHILD=0")
			require.Contains(t, env, "TMPDIR=/general-temp")
			require.Contains(t, env, "KEEP=unchanged")
			require.Equal(t, before, parent)
		})
	}
	// The environment must fail setup before exec if the intended parent is
	// unusable. It must not silently select the system temp directory.
	file := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("owned fixture"), 0o600))
	_, err := detachedDaemonEnv(parent, filepath.Join(file, "store.sqlite"))
	require.ErrorContains(t, err, "resolve detached SQLite temp directory")
}
