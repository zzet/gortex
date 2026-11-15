package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

// Only the local session bridge is test-owned. Admission, slot transfer,
// registered handlers, source locks and socket cancellation are production.
type editACKRegisteredDispatcher struct{ server *Server }

func (d editACKRegisteredDispatcher) DispatchClass(name string, args map[string]any) daemon.MCPDispatchClass {
	return d.server.DispatchClass(name, args)
}
func (d editACKRegisteredDispatcher) SessionStarted(session *daemon.Session, write func([]byte) error) {
	_ = d.server.ConnectSession(session.ID, write)
}
func (d editACKRegisteredDispatcher) SessionEnded(session *daemon.Session) {
	d.server.DisconnectSession(session.ID)
	d.server.ReleaseSession(session.ID)
}
func (d editACKRegisteredDispatcher) Dispatch(ctx context.Context, session *daemon.Session, frame []byte) ([]byte, error) {
	ctx = WithSessionCWD(WithSessionID(ctx, session.ID), session.CWD)
	ctx = d.server.WithClientSession(ctx, session.ID)
	d.server.NoteSessionToolPolicy(session.ID, session.ToolSpec, session.ToolMode)
	var request struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &request); err != nil {
		return nil, err
	}
	if request.Params.Name != "" {
		d.server.EnsureToolPromotedForSession(ctx, request.Params.Name)
		ctx = WithAuthorizedToolCall(ctx, request.Params.Name)
	}
	reply := d.server.MCPServer().HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}
	return json.Marshal(reply)
}

type editACKHeldTierWatcher struct {
	watchedTierFixture
	admitted chan string
}

func (w *editACKHeldTierWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.paths[path] = indexer.MutationTierState{Generation: ticket.Generation, AckAt: time.Now(), FileGraphAt: time.Now()}
	w.mu.Unlock()
	w.admitted <- path
	return ticket, nil
}

// Exercise require_fresh through a real daemon socket and its actual shared
// scheduler. A second registered edit to the same physical path must commit
// while the first still waits: that proves the lock release independently of
// the scheduler's measured mutating-to-slow transfer.
func TestEditACKRequireFreshReleasesRealDispatchAndSamePath(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc helper() {}\n"), 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "main.go"}, {"commit", "-q", "-m", "source fixture"}} {
		argv := append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
		output, err := exec.CommandContext(t.Context(), "git", argv...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	// The real daemon wires its catalog before serving CWD-aware source edits.
	// Bare NewServer does not supply this request-view dependency.
	s.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()})
	require.NotNil(t, s.Materializer())
	s.ToolCallTimeout = -1
	path := filepath.Join(root, "main.go")
	publication := make(chan indexer.MutationResult, 2)
	complete := sync.OnceFunc(func() {
		for range 2 {
			publication <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
		}
		close(publication)
	})
	watcher := &editACKHeldTierWatcher{watchedTierFixture: watchedTierFixture{mutationTestWatcher: mutationTestWatcher{done: publication}, paths: make(map[string]indexer.MutationTierState)}, admitted: make(chan string, 2)}
	s.SetWatcher(watcher)
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))
	d := daemon.New(socket, "edit-ack-release-test", zap.NewNop())
	d.MCPDispatcher = editACKRegisteredDispatcher{server: s}
	require.NoError(t, d.Listen())
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	t.Cleanup(func() {
		complete()
		require.NoError(t, d.Shutdown())
		require.NoError(t, <-served)
		s.DrainBackground()
	})
	connect := func() *daemon.Client {
		t.Helper()
		client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeMCP, CWD: root, Tools: "full", ToolsMode: "hide"}, time.Now().Add(5*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Conn.SetDeadline(time.Now().Add(8*time.Second)))
		require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"release-proof","version":"1"}}}`)))
		for {
			raw, readErr := client.ReadMCPFrame()
			require.NoError(t, readErr)
			var reply struct {
				ID json.RawMessage `json:"id"`
			}
			require.NoError(t, json.Unmarshal(raw, &reply))
			if string(reply.ID) == "1" {
				break
			}
		}
		require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
		return client
	}
	first, second := connect(), connect()
	readReply := func(client *daemon.Client) ([]byte, error) {
		for {
			raw, err := client.ReadMCPFrame()
			if err != nil {
				return nil, err
			}
			var reply struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(raw, &reply); err != nil {
				return nil, err
			}
			if string(reply.ID) == "2" {
				return raw, nil
			}
		}
	}
	frame := func(old, replacement string, fresh bool) []byte {
		args := map[string]any{"path": path, "old_string": old, "new_string": replacement, "format": "json"}
		if fresh {
			args["require_fresh"], args["freshness_scope"] = true, "relations"
			args["wait_deadline"] = time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
		}
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "edit_file", "arguments": args}})
		require.NoError(t, err)
		return data
	}
	firstDone := make(chan []byte, 1)
	firstError := make(chan error, 1)
	firstJoined := make(chan struct{})
	go func() {
		defer close(firstJoined)
		reply, err := readReply(first)
		firstDone <- reply
		firstError <- err
	}()
	t.Cleanup(func() {
		_ = first.Close()
		select {
		case <-firstJoined:
		case <-time.After(3 * time.Second):
			t.Error("owned response reader did not join")
		}
	})
	require.NoError(t, first.WriteMCPFrame(frame("func helper() {}", "func helper() { _ = 1 }", true)))
	select {
	case got := <-watcher.admitted:
		require.Equal(t, path, got)
	case reply := <-firstDone:
		require.NoError(t, <-firstError)
		t.Fatalf("first registered edit completed before watcher admission: %s", reply)
	case <-time.After(3 * time.Second):
		current, readErr := os.ReadFile(path)
		t.Fatalf("first real source commit was not admitted: dispatch=%+v tiers=%+v current=%q read_error=%v", daemon.MCPDispatchSnapshot(), watcher.TierPaths(), current, readErr)
	}
	require.Eventually(t, func() bool {
		status := daemon.MCPDispatchSnapshot()
		return status.Lanes["mutating"].InFlight == 0 && status.Lanes["slow"].InFlight == 1
	}, 2*time.Second, time.Millisecond, "require_fresh did not return the actual mutating slot before its pending wait")

	require.NoError(t, second.WriteMCPFrame(frame("_ = 1", "_ = 2", false)))
	select {
	case got := <-watcher.admitted:
		require.Equal(t, path, got)
	case <-time.After(2 * time.Second):
		t.Fatal("pending require_fresh retained the physical source path lock")
	}
	reply, err := readReply(second)
	require.NoError(t, err)
	assertSuccess := func(reply []byte) {
		t.Helper()
		var response struct {
			Error  any `json:"error"`
			Result *struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(reply, &response))
		require.Nil(t, response.Error, string(reply))
		require.NotNil(t, response.Result, string(reply))
		require.False(t, response.Result.IsError, string(reply))
	}
	assertSuccess(reply)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "func helper() { _ = 2 }")
	select {
	case reply := <-firstDone:
		t.Fatalf("first require_fresh returned before relations were published: %s", reply)
	default:
	}
	watcher.mu.Lock()
	state := watcher.paths[path]
	state.RelationsAt = time.Now()
	watcher.paths[path] = state
	watcher.mu.Unlock()
	complete()
	select {
	case reply := <-firstDone:
		require.NoError(t, <-firstError)
		assertSuccess(reply)
	case <-time.After(3 * time.Second):
		t.Fatal("released freshness waiter did not join after publication")
	}
	require.Eventually(t, func() bool {
		status := daemon.MCPDispatchSnapshot()
		return status.Lanes["mutating"].InFlight == 0 && status.Lanes["slow"].InFlight == 0
	}, time.Second, time.Millisecond, "owned dispatch capacity remained after both calls")
}
