//go:build acceptance && (linux || darwin)

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The upgrade-with-kill scenario runs a real gortex binary over a copy of an
// old store: it SIGKILLs the daemon as soon as it reports a schema migration,
// restarts it, and records the whole upgrade window as a JSON receipt.
//
//	GORTEX_UPGRADE_SCENARIO_BINARY=<gortex binary under test>
//	GORTEX_UPGRADE_SCENARIO_STORE=<store an older release wrote>
//	GORTEX_UPGRADE_SCENARIO_OUT=<receipt.json>   (optional)
//	go test -tags acceptance -run '^TestUpgradeKillScenario$' ./cmd/gortex/
//
// The daemon opens its socket only after the migration, so nothing can
// answer tools/list while it runs. The receipt measures that window (every
// sample's socket state) instead of asserting a serving gate the daemon
// cannot meet yet.
const (
	upgradeScenarioBinaryEnv = "GORTEX_UPGRADE_SCENARIO_BINARY"
	upgradeScenarioStoreEnv  = "GORTEX_UPGRADE_SCENARIO_STORE"
	upgradeScenarioOutEnv    = "GORTEX_UPGRADE_SCENARIO_OUT"
)

type upgradeScenarioSample struct {
	AtMillis int64  `json:"at_ms"`
	Phase    string `json:"phase"`
	Socket   string `json:"socket"` // "none" or "accepting"
	WALBytes int64  `json:"wal_bytes"`
}

type upgradeScenarioRun struct {
	PID             int   `json:"pid"`
	Killed          bool  `json:"killed"`
	KilledAtMillis  int64 `json:"killed_at_ms,omitempty"`
	KilledInVersion int   `json:"killed_in_migration_version,omitempty"`
	ServingAtMillis int64 `json:"serving_at_ms,omitempty"`
	// ToolsListMillis covers what a fresh editor session waits for: dial,
	// initialize and tools/list.
	ToolsListMillis  int64                   `json:"tools_list_ms,omitempty"`
	ToolsListOutcome string                  `json:"tools_list_outcome,omitempty"`
	BytesWritten     *int64                  `json:"bytes_written,omitempty"` // Linux /proc/<pid>/io only
	Samples          []upgradeScenarioSample `json:"samples"`
}

type upgradeScenarioReceipt struct {
	FromVersion      int   `json:"from_version"`
	StoreBytesBefore int64 `json:"store_bytes_before"`
	StoreBytesAfter  int64 `json:"store_bytes_after"`
	WALPeakBytes     int64 `json:"wal_peak_bytes"`
	BackupBytes      int64 `json:"backup_bytes"`
	// WallMillis runs from the first start until the restarted daemon serves.
	WallMillis                 int64              `json:"wall_ms"`
	Interrupted                upgradeScenarioRun `json:"interrupted"`
	Restarted                  upgradeScenarioRun `json:"restarted"`
	MigratingSamples           int                `json:"migrating_samples"`
	MigratingSamplesWithSocket int                `json:"migrating_samples_with_socket"`
	ServingDuringMigration     bool               `json:"serving_during_migration"`
	Note                       string             `json:"note"`
}

func TestUpgradeKillScenario(t *testing.T) {
	binary, source := os.Getenv(upgradeScenarioBinaryEnv), os.Getenv(upgradeScenarioStoreEnv)
	if binary == "" || source == "" {
		t.Skipf("set %s and %s to run the upgrade-with-kill scenario", upgradeScenarioBinaryEnv, upgradeScenarioStoreEnv)
	}
	// A short root keeps the socket path under the platform's sun_path limit.
	root, err := os.MkdirTemp("", "gx-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	store := filepath.Join(root, "store", "store.sqlite")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		// The store must exist; its WAL is optional.
		if err := copyScenarioFile(source+suffix, store+suffix); err != nil && (suffix == "" || !os.IsNotExist(err)) {
			t.Fatal(err)
		}
	}
	from := scenarioUserVersion(t, store)
	receipt := upgradeScenarioReceipt{FromVersion: from, StoreBytesBefore: fileSize(store)}

	home := filepath.Join(root, "home")
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"GORTEX_DAEMON_SOCKET="+filepath.Join(root, "d.sock"),
		"GORTEX_DAEMON_STATEFILE="+filepath.Join(root, "state.json"),
	)
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(root, "state.json"))
	socket := filepath.Join(root, "d.sock")

	start := time.Now()
	receipt.Interrupted = runScenarioDaemon(t, binary, env, store, socket, start, true)
	receipt.Restarted = runScenarioDaemon(t, binary, env, store, socket, start, false)
	receipt.WallMillis = receipt.Restarted.ServingAtMillis
	receipt.StoreBytesAfter = fileSize(store)
	receipt.BackupBytes = fileSize(store_sqlite.UpgradeBackupPath(store, from))
	for _, run := range []upgradeScenarioRun{receipt.Interrupted, receipt.Restarted} {
		for _, s := range run.Samples {
			receipt.WALPeakBytes = max(receipt.WALPeakBytes, s.WALBytes)
			if s.Phase == string(daemon.StartupMigrating) {
				receipt.MigratingSamples++
				if s.Socket == "accepting" {
					receipt.MigratingSamplesWithSocket++
				}
			}
		}
	}
	receipt.ServingDuringMigration = receipt.MigratingSamplesWithSocket > 0
	receipt.Note = "the daemon opens its socket only after the store migration completes, so tools/list and graph calls " +
		"cannot be answered (not even with a retryable repo_loading) while it runs; serving during the migration " +
		"belongs to the startup readiness work and is measured here, not gated"

	if out := os.Getenv(upgradeScenarioOutEnv); out != "" {
		blob, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("upgrade v%d: wall %dms, WAL peak %d, store %d -> %d, backup %d, migrating samples %d (with socket %d)",
		from, receipt.WallMillis, receipt.WALPeakBytes, receipt.StoreBytesBefore, receipt.StoreBytesAfter,
		receipt.BackupBytes, receipt.MigratingSamples, receipt.MigratingSamplesWithSocket)
	if got := scenarioUserVersion(t, store); got <= from {
		t.Fatalf("the restarted daemon left the store at v%d", got)
	}
	if !receipt.Interrupted.Killed {
		t.Fatalf("the first daemon finished before reporting a migration; nothing was interrupted")
	}
}

// runScenarioDaemon starts the daemon in the foreground and samples its
// startup until it serves; with kill set it SIGKILLs the daemon at the first
// migrating sample instead.
func runScenarioDaemon(t *testing.T, binary string, env []string, store, socket string, origin time.Time, kill bool) upgradeScenarioRun {
	t.Helper()
	cmd := exec.Command(binary, "daemon", "start", "--no-progress", "--backend-path", store)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	run := upgradeScenarioRun{PID: cmd.Process.Pid}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(2 * time.Hour)
	for time.Now().Before(deadline) {
		sample := upgradeScenarioSample{AtMillis: time.Since(origin).Milliseconds(), Socket: "none", WALBytes: fileSize(store + "-wal")}
		st, ok := daemon.ReadRuntimeStateOf(cmd.Process.Pid)
		if ok {
			sample.Phase = string(st.StartupPhase)
		}
		if conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond); err == nil {
			sample.Socket = "accepting"
			_ = conn.Close()
		}
		run.Samples = append(run.Samples, sample)
		if kill && sample.Phase == string(daemon.StartupMigrating) {
			run.BytesWritten = procBytesWritten(cmd.Process.Pid)
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-exited
			run.Killed, run.KilledAtMillis, run.KilledInVersion = true, sample.AtMillis, st.MigrationVersion
			return run
		}
		if sample.Socket == "accepting" && sample.Phase == string(daemon.StartupServing) {
			run.ServingAtMillis = sample.AtMillis
			run.ToolsListMillis, run.ToolsListOutcome = scenarioToolsList(socket)
			run.BytesWritten = procBytesWritten(cmd.Process.Pid)
			stop := exec.Command(binary, "daemon", "stop", "--no-progress")
			stop.Env = env
			_ = stop.Run()
			select {
			case <-exited:
			case <-time.After(2 * time.Minute):
				_ = cmd.Process.Kill()
				<-exited
			}
			return run
		}
		select {
		case err := <-exited:
			t.Fatalf("daemon exited during startup: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	t.Fatal("daemon did not serve within the scenario deadline")
	return run
}

// scenarioToolsList opens a fresh MCP session on the daemon socket, the way
// an editor's proxy does, and lists the tools. The private daemon tracks no
// repository, so the session is unbound, which tools/list allows.
func scenarioToolsList(socket string) (int64, string) {
	began := time.Now()
	elapsed := func() int64 { return time.Since(began).Milliseconds() }
	c, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, ClientName: "upgrade-scenario"})
	if err != nil {
		return elapsed(), "dial: " + err.Error()
	}
	defer c.Close()
	_ = c.Conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := handoffMCPRequest(c, 1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"upgrade-scenario","version":"1"}}}`); err != nil {
		return elapsed(), "initialize: " + err.Error()
	}
	if err := c.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return elapsed(), "initialized: " + err.Error()
	}
	listed, err := handoffMCPRequest(c, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return elapsed(), "tools/list: " + err.Error()
	}
	var tools struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(listed, &tools); err != nil {
		return elapsed(), "tools/list: " + err.Error()
	}
	return elapsed(), fmt.Sprintf("ok (%d tools)", len(tools.Tools))
}

// procBytesWritten reads the bytes a process caused to be written to storage,
// where the OS reports it.
func procBytesWritten(pid int) *int64 {
	blob, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io"))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(blob), "\n") {
		if value, ok := strings.CutPrefix(line, "write_bytes: "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				return &n
			}
		}
	}
	return nil
}

func scenarioUserVersion(t *testing.T, store string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+store+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func copyScenarioFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
