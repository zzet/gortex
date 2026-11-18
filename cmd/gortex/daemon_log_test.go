package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestDaemonStartServiceLogFlagHidden(t *testing.T) {
	flag := daemonStartCmd.Flags().Lookup("service-log")
	require.NotNil(t, flag)
	assert.True(t, flag.Hidden)
	assert.Equal(t, "false", flag.DefValue)
}

func TestResolveDaemonLogLimit(t *testing.T) {
	for _, tt := range []struct {
		name     string
		value    *int
		fallback int
		minimum  int
		want     int
		invalid  bool
	}{
		{"default_size", nil, 50, 0, 50, false},
		{"default_backups", nil, 3, 1, 3, false},
		{"zero_size", new(0), 50, 0, 0, false},
		{"zero_backups", new(0), 3, 1, 3, true},
		{"custom", new(10), 50, 0, 10, false},
		{"negative_size", new(-1), 50, 0, 50, true},
		{"negative_backups", new(-1), 3, 1, 3, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, invalid := resolveDaemonLogLimit(tt.value, tt.fallback, tt.minimum)
			assert.Equal(t, tt.want, value)
			assert.Equal(t, tt.invalid, invalid)
		})
	}
}

func stderrTestLogger(stderr *os.File) *zap.Logger {
	cfg := newLoggerConfig()
	return zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(cfg.EncoderConfig), zapcore.AddSync(stderr), cfg.Level))
}

func TestConfigureDaemonLoggerRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	old := "{\"msg\":\"" + strings.Repeat("x", (1<<20)-512) + "\"}\n"
	require.NoError(t, os.WriteFile(path, []byte(old), 0600))
	stderr, err := os.OpenFile(filepath.Join(dir, "daemon.stderr.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderr.Close() })
	logger, closeLog := configureDaemonLogger(stderrTestLogger(stderr), config.DaemonConfig{LogMaxSize: new(1)}, path, &stderr, &stderr)
	t.Cleanup(closeLog)
	logger.Info("rotated", zap.String("payload", strings.Repeat("y", 512)))
	logger.Info("continued")

	backups, err := filepath.Glob(filepath.Join(dir, "daemon-*.log"))
	require.NoError(t, err)
	require.Len(t, backups, 1)
	backup, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, old, string(backup))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	require.Len(t, lines, 2)
	for i, message := range []string{"rotated", "continued"} {
		var entry map[string]any
		require.NoError(t, json.Unmarshal(lines[i], &entry))
		assert.Equal(t, message, entry["msg"])
		assert.Equal(t, "info", entry["level"])
	}
	crashes, err := os.ReadFile(stderr.Name())
	require.NoError(t, err)
	assert.Empty(t, crashes, "JSON logger must not also write to inherited stderr")
}

func TestConfigureDaemonLoggerDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	stderr, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer stderr.Close()
	base := stderrTestLogger(stderr)
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{LogMaxSize: new(0)}, path, &stderr, &stderr)
	defer closeLog()
	assert.Same(t, base, logger)
	logger.Info("still stderr")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "still stderr")
	_, err = os.Stat(filepath.Join(filepath.Dir(path), "daemon.stderr.log"))
	assert.True(t, os.IsNotExist(err), "disabled rotation must not redirect descriptors")
}

func TestConfigureDaemonLoggerNegativeWarns(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer func() { _ = w.Close() }()
	base := zap.New(core)
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, closeLog := configureDaemonLogger(base, config.DaemonConfig{
		LogMaxSize: new(-1), LogMaxBackups: new(-1),
	}, path, &w, &w)
	defer closeLog()
	assert.Same(t, base, logger, "a pipe is not a daemon log")
	require.Equal(t, 2, logs.Len())
	for _, entry := range logs.All() {
		assert.Equal(t, zap.WarnLevel, entry.Level)
	}
	assert.Contains(t, logs.All()[0].ContextMap()["reason"], "daemon.log_max_size")
	assert.Contains(t, logs.All()[1].ContextMap()["reason"], "daemon.log_max_backups")
}

func TestDaemonLogsReadsRotatingLog(t *testing.T) {
	for _, kind := range []string{"nonempty", "empty", "absent"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			t.Setenv("GORTEX_DAEMON_LOGFILE", path)
			require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0600))
			stderrPath := filepath.Join(filepath.Dir(path), "daemon.stderr.log")
			if kind != "absent" {
				content := ""
				if kind == "nonempty" {
					content = "crash\n"
				}
				require.NoError(t, os.WriteFile(stderrPath, []byte(content), 0600))
			}
			oldTail := daemonTail
			daemonTail = 1
			t.Cleanup(func() { daemonTail = oldTail })
			cmd := &cobra.Command{}
			var output, hints bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&hints)
			require.NoError(t, runDaemonLogs(cmd, nil))
			assert.Equal(t, "second\n", output.String())
			if kind == "nonempty" {
				assert.Equal(t, "Crash output and raw stderr: "+stderrPath+"\n", hints.String())
			} else {
				assert.Empty(t, hints.String())
			}
		})
	}
}

func TestDaemonLogsMissingLogShowsStderrHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	t.Setenv("GORTEX_DAEMON_LOGFILE", path)
	stderrPath := filepath.Join(filepath.Dir(path), "daemon.stderr.log")
	require.NoError(t, os.WriteFile(stderrPath, []byte("crash\n"), 0600))
	cmd := &cobra.Command{}
	var output, hints bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&hints)
	err := runDaemonLogs(cmd, nil)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "open log "+path)
	assert.Empty(t, output.String())
	assert.Equal(t, "Crash output and raw stderr: "+stderrPath+"\n", hints.String())
}

func TestNewDaemonLogWriter(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		policy                  config.DaemonConfig
		size, backups, warnings int
	}{
		{"defaults", config.DaemonConfig{}, 50, 3, 0},
		{"custom", config.DaemonConfig{LogMaxSize: new(8), LogMaxBackups: new(7)}, 8, 7, 0},
		{"zero", config.DaemonConfig{LogMaxSize: new(0), LogMaxBackups: new(0)}, 0, 3, 1},
		{"negative", config.DaemonConfig{LogMaxSize: new(-1), LogMaxBackups: new(-1)}, 50, 3, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writer, warnings := newDaemonLogWriter(tt.policy, "custom.log")
			assert.Equal(t, "custom.log", writer.Filename)
			assert.Equal(t, tt.size, writer.MaxSize)
			assert.Equal(t, tt.backups, writer.MaxBackups)
			assert.Len(t, warnings, tt.warnings)
		})
	}
}
