//go:build linux

package store_sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func vacuumOpenIsAnonymous(flags int32) bool {
	// O_TMPFILE contains the directory flag; equality avoids treating an ordinary
	// directory open as an anonymous file. Its value is architecture-specific.
	return flags&int32(unix.O_TMPFILE) == int32(unix.O_TMPFILE)
}

func TestVacuumPhysicalProbeObservesRealAnonymousLinuxDescriptor(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	flags := unix.O_RDWR | unix.O_EXCL | unix.O_TMPFILE
	fd, err := unix.Open(directory, flags, 0600)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		t.Skipf("this filesystem/kernel does not support native O_TMPFILE; named fallback still requires physical VACUUM proof: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unix.Close(fd)) })
	observed, recognized, err := inspectVacuumNativeOpen(int32(fd), directory, int32(flags), directory)
	require.NoError(t, err)
	require.True(t, recognized)
	require.True(t, observed.anonymous)
	require.Equal(t, directory, observed.directory)
	require.Zero(t, observed.links)
	var actual unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &actual))
	require.Equal(t, uint64(actual.Dev), observed.device)
	require.Equal(t, uint64(actual.Ino), observed.inode)
	require.False(t, vacuumOpenIsAnonymous(int32(unix.O_RDONLY|unix.O_DIRECTORY)), "ordinary directory flags must not be mistaken for O_TMPFILE")
	wrong, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, recognized, err = inspectVacuumNativeOpen(int32(fd), directory, int32(flags), wrong)
	require.True(t, recognized)
	require.Error(t, err, "an anonymous descriptor on another directory is not accepted")
}
