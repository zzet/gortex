//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// A leftover PID file whose PID was reused by another user's process must
// still read as stale: the daemon can't signal (or stop) that process, so it
// isn't a running daemon, and a new daemon must be able to replace the file.
func TestRunningPIDOtherUsersProcess(t *testing.T) {
	if err := syscall.Kill(1, 0); !errors.Is(err, syscall.EPERM) {
		t.Skipf("kill(1, 0) = %v; needs a PID 1 owned by another user", err)
	}
	pidPath := filepath.Join(t.TempDir(), "daemon.pid")
	t.Setenv("GORTEX_DAEMON_PIDFILE", pidPath)
	writePID(t, pidPath, 1)

	if pid, ok := RunningPID(); ok {
		t.Fatalf("pid file naming another user's process must read as not running, got (%d,%v)", pid, ok)
	}
	if err := (&Server{}).writePIDFile(); err != nil {
		t.Fatalf("writePIDFile over a stale pid file: %v", err)
	}
	b, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file = %q, want %d", got, os.Getpid())
	}
}
