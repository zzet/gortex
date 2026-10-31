//go:build linux || darwin

package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

// The helper owns a real SQLite store and exclusive daemon-style flock. Its
// controlled slow-close interval begins after SIGTERM, while both remain open;
// the final committed marker proves the old three-second kill did not interrupt
// shutdown. No production failure-injection or protocol-version knob is added.
func TestDaemonHandoffProcessHelper(t *testing.T) {
	root := os.Getenv("GORTEX_HANDOFF_TEST_ROOT")
	if root == "" {
		t.Skip("owned subprocess helper")
	}
	for key, name := range map[string]string{"GORTEX_DAEMON_SOCKET": "s", "GORTEX_DAEMON_PIDFILE": "p", "GORTEX_DAEMON_STATEFILE": "state.json"} {
		if err := os.Setenv(key, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "store.sqlite")
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("helper lock: %v, %v", ok, err)
	}
	defer lock.Unlock() //nolint:errcheck // helper cleanup
	s, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // explicit successful close below
	s.AddNode(&graph.Node{ID: "handoff:sentinel", Name: "BeforeHandoff", Kind: graph.KindFunction, FilePath: "fixture.go"})
	if err := os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteRuntimeState(daemon.RuntimeState{PID: os.Getpid(), BackendPath: path}); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, platform.ShutdownSignals()...)
	defer signal.Stop(signals)
	listener, err := net.Listen("unix", daemon.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // helper cleanup
	var handlers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(time.Second))
				if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil {
					return
				}
				ack := daemon.HandshakeAck{ErrorCode: daemon.ErrProtocolMismatch, ErrorMsg: "daemon expects protocol 0, client sent 1"}
				if os.Getenv("GORTEX_HANDOFF_TEST_LEGACY") != "1" {
					zero := 0
					ack.DaemonProtocol = &zero
					ack.DaemonVersion = "v0.64.10"
					ack.DaemonInstance = fmt.Sprintf("old-%d", os.Getpid())
				}
				_ = daemon.WriteJSONLine(c, ack)
			}()
		}
	}()
	if err := os.WriteFile(filepath.Join(root, "ready"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-signals
	_ = listener.Close()
	<-accepted
	handlers.Wait()
	started := time.Now()
	if err := os.WriteFile(filepath.Join(root, "closing"), []byte(strconv.FormatInt(started.UnixNano(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GORTEX_HANDOFF_TEST_SLOW") == "1" {
		time.Sleep(3500 * time.Millisecond)
	}
	s.AddNode(&graph.Node{ID: "handoff:closed", Name: "CloseCompleted", Kind: graph.KindFunction, FilePath: "fixture.go"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "closed"), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
}

type ownedHandoffChild struct {
	root, path string
	cmd        *exec.Cmd
	done       chan error
}

func startOwnedHandoffChild(t *testing.T, legacy, slow bool) *ownedHandoffChild {
	t.Helper()
	root := testenv.ShortTempDir(t)
	for key, name := range map[string]string{"GORTEX_DAEMON_SOCKET": "s", "GORTEX_DAEMON_PIDFILE": "p", "GORTEX_DAEMON_STATEFILE": "state.json"} {
		t.Setenv(key, filepath.Join(root, name))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHandoffProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GORTEX_HANDOFF_TEST_ROOT="+root)
	if legacy {
		cmd.Env = append(cmd.Env, "GORTEX_HANDOFF_TEST_LEGACY=1")
	}
	if slow {
		cmd.Env = append(cmd.Env, "GORTEX_HANDOFF_TEST_SLOW=1")
	}
	log, err := os.Create(filepath.Join(root, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); _ = log.Close() }()
	c := &ownedHandoffChild{root: root, path: filepath.Join(root, "store.sqlite"), cmd: cmd, done: done}
	t.Cleanup(func() {
		if platform.ProcessAlive(cmd.Process.Pid) {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-done:
			if err != nil {
				b, _ := os.ReadFile(filepath.Join(root, "child.log"))
				t.Logf("owned child exit: %v\n%s", err, b)
			}
		case <-time.After(10 * time.Second):
			t.Error("owned handoff child was not reaped")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			return c
		}
		if !platform.ProcessAlive(cmd.Process.Pid) || !time.Now().Before(deadline) {
			b, _ := os.ReadFile(filepath.Join(root, "child.log"))
			t.Fatalf("owned mismatch daemon not ready:\n%s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertHandoffStoreIntegrity(t *testing.T, path string) {
	t.Helper()
	s, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatalf("replacement open/migration: %v", err)
	}
	for _, id := range []string{"handoff:sentinel", "handoff:closed"} {
		if s.GetNode(id) == nil {
			_ = s.Close()
			t.Fatalf("handoff lost committed payload %s", id)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity = %q (%v)", result, err)
	}
}

func TestDaemonHandoffWaitsPastThreeSecondsAndReleasesIntactStore(t *testing.T) {
	isolateSpawnLock(t)
	t.Cleanup(restoreSeams)
	binaryVersion = func() string { return "v0.64.12" }
	child := startOwnedHandoffChild(t, false, true)
	identity := dialRunningDaemonIdentity()
	if !identity.mismatch() || identity.peerPID != child.cmd.Process.Pid {
		t.Fatalf("kernel mismatch identity = %+v", identity)
	}
	started := time.Now()
	if err := stopDaemonForHandoff(identity); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 3500*time.Millisecond {
		t.Fatalf("slow close was interrupted after %s", elapsed)
	}
	if _, err := os.Stat(filepath.Join(child.root, "closed")); err != nil {
		t.Fatalf("child never completed close: %v", err)
	}
	lock := flock.New(child.path + ".lock")
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("store lock after stop: %v %v", ok, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	assertHandoffStoreIntegrity(t, child.path)
	t.Logf("owned graceful stop %s; committed close marker retained; store lock free; integrity ok", elapsed)
}

func TestDaemonHandoffRejectsUnrelatedPIDAndChangedRuntime(t *testing.T) {
	isolateSpawnLock(t)
	t.Cleanup(restoreSeams)
	binaryVersion = func() string { return "v0.64.12" }
	child := startOwnedHandoffChild(t, false, false)
	identity := dialRunningDaemonIdentity()
	g, err := captureDaemonHandoffPID(identity)
	if err != nil {
		t.Fatal(err)
	}
	// The captured socket still names the owned child. A replaced PID file
	// or runtime must not authorize signaling any process through it.
	if err := os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.validate(true); err == nil {
		t.Fatal("changed PID file accepted")
	}
	if !platform.ProcessAlive(child.cmd.Process.Pid) {
		t.Fatal("identity refusal terminated the owned daemon")
	}
	if err := os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(child.cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	state, ok := daemon.ReadRuntimeState()
	if !ok || state.PID != child.cmd.Process.Pid {
		t.Fatalf("owned child runtime before path change: %+v (valid=%v)", state, ok)
	}
	state.BackendPath = filepath.Join(child.root, "other.sqlite")
	// WriteRuntimeState stamps the caller's PID. This fixture must preserve
	// the actual owned child's identity and change only its declared store.
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.RuntimeStatePath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, ok := daemon.ReadRuntimeState()
	if !ok || changed.PID != child.cmd.Process.Pid || changed.BackendPath != state.BackendPath {
		t.Fatalf("fixture lost child PID while changing store: %+v (valid=%v)", changed, ok)
	}
	if err := g.validate(true); err == nil || !strings.Contains(err.Error(), "PID/runtime/store identity changed") {
		t.Fatalf("changed store refusal = %v", err)
	}
	if !platform.ProcessAlive(child.cmd.Process.Pid) {
		t.Fatal("runtime refusal terminated the owned daemon")
	}
}

// Four real clients dispatch through the production MCP server after one
// old-daemon replacement. Only lifecycle creation is injected; neither the
// handshake nor tools/list result is a counter-only stub.
func TestEnsureDaemonMismatchedHandoffConcurrentClientsReachPublicTools(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "structured"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			isolateSpawnLock(t)
			t.Cleanup(restoreSeams)
			child := startOwnedHandoffChild(t, legacy, false)
			binaryVersion = func() string { return "v0.64.12" }
			serviceActive = func() bool { return false }
			var spawns atomic.Int32
			var newDaemon *daemon.Server
			var newStore *store_sqlite.Store
			var storeLock *flock.Flock
			var serveDone chan error
			t.Cleanup(func() {
				if newDaemon != nil {
					_ = newDaemon.Shutdown()
					select {
					case err := <-serveDone:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(5 * time.Second):
						t.Error("replacement daemon did not stop")
					}
				}
				if newStore != nil {
					if err := newStore.Close(); err != nil {
						t.Error(err)
					}
				}
				if storeLock != nil {
					if err := storeLock.Unlock(); err != nil {
						t.Error(err)
					}
				}
			})
			spawnDaemon = func() error {
				spawns.Add(1)
				storeLock = flock.New(child.path + ".lock")
				if ok, err := storeLock.TryLock(); err != nil || !ok {
					return fmt.Errorf("replacement store lock: %v %v", ok, err)
				}
				var err error
				newStore, err = store_sqlite.Open(child.path)
				if err != nil {
					return err
				}
				server := gortexmcp.NewServer(query.NewEngine(newStore), newStore, nil, nil, zap.NewNop(), nil)
				newDaemon = daemon.New(daemon.SocketPath(), binaryVersion(), zap.NewNop())
				newDaemon.MCPDispatcher = newMCPDispatcher(server, nil, zap.NewNop())
				if err := newDaemon.Listen(); err != nil {
					return err
				}
				serveDone = make(chan error, 1)
				go func() { serveDone <- newDaemon.Serve() }()
				return nil
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			stderr, _ := captureLegacyStderr(t, func() {
				for range 4 {
					wg.Go(func() {
						if _, _, err := ensureDaemonReadyWithError(true); err != nil {
							errs <- err
							return
						}
						c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeMCP, ClientName: "handoff-test", Tools: "facade-v1", ToolsMode: "hide"})
						if err != nil {
							errs <- err
							return
						}
						defer c.Close()
						if c.Ack.DaemonVersion != "v0.64.12" {
							errs <- fmt.Errorf("replacement version %q", c.Ack.DaemonVersion)
							return
						}
						_ = c.Conn.SetDeadline(time.Now().Add(5 * time.Second))
						initialized, err := handoffMCPRequest(c, 1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"handoff-test","version":"1"}}}`)
						if err != nil {
							errs <- err
							return
						}
						var initResult struct {
							ServerInfo struct {
								Name string `json:"name"`
							} `json:"serverInfo"`
						}
						if json.Unmarshal(initialized, &initResult) != nil || initResult.ServerInfo.Name != "gortex" {
							errs <- fmt.Errorf("public initialize identity: %s", initialized)
							return
						}
						if err := c.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
							errs <- err
							return
						}
						listed, err := handoffMCPRequest(c, 2, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
						if err != nil {
							errs <- err
							return
						}
						var tools struct {
							Tools []struct {
								Name string `json:"name"`
							} `json:"tools"`
						}
						if err := json.Unmarshal(listed, &tools); err != nil {
							errs <- err
							return
						}
						for _, tool := range tools.Tools {
							if tool.Name == "read" {
								return
							}
						}
						errs <- fmt.Errorf("replacement did not publish public read facade: %s", listed)
					})
				}
				wg.Wait()
			})
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if spawns.Load() != 1 {
				t.Fatalf("four clients caused %d replacements", spawns.Load())
			}
			if strings.Count(stderr, "[gortex mcp] replaced daemon ") != 1 {
				t.Fatalf("handoff line count:\n%s", stderr)
			}
			if !strings.Contains(stderr, "protocol 0") || !strings.Contains(stderr, "v0.64.12 (protocol 1)") {
				t.Fatalf("handoff identities missing:\n%s", stderr)
			}
		})
	}
}

// Daemons may send notifications between responses. Match the actual request
// ID rather than treating an unrelated frame or an empty tool list as success.
func handoffMCPRequest(c *daemon.Client, id int, frame string) (json.RawMessage, error) {
	if err := c.WriteMCPFrame([]byte(frame)); err != nil {
		return nil, err
	}
	for {
		reply, err := c.ReadMCPFrame()
		if err != nil {
			return nil, err
		}
		var response struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(reply, &response); err != nil {
			return nil, err
		}
		if response.ID == nil && response.Method != "" {
			continue
		}
		if response.ID == nil || *response.ID != id || len(response.Error) > 0 && string(response.Error) != "null" || len(response.Result) == 0 {
			return nil, fmt.Errorf("public MCP response for id %d: %s", id, reply)
		}
		return response.Result, nil
	}
}
