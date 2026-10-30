//go:build darwin || linux

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/testenv"
)

func TestRuntimeListenChmodFailureRetainsCloseErrorsAndRemovesOwnedFiles(t *testing.T) {
	dir := testenv.ShortTempDir(t)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "pid"))
	s := New(filepath.Join(dir, "sock"), "runtime-fixture", zap.NewNop())
	chmodFailure, closeFailure := errors.New("chmod failure"), errors.New("close failure after chmod")
	s.socketChmod = func(string, os.FileMode) error {
		// The real created socket must be captured before this failure.
		require.NotNil(t, s.ownedSocket.info)
		s.listener = &runtimeFaultListener{Listener: s.listener, failure: closeFailure}
		return chmodFailure
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	err := s.Listen()
	require.ErrorIs(t, err, chmodFailure)
	require.ErrorIs(t, err, closeFailure)
	for _, path := range []string{s.SocketPath, s.pidPath} {
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}
