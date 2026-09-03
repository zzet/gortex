//go:build linux

package fswatcher

import (
	"context"
	"fmt"
)

// Linux system files used by isSystemFile
var osPrefixes = []string{"~", "#", ".#"}
var osSuffixes = []string{".tmp", ".bak", ".swp", ".save", ".old", ".swo", "~"}

// platformNotifier defines the interface for Linux backend implementations (inotify, fanotify)
type platformNotifier interface {
	addWatch(w *watcher, watchPath *WatchPath) error
	removeWatch(path string) error
}

// startPlatform initializes and runs the Linux-specific watcher backend
func (w *watcher) startPlatform(ctx context.Context) (<-chan struct{}, error) {
	w.streams = make(map[string]any)

	var (
		platform any
		runLoop  func(context.Context, any, chan struct{})
		err      error
		errMsg   string
	)

	switch w.platformLinux {
	case PlatformFanotify:
		platform, err = newFanotify()
		runLoop = func(ctx context.Context, p any, done chan struct{}) {
			w.runFanotifyLoop(ctx, p.(*fanotify), done)
		}
		errMsg = "fanotify backend failed to initialize"
	default: // Default to inotify
		platform, err = newInotify()
		runLoop = func(ctx context.Context, p any, done chan struct{}) {
			w.runInotifyLoop(ctx, p.(*inotify), done)
		}
		errMsg = "inotify backend failed to initialize"
	}

	if err != nil {
		return nil, fmt.Errorf("%s: %w", errMsg, err)
	}
	w.streams["platform"] = platform

	done := make(chan struct{})
	go runLoop(ctx, platform, done)
	return done, nil
}

// addWatch delegates adding a watch to the active Linux backend
func (w *watcher) addWatch(watchPath *WatchPath) error {
	platform, ok := w.streams["platform"].(platformNotifier)
	if !ok {
		return fmt.Errorf("unknown linux platform type")
	}

	if err := platform.addWatch(w, watchPath); err != nil {
		return err
	}

	w.logInfo("Added watch", "path", watchPath.Path)
	return nil
}

// removeWatch delegates removing a watch to the active Linux backend
func (w *watcher) removeWatch(path string) error {
	platform, ok := w.streams["platform"].(platformNotifier)
	if !ok {
		return fmt.Errorf("unknown linux platform type")
	}

	if err := platform.removeWatch(path); err != nil {
		return fmt.Errorf("failed to remove watch for %s: %w", path, err)
	}

	w.logInfo("Platform stopped watching path", "path", path)
	return nil
}
