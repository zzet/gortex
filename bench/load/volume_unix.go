//go:build linux || darwin

package main

import "syscall"

func volumeCapacity(path string) Metric {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return unavailable("bytes", err.Error())
	}
	return measured(float64(stat.Blocks)*float64(stat.Bsize), "bytes")
}
