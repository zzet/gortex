package streamable

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
)

// newTestMCPServer mints an mcp-go server pre-loaded with an `echo`
// tool. The tool's handler observes the in-context session ID via
// gortexmcp.SessionIDFromContext so tests can verify the transport
// threads that value through.
func newTestMCPServer() *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer("test", "0.0.0",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery())
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("echo"),
			mcp.WithString("message"),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			msg, _ := args["message"].(string)
			return mcp.NewToolResultText(msg), nil
		},
	)
	return srv
}

type countingDispatcher struct {
	delegate Dispatcher
	calls    atomic.Int64
}

func (d *countingDispatcher) Dispatch(ctx context.Context, frame []byte) ([]byte, error) {
	d.calls.Add(1)
	return d.delegate.Dispatch(ctx, frame)
}

// newTransport stands up a transport backed by a fresh MemoryStore
// and the test mcp-go server. Returns the transport and a cleanup
// func that releases the store's background sweeper.
func newTransport(t *testing.T) (*Transport, func()) {
	t.Helper()
	store := NewMemoryStore(time.Minute)
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
	})
	return tr, func() { store.Close() }
}

func newCountingTransport(t *testing.T) (*Transport, *MemoryStore, *countingDispatcher) {
	t.Helper()
	store := NewMemoryStore(time.Minute)
	t.Cleanup(store.Close)
	dispatcher := &countingDispatcher{delegate: MCPServerDispatcher{Server: newTestMCPServer()}}
	return New(Config{Dispatcher: dispatcher, Store: store}), store, dispatcher
}

// jsonRPC builds a single JSON-RPC request body. id may be a string,
// number, or nil (notification).
func jsonRPC(id any, method string, params any) []byte {
	env := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if id != nil {
		env["id"] = id
	}
	if params != nil {
		env["params"] = params
	}
	out, _ := json.Marshal(env)
	return out
}

func initializeBody(name, version string) []byte {
	return jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": DefaultProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": name, "version": version},
	})
}

// doPOST sends one request to the transport and returns the response
// recorder. headers is optional; nil = none.
func doPOST(t *testing.T, tr *Transport, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	return rec
}

// The idle TTL is the transport's own policy, so the transport is the
// one to advertise it: a client's first knowledge of session expiry
// must not be the 404/-32001 after it already happened. Every response
// carries the TTL as a header, and the initialize result additionally
// carries it in `_meta` where typed MCP clients can reach it.
func TestSessionIdleTTL_IsAdvertised(t *testing.T) {
	store := NewMemoryStore(5 * time.Minute)
	t.Cleanup(store.Close)
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
		SessionTTL: 5 * time.Minute,
	})

	rec := doPOST(t, tr, initializeBody("claude", "1.0"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderSessionIdleTTL); got != "300" {
		t.Errorf("%s header = %q, want \"300\"", HeaderSessionIdleTTL, got)
	}
	var reply struct {
		Result struct {
			Meta       map[string]any `json:"_meta"`
			ServerInfo map[string]any `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal initialize reply: %v", err)
	}
	got, ok := reply.Result.Meta["gortex/session_idle_ttl_seconds"]
	if !ok {
		t.Fatalf("initialize result _meta missing gortex/session_idle_ttl_seconds: %s",
			rec.Body.String())
	}
	if n, _ := got.(float64); n != 300 {
		t.Errorf("_meta TTL = %v, want 300", got)
	}
	if len(reply.Result.ServerInfo) == 0 {
		t.Error("meta injection dropped serverInfo from the initialize result")
	}

	// A follow-up request also carries the header, so a client that
	// missed it at initialize can still learn the horizon later.
	sid := rec.Header().Get(HeaderSessionID)
	rec2 := doPOST(t, tr, jsonRPC(2, "tools/list", nil), map[string]string{HeaderSessionID: sid})
	if rec2.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d", rec2.Code)
	}
	if got := rec2.Header().Get(HeaderSessionIdleTTL); got != "300" {
		t.Errorf("follow-up %s header = %q, want \"300\"", HeaderSessionIdleTTL, got)
	}
}

// A transport with no known TTL (zero SessionTTL) must not advertise
// one — a made-up horizon is worse than none.
func TestSessionIdleTTL_ZeroAdvertisesNothing(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	rec := doPOST(t, tr, initializeBody("claude", "1.0"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderSessionIdleTTL); got != "" {
		t.Errorf("%s header = %q, want absent when no TTL is known", HeaderSessionIdleTTL, got)
	}
	if strings.Contains(rec.Body.String(), "session_idle_ttl_seconds") {
		t.Error("initialize result advertises a TTL that does not exist")
	}
}

// TestInitializeMintsSessionID asserts the spec's headline behaviour:
// an initialize POST returns a fresh `Mcp-Session-Id` header that
// subsequent requests carry forward.
func TestInitializeMintsSessionID(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	body := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "claude-code",
			"version": "1.0.0",
		},
	})
	rec := doPOST(t, tr, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	sid := rec.Header().Get(HeaderSessionID)
	if sid == "" {
		t.Fatal("response missing Mcp-Session-Id header")
	}
	if tr.store.Len() != 1 {
		t.Errorf("store.Len() = %d, want 1", tr.store.Len())
	}
	state, ok := tr.store.Get(sid)
	if !ok {
		t.Fatalf("store doesn't know session %q", sid)
	}
	if state.ClientName != "claude-code" {
		t.Errorf("ClientName = %q, want claude-code", state.ClientName)
	}
	if state.ClientVersion != "1.0.0" {
		t.Errorf("ClientVersion = %q, want 1.0.0", state.ClientVersion)
	}
}

// TestSessionReplayAcrossRequests covers the load-balancer use case:
// initialize on one logical "worker", then a tools/call hitting a
// fresh worker that only shares the session store. The transport
// must look the session up by id and dispatch the call as if it had
// always known about it.
func TestSessionReplayAcrossRequests(t *testing.T) {
	store := NewMemoryStore(time.Minute)
	defer store.Close()
	// Two transport instances — same store, different MCP servers —
	// model what's behind a load balancer.
	workerA := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
	})
	workerB := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
	})

	// 1) Initialize on worker A.
	initBody := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
	})
	rec := doPOST(t, workerA, initBody, nil)
	sid := rec.Header().Get(HeaderSessionID)
	if sid == "" {
		t.Fatal("worker A initialize: no session id")
	}

	// 2) tools/call on worker B with the same session id.
	callBody := jsonRPC(2, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "hello"},
	})
	rec = doPOST(t, workerB, callBody, map[string]string{HeaderSessionID: sid})
	if rec.Code != http.StatusOK {
		t.Fatalf("worker B status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderSessionID); got != sid {
		t.Errorf("worker B echoed session id = %q, want %q", got, sid)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("worker B response missing tool output: %s", rec.Body.String())
	}
}

// TestUnknownSessionRejected covers the stale-session race: a client
// reconnects with a session id the store no longer knows. The HTTP 404
// lets the client reinitialize, while the pre-dispatch gate prevents a
// retry from duplicating the original operation.
func TestUnknownSessionRejected(t *testing.T) {
	tests := []struct {
		name            string
		protocolVersion string
		wantVersion     string
	}{
		{name: "default protocol version", wantVersion: DefaultProtocolVersion},
		{name: "echoed protocol version", protocolVersion: "2025-06-18", wantVersion: "2025-06-18"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tr, _, dispatcher := newCountingTransport(t)
			body := jsonRPC(1, "tools/call", map[string]any{
				"name": "echo", "arguments": map[string]any{"message": "x"},
			})
			headers := map[string]string{HeaderSessionID: "never_existed"}
			if test.protocolVersion != "" {
				headers[HeaderProtocolVersion] = test.protocolVersion
			}
			rec := doPOST(t, tr, body, headers)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get(HeaderProtocolVersion); got != test.wantVersion {
				t.Errorf("protocol version = %q, want %q", got, test.wantVersion)
			}
			if got := rec.Header().Get(HeaderSessionID); got != "" {
				t.Errorf("stale response session id = %q, want empty", got)
			}
			var env struct {
				ID    json.RawMessage `json:"id"`
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body not JSON: %v\n%s", err, rec.Body.String())
			}
			if string(env.ID) != "1" || env.Error.Code != -32001 {
				t.Errorf("error envelope id/code = %s/%d, want 1/-32001", env.ID, env.Error.Code)
			}
			if !strings.Contains(env.Error.Message, "never_existed") {
				t.Errorf("error message = %q; want it to name the missing session", env.Error.Message)
			}
			if got := dispatcher.calls.Load(); got != 0 {
				t.Errorf("dispatcher calls = %d, want 0", got)
			}
		})
	}
}

func TestUnknownSessionRequestShapes(t *testing.T) {
	t.Run("single notification", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		rec := doPOST(t, tr, jsonRPC(nil, "notifications/cancelled", map[string]any{"requestId": 1}),
			map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Fatalf("status/body = %d/%q, want 404/empty", rec.Code, rec.Body.String())
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})

	t.Run("single response", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		response := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
		rec := doPOST(t, tr, response, map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Fatalf("status/body = %d/%q, want 404/empty", rec.Code, rec.Body.String())
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})

	t.Run("invalid request envelope", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		rec := doPOST(t, tr, []byte(`{"method":"ping","id":1}`),
			map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Fatalf("status/body = %d/%q, want 404/empty", rec.Code, rec.Body.String())
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})

	t.Run("mixed batch", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		body, _ := json.Marshal([]json.RawMessage{
			jsonRPC(1, "ping", nil),
			jsonRPC(nil, "notifications/cancelled", map[string]any{"requestId": 1}),
			json.RawMessage(`{"jsonrpc":"2.0","id":99,"result":{}}`),
			jsonRPC("two", "tools/list", nil),
		})
		rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		var replies []struct {
			ID    json.RawMessage `json:"id"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &replies); err != nil {
			t.Fatalf("body not JSON array: %v\n%s", err, rec.Body.String())
		}
		if len(replies) != 2 || string(replies[0].ID) != "1" || string(replies[1].ID) != `"two"` {
			t.Fatalf("reply ids = %+v, want [1, two]", replies)
		}
		for _, reply := range replies {
			if reply.Error.Code != -32001 {
				t.Errorf("error code = %d, want -32001", reply.Error.Code)
			}
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})

	t.Run("notification-only batch", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		body, _ := json.Marshal([]json.RawMessage{
			jsonRPC(nil, "notifications/initialized", nil),
			jsonRPC(nil, "notifications/cancelled", map[string]any{"requestId": 1}),
		})
		rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Fatalf("status/body = %d/%q, want 404/empty", rec.Code, rec.Body.String())
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})

	t.Run("initialize batch", func(t *testing.T) {
		tr, _, dispatcher := newCountingTransport(t)
		body, _ := json.Marshal([]json.RawMessage{initializeBody("test", "1")})
		rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: "expired"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		var replies []struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &replies); err != nil {
			t.Fatalf("body not JSON array: %v\n%s", err, rec.Body.String())
		}
		if len(replies) != 1 || replies[0].Error.Code != -32001 {
			t.Fatalf("initialize batch response = %s, want one -32001 error", rec.Body.String())
		}
		if got := dispatcher.calls.Load(); got != 0 {
			t.Errorf("dispatcher calls = %d, want 0", got)
		}
	})
}

func TestUnknownSessionStandaloneInitializeRejected(t *testing.T) {
	tr, _, dispatcher := newCountingTransport(t)
	body := initializeBody("test", "1")
	rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: "expired"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderSessionID); got != "" {
		t.Errorf("response session id = %q, want empty", got)
	}
	var env struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, rec.Body.String())
	}
	if string(env.ID) != "1" || env.Error.Code != -32001 {
		t.Errorf("error envelope id/code = %s/%d, want 1/-32001", env.ID, env.Error.Code)
	}
	if got := dispatcher.calls.Load(); got != 0 {
		t.Errorf("dispatcher calls = %d, want 0", got)
	}
}

func TestExpiredSessionReinitializesAndRetries(t *testing.T) {
	tr, store, dispatcher := newCountingTransport(t)
	initialize := initializeBody("opencode", "1")
	first := doPOST(t, tr, initialize, nil)
	oldID := first.Header().Get(HeaderSessionID)
	if first.Code != http.StatusOK || oldID == "" {
		t.Fatalf("first initialize status/id = %d/%q", first.Code, oldID)
	}
	if evicted := store.SweepNow(time.Now().Add(2 * time.Hour)); evicted != 1 {
		t.Fatalf("evicted sessions = %d, want 1", evicted)
	}

	call := jsonRPC(2, "tools/call", map[string]any{
		"name": "echo", "arguments": map[string]any{"message": "recovered"},
	})
	beforeStale := dispatcher.calls.Load()
	stale := doPOST(t, tr, call, map[string]string{HeaderSessionID: oldID})
	if stale.Code != http.StatusNotFound || dispatcher.calls.Load() != beforeStale {
		t.Fatalf("stale status/calls = %d/%d, want 404/%d", stale.Code, dispatcher.calls.Load(), beforeStale)
	}

	second := doPOST(t, tr, initialize, nil)
	newID := second.Header().Get(HeaderSessionID)
	if second.Code != http.StatusOK || newID == "" || newID == oldID {
		t.Fatalf("second initialize status/id = %d/%q; old=%q", second.Code, newID, oldID)
	}
	initialized := doPOST(t, tr, jsonRPC(nil, "notifications/initialized", nil),
		map[string]string{HeaderSessionID: newID})
	if initialized.Code != http.StatusAccepted {
		t.Fatalf("initialized notification status = %d, want 202", initialized.Code)
	}
	beforeRetry := dispatcher.calls.Load()
	retry := doPOST(t, tr, call, map[string]string{HeaderSessionID: newID})
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), "recovered") {
		t.Fatalf("retry status/body = %d/%s", retry.Code, retry.Body.String())
	}
	if got := dispatcher.calls.Load(); got != beforeRetry+1 {
		t.Errorf("dispatcher calls after retry = %d, want %d", got, beforeRetry+1)
	}
}

func TestConcurrentUnknownSessionsNeverDispatch(t *testing.T) {
	tr, _, dispatcher := newCountingTransport(t)
	const workers = 25
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := jsonRPC(i+1, "tools/call", map[string]any{
				"name": "echo", "arguments": map[string]any{"message": "stale"},
			})
			if rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: "expired"}); rec.Code != http.StatusNotFound {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("%d/%d stale requests did not return 404", failures.Load(), workers)
	}
	if got := dispatcher.calls.Load(); got != 0 {
		t.Errorf("dispatcher calls = %d, want 0", got)
	}
}

// TestDeleteDropsSession covers the explicit teardown path.
func TestDeleteDropsSession(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	id, err := tr.store.Create(SessionState{ClientName: "test"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(HeaderSessionID, id)
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, ok := tr.store.Get(id); ok {
		t.Error("session still in store after DELETE")
	}
}

// TestDeleteUnknownIsIdempotent — spec mandates 204 even for unknown ids.
func TestDeleteUnknownIsIdempotent(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(HeaderSessionID, "never_existed")
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

// TestStatelessModeOmitsSessionID — when the store is the StatelessStore,
// initialize succeeds but the response carries no Mcp-Session-Id and
// the store stays empty.
func TestStatelessModeOmitsSessionID(t *testing.T) {
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      StatelessStore{},
	})
	body := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
	})
	rec := doPOST(t, tr, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(HeaderSessionID) != "" {
		t.Errorf("stateless mode set Mcp-Session-Id = %q", rec.Header().Get(HeaderSessionID))
	}
	if tr.store.Len() != 0 {
		t.Errorf("stateless store got %d entries", tr.store.Len())
	}

	call := jsonRPC(2, "tools/call", map[string]any{
		"name": "echo", "arguments": map[string]any{"message": "stateless"},
	})
	stale := doPOST(t, tr, call, map[string]string{HeaderSessionID: "expired"})
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale stateless status = %d, want 404", stale.Code)
	}
	retry := doPOST(t, tr, call, nil)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), "stateless") {
		t.Fatalf("sessionless retry status/body = %d/%s", retry.Code, retry.Body.String())
	}
}

// TestBatchRequestPreservesOrder asserts the JSON-RPC batch contract:
// multiple frames in one POST produce a JSON array of replies in the
// same order as the input frames.
func TestBatchRequestPreservesOrder(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	// Build a 3-call batch (interleaved request + notification +
	// request) — notifications produce no reply, so the reply array
	// should have exactly two entries.
	frames := []json.RawMessage{
		jsonRPC(1, "tools/call", map[string]any{
			"name": "echo", "arguments": map[string]any{"message": "first"},
		}),
		// Notification — no id.
		jsonRPC(nil, "notifications/initialized", nil),
		jsonRPC(2, "tools/call", map[string]any{
			"name": "echo", "arguments": map[string]any{"message": "second"},
		}),
	}
	body, _ := json.Marshal(frames)
	rec := doPOST(t, tr, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var replies []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &replies); err != nil {
		t.Fatalf("response not a JSON array: %v\n%s", err, rec.Body.String())
	}
	if len(replies) != 2 {
		t.Fatalf("replies len = %d, want 2 (notifications produce no reply)", len(replies))
	}
	if !strings.Contains(string(replies[0]), "first") {
		t.Errorf("reply[0] = %s, want it to contain 'first'", string(replies[0]))
	}
	if !strings.Contains(string(replies[1]), "second") {
		t.Errorf("reply[1] = %s, want it to contain 'second'", string(replies[1]))
	}
}

// TestNotificationOnlyBatchReturns202 — when every frame is a
// notification, the spec calls for HTTP 202 with an empty body.
func TestNotificationOnlyBatchReturns202(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	frames := []json.RawMessage{
		jsonRPC(nil, "notifications/initialized", nil),
		jsonRPC(nil, "notifications/cancelled", map[string]any{"requestId": 1}),
	}
	body, _ := json.Marshal(frames)
	rec := doPOST(t, tr, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() > 0 {
		t.Errorf("body not empty: %q", rec.Body.String())
	}
}

// TestOriginAllowlist — when configured, only matching Origin
// headers are accepted; missing Origin is allowed (same-origin
// requests).
func TestOriginAllowlist(t *testing.T) {
	tr := New(Config{
		Dispatcher:     MCPServerDispatcher{Server: newTestMCPServer()},
		Store:          NewMemoryStore(time.Minute),
		AllowedOrigins: []string{"https://app.example.com"},
	})
	body := jsonRPC(1, "tools/list", nil)

	// Allowed origin.
	rec := doPOST(t, tr, body, map[string]string{"Origin": "https://app.example.com"})
	if rec.Code != http.StatusOK {
		t.Errorf("allowed origin: status = %d", rec.Code)
	}
	// Disallowed origin.
	rec = doPOST(t, tr, body, map[string]string{"Origin": "https://attacker.example.org"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("disallowed origin: status = %d, want 403", rec.Code)
	}
	// Missing origin (same-origin / curl) — allowed.
	rec = doPOST(t, tr, body, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("missing origin: status = %d", rec.Code)
	}
}

// TestEmptyBodyReturnsParseError — guards the request-parse branch.
func TestEmptyBodyReturnsParseError(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	rec := doPOST(t, tr, []byte{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestMalformedJSONReturnsParseError covers garbage inputs that don't
// even look like JSON.
func TestMalformedJSONReturnsParseError(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	rec := doPOST(t, tr, []byte("not json"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestMethodNotAllowed — PUT on /mcp should return 405.
func TestMethodNotAllowed(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPut, "/mcp", nil)
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestServerInitiatedPush — opens a GET stream, pushes a frame, and
// confirms the SSE delivery format.
func TestServerInitiatedPush(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	id, err := tr.store.Create(SessionState{ClientName: "test"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	srv := httptest.NewServer(tr)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set(HeaderSessionID, id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Push a frame after the GET handler is wired up. Poll up to
	// a short deadline so we don't race the stream registration.
	var pushed bool
	for i := 0; i < 50 && !pushed; i++ {
		pushed = tr.Push(id, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/message","params":{"text":"hi"}}`))
		if !pushed {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !pushed {
		t.Fatal("Push never returned true — stream not registered")
	}

	// Read until we see the data line for our frame, with a bounded
	// timeout in case SSE delivery breaks.
	type readResult struct {
		line string
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), `notifications/message`) {
					done <- readResult{line: acc.String()}
					return
				}
			}
			if err != nil {
				done <- readResult{err: err}
				return
			}
		}
	}()
	select {
	case r := <-done:
		if r.err != nil && !strings.Contains(r.err.Error(), "notifications/message") {
			t.Fatalf("read: %v", r.err)
		}
		if !strings.Contains(r.line, "event: message") {
			t.Errorf("missing 'event: message' line in %q", r.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE delivery")
	}
}

// TestGetRejectsUnknownSession — GET requires a known session.
func TestGetRejectsUnknownSession(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(HeaderSessionID, "nope")
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestGetRequiresSessionHeader — bare GET without Mcp-Session-Id is
// rejected.
func TestGetRequiresSessionHeader(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestConcurrentDispatchesShareSession — many goroutines fire
// requests against the same session and the transport never trips
// over them.
func TestConcurrentDispatchesShareSession(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	id, _ := tr.store.Create(SessionState{Initialized: true, ClientName: "test"})

	const workers = 25
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := jsonRPC(i+100, "tools/call", map[string]any{
				"name":      "echo",
				"arguments": map[string]any{"message": "worker-" + strconv.Itoa(i)},
			})
			rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: id})
			if rec.Code != http.StatusOK {
				failures.Add(1)
				return
			}
			if !strings.Contains(rec.Body.String(), "worker-"+strconv.Itoa(i)) {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("%d/%d concurrent dispatches failed", failures.Load(), workers)
	}
}

// TestInitializeReplacesPreviousSession — initialize always mints a
// fresh session id even when the client carried a stale one.
func TestInitializeReplacesPreviousSession(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()

	stale, _ := tr.store.Create(SessionState{ClientName: "old"})
	body := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "new", "version": "1.0.0"},
	})
	rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: stale})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	fresh := rec.Header().Get(HeaderSessionID)
	if fresh == "" {
		t.Fatal("no Mcp-Session-Id on initialize response")
	}
	if fresh == stale {
		t.Errorf("initialize reused stale id %q", stale)
	}
	if _, ok := tr.store.Get(stale); ok {
		t.Error("stale session not evicted")
	}
}

// TestProtocolVersionHeaderEcho — when the client supplies an
// Mcp-Protocol-Version header, the response carries the same value;
// when absent, the transport advertises its DefaultProtocolVersion.
func TestProtocolVersionHeaderEcho(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	body := jsonRPC(1, "tools/list", nil)

	rec := doPOST(t, tr, body, nil)
	if got := rec.Header().Get(HeaderProtocolVersion); got != DefaultProtocolVersion {
		t.Errorf("default version = %q, want %q", got, DefaultProtocolVersion)
	}

	rec = doPOST(t, tr, body, map[string]string{HeaderProtocolVersion: "2025-12-01"})
	if got := rec.Header().Get(HeaderProtocolVersion); got != "2025-12-01" {
		t.Errorf("echoed version = %q, want 2025-12-01", got)
	}
}

// TestOptionsReturnsAllow covers the CORS preflight branch.
func TestOptionsReturnsAllow(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Error("Allow header missing on OPTIONS reply")
	}
}

// TestNewPanicsWithoutDispatcher — programmer-error guard. Caught at
// process startup, not at first request.
func TestNewPanicsWithoutDispatcher(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("New(Config{}) did not panic")
		}
	}()
	_ = New(Config{Store: NewMemoryStore(time.Minute)})
}

// TestNewFallsBackToMemoryStore — convenience path: Store left nil
// gets a MemoryStore with the supplied TTL.
func TestNewFallsBackToMemoryStore(t *testing.T) {
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		SessionTTL: time.Minute,
	})
	if _, ok := tr.Store().(*MemoryStore); !ok {
		t.Errorf("Store() = %T, want *MemoryStore", tr.Store())
	}
}

// TestSplitJSONRPC_Inputs unit-tests the body splitter directly.
func TestSplitJSONRPC_Inputs(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCount int
		wantBatch bool
		wantErr   bool
	}{
		{name: "single object", body: `{"jsonrpc":"2.0","id":1,"method":"x"}`, wantCount: 1, wantBatch: false},
		{name: "batch of two", body: `[{"id":1,"method":"a"},{"id":2,"method":"b"}]`, wantCount: 2, wantBatch: true},
		{name: "whitespace prefix", body: "   {\"id\":1,\"method\":\"x\"}", wantCount: 1, wantBatch: false},
		{name: "empty array", body: "[]", wantBatch: true, wantErr: true},
		{name: "garbage", body: "garbage", wantErr: true},
		{name: "blank", body: "    ", wantErr: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			frames, batch, err := splitJSONRPC([]byte(c.body))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if len(frames) != c.wantCount {
				t.Errorf("frames = %d, want %d", len(frames), c.wantCount)
			}
			if batch != c.wantBatch {
				t.Errorf("batched = %v, want %v", batch, c.wantBatch)
			}
		})
	}
}

// TestLargeBodyClampedByMaxBytes — the io.LimitReader cap should
// prevent unbounded growth from a malicious / runaway client.
func TestLargeBodyClampedByMaxBytes(t *testing.T) {
	tr := New(Config{
		Dispatcher:      MCPServerDispatcher{Server: newTestMCPServer()},
		Store:           NewMemoryStore(time.Minute),
		MaxRequestBytes: 256,
	})
	huge := make([]byte, 4096)
	for i := range huge {
		huge[i] = 'x'
	}
	body := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"junk":"`), huge...)
	body = append(body, []byte(`"}}`)...)
	rec := doPOST(t, tr, body, nil)
	// The body is truncated mid-stream so mcp-go's JSON-RPC parser
	// returns a parse-error envelope (HTTP 200 + JSON-RPC error).
	// The load-bearing assertion is that the transport refused to
	// process the trailing bytes — a buggy implementation that
	// disabled the io.LimitReader would have ingested the full
	// 4 KiB payload and produced a successful response. We confirm
	// the response carries an error block in the JSON envelope.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC parse error envelope)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body missing error envelope after truncation: %s", rec.Body.String())
	}
}

// TestInitializeHookFires asserts the daemon-side hook contract: the
// transport invokes InitializeHook synchronously after extracting
// clientInfo. The daemon adapter relies on this to register the
// detached session up-front.
func TestInitializeHookFires(t *testing.T) {
	var called atomic.Bool
	var seenName string
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      NewMemoryStore(time.Minute),
		InitializeHook: func(_ context.Context, state *SessionState) {
			called.Store(true)
			seenName = state.ClientName
		},
	})
	body := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cursor", "version": "3.0.0"},
	})
	rec := doPOST(t, tr, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !called.Load() {
		t.Fatal("InitializeHook never fired")
	}
	if seenName != "cursor" {
		t.Errorf("hook saw ClientName = %q, want cursor", seenName)
	}
}

// TestMCPServerDispatcherNilFailsCleanly — guard against the
// constructor-misuse case where the embedded server pointer is nil.
func TestMCPServerDispatcherNilFailsCleanly(t *testing.T) {
	d := MCPServerDispatcher{}
	_, err := d.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))
	if err == nil {
		t.Error("Dispatch(nil server) returned nil error")
	}
}

// TestRouterPreservesFullArguments pins the regression fix: when the
// streamable transport routes a tools/call through a daemon.Router
// whose local executor unmarshals to a map, the executor must see the
// caller's ORIGINAL arguments — not a stripped {workspace,cwd} peek.
//
// A previous version of tryRouteToolCall re-marshalled only the typed
// peek struct (workspace+cwd) and dropped every other key on the
// floor, breaking every real MCP usage with "X is required" because
// the args map was effectively empty. This test fails on that bug
// and passes on the fix.
func TestRouterPreservesFullArguments(t *testing.T) {
	var seenBody []byte
	router := daemon.NewRouter(daemon.RouterConfig{
		LocalExecute: func(_ context.Context, _ string, body []byte) ([]byte, int, error) {
			seenBody = append([]byte(nil), body...)
			// Mirror the production local executor: unwrap
			// `{"arguments": {...}}` then assert every caller key
			// survived the round-trip.
			var nested struct {
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(body, &nested); err != nil {
				return nil, 500, err
			}
			if nested.Arguments == nil {
				return []byte(`{"error":"no arguments"}`), 200, nil
			}
			out, _ := json.Marshal(map[string]any{"ok": true, "args": nested.Arguments})
			return out, 200, nil
		},
		Logger: zap.NewNop(),
	})

	store := NewMemoryStore(time.Minute)
	defer store.Close()
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
		Router:     router,
	})

	// Seed an initialized session so the transport accepts the call.
	sid, err := store.Create(SessionState{Initialized: true, ClientName: "test"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	callBody := jsonRPC(7, "tools/call", map[string]any{
		"name": "search_symbols",
		"arguments": map[string]any{
			"query": "NewServer",
			"limit": 10,
		},
	})
	rec := doPOST(t, tr, callBody, map[string]string{HeaderSessionID: sid})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 1) The local executor must have seen the original args.
	var nested struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(seenBody, &nested); err != nil {
		t.Fatalf("local executor body not JSON: %v\nbody=%s", err, string(seenBody))
	}
	if nested.Arguments == nil {
		t.Fatalf("local executor saw nil arguments — args were stripped. body=%s", string(seenBody))
	}
	if got, _ := nested.Arguments["query"].(string); got != "NewServer" {
		t.Errorf("query = %q, want %q (args stripped before reaching executor). body=%s",
			got, "NewServer", string(seenBody))
	}
	// JSON numbers decode to float64 in interface{}; compare as such.
	if got, _ := nested.Arguments["limit"].(float64); got != 10 {
		t.Errorf("limit = %v, want 10 (args stripped before reaching executor). body=%s",
			got, string(seenBody))
	}

	// 2) The wrapped tool result must reach the client too.
	if !strings.Contains(rec.Body.String(), "NewServer") {
		t.Errorf("client response missing forwarded args: %s", rec.Body.String())
	}
}

// TestRouterNotFoundDoesNotExpireSession keeps application-level 404s inside
// the JSON-RPC result boundary. Only the transport's pre-dispatch session gate
// may emit an HTTP 404 that causes a client to reinitialize and retry.
func TestRouterNotFoundDoesNotExpireSession(t *testing.T) {
	router := daemon.NewRouter(daemon.RouterConfig{
		LocalExecute: func(_ context.Context, _ string, _ []byte) ([]byte, int, error) {
			return []byte(`{"error":"missing"}`), http.StatusNotFound, nil
		},
		Logger: zap.NewNop(),
	})
	store := NewMemoryStore(time.Minute)
	defer store.Close()
	tr := New(Config{
		Dispatcher: MCPServerDispatcher{Server: newTestMCPServer()},
		Store:      store,
		Router:     router,
	})
	sid, err := store.Create(SessionState{Initialized: true, ClientName: "test"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	body := jsonRPC(1, "tools/call", map[string]any{
		"name": "missing_tool", "arguments": map[string]any{},
	})
	rec := doPOST(t, tr, body, map[string]string{HeaderSessionID: sid})
	if rec.Code != http.StatusOK {
		t.Fatalf("outer status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderSessionID); got != sid {
		t.Errorf("response session id = %q, want %q", got, sid)
	}
	var env struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, rec.Body.String())
	}
	if env.Error.Code != -32603 {
		t.Errorf("router error code = %d, want -32603; body=%s", env.Error.Code, rec.Body.String())
	}

}

// TestHTTPRoundTripEndToEnd — fires the transport behind an
// httptest.Server so the body actually flows through net/http; covers
// the boundary the per-test recorder can't.
func TestHTTPRoundTripEndToEnd(t *testing.T) {
	tr, cleanup := newTransport(t)
	defer cleanup()
	srv := httptest.NewServer(tr)
	defer srv.Close()

	body := jsonRPC(1, "initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
	})
	resp, err := http.Post(srv.URL+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get(HeaderSessionID) == "" {
		t.Error("response missing Mcp-Session-Id")
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"jsonrpc":"2.0"`) {
		t.Errorf("body missing jsonrpc envelope: %s", out)
	}
}
