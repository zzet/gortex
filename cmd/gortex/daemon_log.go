package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

func resolveDaemonLogLimit(value *int, fallback, minimum int) (int, bool) {
	if value == nil {
		return fallback, false
	}
	if *value < minimum {
		return fallback, true
	}
	return *value, false
}

func newDaemonLogWriter(policy config.DaemonConfig, path string) (*lumberjack.Logger, []string) {
	maxSize, invalidSize := resolveDaemonLogLimit(policy.LogMaxSize, 50, 0)
	maxBackups, invalidBackups := resolveDaemonLogLimit(policy.LogMaxBackups, 3, 1)
	var warnings []string
	if invalidSize {
		warnings = append(warnings, "daemon.log_max_size must be at least 0; using 50")
	}
	if invalidBackups {
		warnings = append(warnings, "daemon.log_max_backups must be at least 1; using 3")
	}
	return &lumberjack.Logger{Filename: path, MaxSize: maxSize, MaxBackups: maxBackups}, warnings
}

// configureDaemonLogger uses log files only for inherited daemon log handles
// or Windows scheduled tasks started with --service-log.
func configureDaemonLogger(logger *zap.Logger, policy config.DaemonConfig, path string, stderr, stdout **os.File) (result *zap.Logger, closeLog func()) {
	writer, warnings := newDaemonLogWriter(policy, path)
	defer func() {
		for _, warning := range warnings {
			result.Warn("daemon: invalid log policy — using default", zap.String("reason", warning))
		}
	}()
	closeStderrLogger := func() { _ = logger.Sync() }
	previousStderr := *stderr
	prepareErr := prepareDaemonLogOutput(path, stderr, stdout)
	logger = refreshDaemonStderrLogger(logger, previousStderr, *stderr)
	if prepareErr != nil {
		logger.Warn("daemon: could not prepare stderr log — keeping stderr logging", zap.Error(prepareErr))
		return logger, closeStderrLogger
	}
	if writer.MaxSize == 0 {
		return logger, closeStderrLogger
	}
	// Lumberjack can rename a directory as though it were an old log. Refuse
	// non-regular paths before its lazy open, preserving the path on failure.
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("daemon: could not inspect rotating log — keeping stderr logging", zap.Error(err))
		return logger, closeStderrLogger
	}
	if err == nil && !info.Mode().IsRegular() {
		logger.Warn("daemon: rotating log is not a regular file — keeping stderr logging", zap.String("path", path))
		return logger, closeStderrLogger
	}
	// Check an existing log's writability without lumberjack: its first open
	// can rotate the log, so inherited descriptors must be redirected before
	// that open. A missing log is left for lumberjack to create.
	if err == nil {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			logger.Warn("daemon: could not open rotating log — keeping stderr logging", zap.Error(err))
			return logger, closeStderrLogger
		}
		if err := file.Close(); err != nil {
			logger.Warn("daemon: could not close rotating log probe — keeping stderr logging", zap.Error(err))
			return logger, closeStderrLogger
		}
	}
	crashPath := daemon.StderrLogPath(path)
	previousStderr = *stderr
	background, err := redirectDaemonLogFDs(path, crashPath, stderr, stdout)
	logger = refreshDaemonStderrLogger(logger, previousStderr, *stderr)
	if err != nil {
		logger.Warn("daemon: log rotation unavailable — keeping stderr logging", zap.Error(err))
		return logger, closeStderrLogger
	}
	if !background {
		return logger, closeStderrLogger
	}
	// Lumberjack opens lazily. A race can still make its open fail after the
	// writability check; keep diagnostics available through the stderr logger.
	if _, err := writer.Write(nil); err != nil {
		_ = writer.Close()
		logger.Warn("daemon: could not open rotating log — keeping stderr logging", zap.Error(err))
		return logger, closeStderrLogger
	}
	rotating := logger.WithOptions(zap.WrapCore(func(zapcore.Core) zapcore.Core {
		return newDaemonLogCore(zapcore.AddSync(writer))
	}))
	return rotating, func() {
		_ = rotating.Sync()
		if err := writer.Close(); err != nil {
			logger.Warn("daemon: could not close rotating log", zap.Error(err))
		}
	}
}

func logFileMatches(info os.FileInfo, path string) (bool, error) {
	other, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(info, other), nil
}

// redirectDaemonLogFDs handles both old service installs (daemon.log) and new
// launchers (daemon.stderr.log). File slots let Windows replace closed or absent
// handles, and let tests use private files rather than process std handles.
func redirectDaemonLogFDs(path, crashPath string, stderr, stdout **os.File) (bool, error) {
	stderrInfo, err := (*stderr).Stat()
	if err != nil && runtime.GOOS != "windows" {
		return false, fmt.Errorf("stat stderr: %w", err)
	}
	legacy, crash := false, false
	if err == nil && stderrInfo.Mode().IsRegular() {
		legacy, err = logFileMatches(stderrInfo, path)
		if err != nil {
			return false, fmt.Errorf("check daemon log: %w", err)
		}
		crash, err = logFileMatches(stderrInfo, crashPath)
		if err != nil {
			return false, fmt.Errorf("check daemon stderr log: %w", err)
		}
	} else if runtime.GOOS != "windows" {
		return false, nil
	}
	// Windows also redirects stdout-only daemon.log inheritance; Unix keys on stderr.
	if !legacy && !crash && runtime.GOOS != "windows" {
		return false, nil
	}
	stdoutLegacy := false
	if stdoutInfo, err := (*stdout).Stat(); err == nil {
		stdoutLegacy, err = logFileMatches(stdoutInfo, path)
		if err != nil {
			return false, fmt.Errorf("check stdout log: %w", err)
		}
	}
	if !legacy && !crash && !stdoutLegacy {
		return false, nil
	}
	if !legacy && !stdoutLegacy {
		return true, nil
	}
	output, err := openDaemonStderrLog(path, crashPath)
	if err != nil {
		return false, err
	}
	defer output.Close()
	if legacy && stdoutLegacy {
		if err := replaceDaemonLogFDPair(stderr, stdout, output); err != nil {
			return false, err
		}
		return true, nil
	}
	if legacy {
		if err := replaceDaemonLogFD(stderr, output); err != nil {
			return false, fmt.Errorf("redirect stderr: %w", err)
		}
	}
	if stdoutLegacy {
		if err := replaceDaemonLogFD(stdout, output); err != nil {
			return false, fmt.Errorf("redirect stdout (stderr is now %s): %w", crashPath, err)
		}
	}
	return true, nil
}

func openDaemonStderrLog(path, crashPath string) (*os.File, error) {
	// Opening a stale FIFO for writing would block before we could inspect it.
	// Stat follows a symlink, so a link to a regular file is still accepted.
	info, err := os.Stat(crashPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect daemon stderr log: %w", err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("daemon stderr log is not a regular file: %s", crashPath)
	}
	output, err := os.OpenFile(crashPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open daemon stderr log: %w", err)
	}
	outputInfo, err := output.Stat()
	if err != nil {
		_ = output.Close()
		return nil, fmt.Errorf("stat daemon stderr log: %w", err)
	}
	same, err := logFileMatches(outputInfo, path)
	if err != nil {
		_ = output.Close()
		return nil, fmt.Errorf("check redirected output: %w", err)
	}
	if same {
		_ = output.Close()
		return nil, fmt.Errorf("daemon stderr log must be separate from the rotating log")
	}
	return output, nil
}

func newDaemonLogCore(output zapcore.WriteSyncer) zapcore.Core {
	cfg := newLoggerConfig()
	core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg.EncoderConfig), output, cfg.Level)
	if sampling := cfg.Sampling; sampling != nil {
		core = zapcore.NewSamplerWithOptions(core, time.Second, sampling.Initial, sampling.Thereafter)
	}
	return core
}

func refreshDaemonStderrLogger(logger *zap.Logger, previous, current *os.File) *zap.Logger {
	if previous == current {
		return logger
	}
	// Zap captured the old *os.File. Keep fallback diagnostics on the new one.
	output := zapcore.AddSync(current)
	return logger.WithOptions(
		zap.WrapCore(func(zapcore.Core) zapcore.Core { return newDaemonLogCore(output) }),
		zap.ErrorOutput(output),
	)
}
