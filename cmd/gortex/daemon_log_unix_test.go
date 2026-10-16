//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRedirectDaemonLogFDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
	stderr, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer stderr.Close()
	stdout, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer stdout.Close()
	background, err := redirectDaemonLogFDs(path, daemon.StderrLogPath(path), &stderr, &stdout)
	require.NoError(t, err)
	require.True(t, background)
	crashPath := filepath.Join(filepath.Dir(path), "daemon.stderr.log")
	crashInfo, err := os.Stat(crashPath)
	require.NoError(t, err)
	for _, fd := range []*os.File{stderr, stdout} {
		info, err := fd.Stat()
		require.NoError(t, err)
		assert.True(t, os.SameFile(crashInfo, info))
	}
	_, err = stderr.WriteString("stderr")
	require.NoError(t, err)
	_, err = stdout.WriteString("stdout")
	require.NoError(t, err)
	data, err := os.ReadFile(crashPath)
	require.NoError(t, err)
	assert.Equal(t, "stderrstdout", string(data))
	old, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old log", string(old))
}

func TestRedirectDaemonLogFDsSymlinkedStderrLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
	target := filepath.Join(dir, "crash-target.log")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	crashPath := daemon.StderrLogPath(path)
	require.NoError(t, os.Symlink(target, crashPath))
	stderr, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer stderr.Close()
	stdout, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer stdout.Close()
	background, err := redirectDaemonLogFDs(path, crashPath, &stderr, &stdout)
	require.NoError(t, err)
	require.True(t, background)
	_, err = stderr.WriteString("stderr")
	require.NoError(t, err)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "stderr", string(data))
}

func TestRedirectDaemonLogFDsUnrelated(t *testing.T) {
	for _, kind := range []string{"pipe", "other_file"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")
			require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
			var stderr *os.File
			if kind == "pipe" {
				r, w, err := os.Pipe()
				require.NoError(t, err)
				t.Cleanup(func() { _ = r.Close() })
				stderr = w
			} else {
				var err error
				stderr, err = os.Create(filepath.Join(dir, "other.log"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { _ = stderr.Close() })
			before, err := stderr.Stat()
			require.NoError(t, err)
			background, err := redirectDaemonLogFDs(path, daemon.StderrLogPath(path), &stderr, &stderr)
			require.NoError(t, err)
			assert.False(t, background)
			after, err := stderr.Stat()
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after))
			_, err = os.Stat(filepath.Join(dir, "daemon.stderr.log"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestDaemonLogRedirectFailureKeepsStderr(t *testing.T) {
	for _, kind := range []string{"directory", "same_file", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
			stderr, err := os.OpenFile(path, os.O_WRONLY, 0600)
			require.NoError(t, err)
			defer stderr.Close()
			crashPath := filepath.Join(filepath.Dir(path), "daemon.stderr.log")
			switch kind {
			case "directory":
				require.NoError(t, os.Mkdir(crashPath, 0700))
			case "fifo":
				if err := syscall.Mkfifo(crashPath, 0600); err != nil {
					if err == syscall.ENOSYS || err == syscall.EOPNOTSUPP {
						t.Skipf("FIFO unsupported: %v", err)
					}
					require.NoError(t, err)
				}
			default:
				require.NoError(t, os.Link(path, crashPath))
			}
			core, logs := observer.New(zap.InfoLevel)
			base := zap.New(core)
			logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stderr)
			defer closeLog()
			assert.Same(t, base, logger)
			require.Equal(t, 1, logs.Len())
			assert.Equal(t, zap.WarnLevel, logs.All()[0].Level)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "old log", string(data))
		})
	}
}

func TestConfigureDaemonLoggerLegacyFD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("{\"msg\":\"old\"}\n"), 0600))
	stderr, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	logger, closeLog := configureDaemonLogger(stderrTestLogger(stderr), config.DaemonConfig{
		LogMaxSize: new(1), LogMaxBackups: new(0),
	}, path, &stderr, &stderr)
	t.Cleanup(closeLog)
	logger.Info("event")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	require.Len(t, lines, 3)
	var warning, entry map[string]any
	require.NoError(t, json.Unmarshal(lines[1], &warning))
	assert.Equal(t, "warn", warning["level"])
	assert.Contains(t, warning["reason"], "daemon.log_max_backups")
	require.NoError(t, json.Unmarshal(lines[2], &entry))
	assert.Equal(t, "event", entry["msg"])
	crashes, err := os.ReadFile(daemon.StderrLogPath(path))
	require.NoError(t, err)
	assert.Empty(t, crashes, "policy warnings and JSON events must use the final logger")
}

func TestRedirectDaemonLogFDsMixed(t *testing.T) {
	for _, kind := range []string{"new_stderr_old_stdout", "old_stderr_pipe_stdout", "closed_stdout"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			crashPath := daemon.StderrLogPath(path)
			require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
			stderrPath := path
			if kind == "new_stderr_old_stdout" {
				stderrPath = crashPath
			}
			stderr, err := os.OpenFile(stderrPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			var stdout, pipeReader *os.File
			if kind == "old_stderr_pipe_stdout" {
				pipeReader, stdout, err = os.Pipe()
				require.NoError(t, err)
				t.Cleanup(func() { _ = pipeReader.Close() })
			} else {
				stdout, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
			}
			t.Cleanup(func() { _ = stdout.Close() })
			stderrBefore, err := stderr.Stat()
			require.NoError(t, err)
			stdoutBefore, err := stdout.Stat()
			require.NoError(t, err)
			if kind == "closed_stdout" {
				require.NoError(t, stdout.Close())
			}
			background, err := redirectDaemonLogFDs(path, crashPath, &stderr, &stdout)
			require.NoError(t, err)
			require.True(t, background)
			crashInfo, err := os.Stat(crashPath)
			require.NoError(t, err)
			stderrAfter, err := stderr.Stat()
			require.NoError(t, err)
			assert.True(t, os.SameFile(crashInfo, stderrAfter))
			switch kind {
			case "new_stderr_old_stdout":
				assert.True(t, os.SameFile(stderrBefore, stderrAfter), "stderr was already redirected")
				stdoutAfter, err := stdout.Stat()
				require.NoError(t, err)
				assert.True(t, os.SameFile(crashInfo, stdoutAfter))
			case "old_stderr_pipe_stdout":
				stdoutAfter, err := stdout.Stat()
				require.NoError(t, err)
				assert.True(t, os.SameFile(stdoutBefore, stdoutAfter))
				_, err = stdout.WriteString("pipe")
				require.NoError(t, err)
				var buf [4]byte
				_, err = io.ReadFull(pipeReader, buf[:])
				require.NoError(t, err)
				assert.Equal(t, "pipe", string(buf[:]))
			case "closed_stdout":
				_, err := stdout.Stat()
				assert.ErrorIs(t, err, os.ErrClosed)
			}
		})
	}
}

func TestConfigureDaemonLoggerNonRegularPathKeepsFDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.Mkdir(path, 0700))
	crashPath := daemon.StderrLogPath(path)
	stderr, err := os.OpenFile(crashPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer stderr.Close()
	// This private directory descriptor would be redirected if the path check
	// happened after redirectDaemonLogFDs.
	stdout, err := os.Open(path)
	require.NoError(t, err)
	defer stdout.Close()
	stderrBefore, err := stderr.Stat()
	require.NoError(t, err)
	stdoutBefore, err := stdout.Stat()
	require.NoError(t, err)
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stdout)
	defer closeLog()
	assert.Same(t, base, logger)
	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zap.WarnLevel, logs.All()[0].Level)
	assert.Contains(t, logs.All()[0].Message, "not a regular file")
	stdoutInfo, err := stdout.Stat()
	require.NoError(t, err)
	assert.True(t, os.SameFile(stdoutBefore, stdoutInfo), "stdout must not be redirected")
	stderrInfo, err := stderr.Stat()
	require.NoError(t, err)
	assert.True(t, os.SameFile(stderrBefore, stderrInfo), "stderr must not be redirected")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "probe must not rename or replace the directory")
}

func TestConfigureDaemonLoggerUnwritablePathKeepsFDs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write read-only paths")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("old log"), 0600))
	require.NoError(t, os.WriteFile(daemon.StderrLogPath(path), nil, 0600))
	stderr, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	stdout, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdout.Close() })
	before, err := os.Stat(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	require.NoError(t, os.Chmod(path, 0400))
	require.NoError(t, os.Chmod(dir, 0500))

	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stdout)
	defer closeLog()
	assert.Same(t, base, logger)
	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zap.WarnLevel, logs.All()[0].Level)
	assert.Contains(t, logs.All()[0].Message, "could not open rotating log")
	for _, fd := range []*os.File{stderr, stdout} {
		after, err := fd.Stat()
		require.NoError(t, err)
		assert.True(t, os.SameFile(before, after), "descriptor must not be redirected")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old log", string(data))
}

func TestConfigureDaemonLoggerServiceFlagIgnored(t *testing.T) {
	previous := daemonServiceLog
	daemonServiceLog = true
	t.Cleanup(func() { daemonServiceLog = previous })
	dir := t.TempDir()
	stderr, err := os.Create(filepath.Join(dir, "capture.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	base := stderrTestLogger(stderr)
	path := filepath.Join(dir, "daemon.log")
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stderr)
	t.Cleanup(closeLog)
	assert.Same(t, base, logger)
	logger.Info("foreground")
	data, err := os.ReadFile(stderr.Name())
	require.NoError(t, err)
	assert.Contains(t, string(data), `"msg":"foreground"`)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(daemon.StderrLogPath(path))
	assert.True(t, os.IsNotExist(err))
}
