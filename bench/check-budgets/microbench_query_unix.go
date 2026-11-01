//go:build unix

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Query executables are admitted compiled Go test binaries, not go launchers.
// Their descendants inherit this owned group. SIGQUIT targets the Go process
// first; only after its bounded Wait finishes do we stop any surviving group.
func ownQueryMicroProcess(cmd *exec.Cmd) func() error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid := cmd.Process.Pid
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil {
			return fmt.Errorf("query group %d quiescence unknown: %w", pgid, err)
		}
		// A normally exiting benchmark must not leave background work either.
		// Retain that failure even when the following cleanup succeeds.
		leftover := fmt.Errorf("query executable left owned descendants in group %d", pgid)
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			if err := syscall.Kill(-pgid, sig); errors.Is(err, syscall.ESRCH) {
				return leftover
			} else if err != nil {
				return errors.Join(leftover, fmt.Errorf("query group cleanup %s: %w", sig, err))
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
					return leftover
				} else if err != nil {
					return errors.Join(leftover, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		return errors.Join(leftover, errors.New("owned query group did not disappear after bounded cleanup"))
	}
}
