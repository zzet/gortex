//go:build !windows

package gitcmd

import (
	"os/exec"
	"syscall"
	"time"
)

// indexRefreshStartupGrace is how long a refresh cancelled at its start is left
// to run before it is signalled.
//
// Git creates index.lock before it installs the signal handler that removes
// it, so a SIGTERM that lands between the two ends the child by the default
// action and leaves the lock behind, and every later Git write to the checkout
// fails on it. A refresh cancelled while it is still starting up (a checkout
// coordinator closed just after it started one) finishes, or gets well past
// that point, before it is asked to stop.
const indexRefreshStartupGrace = 250 * time.Millisecond

func configureIndexRefreshCancellation(cmd *exec.Cmd) {
	started := time.Now()
	cmd.Cancel = func() error {
		for time.Since(started) < indexRefreshStartupGrace {
			// os.ErrProcessDone once the child has exited on its own.
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	// Git's tempfile signal handler removes its own index.lock. A stuck child
	// is still killed and joined; no application code removes an unknown lock.
	cmd.WaitDelay = 2 * time.Second
}
