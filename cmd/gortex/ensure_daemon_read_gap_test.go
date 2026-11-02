package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gofrs/flock"

	"github.com/zzet/gortex/internal/daemon"
)

// Characterize the retained Windows trace without claiming its first read
// failed for this reason: progress reads once, then the diagnostic reads again.
// A fresh second read cannot prove the first read succeeded.
func TestStartupReadGapCanProduceFalseProgressWithFreshSecondRead(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	writeHandoffStateFixture(t, daemon.StartupMigrating, now)
	body, err := os.ReadFile(daemon.RuntimeStatePath())
	require.NoError(t, err)
	waiter := daemonStartupHeartbeatWaiter()
	lock := &testSpawnLock{held: true}
	reads := 0
	falseWithFreshSecondRead := false
	locked, err := waitForSpawnLock(t.Context(), lock, spawnLockWaitOptions{
		now:        func() time.Time { return now },
		inactivity: migrationProxyTestInactivity,
		poll:       spawnLockPollInterval,
		startupProgress: func(at time.Time) bool {
			reads++
			fresh := waiter(at)
			if reads == 2 {
				require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), body, 0o600))
				state, ok := daemon.ReadRuntimeState()
				falseWithFreshSecondRead = !fresh && ok && state.StartupProgressFresh(at, spawnProgressFreshness)
			}
			return fresh
		},
		wait: func(_ context.Context, pause time.Duration) error {
			require.Equal(t, migrationProxyTestInactivity, pause)
			now = now.Add(pause)
			return os.Remove(daemon.RuntimeStatePath())
		},
	})
	require.False(t, locked)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 2, reads)
	require.True(t, falseWithFreshSecondRead)
}

// Fixture synchronization must not turn a cancelled observation into progress
// or wait beyond the existing caller's cancellation.
func TestMigrationProxyStateAccessCancellationDoesNotRead(t *testing.T) {
	root := t.TempDir()
	guard := flock.New(filepath.Join(root, "runtime-publication.guard"))
	require.NoError(t, guard.Lock())
	defer guard.Close() //nolint:errcheck // Test owns this guard.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	read := false
	err := migrationProxyStateAccess(ctx, root, func() error {
		read = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, read)
}
