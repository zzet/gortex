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
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/testenv"
)

const migrationProxyTestInactivity = 50 * time.Millisecond

type migrationProxyWaitWitness struct {
	At            time.Time           `json:"at"`
	Checks        int                 `json:"checks"`
	LockContended bool                `json:"lock_contended"`
	State         daemon.RuntimeState `json:"state"`
}

type migrationProxyHelperInput struct {
	Root       string `json:"root"`
	ParentPID  int    `json:"parent_pid"`
	SpawnLock  string `json:"spawn_lock"`
	GlobalPath string `json:"global_config"`
}

// The helper's TestMain installs a new sandbox before this test runs. Restore
// only these paths derived from its parent's fresh short fixture, not an
// inherited environment dump or any of the user's per-user state.
func migrationProxyOwnedEnv(root string) map[string]string {
	home := filepath.Join(root, "home")
	return map[string]string{
		"HOME": home, "USERPROFILE": home,
		"AppData":                 filepath.Join(home, "AppData", "Roaming"),
		"LocalAppData":            filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME":         filepath.Join(root, "config"),
		"XDG_DATA_HOME":           filepath.Join(root, "data"),
		"XDG_CACHE_HOME":          filepath.Join(root, "cache"),
		"XDG_STATE_HOME":          filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR":         filepath.Join(root, "runtime"),
		"GORTEX_DAEMON_SOCKET":    filepath.Join(root, "s"),
		"GORTEX_DAEMON_PIDFILE":   filepath.Join(root, "p"),
		"GORTEX_DAEMON_STATEFILE": filepath.Join(root, "state.json"),
		"GORTEX_DAEMON_LOGFILE":   filepath.Join(root, "daemon.log"),
		"GORTEX_DAEMON_SNAPSHOT":  filepath.Join(root, "daemon.gob.gz"),
	}
}

// This helper runs the real MCP selection and proxy in a fresh process. Only
// that process shortens the private inactivity budget. Its liveness seam still
// returns the actual socket result; the extra lock/heartbeat reads only record
// that it reached the contended wait before the parent begins its hold.
func TestMigrationWindowMCPProxyHelper(t *testing.T) {
	if os.Getenv("GORTEX_TEST_MIGRATION_PROXY") != "1" {
		t.Skip("invoked only by TestMCPWaitsThroughMigrationWindow")
	}
	spawnLockInactivityTimeout = migrationProxyTestInactivity
	inputPath := os.Getenv("GORTEX_TEST_MIGRATION_INPUT")
	require.True(t, filepath.IsAbs(inputPath))
	body, err := os.ReadFile(inputPath)
	require.NoError(t, err)
	var input migrationProxyHelperInput
	require.NoError(t, json.Unmarshal(body, &input))
	require.Equal(t, os.Getppid(), input.ParentPID)
	require.True(t, filepath.IsAbs(input.Root))
	require.True(t, strings.HasPrefix(filepath.Base(input.Root), "gx"))
	require.Equal(t, filepath.Join(input.Root, "migration-helper-input.json"), inputPath)
	for name, value := range migrationProxyOwnedEnv(input.Root) {
		t.Setenv(name, value)
	}
	require.Equal(t, input.SpawnLock, daemon.SpawnLockPath())
	require.Equal(t, input.GlobalPath, config.DefaultGlobalConfigPath())
	witnessDir := input.Root
	checks := 0
	isDaemonRunning = func() bool {
		running := daemon.IsRunning()
		checks++
		if !running && checks >= 2 {
			probe := flock.New(daemon.SpawnLockPath())
			locked, err := probe.TryLock()
			require.NoError(t, err)
			require.NoError(t, probe.Close())
			if locked {
				return running // the parent may just have released startup
			}
			state, ok := daemon.ReadRuntimeState()
			if !ok || state.StartupPhase != daemon.StartupMigrating {
				return running // migration may have completed before Listen
			}
			require.True(t, state.StartupProgressFresh(time.Now(), spawnProgressFreshness))
			w := migrationProxyWaitWitness{At: time.Now(), Checks: checks, LockContended: true, State: state}
			body, err := json.Marshal(w)
			require.NoError(t, err)
			path := filepath.Join(witnessDir, "latest.json")
			if checks == 2 {
				path = filepath.Join(witnessDir, "first.json")
			}
			require.NoError(t, os.WriteFile(path+".tmp", body, 0o600))
			require.NoError(t, os.Rename(path+".tmp", path))
		}
		return running
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mcpCmd.SetContext(ctx)
	require.NoError(t, runMCP(mcpCmd, nil))
}

// This exercises an actual pending SQLite migration, heartbeat file, spawn
// lock, runMCP selection, stdio relay and public loading reply. The short hold
// proves renewal from a fresh PID-bound startup record, not the reporter's
// two-second periodic heartbeat. Historical schema-v21 workload acceptance is
// separate; this fixture deliberately replays the idempotent v40 step.
func TestMCPWaitsThroughMigrationWindow(t *testing.T) {
	base := testenv.ShortTempDir(t)
	// Keep this migration regression's config and actual child CWD under the
	// same physical spelling. Alias admission is a separate unresolved case;
	// /tmp versus /private/tmp must not conceal the migration wait contract.
	base, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	testenv.SandboxAt(t, filepath.Join(base, "home"))
	for name, value := range migrationProxyOwnedEnv(base) {
		t.Setenv(name, value)
	}
	t.Setenv("GORTEX_AUTOSTART", "1")
	t.Setenv("GORTEX_TELEMETRY", "0")
	t.Setenv("GORTEX_TOOLS", "")
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc MigrationProbe() {}\n"), 0o600))
	globalPath := config.DefaultGlobalConfigPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o700))
	// Opting into the fallback makes a premature embedded selection visible;
	// it must not replace the configured daemon during an active migration.
	require.NoError(t, os.WriteFile(globalPath, []byte(fmt.Sprintf("repos:\n  - path: %q\n    name: migrating\nmcp:\n  allow_embedded: true\n", root)), 0o600))
	oldConfig, oldPath, oldBackend := cfgFile, daemonBackendPath, daemonBackend
	t.Cleanup(func() { cfgFile, daemonBackendPath, daemonBackend = oldConfig, oldPath, oldBackend })
	cfgFile, daemonBackendPath, daemonBackend = filepath.Join(base, "repo.yaml"), filepath.Join(base, "store.sqlite"), "sqlite"
	require.NoError(t, os.WriteFile(cfgFile, []byte("semantic:\n  enabled: false\n"), 0o600))
	seed, err := store_sqlite.Open(daemonBackendPath)
	require.NoError(t, err)
	closeSeed := sync.OnceValue(seed.Close)
	t.Cleanup(func() { _ = closeSeed() })
	require.NoError(t, closeSeed())
	db, err := sql.Open("sqlite", daemonBackendPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA user_version=39")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	lock := flock.New(daemon.SpawnLockPath())
	require.NoError(t, os.MkdirAll(filepath.Dir(daemon.SpawnLockPath()), 0o700))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	reporter := newDaemonStartupReporter(zap.NewNop())
	t.Cleanup(reporter.Stop)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	type buildResult struct {
		state *daemonState
		err   error
	}
	built := make(chan buildResult, 1)
	go func() {
		state, buildErr := buildDaemonState(zap.NewNop(), func(p store_sqlite.MigrationProgress) {
			reporter.ObserveMigration(p)
			if p.Version == 40 && p.Phase == store_sqlite.MigrationStarted {
				close(entered)
				<-release
			}
		})
		built <- buildResult{state, buildErr}
	}()
	var result buildResult
	buildJoined := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !buildJoined {
			select {
			case result = <-built:
			case <-time.After(10 * time.Second):
				t.Error("migration build did not join")
				return
			}
		}
		if result.state != nil {
			require.NoError(t, result.state.shared.Close())
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the actual v40 migration did not start")
	}
	state, ok := daemon.ReadRuntimeState()
	require.True(t, ok)
	require.Equal(t, daemon.StartupMigrating, state.StartupPhase)
	require.Equal(t, os.Getpid(), state.PID)
	require.False(t, daemon.IsRunning(), "the real socket must be absent during migration")

	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestMigrationWindowMCPProxyHelper$")
	child.Dir = root
	inputPath := filepath.Join(base, "migration-helper-input.json")
	input, err := json.Marshal(migrationProxyHelperInput{
		Root: base, ParentPID: os.Getpid(), SpawnLock: daemon.SpawnLockPath(), GlobalPath: globalPath,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(inputPath, input, 0o600))
	child.Env = append(os.Environ(), "GORTEX_TEST_MIGRATION_PROXY=1", "GORTEX_TEST_MIGRATION_INPUT="+inputPath)
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	require.NoError(t, child.Start())
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	reaped := false
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		if !reaped {
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				t.Error("owned MCP helper did not reap")
			}
		}
	})
	frames := streamProxyFrames(ctx, stdout)
	_, err = fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"migration-probe","version":"1"}}}`)
	require.NoError(t, err)
	var first migrationProxyWaitWitness
	require.Eventually(t, func() bool {
		body, readErr := os.ReadFile(filepath.Join(base, "first.json"))
		return readErr == nil && json.Unmarshal(body, &first) == nil
	}, 5*time.Second, 5*time.Millisecond)
	require.True(t, first.LockContended)
	require.Equal(t, os.Getpid(), first.State.PID)
	require.Equal(t, daemon.StartupMigrating, first.State.StartupPhase)
	// Time the hold only after an independent subprocess has witnessed both
	// the actual failed flock and the actual active migration heartbeat.
	select {
	case frame := <-frames:
		t.Fatalf("premature fallback/embedded output during migration: %s (%v)", frame.frame, frame.err)
	case childErr := <-exited:
		reaped = true
		t.Fatalf("MCP exited during migration: %v", childErr)
	case <-time.After(3 * migrationProxyTestInactivity):
	}
	var latest migrationProxyWaitWitness
	body, err := os.ReadFile(filepath.Join(base, "latest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &latest))
	require.Greater(t, latest.Checks, first.Checks, "the real contended wait must continue after its initial inactivity budget")
	require.True(t, latest.LockContended)
	require.Equal(t, first.State.PID, latest.State.PID)
	select {
	case frame := <-frames:
		t.Fatalf("MCP produced output before migration release: %s (%v)", frame.frame, frame.err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case result = <-built:
		buildJoined = true
	case <-ctx.Done():
		t.Fatal("migration did not complete after release")
	}
	require.NoError(t, result.err)
	srv := daemon.New(os.Getenv("GORTEX_DAEMON_SOCKET"), "test", zap.NewNop())
	srv.MCPDispatcher = newMCPDispatcher(result.state.mcpServer, result.state.multiIndexer, zap.NewNop())
	require.NoError(t, listenConfiguredDaemon(srv, result.state))
	reporter.Serving(daemonBackendPath)
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown())
		select {
		case serveErr := <-done:
			require.NoError(t, serveErr)
		case <-time.After(5 * time.Second):
			t.Error("owned migration-window server did not join")
		}
	})
	require.NoError(t, lock.Unlock())
	readReply := func() []byte {
		t.Helper()
		select {
		case frame := <-frames:
			require.NoError(t, frame.err)
			return frame.frame
		case <-ctx.Done():
			t.Fatal("MCP reply deadline")
			return nil
		}
	}
	var initialized struct {
		Result struct{ Instructions string } `json:"result"`
	}
	require.NoError(t, json.Unmarshal(readReply(), &initialized))
	assert.Contains(t, initialized.Result.Instructions, gortexmcp.ServerInstructionsLoading("migrating", "starting", 0))
	_, err = fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"MigrationProbe"}}}`)
	require.NoError(t, err)
	reply := readReply()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	data := errorData(t, parsed)
	assert.Equal(t, "repo_loading", data["error_code"])
	assert.Equal(t, true, data["retryable"])
	assert.Contains(t, data, "phase")
	assert.Contains(t, data, "percent")
	assert.NotContains(t, string(reply), "gortex track")
	assert.NotContains(t, string(reply), "repos reconcile")
	require.NoError(t, stdin.Close())
	select {
	case childErr := <-exited:
		reaped = true
		require.NoError(t, childErr, "%s", stderr.String())
	case <-ctx.Done():
		t.Fatal("MCP helper did not exit after closing stdio")
	}
	assert.NotContains(t, stderr.String(), "embedded")
}
