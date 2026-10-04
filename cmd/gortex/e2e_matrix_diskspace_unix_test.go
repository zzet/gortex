//go:build !windows

package main

import "syscall"

// e2eLifecycleFreeBytes is the free space on the volume holding path. Only the
// disk-full row uses it, and only against a volume this file created.
//
// statfs f_bavail × f_bsize: the blocks reserved for root are deliberately
// excluded, since the filler in e2eLifecycleFillVolume writes as the test user
// and "full" has to mean full for that user.
func e2eLifecycleFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(uint64(stat.Bavail) * uint64(stat.Bsize)), nil
}
