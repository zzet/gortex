//go:build windows

package main

import (
	"math"

	"golang.org/x/sys/windows"
)

// e2eLifecycleFreeBytes is the free space on the volume holding path. Only the
// disk-full row uses it, and only against a volume this file created.
//
// GetDiskFreeSpaceEx's first output is the space available to the calling
// user, which is the Windows counterpart of statfs f_bavail: per-user quotas
// are respected, so "full" means full for the process that writes the filler.
// golang.org/x/sys is already a direct dependency of this module (see
// internal/platform/disk_windows.go), so this adds no new one.
//
// The disk-full row itself is Darwin-only (it builds its volume with hdiutil)
// and records a named skip elsewhere; this implementation exists so the
// cmd/gortex test package builds — and every other test in it runs — on
// Windows. It still reports a real number rather than a stub, so a caller that
// probes free space on Windows is told the truth.
func e2eLifecycleFreeBytes(path string) (int64, error) {
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var availToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(wide, &availToCaller, &total, &totalFree); err != nil {
		return 0, err
	}
	if availToCaller > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(availToCaller), nil
}
