//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestDiskStampDoesNotOpenFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	require.NotZero(t, stamp.info.Mode()&os.ModeNamedPipe)
}
