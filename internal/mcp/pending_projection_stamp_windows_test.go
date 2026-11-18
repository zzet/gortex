//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiskStampFailsClosedWhenWindowsIdentityCannotBeLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.go")
	require.NoError(t, os.WriteFile(path, []byte("package example\n"), 0o600))
	info, err := os.Lstat(path)
	require.NoError(t, err)
	// Exercise the real race between Lstat metadata and eager ID capture.
	// Go 1.27.3 retains the path for lazy identity loading; removing that
	// path makes the ID unavailable without relying on sharing semantics.
	// Do not call SameFile before removal, which would initialize the ID.
	require.NoError(t, os.Remove(path))
	stamp, err := freezeDiskStampIdentity(path, info)
	require.ErrorContains(t, err, "file identity unavailable")
	require.Nil(t, stamp.info, "metadata without a captured identity cannot become a trusted stamp")
}
