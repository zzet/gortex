package fswatcher

import "fmt"

// WatcherError is a structured error for watcher-related failures
type WatcherError struct {
	Op   string
	Path string
	Err  error
}

// newError creates a new WatcherError
func newError(op, path string, err error) *WatcherError {
	return &WatcherError{Op: op, Path: path, Err: err}
}

// Error returns a formatted error string
func (e *WatcherError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("watcher %s %s: %v", e.Op, e.Path, e.Err)
	}
	return fmt.Sprintf("watcher %s: %v", e.Op, e.Err)
}

// Unwrap returns the underlying error for error chaining
func (e *WatcherError) Unwrap() error {
	return e.Err
}
