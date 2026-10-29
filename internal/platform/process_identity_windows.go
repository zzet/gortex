package platform

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func processHandleBirth(handle windows.Handle) ([2]uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return [2]uint64{}, err
	}
	return [2]uint64{uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), 0}, nil
}

func processBirth(pid int) ([2]uint64, error) {
	birth, _, err := processBirthAndRunning(pid)
	return birth, err
}

func processBirthAndRunning(pid int) (birth [2]uint64, running bool, err error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// OpenProcess documents ERROR_INVALID_PARAMETER for an absent PID.
		// Access denied and all other failures remain unknown, not exited.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			err = errors.Join(os.ErrProcessDone, err)
		}
		return birth, false, err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	birth, err = processHandleBirth(handle)
	if err != nil {
		return birth, false, err
	}
	var code uint32
	if err = windows.GetExitCodeProcess(handle, &code); err != nil {
		return birth, false, err
	}
	return birth, code == stillActive, nil
}

func signalProcessIdentity(p ProcessIdentity, _ bool) (err error) {
	if p.pid <= 0 || p.birth == [2]uint64{} {
		return errors.New("invalid captured process identity")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(p.pid))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	birth, err := processHandleBirth(handle)
	if err != nil {
		return err
	}
	if birth != p.birth {
		return fmt.Errorf("refusing to signal changed process incarnation %d", p.pid)
	}
	return windows.TerminateProcess(handle, 1)
}

func quitProcessIdentity(ProcessIdentity) error {
	return errors.New("SIGQUIT diagnostic termination is unavailable on Windows")
}
