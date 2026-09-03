package fswatcher

import (
	"log/slog"
)

// Severity defines the logging verbosity
type Severity int

// Logging levels mapping to slog.Level
const (
	SeverityNone  Severity = -8
	SeverityError Severity = Severity(slog.LevelError)
	SeverityWarn  Severity = Severity(slog.LevelWarn)
	SeverityInfo  Severity = Severity(slog.LevelInfo)
	SeverityDebug Severity = Severity(slog.LevelDebug)
)

// logError logs an Error level message
func (w *watcher) logError(msg string, args ...any) {
	if w.logger != nil {
		w.logger.Error(msg, args...)
	}
}

// logWarn logs a Warn level message
func (w *watcher) logWarn(msg string, args ...any) {
	if w.logger != nil {
		w.logger.Warn(msg, args...)
	}
}

// logInfo logs an Info level message
func (w *watcher) logInfo(msg string, args ...any) {
	if w.logger != nil {
		w.logger.Info(msg, args...)
	}
}

// logDebug logs a Debug level message
func (w *watcher) logDebug(msg string, args ...any) {
	if w.logger != nil {
		w.logger.Debug(msg, args...)
	}
}
