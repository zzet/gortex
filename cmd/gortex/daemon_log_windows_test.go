//go:build windows

package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/windows"
)

func TestWindowsDaemonLogRedirectClosesOldHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	target, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	original := target
	stdlibOutput := log.Writer()
	crashPath := daemon.StderrLogPath(path)
	source, err := os.OpenFile(crashPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer source.Close()
	_, err = source.WriteString("before\n")
	require.NoError(t, err)
	processStderr, processStdout := os.Stderr, os.Stdout
	require.NoError(t, replaceDaemonLogFD(&target, source))
	assert.Same(t, stdlibOutput, log.Writer(), "private file slots must not change the stdlib logger")
	_, err = original.WriteString("must be closed")
	require.ErrorIs(t, err, os.ErrClosed)
	assert.Same(t, processStderr, os.Stderr)
	assert.Same(t, processStdout, os.Stdout)
	// The duplicate must remain valid after redirectDaemonLogFDs closes source.
	require.NoError(t, source.Close())
	_, err = target.WriteString("after\n")
	require.NoError(t, err)
	data, err := os.ReadFile(crashPath)
	require.NoError(t, err)
	assert.Equal(t, "before\nafter\n", string(data))
	old, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Empty(t, old)
}

func TestWindowsDaemonLogRotatesAfterRedirect(t *testing.T) {
	for _, kind := range []string{"separate_handles", "shared_file", "shared_handle"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			old := strings.Repeat("x", (1<<20)-128)
			require.NoError(t, os.WriteFile(path, []byte(old), 0600))
			stderr, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			stdout := stderr
			switch kind {
			case "separate_handles":
				stdout, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
			case "shared_handle":
				stdout = os.NewFile(stderr.Fd(), path)
			}
			t.Cleanup(func() { _ = stdout.Close() })
			originalStdout := stdout
			retained := len(daemonLogRetainedFiles)
			logger, closeLog := configureDaemonLogger(stderrTestLogger(stderr), config.DaemonConfig{
				LogMaxSize: new(1),
			}, path, &stderr, &stdout)
			t.Cleanup(closeLog)
			if kind == "shared_handle" {
				require.Len(t, daemonLogRetainedFiles, retained+1)
				assert.Same(t, originalStdout, daemonLogRetainedFiles[retained])
			} else {
				assert.Len(t, daemonLogRetainedFiles, retained)
			}
			logger.Info("rotated", zap.String("payload", strings.Repeat("y", 256)))
			backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "daemon-*.log"))
			require.NoError(t, err)
			require.Len(t, backups, 1, "rotation must rename daemon.log after both inherited handles close")
			backup, err := os.ReadFile(backups[0])
			require.NoError(t, err)
			assert.Equal(t, old, string(backup))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(data), `"msg":"rotated"`)
			_, err = stderr.WriteString("stderr\n")
			require.NoError(t, err)
			_, err = stdout.WriteString("stdout\n")
			require.NoError(t, err)
			crashes, err := os.ReadFile(daemon.StderrLogPath(path))
			require.NoError(t, err)
			assert.Equal(t, "stderr\nstdout\n", string(crashes))
		})
	}
}

func TestWindowsDaemonLogServiceUsesBackground(t *testing.T) {
	previous := daemonServiceLog
	daemonServiceLog = true
	t.Cleanup(func() { daemonServiceLog = previous })
	for _, kind := range []string{"file", "pipe", "nul", "absent", "invalid", "closed"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "logs", "daemon.log")
			var stderr *os.File
			var err error
			switch kind {
			case "file", "closed":
				stderr, err = os.Create(filepath.Join(dir, "other.log"))
			case "pipe":
				var reader *os.File
				reader, stderr, err = os.Pipe()
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Close() })
			case "nul":
				stderr, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			case "absent":
				stderr = os.NewFile(0, "absent")
			case "invalid":
				stderr = os.NewFile(uintptr(windows.InvalidHandle), "invalid")
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			if kind == "closed" {
				require.NoError(t, stderr.Close())
			}
			base := stderrTestLogger(stderr)
			logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stderr)
			t.Cleanup(closeLog)
			assert.NotSame(t, base, logger)
			logger.Info("task event")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(data), `"msg":"task event"`)
			_, err = stderr.WriteString("raw output\n")
			require.NoError(t, err)
			crashes, err := os.ReadFile(daemon.StderrLogPath(path))
			require.NoError(t, err)
			assert.Equal(t, "raw output\n", string(crashes))
		})
	}
}

func TestWindowsDaemonLogUnrelatedKeepsForeground(t *testing.T) {
	for _, kind := range []string{"file", "pipe", "nul"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")
			var stderr, reader *os.File
			var err error
			switch kind {
			case "file":
				stderr, err = os.Create(filepath.Join(dir, "capture.log"))
			case "pipe":
				reader, stderr, err = os.Pipe()
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Close() })
			case "nul":
				stderr, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			original := stderr
			base := stderrTestLogger(stderr)
			logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stderr)
			t.Cleanup(closeLog)
			assert.Same(t, original, stderr)
			assert.Same(t, base, logger)
			logger.Info("foreground")
			var data []byte
			switch kind {
			case "file":
				data, err = os.ReadFile(stderr.Name())
			case "pipe":
				require.NoError(t, stderr.Close())
				data, err = io.ReadAll(reader)
			}
			require.NoError(t, err)
			if kind != "nul" {
				assert.Contains(t, string(data), `"msg":"foreground"`)
			}
			_, err = os.Stat(path)
			assert.True(t, os.IsNotExist(err))
			_, err = os.Stat(daemon.StderrLogPath(path))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestWindowsDaemonLogServiceRotationDisabled(t *testing.T) {
	previous := daemonServiceLog
	daemonServiceLog = true
	t.Cleanup(func() { daemonServiceLog = previous })
	path := filepath.Join(t.TempDir(), "daemon.log")
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	base := stderrTestLogger(stderr)
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{LogMaxSize: new(0)}, path, &stderr, &stderr)
	defer closeLog()
	logger.Info("unrotated task event")
	data, err := os.ReadFile(daemon.StderrLogPath(path))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"msg":"unrotated task event"`)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestWindowsDaemonLogRedirectFailureKeepsInheritedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	stderr, err := os.Create(path)
	require.NoError(t, err)
	defer stderr.Close()
	require.NoError(t, os.Mkdir(daemon.StderrLogPath(path), 0700))
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{}, path, &stderr, &stderr)
	defer closeLog()
	assert.Same(t, base, logger)
	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zap.WarnLevel, logs.All()[0].Level)
	_, err = stderr.WriteString("still inherited")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "still inherited", string(data))
}

func TestWindowsDaemonLogStdoutOnlyInheritance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	stdout, err := os.Create(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdout.Close() })
	reader, stderr, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	t.Cleanup(func() { _ = stderr.Close() })
	originalStderr := stderr
	logger, closeLog := configureDaemonLogger(stderrTestLogger(stderr), config.DaemonConfig{}, path, &stderr, &stdout)
	t.Cleanup(closeLog)
	assert.Same(t, originalStderr, stderr, "leave the unrelated stderr capture alone")
	logger.Info("stdout inherited")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"msg":"stdout inherited"`)
	_, err = stdout.WriteString("raw stdout\n")
	require.NoError(t, err)
	data, err = os.ReadFile(daemon.StderrLogPath(path))
	require.NoError(t, err)
	assert.Equal(t, "raw stdout\n", string(data))
}

func TestWindowsDaemonLogSetStdHandleFailureIsAtomic(t *testing.T) {
	stderrHandle, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	require.NoError(t, err)
	stdoutHandle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	require.NoError(t, err)
	processStderr, processStdout := os.Stderr, os.Stdout
	setStdHandle := daemonLogSetStdHandle
	t.Cleanup(func() {
		daemonLogSetStdHandle = setStdHandle
		os.Stderr, os.Stdout = processStderr, processStdout
		assert.NoError(t, windows.SetStdHandle(windows.STD_ERROR_HANDLE, stderrHandle))
		assert.NoError(t, windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, stdoutHandle))
	})
	dir := t.TempDir()
	stderr, err := os.Create(filepath.Join(dir, "stderr.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	stdout, err := os.Create(filepath.Join(dir, "stdout.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdout.Close() })
	source, err := os.Create(filepath.Join(dir, "replacement.log"))
	require.NoError(t, err)
	defer source.Close()
	os.Stderr, os.Stdout = stderr, stdout
	stdlibOutput := log.Writer()
	retained := len(daemonLogRetainedFiles)
	var calls []uint32
	daemonLogSetStdHandle = func(kind uint32, handle windows.Handle) error {
		calls = append(calls, kind)
		if len(calls) == 2 {
			return windows.ERROR_ACCESS_DENIED
		}
		return windows.SetStdHandle(kind, handle)
	}
	err = replaceDaemonLogFDPair(&os.Stderr, &os.Stdout, source)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Equal(t, []uint32{
		windows.STD_ERROR_HANDLE, windows.STD_OUTPUT_HANDLE,
		windows.STD_ERROR_HANDLE,
	}, calls)
	current, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	require.NoError(t, err)
	assert.Equal(t, stderrHandle, current)
	current, err = windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	require.NoError(t, err)
	assert.Equal(t, stdoutHandle, current)
	assert.Same(t, stderr, os.Stderr)
	assert.Same(t, stdout, os.Stdout)
	assert.Same(t, stdlibOutput, log.Writer())
	assert.Len(t, daemonLogRetainedFiles, retained)
	_, err = stderr.WriteString("still open\n")
	require.NoError(t, err)
	_, err = stdout.WriteString("still open\n")
	require.NoError(t, err)
}

func TestWindowsDaemonLogSetStdHandleRollback(t *testing.T) {
	for _, kind := range []string{"invalid_original", "restore_failure"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			stderr, err := os.Create(filepath.Join(dir, "stderr.log"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = stderr.Close() })
			stdout, err := os.Create(filepath.Join(dir, "stdout.log"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = stdout.Close() })
			source, err := os.Create(filepath.Join(dir, "replacement.log"))
			require.NoError(t, err)
			defer source.Close()
			processStderr, processStdout := os.Stderr, os.Stdout
			getStdHandle, setStdHandle := daemonLogGetStdHandle, daemonLogSetStdHandle
			retained := len(daemonLogRetainedFiles)
			t.Cleanup(func() {
				os.Stderr, os.Stdout = processStderr, processStdout
				daemonLogGetStdHandle, daemonLogSetStdHandle = getStdHandle, setStdHandle
				for _, file := range daemonLogRetainedFiles[retained:] {
					_ = file.Close()
				}
				daemonLogRetainedFiles = daemonLogRetainedFiles[:retained]
			})
			os.Stderr, os.Stdout = stderr, stdout
			oldStderr := windows.Handle(stderr.Fd())
			if kind == "invalid_original" {
				oldStderr = windows.InvalidHandle
			}
			daemonLogGetStdHandle = func(std uint32) (windows.Handle, error) {
				if std == windows.STD_ERROR_HANDLE {
					if oldStderr == windows.InvalidHandle {
						return oldStderr, windows.ERROR_INVALID_HANDLE
					}
					return oldStderr, nil
				}
				return windows.Handle(stdout.Fd()), nil
			}
			type standardHandle struct {
				kind   uint32
				handle windows.Handle
			}
			var calls []standardHandle
			daemonLogSetStdHandle = func(std uint32, handle windows.Handle) error {
				calls = append(calls, standardHandle{std, handle})
				if len(calls) == 2 {
					return windows.ERROR_ACCESS_DENIED
				}
				if len(calls) == 3 && kind == "restore_failure" {
					return windows.ERROR_INVALID_HANDLE
				}
				return nil
			}
			err = replaceDaemonLogFDPair(&os.Stderr, &os.Stdout, source)
			require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
			require.Len(t, calls, 3)
			assert.Equal(t, []standardHandle{
				{windows.STD_ERROR_HANDLE, calls[0].handle},
				{windows.STD_OUTPUT_HANDLE, calls[0].handle},
				{windows.STD_ERROR_HANDLE, oldStderr},
			}, calls)
			assert.Same(t, stderr, os.Stderr)
			assert.Same(t, stdout, os.Stdout)
			if kind == "restore_failure" {
				require.ErrorIs(t, err, windows.ERROR_INVALID_HANDLE)
				require.Len(t, daemonLogRetainedFiles, retained+1)
				replacement := daemonLogRetainedFiles[retained]
				assert.Equal(t, calls[0].handle, windows.Handle(replacement.Fd()))
				_, err = replacement.WriteString("still open after failed restore\n")
				require.NoError(t, err)
			} else {
				assert.Len(t, daemonLogRetainedFiles, retained)
			}
		})
	}
}

func TestWindowsDaemonLogServiceSubprocess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	child := exec.Command(os.Args[0], "-test.run=^TestWindowsDaemonLogServiceHelper$")
	child.Env = append(os.Environ(), "GORTEX_TEST_SERVICE_LOG=1", "GORTEX_TEST_SERVICE_LOGFILE="+path, "TMP="+dir, "TEMP="+dir)
	// Nil Stdout/Stderr gives the helper NUL, as for a scheduled task.
	var exitErr *exec.ExitError
	require.ErrorAs(t, child.Run(), &exitErr, "the helper must panic")
	require.Equal(t, 2, exitErr.ExitCode())
	data, err := os.ReadFile(daemon.StderrLogPath(path))
	require.NoError(t, err)
	assert.Contains(t, string(data), "stdlib-line")
	assert.Contains(t, string(data), "panic: service-log-panic")
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	var entry map[string]any
	require.NoError(t, json.Unmarshal(data, &entry))
	assert.Equal(t, "zap-line", entry["msg"])
	assert.Equal(t, "info", entry["level"])
}

func TestWindowsDaemonLogServiceHelper(t *testing.T) {
	if os.Getenv("GORTEX_TEST_SERVICE_LOG") != "1" {
		return
	}
	// TestMain installs a fresh sandbox, including its own log path.
	t.Setenv("GORTEX_DAEMON_LOGFILE", os.Getenv("GORTEX_TEST_SERVICE_LOGFILE"))
	require.NoError(t, daemonStartCmd.Flags().Set("service-log", "true"))
	logger, closeLog := configureDaemonLogger(newLogger(), config.DaemonConfig{}, daemon.LogFilePath(), &os.Stderr, &os.Stdout)
	log.Print("stdlib-line")
	logger.Info("zap-line")
	closeLog()
	panic("service-log-panic")
}
