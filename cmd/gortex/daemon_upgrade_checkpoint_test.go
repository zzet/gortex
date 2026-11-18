//go:build acceptance && (linux || darwin)

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testenv"
)

// Use the actual daemon state builder and its real lock/config/Open chain.
// No process, signal, protected store, or runtime-state inference is involved.
func TestDaemonUpgradeCheckpointHoldsActualOpen(t *testing.T) {
	root := testenv.ShortTempDir(t)
	for key, value := range migrationProxyOwnedEnv(root) {
		t.Setenv(key, value)
	}
	oldConfig, oldPath, oldBackend := cfgFile, daemonBackendPath, daemonBackend
	t.Cleanup(func() { cfgFile, daemonBackendPath, daemonBackend = oldConfig, oldPath, oldBackend })
	cfgFile, daemonBackendPath, daemonBackend = filepath.Join(root, "repo.yaml"), filepath.Join(root, "store.sqlite"), "sqlite"
	require.NoError(t, os.WriteFile(cfgFile, []byte("semantic:\n  enabled: false\n"), 0o600))
	store, err := store_sqlite.Open(daemonBackendPath)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	db, err := sql.Open("sqlite", daemonBackendPath)
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA user_version=40")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	type result struct {
		state *daemonState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := buildDaemonStateWithUpgradeCheckpoint(zap.NewNop(), func(point string, version int) {
			if point == "in_step" && version == 41 {
				close(entered)
				<-release
			}
		})
		done <- result{state, err}
	}()
	closeState := func(r result) {
		if r.state != nil {
			require.NoError(t, r.state.shared.Close())
		}
	}
	select {
	case <-entered:
	case r := <-done:
		closeState(r)
		t.Fatalf("actual daemon Open returned without entering its selected hook: %v", r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("actual daemon Open did not enter selected callback")
	}
	select {
	case r := <-done:
		closeState(r)
		t.Fatal("daemon state advanced while actual Open hook was held")
	default:
	}
	unblock()
	select {
	case r := <-done:
		require.NoError(t, r.err)
		closeState(r)
	case <-time.After(10 * time.Second):
		t.Fatal("actual daemon Open did not resume after hook release")
	}
}
