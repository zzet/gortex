//go:build unix

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureOwnedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killOwnedProcess(cmd *exec.Cmd) error {
	// The process group is created exclusively for this invocation. It also
	// contains owned children left by an early-exiting parent; never search
	// process names or touch another daemon's PID file/socket.
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Wait has completed, but children can still be alive in the owned Unix group.
func cleanupExitedOwnedProcess(cmd *exec.Cmd) error { return killOwnedProcess(cmd) }
