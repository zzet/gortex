//go:build darwin || linux || windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

func TestDaemonStoreFreeProbeUsesExistingLockAndReleasesEveryPoll(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "store.sqlite")
	owner := flock.New(backend + ".lock") // Same physical lock convention as serverstack.
	held, err := owner.TryLock()
	require.NoError(t, err)
	require.True(t, held)
	t.Cleanup(func() { _ = owner.Unlock() })
	lock, err := captureDaemonStoreLock(backend)
	require.NoError(t, err)
	free, err := lock.probe()
	require.NoError(t, err)
	require.False(t, free, "a held existing lock was reported free")
	require.NoError(t, owner.Unlock())
	for range 3 {
		free, err = lock.probe()
		require.NoError(t, err)
		require.True(t, free)
		// A successful poll must release immediately, not retain a probe lock
		// until the next poll or until the CLI returns.
		held, err = owner.TryLock()
		require.NoError(t, err)
		require.True(t, held, "store-free probe retained its exclusive lock")
		require.NoError(t, owner.Unlock())
	}
}

func TestDaemonStoreFreeProbeRefusesMissingReplacedAndSymlinkFiles(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "store.sqlite")
	_, err := captureDaemonStoreLock(backend)
	require.Error(t, err)
	_, err = os.Lstat(backend + ".lock")
	require.ErrorIs(t, err, os.ErrNotExist, "capture must never create a lock")
	require.NoError(t, os.WriteFile(backend+".lock", nil, 0o600))
	lock, err := captureDaemonStoreLock(backend)
	require.NoError(t, err)
	require.NoError(t, os.Rename(backend+".lock", backend+".lock.previous"))
	free, err := lock.probe()
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, free)
	_, err = os.Lstat(backend + ".lock")
	require.ErrorIs(t, err, os.ErrNotExist, "probe must never recreate a missing lock")
	require.NoError(t, os.WriteFile(backend+".lock", nil, 0o600))
	free, err = lock.probe()
	require.ErrorContains(t, err, "identity changed")
	require.False(t, free)

	t.Run("symlink", func(t *testing.T) {
		linkBackend := filepath.Join(t.TempDir(), "link.sqlite")
		if err := os.Symlink(backend+".lock", linkBackend+".lock"); err != nil {
			t.Skipf("native symlink creation unavailable: %v", err)
		}
		_, err := captureDaemonStoreLock(linkBackend)
		require.ErrorContains(t, err, "not a regular file")
		// The actual no-follow opener also refuses a symlink introduced
		// after capture (on Windows it opens the reparse point itself).
		file, err := openExistingDaemonStoreLock(linkBackend + ".lock")
		if err == nil {
			defer func() { require.NoError(t, file.Close()) }()
			info, statErr := file.Stat()
			require.NoError(t, statErr)
			require.False(t, info.Mode().IsRegular())
		}
	})
}

func TestDaemonStoreFreeProbeRetainsUnlockErrorAndClosesPhysicalLock(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "store.sqlite")
	require.NoError(t, os.WriteFile(backend+".lock", nil, 0o600))
	lock, err := captureDaemonStoreLock(backend)
	require.NoError(t, err)
	failure := errors.New("injected unlock failure")
	var opened *os.File
	free, err := lock.probeWith(tryDaemonStoreLock, func(file *os.File) error {
		opened = file
		// Leave the actual exclusive lock held to verify the unconditional
		// descriptor close still releases it on this failure path.
		return failure
	})
	require.ErrorIs(t, err, failure)
	require.False(t, free)
	require.NotNil(t, opened)
	// A second Close has the portable os.ErrClosed contract. Stat on a
	// closed Windows file may instead return native ERROR_INVALID_HANDLE.
	require.ErrorIs(t, opened.Close(), os.ErrClosed)
	owner := flock.New(backend + ".lock")
	held, err := owner.TryLock()
	require.NoError(t, err)
	require.True(t, held, "unlock failure leaked the physical probe lock")
	require.NoError(t, owner.Unlock())
}
