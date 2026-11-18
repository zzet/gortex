//go:build linux || darwin

package main

import (
	"fmt"
	"syscall"
)

func readbackReferenceVolumeIdentity(path string) (string, error) {
	var file syscall.Stat_t
	if err := syscall.Stat(path, &file); err != nil {
		return "", err
	}
	var volume syscall.Statfs_t
	if err := syscall.Statfs(path, &volume); err != nil {
		return "", err
	}
	return fmt.Sprintf("device=%d;fsid=%v", file.Dev, volume.Fsid), nil
}
