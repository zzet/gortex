package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testenv"
)

// This native gate intentionally uses a prebuilt binary: the release runner
// owns its pinned toolchain/build flags and records that artifact separately.
// No service is installed, started or stopped by this fixture.
func TestOfflineUntrackEscapeHatch(t *testing.T) {
	if os.Getenv("GORTEX_E2E") != "1" {
		t.Skip("set GORTEX_E2E=1 and GORTEX_E2E_BINARY to run the native offline-untrack gate")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the release gate targets native macOS and Linux")
	}
	binary := os.Getenv("GORTEX_E2E_BINARY")
	require.True(t, filepath.IsAbs(binary), "GORTEX_E2E_BINARY must name the pinned absolute binary")
	root := t.TempDir()
	socketDir := testenv.ShortTempDir(t)
	// Registered before owned-child cleanup: its joined logs are dumped before
	// temporary directories are removed, including on startup/cleanup failures.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for run := 1; run <= 2; run++ {
			path := filepath.Join(root, fmt.Sprintf("daemon-%d.log", run))
			file, err := os.Open(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Logf("owned daemon log %s unavailable: %v", path, err)
				continue
			}
			info, statErr := file.Stat()
			if statErr == nil {
				const limit = int64(32 << 10)
				offset := max(int64(0), info.Size()-limit)
				data, readErr := io.ReadAll(io.NewSectionReader(file, offset, limit))
				t.Logf("owned daemon log %s tail offset=%d read_error=%v:\n%s", path, offset, readErr, data)
			} else {
				t.Logf("owned daemon log %s stat: %v", path, statErr)
			}
			if err := file.Close(); err != nil {
				t.Logf("owned daemon log %s close: %v", path, err)
			}
		}
	})
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	// Both service detectors first require their HOME-relative installed unit
	// file. A fresh HOME makes the existing unsupervised route deterministic;
	// check the actual path helpers before any product lifecycle invocation.
	t.Setenv("HOME", home)
	for _, pathFor := range []func() (string, error){launchdPlistPath, systemdUnitPath} {
		path, err := pathFor()
		require.NoError(t, err)
		rel, err := filepath.Rel(home, path)
		require.NoError(t, err)
		require.False(t, rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
		_, err = os.Lstat(path)
		require.True(t, os.IsNotExist(err), "private HOME must have no installed service: %s", path)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(socketDir, "s"))
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(root, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(root, "state"))
	t.Setenv("GORTEX_AUTOSTART", "off")
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GORTEX_") || strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	for _, key := range []string{"GORTEX_DAEMON_SOCKET", "GORTEX_DAEMON_PIDFILE", "GORTEX_DAEMON_STATEFILE", "GORTEX_AUTOSTART"} {
		env = append(env, key+"="+os.Getenv(key))
	}
	env = append(env, "GORTEX_TELEMETRY=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_TERMINAL_PROMPT=0")
	a, b := filepath.Join(root, "keep"), filepath.Join(root, "runaway")
	for _, dir := range []string{a, b} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(a, "keep.go"), []byte("package keep\nfunc Kept() {}\n"), 0o600))
	for file := range 2048 {
		var source strings.Builder
		source.WriteString("package runaway\n")
		for symbol := range 64 {
			fmt.Fprintf(&source, "func Value%d_%d() int { return %d }\n", file, symbol, symbol)
		}
		require.NoError(t, os.WriteFile(filepath.Join(b, fmt.Sprintf("file%04d.go", file)), []byte(source.String()), 0o600))
	}
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = []config.RepoEntry{{Path: a, Name: "escape-a"}, {Path: b, Name: "escape-b"}}
	require.NoError(t, gc.Save())
	storePath := filepath.Join(root, "store.sqlite")
	store, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	// An owned old row makes the orphan-purge assertion non-vacuous. No
	// mtime sidecar is seeded: B must still perform a real cold full index.
	// This row is not evidence of indexing progress; the exact full-index
	// start log plus simultaneous Loading status below proves that separately.
	store.AddBatch([]*graph.Node{{ID: "escape-b/sentinel.go::Old", Kind: graph.KindFunction, Name: "Old", FilePath: "escape-b/sentinel.go", RepoPrefix: "escape-b"}}, nil)
	require.NoError(t, store.Close())
	command := func(limit time.Duration, args ...string) ([]byte, error) {
		// Cleanup also needs this bounded CLI after testing cancels t.Context.
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env, cmd.Dir = env, root
		return cmd.CombinedOutput()
	}
	var child *exec.Cmd
	var exited chan error
	var logPath string
	start := func(run int) {
		logPath = filepath.Join(root, fmt.Sprintf("daemon-%d.log", run))
		log, openErr := os.Create(logPath)
		require.NoError(t, openErr)
		// Match the existing native fixture's owned cancellation shape: ask
		// this private daemon to stop before exec's last-resort bounded kill.
		deadline := time.Now().Add(5 * time.Minute)
		if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-30*time.Second).Before(deadline) {
			deadline = testDeadline.Add(-30 * time.Second)
		}
		require.True(t, deadline.After(time.Now()), "insufficient test deadline for owned daemon cleanup")
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		cmd := exec.CommandContext(ctx, binary, "daemon", "start", "--backend", "sqlite", "--backend-path", storePath, "--embeddings=false", "--no-progress")
		cmd.Cancel = func() error {
			_, stopErr := command(6*time.Second, "daemon", "stop")
			return stopErr
		}
		cmd.WaitDelay = 10 * time.Second
		cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, root, log, log
		if startErr := cmd.Start(); startErr != nil {
			cancel()
			_ = log.Close()
			t.Fatal(startErr)
		}
		child = cmd
		t.Logf("owned daemon run=%d pid=%d root=%s socket=%s", run, cmd.Process.Pid, root, daemon.SocketPath())
		exited = make(chan error, 1)
		go func() { waitErr := cmd.Wait(); cancel(); _ = log.Close(); exited <- waitErr }()
	}
	stop := func(label string) {
		started := time.Now()
		output, stopErr := command(6*time.Second, "daemon", "stop")
		cliElapsed := time.Since(started)
		t.Logf("%s cli_stop=%s output=%s", label, cliElapsed, output)
		require.NoError(t, stopErr)
		select {
		case waitErr := <-exited:
			pid := child.Process.Pid
			child = nil // The captured child is reaped even if the assertion fails.
			require.NoError(t, waitErr, "captured daemon must exit normally, without SIGKILL")
			t.Logf("%s pid=%d sigkill=false", label, pid)
		case <-time.After(5*time.Second - time.Since(started)):
			t.Fatal("captured daemon did not exit within the complete 5-second stop budget")
		}
		_, statErr := os.Lstat(daemon.SocketPath())
		require.True(t, os.IsNotExist(statErr), "stop must remove the socket")
		elapsed := time.Since(started)
		t.Logf("%s complete_stop=%s cli_stop=%s socket_removed=true", label, elapsed, cliElapsed)
		require.LessOrEqual(t, elapsed, 5*time.Second, "budget includes CLI startup, captured join and socket removal")
	}
	t.Cleanup(func() {
		if child != nil {
			_, _ = command(6*time.Second, "daemon", "stop")
			select {
			case waitErr := <-exited:
				if waitErr != nil {
					t.Errorf("owned daemon cleanup exit: %v", waitErr)
				}
			case <-time.After(10 * time.Second):
				t.Error("owned daemon required cleanup SIGKILL; gate failed")
				_ = child.Process.Kill()
				<-exited
			}
		}
	})
	status := func() (daemon.StatusResponse, bool) {
		client, dialErr := daemon.DialTo(daemon.SocketPath(), daemon.Handshake{Mode: daemon.ModeControl, CWD: a, ClientName: "offline-untrack-gate"})
		if dialErr != nil {
			return daemon.StatusResponse{}, false
		}
		defer client.Close()
		response, callErr := client.ControlWithTimeout(daemon.ControlStatus, nil, time.Second)
		var result daemon.StatusResponse
		if callErr != nil || !response.OK || json.Unmarshal(response.Result, &result) != nil {
			return daemon.StatusResponse{}, false
		}
		return result, true
	}
	start(1)
	require.Eventually(t, func() bool {
		current, ok := status()
		if !ok {
			return false
		}
		data, readErr := os.ReadFile(logPath)
		if readErr != nil {
			return false
		}
		started := offlineUntrackColdStarted(data, "escape-b")
		for _, repo := range current.TrackedRepos {
			if repo.Prefix == "escape-b" && repo.Loading && started {
				return true
			}
		}
		return false
	}, 2*time.Minute, 20*time.Millisecond, "B must be actively cold-indexing, not merely configured or already ready")
	stop("cold")
	store, err = store_sqlite.Open(storePath)
	require.NoError(t, err, "fresh open proves the captured store lock was released")
	beforeRows := len(store.GetRepoNodes("escape-b"))
	require.NoError(t, store.Close())
	require.Positive(t, beforeRows, "orphan purge must have actual B rows to remove")
	started := time.Now()
	output, err := command(2*time.Second, "untrack", b)
	untrackElapsed := time.Since(started)
	t.Logf("offline_untrack=%s output=%s", untrackElapsed, output)
	require.NoError(t, err, "%s", output)
	require.LessOrEqual(t, untrackElapsed, time.Second)
	require.Contains(t, string(output), "(config only)")
	gc, err = config.LoadGlobal()
	require.NoError(t, err)
	require.Len(t, gc.Repos, 1)
	require.Equal(t, "escape-a", gc.Repos[0].Name)
	start(2)
	require.Eventually(t, func() bool {
		current, ok := status()
		if !ok || !current.Ready {
			return false
		}
		kept := false
		for _, repo := range current.TrackedRepos {
			if repo.Prefix == "escape-b" {
				return false
			}
			kept = kept || (repo.Prefix == "escape-a" && !repo.Loading && repo.Nodes > 0)
		}
		return kept
	}, 2*time.Minute, 20*time.Millisecond)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.False(t, offlineUntrackColdStarted(data, "escape-b"), "removed B must not be cold-indexed on restart")
	stop("restart")
	store, err = store_sqlite.Open(storePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	require.Empty(t, store.GetRepoNodes("escape-b"))
	require.NotEmpty(t, store.GetRepoNodes("escape-a"))
}

func offlineUntrackColdStarted(data []byte, prefix string) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		// newLoggerConfig uses zap's production JSON message key; the
		// admitted full-index event in indexer.go supplies the repo prefix.
		var event struct {
			Msg  string `json:"msg"`
			Repo string `json:"repo"`
		}
		if json.Unmarshal(line, &event) == nil && event.Msg == "indexer: full index started" && event.Repo == prefix {
			return true
		}
	}
	return false
}

func TestOfflineUntrackColdStartProofRequiresExactProducer(t *testing.T) {
	for _, test := range []struct {
		log  string
		want bool
	}{
		{`{"msg":"indexer: full index started","repo":"escape-b"}`, true},
		{`{"msg":"indexer: full index started","repo":"escape-a"}`, false},
		{`{"msg":"daemon: warmup phase start","repo":"escape-b"}`, false},
		{`{"msg":"indexer: full index started","repo":`, false},
	} {
		require.Equal(t, test.want, offlineUntrackColdStarted([]byte(test.log), "escape-b"), test.log)
	}
}
