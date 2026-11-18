//go:build unix

package citest

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Each invocation owns a new process group. Cancellation cannot reach another
// session, and also terminates test binaries started by the go command.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
