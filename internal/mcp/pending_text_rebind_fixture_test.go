package mcp

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// Windows' pinned Go file/root opens may omit FILE_SHARE_DELETE. In that
// case the kernel prevents the fixture's rename while our handles are live.
// Verify that stronger lifetime restriction instead of pretending a rename
// occurred or skipping the complete test. Other errors remain test failures.
type pendingTextTestRename struct {
	blocked  bool
	from, to string
	before   os.FileInfo
}

func pendingTextWindowsRenameDenied(goos string, err error) bool {
	return goos == "windows" && (errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)))
}

func (r *pendingTextTestRename) try(t *testing.T, from, to string) bool {
	t.Helper()
	before, err := os.Stat(from)
	require.NoError(t, err)
	err = os.Rename(from, to)
	if err == nil {
		return true
	}
	require.True(t, pendingTextWindowsRenameDenied(runtime.GOOS, err), "unexpected fixture rename failure: %v", err)
	after, statErr := os.Stat(from)
	require.NoError(t, statErr)
	require.True(t, os.SameFile(before, after), "denied rename must retain the named identity")
	_, targetErr := os.Lstat(to)
	require.ErrorIs(t, targetErr, os.ErrNotExist, "denied rename must not install a destination")
	r.blocked, r.from, r.to, r.before = true, from, to, before
	t.Logf("actual Windows sharing/access refusal while handles are open: %v", err)
	return false
}

func (r *pendingTextTestRename) afterClose(t *testing.T) {
	t.Helper()
	require.True(t, r.blocked)
	require.NoError(t, os.Rename(r.from, r.to), "same rename must succeed after every owned handle closes; ACL/fixture errors are not sharing proof")
	after, err := os.Stat(r.to)
	require.NoError(t, err)
	require.True(t, os.SameFile(r.before, after))
	_, err = os.Lstat(r.from)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPendingTextWindowsRenameDenialClassification(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, errno := range []syscall.Errno{5, 32, 2, 13} {
			err := &os.LinkError{Op: "rename", Old: "owned", New: "owned.old", Err: errno}
			require.Equal(t, goos == "windows" && (errno == 5 || errno == 32), pendingTextWindowsRenameDenied(goos, err))
		}
	}
	require.False(t, pendingTextWindowsRenameDenied("windows", nil))
}
