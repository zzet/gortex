//go:build !windows

package gitcmd

import (
	"os/exec"
	"syscall"
	"time"
)

func configureIndexRefreshCancellation(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// Git's tempfile signal handler removes its own index.lock. A stuck child
	// is still killed and joined; no application code removes an unknown lock.
	cmd.WaitDelay = 2 * time.Second
}
