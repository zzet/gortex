package fswatcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// WatcherOpt is a function that configures a watcher instance
type WatcherOpt func(*watcher)

// WithCooldown sets the debouncing cooldown period, events on the same path arriving within this duration will be merged
func WithCooldown(d time.Duration) WatcherOpt {
	return func(w *watcher) {
		w.cooldown = d
	}
}

// WithBufferSize sets the size of the main event channel
func WithBufferSize(size int) WatcherOpt {
	return func(w *watcher) {
		w.bufferSize = size
	}
}

// WithReadBufferSize sets the size of the internal OS read buffer used by native backends.
func WithReadBufferSize(size int) WatcherOpt {
	return func(w *watcher) {
		w.readBufferSize = size
	}
}

// WithIncRegex sets the regex patterns for paths to include
// If no patterns are provided, all non-excluded paths are included
func WithIncRegex(patterns ...string) WatcherOpt {
	return func(w *watcher) {
		w.incRegexPatterns = patterns
	}
}

// WithExcRegex sets the regex patterns for paths to exclude; exclusions take precedence over inclusions
func WithExcRegex(patterns ...string) WatcherOpt {
	return func(w *watcher) {
		w.excRegexPatterns = patterns
	}
}

// WithCustomChannels allows providing external channels for events
func WithCustomChannels(events chan WatchEvent, dropped chan WatchEvent) WatcherOpt {
	return func(w *watcher) {
		if events != nil {
			w.events = events
			w.ownsEventsChannel = false
		}
		if dropped != nil {
			w.dropped = dropped
			w.ownsDroppedChannel = false
		}
	}
}

// WithReadyChannel provides a channel that is closed when the watcher is ready
func WithReadyChannel(ready chan struct{}) WatcherOpt {
	return func(w *watcher) {
		w.readyChan = ready
	}
}

// WithLogFile sets a file for logging; if a path is empty, logging is disabled; if "stdout", logs to standard output
func WithLogFile(path string) WatcherOpt {
	return func(w *watcher) {
		w.logPath = path
	}
}

// WithSeverity sets the logging verbosity (default is SeverityWarn)
func WithSeverity(level Severity) WatcherOpt {
	return func(w *watcher) {
		w.severity = level
	}
}

// WithPath adds an initial path to watch
func WithPath(path string, options ...PathOption) WatcherOpt {
	return func(w *watcher) {
		if w.init != nil {
			return
		}

		// Create a temporary WatchPath to apply options and get depth
		tempWp := &WatchPath{Path: path, Depth: WatchNested}
		for _, opt := range options {
			opt(tempWp)
		}

		// Validate the path using the new centralized function
		wp, err := validateWatchPath(path, tempWp.Depth)
		if err != nil {
			w.init = err
			return
		}

		// Preserve other options like filter and event mask that were set on tempWp
		wp.filter = tempWp.filter
		wp.eventMask = tempWp.eventMask

		// Build per-path pattern filter from regex patterns if no explicit filter was set
		if wp.filter == nil {
			f, err := buildWatchPathFilter(tempWp)
			if err != nil {
				w.init = err
				return
			}
			wp.filter = f
		}

		w.paths = append(w.paths, wp)
	}
}

// WatchDepth defines how deeply a directory structure should be watched
type WatchDepth int

const (
	WatchNested   = 0 // Watch the directory and all its subdirectories
	WatchTopLevel = 1 // Watch only the top-level directory
)

// WatchPath holds the configuration for a single watched path
type WatchPath struct {
	Path             string
	Depth            WatchDepth
	filter           PathFilter
	eventMask        map[EventType]bool
	incRegexPatterns []string
	excRegexPatterns []string
}

// PathOption is a function that configures a WatchPath
type PathOption func(*WatchPath)

// WithDepth sets the watch depth for a specific path
func WithDepth(depth WatchDepth) PathOption {
	return func(p *WatchPath) {
		p.Depth = depth
	}
}

// WithPathFilter sets a custom PathFilter for a specific watched path
func WithPathFilter(filter PathFilter) PathOption {
	return func(p *WatchPath) {
		p.filter = filter
	}
}

// WithPathIncRegex sets include regex patterns for a specific watched path
func WithPathIncRegex(patterns ...string) PathOption {
	return func(p *WatchPath) {
		p.incRegexPatterns = append(p.incRegexPatterns, patterns...)
	}
}

// WithPathExcRegex sets exclude regex patterns for a specific watched path
func WithPathExcRegex(patterns ...string) PathOption {
	return func(p *WatchPath) {
		p.excRegexPatterns = append(p.excRegexPatterns, patterns...)
	}
}

// WithEventMask restricts which event types are delivered for a specific watched path
// Only events whose type is in the mask will be forwarded to the Events() channel;
// all other types are silently dropped for that path
// If no mask is set (the default), all event types are delivered
//
// Example: only receive create and delete events for a directory:
//
//	fswatcher.WithPath("/var/data",
//	    fswatcher.WithEventMask(fswatcher.EventCreate, fswatcher.EventRemove),
//	)
func WithEventMask(eventTypes ...EventType) PathOption {
	return func(p *WatchPath) {
		if p.eventMask == nil {
			p.eventMask = make(map[EventType]bool)
		}
		for _, et := range eventTypes {
			p.eventMask[et] = true
		}
	}
}

// buildWatchPathFilter compiles regex patterns into a PathFilter for a WatchPath
func buildWatchPathFilter(wp *WatchPath) (PathFilter, error) {
	if len(wp.incRegexPatterns) == 0 && len(wp.excRegexPatterns) == 0 {
		return nil, nil
	}
	var inc, exc []*regexp.Regexp
	var err error
	if len(wp.incRegexPatterns) > 0 {
		inc, err = compileRegex(wp.incRegexPatterns)
		if err != nil {
			return nil, fmt.Errorf("path include regex: %w", err)
		}
	}
	if len(wp.excRegexPatterns) > 0 {
		exc, err = compileRegex(wp.excRegexPatterns)
		if err != nil {
			return nil, fmt.Errorf("path exclude regex: %w", err)
		}
	}
	return newPatternFilter(inc, exc), nil
}

// validateWatchPath creates and validates a WatchPath struct
func validateWatchPath(path string, depth WatchDepth) (*WatchPath, error) {
	if path == "" {
		return nil, newError("ValidateWatchPath", "", errors.New("path cannot be empty"))
	}

	var cleanPath string
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, newError("ValidateWatchPath", path, fmt.Errorf("failed to stat path: %w", err))
		}

		// Path does not exist, try from working directory
		cwd, getwdErr := os.Getwd()
		if getwdErr != nil {
			return nil, newError("ValidateWatchPath", "", fmt.Errorf("could not get working directory: %w", getwdErr))
		}
		cleanPath = filepath.Join(cwd, path)

		info, err = os.Stat(cleanPath)
		if err != nil {
			return nil, newError("ValidateWatchPath", path, errors.New("path does not exist as provided or relative to working directory"))
		}
	} else {
		cleanPath = path
	}

	if !info.IsDir() {
		return nil, newError("ValidateWatchPath", cleanPath, errors.New("path is not a directory"))
	}

	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return nil, newError("ValidateWatchPath", cleanPath, fmt.Errorf("failed to get absolute path: %w", err))
	}

	return &WatchPath{Path: absPath, Depth: depth}, nil
}

// PlatformLinux specifies which Linux backend to use
type PlatformLinux int

const (
	PlatformInotify PlatformLinux = iota
	PlatformFanotify
)

// WithLinuxPlatform sets a specific backend (fanotify or inotify) on Linux
func WithLinuxPlatform(platform PlatformLinux) WatcherOpt {
	return func(w *watcher) {
		w.platformLinux = platform
	}
}
