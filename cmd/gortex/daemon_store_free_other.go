//go:build !darwin && !linux && !windows

package main

import (
	"errors"
	"os"
)

func openExistingDaemonStoreLock(_ string) (*os.File, error) {
	return nil, errors.New("existing daemon store lock probe unsupported on this platform")
}

func tryDaemonStoreLock(_ *os.File) (bool, error) {
	return false, errors.New("existing daemon store lock probe unsupported on this platform")
}

func unlockDaemonStoreLock(_ *os.File) error {
	return errors.New("existing daemon store lock probe unsupported on this platform")
}
