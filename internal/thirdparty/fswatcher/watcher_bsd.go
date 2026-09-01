//go:build freebsd || openbsd || netbsd || dragonfly

package fswatcher

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// BSD system files used by isSystemFile
var osPrefixes = []string{"~", ".Trash"}
var osSuffixes = []string{".tmp", ".bak", ".swp", ".swo", "~"}

// kqueueOpenFlags is used to open files for kqueue monitoring on BSD
const kqueueOpenFlags = unix.O_RDONLY

// platformNotifier defines the interface for BSD (kqueue) backend implementations
type platformNotifier interface {
	addWatch(watchPath *WatchPath) error
	removeWatch(path string) error
}

// addWatch delegates adding a watch to the active kqueue backend
func (w *watcher) addWatch(watchPath *WatchPath) error {
	w.streamMu.Lock()
	platform, ok := w.streams["platform"].(platformNotifier)
	w.streamMu.Unlock()

	if !ok {
		return fmt.Errorf("unknown platform type")
	}

	if err := platform.addWatch(watchPath); err != nil {
		return err
	}

	return nil
}

// removeWatch delegates removing a watch to the active kqueue backend
func (w *watcher) removeWatch(path string) error {
	w.streamMu.Lock()
	platform, ok := w.streams["platform"].(platformNotifier)
	w.streamMu.Unlock()

	if !ok {
		return fmt.Errorf("unknown platform type")
	}

	if err := platform.removeWatch(path); err != nil {
		return fmt.Errorf("failed to remove watch for %s: %w", path, err)
	}

	return nil
}
