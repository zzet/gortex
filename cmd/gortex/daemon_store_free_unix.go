//go:build darwin || linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openExistingDaemonStoreLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
}

func tryDaemonStoreLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockDaemonStoreLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return err
}
