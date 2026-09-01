package fswatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Default values for watcher config
const (
	DefaultCooldown       = 100 * time.Millisecond
	DefaultBufferSize     = 4096
	DefaultReadBufferSize = 64 * 1024
)

// Limits and validation constants
const (
	MaxCooldownDuration   = 10 * time.Second
	MaxBufferSize         = 256 * 1024
	MaxReadBufferSize     = 1024 * 1024
	MinDroppedBuffer      = 16
	MaxDroppedBufferRatio = 4
)

// WatcherStats holds runtime statistics
type WatcherStats struct {
	StartTime       time.Time     // StartTime is when the watcher was created
	Uptime          time.Duration // Uptime is the duration the watcher has been running
	EventsProcessed int64         // EventsProcessed is the count of events sent to the Events channel
	EventsDropped   int64         // EventsDropped is the count of events sent to the Dropped channel
	EventsFiltered  int64         // EventsFiltered is the count of events ignored by filters or debouncing
	EventsLost      int64         // EventsLost is the count of events lost when all channels were full
	EventsPartial   int64         // EventsPartial is the count of partial backend registrations
	ProcessingRate  float64       // ProcessingRate is the number of events processed per second
}

// watcherStats holds internal, mutable statistics
type watcherStats struct {
	eventsProcessed int64
	eventsDropped   int64
	eventsFiltered  int64
	eventsLost      int64
	eventsPartial   int64
	startTime       time.Time
}

// watcher implements the Watcher interface
type watcher struct {
	// Atomic fields
	stats watcherStats

	// Atomic booleans
	isRunning      atomic.Bool
	isShuttingDown atomic.Bool
	isUsed         atomic.Bool

	// Configuration fields
	init               error
	cooldown           time.Duration
	bufferSize         int
	readBufferSize     int
	severity           Severity
	ownsEventsChannel  bool
	ownsDroppedChannel bool
	incRegexPatterns   []string
	excRegexPatterns   []string

	// Linux fields
	platformLinux PlatformLinux

	// Channels
	events    chan WatchEvent
	dropped   chan WatchEvent
	done      chan struct{}
	readyChan chan struct{}
	scanCtx   context.Context

	// Components
	aggregator *EventAggregator
	filter     PathFilter

	// Paths
	paths        []*WatchPath
	watchedPaths map[string]*WatchPath
	pathMu       sync.RWMutex
	opsMu        sync.Mutex

	// Synchronization and logs
	logMu   sync.Mutex
	logPath string
	logFile *os.File
	logger  *slog.Logger

	// Darwin fields
	handle   uintptr
	streams  map[string]any
	streamMu sync.Mutex
}

// Watcher defines the public interface for the file system watcher
type Watcher interface {
	// Watch starts the watcher and blocks until the context is canceled
	Watch(ctx context.Context) error
	// AddPath adds a new path to watch at runtime
	AddPath(path string, options ...PathOption) error
	// DropPath removes a path from watching at runtime
	DropPath(path string) error
	// Events returns the channel for receiving file system events
	Events() <-chan WatchEvent
	// Dropped returns the channel for receiving dropped events
	Dropped() <-chan WatchEvent
	// IsRunning returns true if the watcher is currently running
	IsRunning() bool
	// Stats returns the current watcher statistics
	Stats() WatcherStats
	// Paths returns a slice of the absolute paths currently being watched
	Paths() []string
	// Log handles leveled logging using the watcher's configured logger
	Log(level Severity, msg string, args ...any)
	// Close gracefully shuts down the watcher
	Close()
}

// New creates a new Watcher with the given options
func New(options ...WatcherOpt) (Watcher, error) {
	// Create a watcher with default values
	w := &watcher{
		done:               make(chan struct{}),
		cooldown:           DefaultCooldown,
		bufferSize:         DefaultBufferSize,
		readBufferSize:     DefaultReadBufferSize,
		ownsEventsChannel:  true,
		ownsDroppedChannel: true,
		severity:           SeverityWarn,
		watchedPaths:       make(map[string]*WatchPath),
	}

	// Apply user-provided options
	for _, opt := range options {
		if opt != nil {
			opt(w)
		}
	}

	// Check for any initialization errors from options
	if w.init != nil {
		return nil, w.init
	}

	if err := w.validateConfig(); err != nil {
		return nil, newError("config", "", err)
	}

	// If no path is specified, use the current working directory
	if len(w.paths) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, newError("resolveDefaultPath", "", err)
		}
		wp, err := validateWatchPath(cwd, WatchNested)
		if err != nil {
			return nil, err
		}
		w.paths = append(w.paths, wp)
	}

	// Add initial paths to the watchedPaths map
	for _, wp := range w.paths {
		w.watchedPaths[wp.Path] = wp
	}
	w.paths = nil

	// Create channels
	if w.events == nil {
		w.events = make(chan WatchEvent, w.bufferSize)
	}
	if w.dropped == nil {
		// Create a buffer for dropped events
		droppedSize := max(w.bufferSize/MaxDroppedBufferRatio, MinDroppedBuffer)
		w.dropped = make(chan WatchEvent, droppedSize)
	}

	// Initialize internal components
	var includePatterns, excludePatterns []*regexp.Regexp
	if len(w.incRegexPatterns) > 0 {
		var err error
		includePatterns, err = compileRegex(w.incRegexPatterns)
		if err != nil {
			return nil, newError("compileRegex", "include", err)
		}
	}
	if len(w.excRegexPatterns) > 0 {
		var err error
		excludePatterns, err = compileRegex(w.excRegexPatterns)
		if err != nil {
			return nil, newError("compileRegex", "exclude", err)
		}
	}

	w.filter = newPatternFilter(includePatterns, excludePatterns)
	w.aggregator = newEventAggregator(w, w.cooldown)
	w.stats.startTime = time.Now()
	return w, nil
}

// compileRegex compiles a slice of string patterns into a slice of *regexp.Regexp
func compileRegex(patterns []string) ([]*regexp.Regexp, error) {
	var compiled []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// Watch starts the main event loop for the watcher
func (w *watcher) Watch(ctx context.Context) (retErr error) {
	if !w.isUsed.CompareAndSwap(false, true) {
		return newError("start", "", errors.New("watcher is single-use; create a new watcher instance"))
	}

	if err := w.initLogger(); err != nil {
		return err
	}
	startup := false
	defer func() {
		if retErr == nil || startup {
			return
		}
		w.logMu.Lock()
		defer w.logMu.Unlock()
		if w.logFile != nil {
			_ = w.logFile.Close()
			w.logFile = nil
		}
	}()

	if w.severity <= SeverityDebug {
		w.logDebug("Watcher configuration",
			"cooldown", w.cooldown,
			"bufferSize", w.bufferSize)
		w.pathMu.RLock()
		paths := make([]string, 0, len(w.watchedPaths))
		for p := range w.watchedPaths {
			paths = append(paths, p)
		}
		w.pathMu.RUnlock()
		w.logDebug("Watcher paths", "paths", paths)
	}

	if !w.isRunning.CompareAndSwap(false, true) {
		return newError("start", "", errors.New("watcher is already running"))
	}
	defer w.isRunning.Store(false)

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.scanCtx = watchCtx

	go func() {
		select {
		case <-w.done:
		case <-ctx.Done():
		}
		cancel()
	}()

	// Start the platform-specific watching mechanism
	done, err := w.startPlatform(watchCtx)
	if err != nil {
		return err
	}

	// Start watching the initial set of paths synchronously
	// If any path fails to initialize, fail fast and return an error
	w.pathMu.RLock()
	initialPaths := make([]*WatchPath, 0, len(w.watchedPaths))
	for _, wp := range w.watchedPaths {
		initialPaths = append(initialPaths, wp)
	}
	w.pathMu.RUnlock()

	for _, watchPath := range initialPaths {
		if err := w.addWatch(watchPath); err != nil {
			cancel()
			<-done
			return newError("startWatch", watchPath.Path, err)
		}
	}

	if w.readyChan != nil {
		close(w.readyChan)
	}
	startup = true

	// Wait for the context to be canceled
	<-watchCtx.Done()

	// Wait for the platform watcher to shut down completely
	<-done

	w.logInfo("Watcher shutting down...")
	w.isShuttingDown.Store(true)

	if w.aggregator != nil {
		w.aggregator.close()
	}

	if w.ownsEventsChannel {
		close(w.events)
	}
	if w.ownsDroppedChannel {
		close(w.dropped)
	}

	w.logInfo("Watcher shutdown completed")

	if w.logFile != nil {
		if err := w.logFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "fswatcher: error closing log file: %v\n", err)
		}
	}
	return nil
}

// AddPath adds a new directory to watch at runtime
func (w *watcher) AddPath(path string, options ...PathOption) error {
	if !w.IsRunning() {
		return newError("AddPath", path, errors.New("watcher is not running"))
	}
	w.opsMu.Lock()
	defer w.opsMu.Unlock()

	// Create a temporary WatchPath to apply options
	watchPath := &WatchPath{Path: path, Depth: WatchNested}
	for _, opt := range options {
		opt(watchPath)
	}

	wp, err := validateWatchPath(watchPath.Path, watchPath.Depth)
	if err != nil {
		return err
	}
	// Preserve the configured filter and event mask
	wp.filter = watchPath.filter
	wp.eventMask = watchPath.eventMask

	// Build per-path pattern filter from regex patterns if no explicit filter was set
	if wp.filter == nil {
		f, err := buildWatchPathFilter(watchPath)
		if err != nil {
			return err
		}
		wp.filter = f
	}

	w.pathMu.Lock()
	if _, exists := w.watchedPaths[wp.Path]; exists {
		w.pathMu.Unlock()
		return newError("AddPath", wp.Path, errors.New("path is already being watched"))
	}
	// Reserve first so concurrent AddPath calls for the same path are deterministic
	w.watchedPaths[wp.Path] = wp
	w.pathMu.Unlock()

	if err := w.addWatch(wp); err != nil {
		w.pathMu.Lock()
		delete(w.watchedPaths, wp.Path)
		w.pathMu.Unlock()
		return err
	}

	w.logInfo("Successfully added new watch path", "path", wp.Path, "depth", wp.Depth)
	return nil
}

// DropPath stops monitoring a path at runtime
func (w *watcher) DropPath(path string) error {
	if !w.IsRunning() {
		return newError("DropPath", path, errors.New("watcher is not running"))
	}
	w.opsMu.Lock()
	defer w.opsMu.Unlock()

	wp, err := validateWatchPath(path, WatchNested) // Depth doesn't matter for removal
	if err != nil {
		return err
	}

	w.pathMu.Lock()
	if _, exists := w.watchedPaths[wp.Path]; !exists {
		w.pathMu.Unlock()
		return newError("DropPath", path, errors.New("path is not being watched"))
	}
	w.pathMu.Unlock()

	if err := w.removeWatch(wp.Path); err != nil {
		w.logError("Platform backend failed to remove watch", "path", wp.Path, "error", err)
		return err
	}

	w.pathMu.Lock()
	delete(w.watchedPaths, wp.Path)
	w.pathMu.Unlock()

	w.logInfo("Successfully removed watch path", "path", wp.Path)
	return nil
}

// Events returns the event channel
func (w *watcher) Events() <-chan WatchEvent {
	return w.events
}

// Dropped returns the dropped event channel
func (w *watcher) Dropped() <-chan WatchEvent { return w.dropped }

// IsRunning returns the current running state of the watcher
func (w *watcher) IsRunning() bool { return w.isRunning.Load() }

// Stats returns the current watcher statistics
func (w *watcher) Stats() WatcherStats {
	stats := WatcherStats{
		StartTime:       w.stats.startTime,
		EventsProcessed: atomic.LoadInt64(&w.stats.eventsProcessed),
		EventsDropped:   atomic.LoadInt64(&w.stats.eventsDropped),
		EventsFiltered:  atomic.LoadInt64(&w.stats.eventsFiltered),
		EventsLost:      atomic.LoadInt64(&w.stats.eventsLost),
		EventsPartial:   atomic.LoadInt64(&w.stats.eventsPartial),
	}

	// Add calculated fields
	uptime := time.Since(stats.StartTime)
	stats.Uptime = uptime

	if uptime.Seconds() > 0 {
		stats.ProcessingRate = float64(stats.EventsProcessed) / uptime.Seconds()
	}

	return stats
}

// Paths returns a slice of the absolute paths currently being watched
func (w *watcher) Paths() []string {
	w.pathMu.RLock()
	defer w.pathMu.RUnlock()

	paths := make([]string, 0, len(w.watchedPaths))
	for path := range w.watchedPaths {
		paths = append(paths, path)
	}
	return paths
}

// Log handles leveled logging
func (w *watcher) Log(level Severity, msg string, args ...any) {
	switch level {
	case SeverityError:
		w.logError(msg, args...)
	case SeverityWarn:
		w.logWarn(msg, args...)
	case SeverityInfo:
		w.logInfo(msg, args...)
	case SeverityDebug:
		w.logDebug(msg, args...)
	default:
		if w.logger != nil {
			w.logger.Log(context.Background(), slog.Level(level), msg, args...)
		}
	}
}

// Close gracefully shuts down the watcher
func (w *watcher) Close() {
	if !w.IsRunning() {
		return
	}
	if w.isShuttingDown.CompareAndSwap(false, true) {
		w.logInfo("Close called, initiating shutdown...")
		close(w.done)
	}
}

// validateConfig checks if the watcher configuration is valid
func (w *watcher) validateConfig() error {
	if w.bufferSize <= 0 || w.bufferSize > MaxBufferSize {
		return fmt.Errorf("buffer size must be between 1 and %d", MaxBufferSize)
	}
	if w.readBufferSize <= 0 || w.readBufferSize > MaxReadBufferSize {
		return fmt.Errorf("read buffer size must be between 1 and %d", MaxReadBufferSize)
	}
	if w.cooldown < 0 || w.cooldown > MaxCooldownDuration {
		return fmt.Errorf("cooldown must be between 0 and %v", MaxCooldownDuration)
	}
	return nil
}

// initLogger initializes the logger based on logPath
func (w *watcher) initLogger() error {
	w.logMu.Lock()
	defer w.logMu.Unlock()

	if w.severity == SeverityNone || w.logPath == "" {
		w.logger = nil
		w.logFile = nil
		return nil
	}

	opts := &slog.HandlerOptions{
		Level: slog.Level(w.severity),
	}

	var handler slog.Handler
	if w.logPath == "stdout" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		file, err := os.OpenFile(w.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return newError("initLogger", w.logPath, fmt.Errorf("failed to open log file: %w", err))
		}
		w.logFile = file
		handler = slog.NewTextHandler(file, opts)
	}

	w.logger = slog.New(handler)
	return nil
}

// sendToChannel sends an event to the appropriate channel
func (w *watcher) sendToChannel(event WatchEvent) {
	w.logDebug("Raw event",
		"id", event.ID,
		"path", event.Path,
		"types", event.Types,
		"flags", event.Flags,
		"time", event.Time)
	select {
	case w.events <- event:
		atomic.AddInt64(&w.stats.eventsProcessed, 1)
	default:
		// Main channel is full, try the dropped channel
		select {
		case w.dropped <- event:
			atomic.AddInt64(&w.stats.eventsDropped, 1)
			w.logWarn("Event channel full, event sent to dropped channel",
				"id", event.ID,
				"path", event.Path,
				"types", event.Types)
		default:
			// Both channels are full, the event is lost
			atomic.AddInt64(&w.stats.eventsLost, 1)
			w.logWarn("Event and dropped channels full, event lost",
				"id", event.ID,
				"path", event.Path,
				"types", event.Types)
		}
	}
}

// scanDirectory performs a manual scan of a directory and emits events for its contents
func (w *watcher) scanDirectory(path string) {
	if w.isShuttingDown.Load() {
		return
	}

	w.logInfo("Starting manual scan of directory", "path", path)

	ctx := w.scanCtx
	if ctx == nil {
		ctx = context.Background()
	}

	const scanYieldEvery = 512
	scanned := 0
	err := filepath.WalkDir(path, func(filePath string, d os.DirEntry, err error) error {
		if ctx.Err() != nil || w.isShuttingDown.Load() {
			return filepath.SkipAll
		}
		if err != nil {
			return nil // Skip files with errors
		}

		// Don't emit event for the root path being scanned
		if filePath == path {
			return nil
		}

		scanned++
		if scanned%scanYieldEvery == 0 {
			select {
			case <-ctx.Done():
				return filepath.SkipAll
			default:
				time.Sleep(time.Millisecond)
			}
		}

		// Emit a generic Mod event for everything found
		// This ensures consumers are notified that something changed
		w.handlePlatformEvent(WatchEvent{
			Path:  filePath,
			Types: []EventType{EventMod},
			Time:  time.Now(),
		})

		// If it's a directory and we are not recursive, don't walk into it
		if d.IsDir() {
			parentWatch := w.findMostSpecificWatchPath(filePath)

			if parentWatch != nil && parentWatch.Depth == WatchTopLevel {
				return filepath.SkipDir
			}
		}

		return nil
	})

	if err != nil {
		w.logError("Manual scan failed", "path", path, "error", err)
	} else {
		w.logInfo("Manual scan completed", "path", path)
	}
}

// backoffState maintains the retry state for a platform loop.
//
//nolint:unused // Used by Linux, Windows, and kqueue backend builds.
type backoffState struct {
	retryCount int
	duration   time.Duration
}

// newBackoffState creates a new backoffState with default values
//
//nolint:unused // Used by Linux, Windows, and kqueue backend builds.
func newBackoffState() *backoffState {
	return &backoffState{
		duration: 10 * time.Millisecond,
	}
}

// handleLoopError applies exponential backoff and returns false if retries are exhausted
//
//nolint:unused // Used by Linux, Windows, and kqueue backend builds.
func (w *watcher) handleLoopError(platform string, err error, state *backoffState) bool {
	const maxRetries = 5
	w.logError("Platform loop error", "platform", platform, "error", err)
	state.retryCount++
	if state.retryCount > maxRetries {
		w.logError("Too many platform loop errors, shutting down", "platform", platform, "retries", state.retryCount)
		return false
	}
	time.Sleep(state.duration)
	state.duration *= 2
	return true
}

// resetBackoff resets the retry state on success
//
//nolint:unused // Used by Linux, Windows, and kqueue backend builds.
func (w *watcher) resetBackoff(state *backoffState) {
	state.retryCount = 0
	state.duration = 10 * time.Millisecond
}

// findMostSpecificWatchPath resolves the most specific watched root for a path
func (w *watcher) findMostSpecificWatchPath(path string) *WatchPath {
	w.pathMu.RLock()
	defer w.pathMu.RUnlock()

	var parentWatch *WatchPath
	bestMatchLen := -1

	for watchedDir, wp := range w.watchedPaths {
		if path == watchedDir || isSubpath(watchedDir, path) {
			if n := len(watchedDir); n > bestMatchLen {
				bestMatchLen = n
				parentWatch = wp
			}
		}
	}

	return parentWatch
}

// handlePlatformEvent processes raw events from the OS
func (w *watcher) handlePlatformEvent(event WatchEvent) {
	if w.isShuttingDown.Load() {
		return
	}

	// Overflow events have no path and must bypass all path-based filtering
	if slices.Contains(event.Types, EventOverflow) {
		w.aggregator.addEvent(event)
		return
	}

	// Platform-agnostic depth filtering
	parentWatch := w.findMostSpecificWatchPath(event.Path)

	// If the parent watch is configured for top-level only, check the depth
	if parentWatch != nil && parentWatch.Depth == WatchTopLevel {
		// Get the directory of the event path
		eventDir := filepath.Dir(event.Path)
		// If the event's directory is not the same as the watched path, it's a subdirectory event so will be ignored
		if filepath.Clean(eventDir) != filepath.Clean(parentWatch.Path) {
			w.logDebug("Filtered by depth", "path", event.Path)
			atomic.AddInt64(&w.stats.eventsFiltered, 1)
			return
		}
	}

	w.logDebug("Received event",
		"id", event.ID,
		"path", event.Path,
		"types", event.Types,
		"flags", event.Flags,
		"time", event.Time)

	// Filter system files
	if isSystemFile(event.Path) {
		w.logDebug("Filtered system file", "path", event.Path)
		atomic.AddInt64(&w.stats.eventsFiltered, 1)
		return
	}

	// Filter by global patterns
	if !w.filter.ShouldInclude(event.Path) {
		w.logDebug("Filtered by pattern", "path", event.Path)
		atomic.AddInt64(&w.stats.eventsFiltered, 1)
		return
	}

	// Filter by per-path patterns
	if parentWatch != nil && parentWatch.filter != nil && !parentWatch.filter.ShouldInclude(event.Path) {
		w.logDebug("Filtered by path pattern", "path", event.Path)
		atomic.AddInt64(&w.stats.eventsFiltered, 1)
		return
	}

	// Filter by per-path event mask
	if parentWatch != nil && len(parentWatch.eventMask) > 0 {
		filtered := event.Types[:0]
		for _, t := range event.Types {
			if parentWatch.eventMask[t] {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			w.logDebug("Filtered by event mask", "path", event.Path)
			atomic.AddInt64(&w.stats.eventsFiltered, 1)
			return
		}
		event.Types = filtered
	}

	// Aggregate
	w.aggregator.addEvent(event)
}
