//go:build windows

package platform

import (
	"time"

	"golang.org/x/sys/windows"
)

// ProcessCPUTime returns the current process's cumulative user plus system CPU.
// Kernel/user FILETIMEs are durations in 100ns ticks, not absolute epoch dates.
func ProcessCPUTime() (time.Duration, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return processCPUFiletimes(kernel, user)
}

func processCPUFiletimes(kernel, user windows.Filetime) (time.Duration, error) {
	kernelTicks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	userTicks := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	const maxTicks = uint64((1<<63 - 1) / 100)
	if kernelTicks > maxTicks || userTicks > maxTicks-kernelTicks {
		return 0, errInvalidProcessCPUTime
	}
	return time.Duration(kernelTicks+userTicks) * 100 * time.Nanosecond, nil
}
