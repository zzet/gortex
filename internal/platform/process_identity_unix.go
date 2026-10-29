//go:build !windows

package platform

import (
	"fmt"
	"syscall"
)

func signalProcessIdentity(p ProcessIdentity, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return signalCheckedProcessIdentity(p, signal)
}

func quitProcessIdentity(p ProcessIdentity) error {
	return signalCheckedProcessIdentity(p, syscall.SIGQUIT)
}

func signalCheckedProcessIdentity(p ProcessIdentity, signal syscall.Signal) error {
	same, err := p.Matches()
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("refusing to signal changed process incarnation %d", p.pid)
	}
	return syscall.Kill(p.pid, signal)
}
