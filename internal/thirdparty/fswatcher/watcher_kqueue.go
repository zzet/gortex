//go:build (darwin && !cgo) || freebsd || openbsd || netbsd || dragonfly

package fswatcher

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// kqueue is the kernel queue-based implementation for macOS (without CGO) and BSD
type kqueue struct {
	w     *watcher
	kqFd  int             // file descriptor
	wds   map[int]string  // watch descriptors to paths
	paths map[string]int  // paths to watch descriptors
	dirs  map[string]bool // directories list
	mu    sync.Mutex
}

// newKqueue creates a new instance of kqueue
func newKqueue(w *watcher) (*kqueue, error) {
	kqFd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	return &kqueue{
		w:     w,
		kqFd:  kqFd,
		wds:   make(map[int]string),
		paths: make(map[string]int),
		dirs:  make(map[string]bool),
	}, nil
}

// startPlatform initializes the kqueue backend
func (w *watcher) startPlatform(ctx context.Context) (<-chan struct{}, error) {
	kq, err := newKqueue(w)
	if err != nil {
		return nil, fmt.Errorf("kqueue init failed: %w", err)
	}

	w.streams = make(map[string]any)
	w.streams["platform"] = kq

	done := make(chan struct{})

	go w.runKqueueLoop(ctx, kq, done)
	return done, nil
}

// runKqueueLoop starts the event loop
func (w *watcher) runKqueueLoop(ctx context.Context, k *kqueue, done chan struct{}) {
	// Closes all open descriptors and kqueue instance
	defer func() {
		w.logDebug("kqueue platform shutting down")
		k.mu.Lock()
		for fd := range k.wds {
			unix.Close(fd)
		}
		unix.Close(k.kqFd)
		k.wds = nil
		k.paths = nil
		k.dirs = nil
		k.mu.Unlock()
		close(done)
	}()

	// Init event buffer and timeout
	events := make([]unix.Kevent_t, w.bufferSize)
	ts := unix.NsecToTimespec(w.cooldown.Nanoseconds())
	timeout := &ts
	backoff := newBackoffState()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Polling
		n, err := unix.Kevent(k.kqFd, nil, events, timeout)
		if err != nil {
			// Interrupted signal
			if err == unix.EINTR {
				continue
			}

			if !w.handleLoopError("kqueue", err, backoff) {
				return
			}
			continue
		}

		// Success, reset backoff
		w.resetBackoff(backoff)

		// No events occurred, loop back
		if n == 0 {
			continue
		}

		// Include the 'n' events returned
		w.processKqueueEvents(k, events[:n])
	}
}

// processKqueueEvents handles events from the kqueue
func (w *watcher) processKqueueEvents(k *kqueue, events []unix.Kevent_t) {
	k.mu.Lock()
	defer k.mu.Unlock()

	dirsToScan := make(map[string]struct{})

	for _, event := range events {
		fd := int(event.Ident)
		path, ok := k.wds[fd]
		if !ok {
			continue
		}

		flags := event.Fflags
		// Map kqueue events flags
		var types []EventType
		if flags&unix.NOTE_DELETE == unix.NOTE_DELETE {
			types = append(types, EventRemove)
		}
		if flags&unix.NOTE_RENAME == unix.NOTE_RENAME {
			types = append(types, EventRename)
		}
		if flags&unix.NOTE_ATTRIB == unix.NOTE_ATTRIB {
			types = append(types, EventChmod)
		}
		if flags&unix.NOTE_WRITE == unix.NOTE_WRITE || flags&unix.NOTE_EXTEND == unix.NOTE_EXTEND {
			if k.dirs[path] {
				// Directory changed, mark for scan
				dirsToScan[path] = struct{}{}
			} else {
				types = append(types, EventMod)
			}
		}

		// Emit the event
		if len(types) > 0 {
			w.handlePlatformEvent(WatchEvent{
				Path:  path,
				Types: uniqueEventTypes(types),
				Time:  time.Now(),
			})
		}

		// Clean up if removed
		if flags&unix.NOTE_DELETE == unix.NOTE_DELETE || flags&unix.NOTE_RENAME == unix.NOTE_RENAME {
			delete(k.wds, fd)
			delete(k.paths, path)
			delete(k.dirs, path)
			unix.Close(fd)
		}
	}

	// Scan directories that had changes (deduplicated)
	for dirPath := range dirsToScan {
		w.scanDirectoryLocked(k, dirPath)
	}
}

// scanDirectoryLocked checks for new files in a watched dir
func (w *watcher) scanDirectoryLocked(k *kqueue, dirPath string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}

	var newEntries []struct {
		path  string
		isDir bool
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dirPath, entry.Name())
		if _, exists := k.paths[fullPath]; !exists {
			newEntries = append(newEntries, struct {
				path  string
				isDir bool
			}{fullPath, entry.IsDir()})
		}
	}

	if len(newEntries) > 0 {
		if err := k.batchAddWatchesLocked(newEntries); err == nil {
			w.logInfo("Detected and added new entries", "count", len(newEntries), "dir", dirPath)
			now := time.Now()
			for _, entry := range newEntries {
				// Emit create only for entries that were successfully registered
				if _, exists := k.paths[entry.path]; !exists {
					continue
				}
				w.handlePlatformEvent(WatchEvent{
					Path:  entry.path,
					Types: []EventType{EventCreate},
					Time:  now,
				})
			}
		} else {
			w.logWarn("Failed to add watches for new entries", "error", err)
		}
	}
}

// batchAddWatchesLocked adds multiple paths to the kqueue in one syscall
func (k *kqueue) batchAddWatchesLocked(entries []struct {
	path  string
	isDir bool
}) error {
	if len(entries) == 0 {
		return nil
	}

	changes := make([]unix.Kevent_t, 0, len(entries))
	// Keep track of what we successfully opened to update maps later
	added := make([]struct {
		fd    int
		path  string
		isDir bool
	}, 0, len(entries))

	for _, e := range entries {
		// double check existence in case of race or duplicates in input
		if _, exists := k.paths[e.path]; exists {
			continue
		}

		if isSystemFile(e.path) {
			continue
		}

		fd, err := unix.Open(e.path, kqueueOpenFlags, 0)
		if err != nil {
			// Try O_RDONLY
			if kqueueOpenFlags != unix.O_RDONLY {
				fd, err = unix.Open(e.path, unix.O_RDONLY, 0)
			}
			if err != nil {
				if err == unix.EMFILE || err == unix.ENFILE {
					k.w.logWarn("kqueue: reached open file limit (ulimit -n), consider increasing system limits", "path", e.path)
				}
				continue // Skip unreadable files
			}
		}

		changes = append(changes, unix.Kevent_t{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_VNODE,
			Flags:  unix.EV_ADD | unix.EV_CLEAR,
			Fflags: unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_ATTRIB | unix.NOTE_EXTEND,
		})
		added = append(added, struct {
			fd    int
			path  string
			isDir bool
		}{fd, e.path, e.isDir})
	}

	if len(changes) == 0 {
		return nil
	}

	// Register all in one syscall
	if _, err := unix.Kevent(k.kqFd, changes, nil, nil); err != nil {
		// If batch registration fails, close all FDs we just opened
		for _, a := range added {
			unix.Close(a.fd)
		}
		return err
	}

	// Update internal state
	for _, a := range added {
		k.wds[a.fd] = a.path
		k.paths[a.path] = a.fd
		if a.isDir {
			k.dirs[a.path] = true
		}
	}

	return nil
}

// addWatch starts a recursive watch
func (k *kqueue) addWatch(watchPath *WatchPath) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	return filepath.WalkDir(watchPath.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		return k.addSingleWatchLocked(path)
	})
}

// addSingleWatchLocked adds a file/dir
func (k *kqueue) addSingleWatchLocked(path string) error {
	if _, exists := k.paths[path]; exists {
		return nil
	}

	// Filter system files
	if isSystemFile(path) {
		return nil
	}

	fd, err := unix.Open(path, kqueueOpenFlags, 0)
	if err != nil {
		// Try O_RDONLY
		if kqueueOpenFlags != unix.O_RDONLY {
			fd, err = unix.Open(path, unix.O_RDONLY, 0)
		}
		if err != nil {
			if err == unix.EMFILE || err == unix.ENFILE {
				k.w.logWarn("kqueue: reached open file limit (ulimit -n), consider increasing system limits", "path", path)
			}
			return err
		}
	}
	events := []unix.Kevent_t{
		{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_VNODE,
			Flags:  unix.EV_ADD | unix.EV_CLEAR,
			Fflags: unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_ATTRIB | unix.NOTE_EXTEND,
		},
	}

	if _, err := unix.Kevent(k.kqFd, events, nil, nil); err != nil {
		unix.Close(fd)
		return err
	}

	k.wds[fd] = path
	k.paths[path] = fd

	fi, err := os.Stat(path)
	if err == nil && fi.IsDir() {
		k.dirs[path] = true
	}

	return nil
}

// removeWatch stops watching a given path
func (k *kqueue) removeWatch(path string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	cleanPath := filepath.Clean(path)
	var fdsToRemove []int

	for p, fd := range k.paths {
		if p == cleanPath || (len(p) > len(cleanPath) && p[len(cleanPath)] == filepath.Separator && p[:len(cleanPath)] == cleanPath) {
			fdsToRemove = append(fdsToRemove, fd)
		}
	}

	for _, fd := range fdsToRemove {
		p := k.wds[fd]
		delete(k.wds, fd)
		delete(k.paths, p)
		delete(k.dirs, p)
		unix.Close(fd)
	}

	return nil
}
