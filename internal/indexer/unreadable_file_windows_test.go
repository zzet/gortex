//go:build windows

package indexer

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// denyFileRead makes path unreadable until restore is called. An exclusive
// byte-range lock denies reads through a second handle, including one opened
// by this process, while leaving the regular file available to os.Stat and
// the indexer's path admission. The lock covers the whole test source file.
// Restore also runs from t.Cleanup so an early test failure leaves the temp
// directory removable.
func denyFileRead(t *testing.T, path string) func() {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)

	var overlapped windows.Overlapped
	const allBytes = ^uint32(0)
	lockErr := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, allBytes, allBytes, &overlapped,
	)
	if lockErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("close unlocked test file %q: %v", path, closeErr)
		}
		t.Fatalf("lock test file %q: %v", path, lockErr)
	}

	var once sync.Once
	restore := func() {
		once.Do(func() {
			unlockErr := windows.UnlockFileEx(
				windows.Handle(file.Fd()), 0, allBytes, allBytes, &overlapped,
			)
			closeErr := file.Close()
			if unlockErr != nil {
				t.Errorf("unlock test file %q: %v", path, unlockErr)
			}
			if closeErr != nil {
				t.Errorf("close test file %q: %v", path, closeErr)
			}
		})
	}
	t.Cleanup(restore)

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "locked file must remain discoverable")
	_, readErr := os.ReadFile(path)
	require.Error(t, readErr, "locked file must deny a second-handle read")
	return restore
}
