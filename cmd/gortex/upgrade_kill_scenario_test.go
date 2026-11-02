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
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// The upgrade-with-kill scenario runs a real gortex binary over a copy of an
// old store: it SIGKILLs the daemon as soon as it reports a schema migration,
// restarts it, and records the whole upgrade window as a JSON receipt.
//
//	GORTEX_UPGRADE_SCENARIO_BINARY=<gortex binary under test>
//	GORTEX_UPGRADE_SCENARIO_STORE=<store an older release wrote>
//	GORTEX_UPGRADE_SCENARIO_REPO=<repository that store indexed>
//	GORTEX_UPGRADE_SCENARIO_OUT=<receipt.json>   (optional)
//	go test -tags acceptance -run '^TestUpgradeKillScenario$' ./cmd/gortex/
//
// The repository is tracked in the scenario's private home, and every sample
// opens a fresh MCP session from it: it times tools/list and makes one graph
// call. During a migration the same listener must answer tools/list within
// one second and graph calls must report retryable repo_loading:migrating.
// A step transition is retained separately from a stable migration sample.

const (
	upgradeScenarioBinaryEnv = "GORTEX_UPGRADE_SCENARIO_BINARY"
	upgradeScenarioStoreEnv  = "GORTEX_UPGRADE_SCENARIO_STORE"
	upgradeScenarioRepoEnv   = "GORTEX_UPGRADE_SCENARIO_REPO"
	upgradeScenarioOutEnv    = "GORTEX_UPGRADE_SCENARIO_OUT"

	upgradeScenarioNoSocket = "no_socket"
	// upgradeScenarioReadyWait bounds how long the restarted daemon may serve
	// before the tracked repository answers a graph call.
	upgradeScenarioReadyWait = 20 * time.Minute
)

type upgradeScenarioSample struct {
	AtMillis int64  `json:"at_ms"`
	Phase    string `json:"phase"`
	Socket   string `json:"socket"` // "none" or "accepting"
	WALBytes int64  `json:"wal_bytes"`
	// ToolsListMillis covers what a fresh editor session waits for: dial,
	// initialize and tools/list.
	ToolsListMillis int64  `json:"tools_list_ms,omitempty"`
	ToolsList       string `json:"tools_list"`
	// GraphCall is the answer to a symbol search from the tracked
	// repository: "ok", the refusal's error_code (with its phase, if any),
	// or no_socket.
	GraphCall       string `json:"graph_call"`
	GraphRetryable  *bool  `json:"graph_retryable,omitempty"`
	StableMigration bool   `json:"stable_migration"`
}

type upgradeScenarioRun struct {
	StdoutPath      string `json:"stdout_path"`
	StderrPath      string `json:"stderr_path"`
	WaitError       string `json:"wait_error,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	PID             int    `json:"pid"`
	Killed          bool   `json:"killed"`
	KilledAtMillis  int64  `json:"killed_at_ms,omitempty"`
	KilledInVersion int    `json:"killed_in_migration_version,omitempty"`
	ServingAtMillis int64  `json:"serving_at_ms,omitempty"`
	// ReadyAtMillis is the first sample whose graph call answered.
	ReadyAtMillis int64                   `json:"ready_at_ms,omitempty"`
	BytesWritten  *int64                  `json:"bytes_written,omitempty"` // Linux /proc/<pid>/io only
	Samples       []upgradeScenarioSample `json:"samples"`
}

type upgradeScenarioReceipt struct {
	FromVersion      int    `json:"from_version"`
	Repo             string `json:"repo"`
	StoreBytesBefore int64  `json:"store_bytes_before"`
	StoreBytesAfter  int64  `json:"store_bytes_after"`
	// WALPeakBytes covers the upgrade, until the restarted daemon serves;
	// WarmupWALPeakBytes covers the load of the tracked repository after it.
	WALPeakBytes       int64 `json:"wal_peak_bytes"`
	WarmupWALPeakBytes int64 `json:"warmup_wal_peak_bytes"`
	BackupBytes        int64 `json:"backup_bytes"`
	// WallMillis runs from the first start until the restarted daemon serves.
	WallMillis                 int64              `json:"wall_ms"`
	Interrupted                upgradeScenarioRun `json:"interrupted"`
	Restarted                  upgradeScenarioRun `json:"restarted"`
	StableMigratingSamples     int                `json:"stable_migrating_samples"`
	MigratingSamples           int                `json:"migrating_samples"`
	MigratingSamplesWithSocket int                `json:"migrating_samples_with_socket"`
	ServingDuringMigration     bool               `json:"serving_during_migration"`
	// ToolsListMaxMillis is the slowest tools/list among the samples that
	// got one.
	ToolsListMaxMillis int64 `json:"tools_list_max_ms"`
	// GraphCalls counts every sample's graph-call answer.
	GraphCalls map[string]int `json:"graph_calls"`
	Note       string         `json:"note"`
}

func TestUpgradeKillScenario(t *testing.T) {
	binary, source, repo := os.Getenv(upgradeScenarioBinaryEnv), os.Getenv(upgradeScenarioStoreEnv), os.Getenv(upgradeScenarioRepoEnv)
	if binary == "" || source == "" || repo == "" {
		t.Skipf("set %s, %s and %s to run the upgrade-with-kill scenario", upgradeScenarioBinaryEnv, upgradeScenarioStoreEnv, upgradeScenarioRepoEnv)
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
	receipt := upgradeScenarioReceipt{FromVersion: from, Repo: repo, StoreBytesBefore: fileSize(store), GraphCalls: map[string]int{}}

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

	// Track the repository the way its user did, so the restarted daemon
	// loads it from the upgraded store. With no daemon running, track only
	// writes the config.
	track := exec.Command(binary, "track", repo, "--no-progress")
	track.Env = append(env, "GORTEX_AUTOSTART=off")
	if out, err := track.CombinedOutput(); err != nil {
		t.Fatalf("track %s: %v\n%s", repo, err, out)
	}

	start := time.Now()
	receipt.Interrupted = runScenarioDaemon(t, binary, env, store, socket, repo, start, true)
	receipt.Restarted = runScenarioDaemon(t, binary, env, store, socket, repo, start, false)
	receipt.WallMillis = receipt.Restarted.ServingAtMillis
	receipt.StoreBytesAfter = fileSize(store)
	receipt.BackupBytes = fileSize(store_sqlite.UpgradeBackupPath(store, from))
	for _, run := range []upgradeScenarioRun{receipt.Interrupted, receipt.Restarted} {
		for _, s := range run.Samples {
			if run.ServingAtMillis > 0 && s.AtMillis >= run.ServingAtMillis {
				receipt.WarmupWALPeakBytes = max(receipt.WarmupWALPeakBytes, s.WALBytes)
			} else {
				receipt.WALPeakBytes = max(receipt.WALPeakBytes, s.WALBytes)
			}
			if s.ToolsList == "ok" {
				receipt.ToolsListMaxMillis = max(receipt.ToolsListMaxMillis, s.ToolsListMillis)
			}
			receipt.GraphCalls[s.GraphCall]++
			if s.Phase == string(daemon.StartupMigrating) {
				receipt.MigratingSamples++
				if s.StableMigration {
					receipt.StableMigratingSamples++
				}
				if err := validateMigrationSocketSample(s); err != nil {
					t.Errorf("migration sample %dms: %v", s.AtMillis, err)
				}
				if s.Socket == "accepting" {
					receipt.MigratingSamplesWithSocket++
				}
			}
		}
	}
	receipt.ServingDuringMigration = receipt.MigratingSamplesWithSocket > 0
	receipt.Note = "Migration-window metadata is gated at <1s with an accepting socket; stable migration steps require retryable repo_loading{phase: migrating}. Step-transition samples remain explicit and do not invent graph-phase evidence."
	if receipt.StableMigratingSamples == 0 {
		t.Error("AC6 has no stable migrating sample; graph-phase evidence is unavailable")
	}

	if out := os.Getenv(upgradeScenarioOutEnv); out != "" {
		blob, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("upgrade v%d: wall %dms, ready %dms, WAL peak %d (warmup %d), store %d -> %d, backup %d, migrating samples %d (with socket %d), tools/list max %dms, graph calls %v",
		from, receipt.WallMillis, receipt.Restarted.ReadyAtMillis, receipt.WALPeakBytes, receipt.WarmupWALPeakBytes, receipt.StoreBytesBefore, receipt.StoreBytesAfter,
		receipt.BackupBytes, receipt.MigratingSamples, receipt.MigratingSamplesWithSocket, receipt.ToolsListMaxMillis, receipt.GraphCalls)
	if got := scenarioUserVersion(t, store); got <= from {
		t.Fatalf("the restarted daemon left the store at v%d", got)
	}
	if !receipt.Interrupted.Killed {
		t.Fatalf("the first daemon finished before reporting a migration; nothing was interrupted")
	}
	if n := receipt.GraphCalls[string(gortexmcp.ErrCodeRepoNotTracked)]; n > 0 {
		t.Errorf("%d graph calls from the tracked repository %s answered repo_not_tracked", n, repo)
	}
}

// runScenarioDaemon starts the daemon in the foreground and samples its
// startup until it serves and the tracked repository answers a graph call;
// with kill set it SIGKILLs the daemon at the first migrating sample instead.
func runScenarioDaemon(t *testing.T, binary string, env []string, store, socket, repo string, origin time.Time, kill bool) (run upgradeScenarioRun) {
	t.Helper()
	cmd := exec.Command(binary, "daemon", "start", "--no-progress", "--backend-path", store)
	cmd.Env = env
	stdout, stderr, persist := scenarioCaptureOutput(t, kill, &run)
	defer persist()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		run.WaitError = "start: " + err.Error()
		t.Fatal(err)
	}
	run.PID = cmd.Process.Pid
	recordWait := func(err error) {
		code := cmd.ProcessState.ExitCode()
		run.ExitCode = &code
		if err != nil {
			run.WaitError = err.Error()
		}
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var servingAt time.Time
	deadline := time.Now().Add(2 * time.Hour)
	for time.Now().Before(deadline) {
		sample := upgradeScenarioSample{
			AtMillis:  time.Since(origin).Milliseconds(),
			Socket:    "none",
			WALBytes:  fileSize(store + "-wal"),
			ToolsList: upgradeScenarioNoSocket,
			GraphCall: upgradeScenarioNoSocket,
		}
		st, ok := daemon.ReadRuntimeStateOf(cmd.Process.Pid)
		if ok {
			sample.Phase = string(st.StartupPhase)
		}
		if conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond); err == nil {
			sample.Socket = "accepting"
			_ = conn.Close()
			sample.ToolsListMillis, sample.ToolsList, sample.GraphCall, sample.GraphRetryable = scenarioProbe(socket, repo)
		}
		after, afterOK := daemon.ReadRuntimeStateOf(cmd.Process.Pid)
		sample.StableMigration = ok && afterOK && st.StartupPhase == daemon.StartupMigrating && after.StartupPhase == daemon.StartupMigrating && st.MigrationVersion == after.MigrationVersion
		run.Samples = append(run.Samples, sample)
		if kill && sample.Phase == string(daemon.StartupMigrating) {
			run.BytesWritten = procBytesWritten(cmd.Process.Pid)
			_ = cmd.Process.Signal(syscall.SIGKILL)
			recordWait(<-exited)
			run.Killed, run.KilledAtMillis, run.KilledInVersion = true, sample.AtMillis, st.MigrationVersion
			return run
		}
		if servingAt.IsZero() && sample.Socket == "accepting" && sample.Phase == string(daemon.StartupServing) {
			servingAt = time.Now()
			run.ServingAtMillis = sample.AtMillis
			run.BytesWritten = procBytesWritten(cmd.Process.Pid)
		}
		// A first daemon that serves without migrating is stopped at once
		// (the scenario then fails); the restarted one is followed through
		// the warmup a session meets after the migration.
		if !servingAt.IsZero() && (kill || sample.GraphCall == "ok") {
			if sample.GraphCall == "ok" {
				run.ReadyAtMillis = sample.AtMillis
			}
			stop := exec.Command(binary, "daemon", "stop", "--no-progress")
			stop.Env = env
			_ = stop.Run()
			select {
			case err := <-exited:
				recordWait(err)
			case <-time.After(2 * time.Minute):
				_ = cmd.Process.Kill()
				recordWait(<-exited)
			}
			return run
		}
		if !servingAt.IsZero() && time.Since(servingAt) > upgradeScenarioReadyWait {
			_ = cmd.Process.Kill()
			recordWait(<-exited)
			t.Fatalf("the tracked repository answered no graph call within %s of serving; last answer: %s", upgradeScenarioReadyWait, sample.GraphCall)
		}
		select {
		case err := <-exited:
			recordWait(err)
			t.Fatalf("daemon exited during startup: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	recordWait(<-exited)
	t.Fatal("the daemon did not answer a graph call within the scenario deadline")
	return run
}

// scenarioProbe opens a fresh MCP session from repo, the way an editor's
// proxy does: it times dial, initialize and tools/list, then makes one graph
// call. It returns the tools/list time and outcome and the graph answer.
func scenarioProbe(socket, repo string) (int64, string, string, *bool) {
	began := time.Now()
	elapsed := func() int64 { return time.Since(began).Milliseconds() }
	c, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeMCP, CWD: repo, ClientName: "upgrade-scenario"}, began.Add(time.Second))
	if err != nil {
		return elapsed(), "dial: " + err.Error(), "dial: " + err.Error(), nil
	}
	defer c.Close()
	initialized, err := scenarioRequest(c, 1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"upgrade-scenario","version":"1"}}}`)
	if outcome := scenarioOutcome(initialized, err); outcome != "ok" {
		return elapsed(), "initialize: " + outcome, "initialize: " + outcome, nil
	}
	if err := c.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return elapsed(), "initialized: " + err.Error(), "initialized: " + err.Error(), nil
	}
	listed, err := scenarioRequest(c, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	toolsListMillis, toolsList := elapsed(), scenarioOutcome(listed, err)
	if err := c.Conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return toolsListMillis, toolsList, "error: " + err.Error(), nil
	}
	called, err := scenarioRequest(c, 3, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"operation":"symbols","query":"Open"}}}`)
	var retryable *bool
	if remote, ok := called["error"].(map[string]any); ok {
		if data, ok := remote["data"].(map[string]any); ok {
			if value, ok := data["retryable"].(bool); ok {
				retryable = &value
			}
		}
	}
	return toolsListMillis, toolsList, scenarioOutcome(called, err), retryable
}

// scenarioRequest sends one request and returns the reply paired to its id,
// skipping whatever the server sends in between.
func scenarioRequest(c *daemon.Client, id int, frame string) (map[string]any, error) {
	if err := c.WriteMCPFrame([]byte(frame)); err != nil {
		return nil, err
	}
	for {
		raw, err := c.ReadMCPFrame()
		if err != nil {
			return nil, err
		}
		var reply map[string]any
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, err
		}
		if got, ok := reply["id"].(float64); ok && int(got) == id && reply["method"] == nil {
			return reply, nil
		}
	}
}

// scenarioOutcome names a reply: "ok", the refusal's error_code with its
// phase when it carries one (repo_loading{phase: migrating}), or the failure.
func scenarioOutcome(reply map[string]any, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if rpcErr, ok := reply["error"].(map[string]any); ok {
		data, _ := rpcErr["data"].(map[string]any)
		code, _ := data["error_code"].(string)
		if code == "" {
			return fmt.Sprintf("error: %v", rpcErr["message"])
		}
		if phase, _ := data["phase"].(string); phase != "" {
			return fmt.Sprintf("%s{phase: %s}", code, phase)
		}
		return code
	}
	if result, _ := reply["result"].(map[string]any); result["isError"] == true {
		// A tool error carries its structured error as the text block.
		content, _ := result["content"].([]any)
		if len(content) > 0 {
			block, _ := content[0].(map[string]any)
			text, _ := block["text"].(string)
			var structured gortexmcp.StructuredError
			if json.Unmarshal([]byte(text), &structured) == nil && structured.ErrorCode != "" {
				return "tool_error: " + string(structured.ErrorCode)
			}
		}
		return "tool_error"
	}
	return "ok"
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

// No fixture is opened by this predicate or its negative controls.
func validateMigrationSocketSample(s upgradeScenarioSample) error {
	if s.Socket != "accepting" || s.ToolsList != "ok" {
		return fmt.Errorf("migration metadata unavailable: socket=%s tools/list=%s", s.Socket, s.ToolsList)
	}
	if s.ToolsListMillis >= 1000 {
		return fmt.Errorf("tools/list took %dms; millisecond evidence must be below 1000ms", s.ToolsListMillis)
	}
	if s.StableMigration && (s.GraphCall != "repo_loading{phase: migrating}" || s.GraphRetryable == nil || !*s.GraphRetryable) {
		return fmt.Errorf("stable migration graph reply is not retryable migrating: %s", s.GraphCall)
	}
	return nil
}

func TestUpgradeMigrationSocketSampleGate(t *testing.T) {
	retryable := true
	valid := upgradeScenarioSample{Socket: "accepting", ToolsList: "ok", ToolsListMillis: 2, GraphCall: "repo_loading{phase: migrating}", GraphRetryable: &retryable, StableMigration: true}
	if err := validateMigrationSocketSample(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*upgradeScenarioSample){
		func(s *upgradeScenarioSample) { s.Socket = "none" },
		func(s *upgradeScenarioSample) { s.ToolsList = upgradeScenarioNoSocket },
		func(s *upgradeScenarioSample) { s.ToolsListMillis = 1000 },
		func(s *upgradeScenarioSample) { s.GraphCall = "repo_not_tracked" },
		func(s *upgradeScenarioSample) { s.GraphRetryable = nil },
	} {
		broken := valid
		mutate(&broken)
		if err := validateMigrationSocketSample(broken); err == nil {
			t.Fatalf("invalid migration evidence passed: %+v", broken)
		}
	}
	transition := valid
	transition.StableMigration = false
	transition.GraphCall = "repo_loading{phase: snapshot_loaded}"
	if err := validateMigrationSocketSample(transition); err != nil {
		t.Fatal("a real transition must not claim a stable migration phase", err)
	}
}
