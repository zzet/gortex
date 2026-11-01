//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testenv"
)

func TestMigrationSocketOwnedHelper(t *testing.T) {
	root := os.Getenv("GORTEX_TEST_MIGRATION_SOCKET_ROOT")
	if root == "" {
		t.Skip("owned child only")
	}
	require.True(t, filepath.IsAbs(root))
	require.True(t, strings.HasPrefix(filepath.Base(root), "gx"), "helper opens only its parent fresh short fixture")
	var owner struct {
		ParentPID int `json:"parent_pid"`
	}
	input, err := os.ReadFile(filepath.Join(root, "migration-owner.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(input, &owner))
	require.Equal(t, os.Getppid(), owner.ParentPID)
	for key, value := range migrationProxyOwnedEnv(root) {
		t.Setenv(key, value)
	}
	cfgFile, daemonBackendPath, daemonBackend = filepath.Join(root, "repo.yaml"), filepath.Join(root, "store.sqlite"), "sqlite"
	daemonDetach = false
	buildDaemonStateForStart = func(logger *zap.Logger, observers ...store_sqlite.MigrationObserver) (*daemonState, error) {
		return buildDaemonState(logger, func(progress store_sqlite.MigrationProgress) {
			if len(observers) > 0 {
				observers[0](progress)
			}
			if progress.Version == 40 && progress.Phase == store_sqlite.MigrationStarted {
				require.NoError(t, os.WriteFile(filepath.Join(root, "migration-entered"), []byte("real migration v40 started\n"), 0o600))
				deadline := time.Now().Add(20 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(root, "release-migration")); err == nil {
						break
					}
					require.True(t, time.Now().Before(deadline), "parent did not release owned migration")
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
	command := &cobra.Command{}
	command.SetContext(t.Context())
	require.NoError(t, runDaemonStart(command, nil))
}

func migrationSocketFrame(t *testing.T, c *daemon.Client, frame string, id int) map[string]any {
	t.Helper()
	require.NoError(t, c.Conn.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, c.WriteMCPFrame([]byte(frame)))
	for {
		raw, err := c.ReadMCPFrame()
		require.NoError(t, err)
		var answer map[string]any
		require.NoError(t, json.Unmarshal(raw, &answer))
		if answer["id"] == float64(id) {
			return answer
		}
	}
}

func TestDaemonMigrationSocketKeepsMetadataAndScopedLoadingAvailable(t *testing.T) {
	root, err := filepath.EvalSymlinks(testenv.ShortTempDir(t))
	require.NoError(t, err)
	for key, value := range migrationProxyOwnedEnv(root) {
		t.Setenv(key, value)
	}
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "subdir"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package p\nfunc MigrationSocketProbe(){}\n"), 0o600))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Migration Fixture", "-c", "user.email=migration@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "fixture")
	linked := filepath.Join(root, "linked")
	git("worktree", "add", "--detach", linked)
	require.NoError(t, os.MkdirAll(filepath.Join(linked, "subdir"), 0o700))
	global := config.DefaultGlobalConfigPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(global), 0o700))
	require.NoError(t, os.WriteFile(global, []byte(fmt.Sprintf("repos:\n  - path: %q\n    name: migrating\n", repo)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "repo.yaml"), []byte("semantic:\n  enabled: false\n"), 0o600))
	path := filepath.Join(root, "store.sqlite")
	seed, err := store_sqlite.Open(path)
	require.NoError(t, err)
	require.NoError(t, seed.Close())
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA user_version=39")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	owner, _ := json.Marshal(map[string]int{"parent_pid": os.Getpid()})
	require.NoError(t, os.WriteFile(filepath.Join(root, "migration-owner.json"), owner, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationSocketOwnedHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "GORTEX_TEST_MIGRATION_SOCKET_ROOT="+root, "GORTEX_TELEMETRY=0")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	require.NoError(t, child.Start())
	finished := make(chan error, 1)
	go func() { finished <- child.Wait() }()
	joined := false
	var clients []*daemon.Client
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
		_ = os.WriteFile(filepath.Join(root, "release-migration"), nil, 0o600)
		if !joined {
			select {
			case <-finished:
				joined = true
			default:
			}
		}
		if !joined {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("owned migration child did not join")
			}
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(root, "migration-entered")); return err == nil }, 15*time.Second, 5*time.Millisecond)
	for _, cwd := range []string{repo, filepath.Join(repo, "subdir"), linked, filepath.Join(linked, "subdir")} {
		started := time.Now()
		client, err := daemon.DialToWithDeadline(filepath.Join(root, "s"), daemon.Handshake{Mode: daemon.ModeMCP, CWD: cwd, Tools: "full", ToolsMode: "hide"}, started.Add(time.Second))
		require.NoError(t, err, "socket must accept while the actual migration is blocked; cwd=%s", cwd)
		clients = append(clients, client)
		init := migrationSocketFrame(t, client, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"migration-window","version":"1"}}}`, 1)
		require.NotContains(t, init, "error")
		list := migrationSocketFrame(t, client, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 2)
		require.NotContains(t, list, "error")
		catalog, ok := list["result"].(map[string]any)
		require.True(t, ok)
		require.NotEmpty(t, catalog["tools"], "tools/list must publish a real catalog during migration")
		require.Less(t, time.Since(started), time.Second, "metadata must remain responsive during migration")
		response := migrationSocketFrame(t, client, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"MigrationSocketProbe","format":"json"}}}`, 3)
		data := errorData(t, response)
		require.Equal(t, "repo_loading", data["error_code"])
		require.Equal(t, true, data["retryable"])
		require.Equal(t, "migrating", data["phase"])
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "release-migration"), nil, 0o600))
	// The same socket sessions must survive publication; no reconnect can hide
	// a temporary catalog or a lost initialized-session state.
	require.Eventually(t, func() bool {
		response := migrationSocketFrame(t, clients[0], `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"MigrationSocketProbe","format":"json"}}}`, 4)
		result, _ := response["result"].(map[string]any)
		content, _ := result["content"].([]any)
		for _, block := range content {
			text, _ := block.(map[string]any)["text"].(string)
			var payload struct {
				Results []struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"results"`
			}
			if json.Unmarshal([]byte(text), &payload) == nil {
				for _, node := range payload.Results {
					if node.Name == "MigrationSocketProbe" && node.ID != "" {
						return true
					}
				}
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	control, err := daemon.DialToWithDeadline(filepath.Join(root, "s"), daemon.Handshake{Mode: daemon.ModeControl}, time.Now().Add(time.Second))
	require.NoError(t, err)
	_, err = control.ControlWithTimeout(daemon.ControlShutdown, nil, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, control.Close())
	select {
	case err := <-finished:
		joined = true
		require.NoError(t, err, "%s", output.String())
	case <-ctx.Done():
		t.Fatal("owned migration daemon did not stop")
	}
	require.NoFileExists(t, filepath.Join(root, "s"))
}
