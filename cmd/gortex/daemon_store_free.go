package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// daemonStoreLock retains only the pre-stop file identity, never an open lock
// descriptor. Missing or replaced files cannot certify that the captured
// daemon's store is free.
type daemonStoreLock struct {
	path string
	info os.FileInfo
}

func captureDaemonStoreLock(backend string) (captured *daemonStoreLock, err error) {
	if backend == "" || !filepath.IsAbs(backend) {
		return nil, errors.New("daemon did not report an absolute store path")
	}
	path := backend + ".lock"
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("capture existing daemon store lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("daemon store lock %q is not a regular file", path)
	}
	file, err := openExistingDaemonStoreLock(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			captured = nil
		}
	}()
	// Capture from the handle: on Windows path-only FileInfo may lazily load
	// its file ID later, after the pathname has already been replaced.
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("opened daemon store lock %q is not a regular file", path)
	}
	captured = &daemonStoreLock{path: path, info: info}
	if err := captured.samePath(); err != nil {
		return nil, err
	}
	return captured, nil
}

func (l *daemonStoreLock) probe() (bool, error) {
	return l.probeWith(tryDaemonStoreLock, unlockDaemonStoreLock)
}

func (l *daemonStoreLock) samePath() error {
	info, err := os.Lstat(l.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(l.info, info) {
		return errors.New("daemon store lock file identity changed")
	}
	return nil
}

// A probe owns its descriptor only within this call. Even an unlock error must
// close it before returning; neither lock nor descriptor survives a CLI poll.
func (l *daemonStoreLock) probeWith(try func(*os.File) (bool, error), unlock func(*os.File) error) (free bool, err error) {
	if l == nil || l.info == nil {
		return false, errors.New("no captured daemon store lock identity")
	}
	if err := l.samePath(); err != nil {
		return false, err
	}
	file, err := openExistingDaemonStoreLock(l.path)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			free = false
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(l.info, info) {
		return false, errors.New("opened daemon store lock does not match captured identity")
	}
	locked, err := try(file)
	if err != nil || !locked {
		return false, err
	}
	defer func() {
		unlockErr := unlock(file)
		err = errors.Join(err, unlockErr)
	}()
	if err := l.samePath(); err != nil {
		return false, err
	}
	return true, nil
}
