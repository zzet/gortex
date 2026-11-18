//go:build unix

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openDaemonExitLogFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func replaceDaemonLogFD(target **os.File, source *os.File) error {
	return unix.Dup2(int(source.Fd()), int((*target).Fd()))
}

func prepareDaemonLogOutput(_ string, _, _ **os.File) error {
	return nil
}

func replaceDaemonLogFDPair(stderr, stdout **os.File, source *os.File) error {
	if err := replaceDaemonLogFD(stderr, source); err != nil {
		return fmt.Errorf("redirect stderr: %w", err)
	}
	if err := replaceDaemonLogFD(stdout, source); err != nil {
		return fmt.Errorf("redirect stdout (stderr is now %s): %w", source.Name(), err)
	}
	return nil
}
