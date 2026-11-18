package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func handoffProcessBirth(pid int) (string, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if p.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d identity unavailable", pid)
	}
	return fmt.Sprintf("%d:%d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec), nil
}
