package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/mcp/streamable"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

// Only the controlled holders are synthetic; all other calls use the real
// dispatcher, facade classifier, HTTP mount, Unix server and remote router.
// No source file is modified by the simulated mutation holders.
type transportLaneDispatcher struct {
	*mcpDispatcher
	started chan struct{}
	release <-chan struct{}
}

func (d *transportLaneDispatcher) Dispatch(ctx context.Context, sess *daemon.Session, frame []byte) ([]byte, error) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Hold bool `json:"lane_test_hold"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return nil, err
	}
	if envelope.Method == "tools/call" && envelope.Params.Name == "edit_file" && envelope.Params.Arguments.Hold {
		d.started <- struct{}{}
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"held":true}}`), nil
	}
	return d.mcpDispatcher.Dispatch(ctx, sess, frame)
}

func TestDaemonHTTPAndUnixShareLanesBeforeRemoteRouting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_TOOLS_MODE", "hide")
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))

	var remoteCalls atomic.Int32
	remoteIdentity := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls.Add(1)
		select {
		case remoteIdentity <- r.Header.Get(streamable.HeaderSessionID):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tools/explore" {
			http.Error(w, "unexpected remote endpoint", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"remote":true}`)
	}))
	t.Cleanup(upstream.Close)
	router := daemon.NewRouter(daemon.RouterConfig{Servers: &daemon.ServersConfig{Server: []daemon.ServerEntry{{Slug: "remote", URL: upstream.URL, Workspaces: []string{"lane-remote"}}}}, LocalSlug: daemon.LocalServerSentinel, Logger: zap.NewNop()})
	g := graph.New()
	policy := gortexmcp.ToolPolicyConfig{Preset: "full"}
	srv := gortexmcp.NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{ToolPolicy: &policy})
	inner := newMCPDispatcher(srv, nil, zap.NewNop())
	inner.SetRouter(router)
	limit := daemon.MCPDispatchSnapshot().Limit
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	dispatcher := &transportLaneDispatcher{mcpDispatcher: inner, started: make(chan struct{}, limit), release: release}
	unix, unixDone := startProxyRestartDaemon(t, socket, dispatcher)
	t.Cleanup(func() { unblock(); stopProxyRestartDaemon(t, unix, unixDone) })
	mount := buildDaemonStreamableHandler(dispatcher, daemon.NewSessionRegistry(), router, zap.NewNop(), nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A server-side request deadline (not the HTTP client's timeout) exercises
		// the required deadline-minus100ms queue budget through the real mount.
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()
		mount.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(httpServer.Close)
	httpClient := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(httpClient.CloseIdleConnections)
	post := func(session string, frame string) ([]byte, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", bytes.NewBufferString(frame))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set(streamable.HeaderSessionID, session)
		}
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("HTTP status%d: %s", response.StatusCode, payload)
		}
		return payload, response.Header.Get(streamable.HeaderSessionID)
	}
	_, sid := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"lane-test","version":"1"}}}`)
	if sid == "" {
		t.Fatal("HTTP initialization minted no session")
	}
	client, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, PID: os.Getpid(), Tools: "full", ToolsMode: "hide"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"lane-test","version":"1"}}}`)); err != nil {
		t.Fatal(err)
	}
	if reply, err := client.ReadMCPFrame(); err != nil || !bytes.Contains(reply, []byte(`"result"`)) {
		t.Fatalf("Unix initialize reply=%s err=%v", reply, err)
	}
	if err := client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	for i := range limit {
		frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"edit_file","arguments":{"lane_test_hold":true}}}`, i+10))
		if err := client.WriteMCPFrame(frame); err != nil {
			t.Fatal(err)
		}
		select {
		case <-dispatcher.started:
		case <-time.After(time.Second):
			t.Fatal("Unix mutation did not occupy shared dispatch capacity")
		}
	}
	before := daemon.MCPDispatchSnapshot()
	if before.Lanes["mutating"].InFlight != limit {
		t.Fatalf("Unix did not occupy process cap: %+v", before)
	}
	frame := `{"jsonrpc":"2.0","id":88,"method":"tools/call","params":{"name":"explore","arguments":{"operation":"outline","workspace":"lane-remote"}}}`
	busy, _ := post(sid, frame)
	var refusal struct {
		Error struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(busy, &refusal); err != nil {
		t.Fatal(err)
	}
	retry, _ := refusal.Error.Data["retry_after_ms"].(float64)
	if refusal.Error.Code != -32002 || refusal.Error.Data["delivery_unknown"] != false || refusal.Error.Data["retryable"] != true || retry < 100 || retry > 5000 {
		t.Fatalf("HTTP lane refusal=%s", busy)
	}
	if remoteCalls.Load() != 0 {
		t.Fatal("HTTP remote call bypassed occupied Unix admission cap")
	}
	after := daemon.MCPDispatchSnapshot()
	if after.Lanes["slow"].Refusals != before.Lanes["slow"].Refusals+1 {
		t.Fatalf("effective explore.outline did not use slow admission: before=%+v after=%+v", before, after)
	}
	unblock()
	for received := 0; received < limit; {
		reply, err := client.ReadMCPFrame()
		if err != nil {
			t.Fatal(err)
		}
		var message struct {
			ID    json.RawMessage `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(reply, &message); err != nil {
			t.Fatal(err)
		}
		if len(message.ID) == 0 {
			continue
		}
		if len(message.Error) > 0 {
			t.Fatalf("holder failed: %s", reply)
		}
		received++
	}
	success, _ := post(sid, frame) // Busy was not a committed/replayed tool result.
	if remoteCalls.Load() != 1 || !bytes.Contains(success, []byte(`"remote":true`)) {
		t.Fatalf("admitted remote retry count=%d reply=%s", remoteCalls.Load(), success)
	}
	select {
	case identity := <-remoteIdentity:
		if identity != sid {
			t.Fatalf("remote identity=%q want=%q", identity, sid)
		}
	default:
		t.Fatal("remote received no session identity")
	}
}

// heldToolDispatcher runs frames through a plain mcp-go server, so an HTTP
// notifications/cancelled reaches the same in-flight cancellation a daemon
// session relies on. Its tools only wait; the classifier lets them fill the
// shared and the reserved mutating slots alike.
type heldToolDispatcher struct {
	server *mcpserver.MCPServer
}

func (d heldToolDispatcher) Dispatch(ctx context.Context, _ *daemon.Session, frame []byte) ([]byte, error) {
	reply := d.server.HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}
	return json.Marshal(reply)
}

func (heldToolDispatcher) DispatchClass(name string, _ map[string]any) daemon.MCPDispatchClass {
	if name == "hold_edit" {
		return daemon.MCPDispatchMutating
	}
	return daemon.MCPDispatchFast
}

func TestDaemonHTTPCancellationReachesAFullDispatchPool(t *testing.T) {
	status := daemon.MCPDispatchSnapshot()
	limit, reserve := status.Limit, status.MutationReserve
	started := make(chan struct{}, limit)
	release := make(chan struct{})
	hold := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return mcp.NewToolResultText("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	tools := mcpserver.NewMCPServer("cancel-test", "1")
	tools.AddTool(mcp.NewTool("hold_read"), hold)
	tools.AddTool(mcp.NewTool("hold_edit"), hold)
	mount := buildDaemonStreamableHandler(heldToolDispatcher{server: tools}, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)
	httpServer := httptest.NewServer(mount)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { unblock(); httpServer.Close() })
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	holderClient := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	// The bound only has to tell "delivered now" from "delivered once a holder
	// finishes": these holders never finish on their own.
	cancelClient := &http.Client{Timeout: 2 * time.Second, Transport: transport}
	type posted struct {
		id      int
		status  int
		session string
		body    []byte
		err     error
	}
	post := func(client *http.Client, id int, session, frame string) posted {
		out := posted{id: id}
		req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", bytes.NewBufferString(frame))
		if err != nil {
			out.err = err
			return out
		}
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set(streamable.HeaderSessionID, session)
		}
		response, err := client.Do(req)
		if err != nil {
			out.err = err
			return out
		}
		defer response.Body.Close()
		out.status, out.session = response.StatusCode, response.Header.Get(streamable.HeaderSessionID)
		out.body, out.err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return out
	}
	initialized := post(holderClient, 1, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cancel-test","version":"1"}}}`)
	if initialized.err != nil || initialized.session == "" {
		t.Fatalf("HTTP initialization: session=%q err=%v body=%s", initialized.session, initialized.err, initialized.body)
	}
	sid := initialized.session

	// Every slot is held: the shared ones by reads, the reserved ones by edits.
	results := make(chan posted, limit)
	for i := range limit {
		name := "hold_read"
		if i >= limit-reserve {
			name = "hold_edit"
		}
		id := 100 + i
		go func() {
			results <- post(holderClient, id, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, id, name))
		}()
		select {
		case <-started:
		case early := <-results:
			t.Fatalf("holder %d returned before holding a slot: status=%d err=%v body=%s", early.id, early.status, early.err, early.body)
		case <-time.After(5 * time.Second):
			t.Fatalf("holder %d did not take a dispatch slot", id)
		}
	}
	full := daemon.MCPDispatchSnapshot()
	if full.Lanes["fast"].InFlight != limit-reserve || full.Lanes["mutating"].InFlight != reserve {
		t.Fatalf("holders do not fill the pool: %+v", full.Lanes)
	}

	cancelled := post(cancelClient, 0, sid, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":100}}`)
	if cancelled.err != nil || cancelled.status != http.StatusAccepted {
		t.Fatalf("cancellation waited for the capacity it frees: status=%d err=%v body=%s", cancelled.status, cancelled.err, cancelled.body)
	}
	select {
	case got := <-results:
		if got.id != 100 || got.err != nil || bytes.Contains(got.body, []byte("released")) {
			t.Fatalf("cancelled holder: id=%d status=%d err=%v body=%s", got.id, got.status, got.err, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("the cancelled call kept its slot")
	}
	// The reply follows the slot's release, so the lane already shows it.
	if after := daemon.MCPDispatchSnapshot(); after.Lanes["fast"].InFlight != limit-reserve-1 || after.Lanes["mutating"].InFlight != reserve {
		t.Fatalf("cancelled call did not return its slot: %+v", after.Lanes)
	}
	unblock()
	for range limit - 1 {
		select {
		case got := <-results:
			if got.err != nil || !bytes.Contains(got.body, []byte("released")) {
				t.Fatalf("holder %d: status=%d err=%v body=%s", got.id, got.status, got.err, got.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("released holders did not finish")
		}
	}
}
