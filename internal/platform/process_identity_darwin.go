package platform

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func processBirth(pid int) ([2]uint64, error) {
	birth, _, err := processBirthAndRunning(pid)
	return birth, err
}

func processBirthAndRunning(pid int) ([2]uint64, bool, error) {
	// The singular x/sys helper maps every short record (including a
	// successful zero-length missing-PID response) to EIO. The slice helper
	// retains that distinction without treating arbitrary EIO as process exit.
	info, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			err = errors.Join(os.ErrProcessDone, err)
		}
		return [2]uint64{}, false, err
	}
	return darwinProcessIdentity(pid, info)
}

func darwinProcessIdentity(pid int, records []unix.KinfoProc) ([2]uint64, bool, error) {
	if len(records) == 0 {
		return [2]uint64{}, false, os.ErrProcessDone
	}
	if len(records) != 1 || records[0].Proc.P_pid != int32(pid) {
		return [2]uint64{}, false, fmt.Errorf("unexpected native process records for PID %d", pid)
	}
	info := records[0]
	return [2]uint64{uint64(info.Proc.P_starttime.Sec), uint64(info.Proc.P_starttime.Usec)}, info.Proc.P_stat != darwinProcessZombie, nil
}
