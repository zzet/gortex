//go:build darwin

package fswatcher

import (
	"fmt"
)

// Darwin system files used by isSystemFile
var osPrefixes = []string{"~", "._", ".DS_Store", ".localized", ".Trash", ".Trashes"}
var osSuffixes = []string{".tmp", ".bak", ".swp", ".swo", "~", ".plist", ".download", ".part"}

// platformNotifier defines the interface for Darwin backend implementations (FSEvents, kqueue)
type platformNotifier interface {
	addWatch(watchPath *WatchPath) error
	removeWatch(path string) error
}

// addWatch delegates adding a watch to the active Darwin backend
func (w *watcher) addWatch(watchPath *WatchPath) error {
	w.streamMu.Lock()
	platform, ok := w.streams["platform"].(platformNotifier)
	w.streamMu.Unlock()

	if !ok {
		return fmt.Errorf("unknown darwin platform type")
	}

	if err := platform.addWatch(watchPath); err != nil {
		return err
	}

	return nil
}

// removeWatch delegates removing a watch to the active Darwin backend
func (w *watcher) removeWatch(path string) error {
	w.streamMu.Lock()
	platform, ok := w.streams["platform"].(platformNotifier)
	w.streamMu.Unlock()

	if !ok {
		return fmt.Errorf("unknown darwin platform type")
	}

	if err := platform.removeWatch(path); err != nil {
		return fmt.Errorf("failed to remove watch for %s: %w", path, err)
	}

	return nil
}
