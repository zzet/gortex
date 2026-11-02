//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestDiskStampFailsClosedWhenWindowsIdentityCannotBeLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.go")
	require.NoError(t, os.WriteFile(path, []byte("package example\n"), 0o600))
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	// Lstat's attribute lookup still succeeds. Go 1.27.3's lazy file-ID
	// loader opens with share=0, which cannot coexist with this real reader.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	defer windows.CloseHandle(handle)
	_, err = os.Lstat(path)
	require.NoError(t, err)
	stamp, err := statDiskStamp(path)
	require.ErrorContains(t, err, "file identity unavailable")
	require.Nil(t, stamp.info, "metadata without a captured identity cannot become a trusted stamp")
}
