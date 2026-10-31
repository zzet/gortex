package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubFrame struct {
	ID         int64          `json:"id"`
	Method     string         `json:"method"`
	Params     map[string]any `json:"params"`
	Kind       string         `json:"kind"`
	Connection int            `json:"-"`
}

// A real newline-framed daemon stub exercises the public CLI's transport;
// it does not import or initialize the application's server/grammar stack.
func startStub(t *testing.T, handle func(stubFrame) (any, *rpcError)) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp" // Darwin's long per-user temp path can exceed sun_path.
	}
	dir, err := os.MkdirTemp(base, "gl-test-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			connectionID := len(connections)
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				decoder := json.NewDecoder(conn)
				var handshake map[string]any
				if decoder.Decode(&handshake) != nil {
					return
				}
				var writeMu sync.Mutex
				encode := func(value any) {
					writeMu.Lock()
					defer writeMu.Unlock()
					_ = json.NewEncoder(conn).Encode(value) // disconnect is expected on cancellation
				}
				encode(map[string]any{"ok": true, "daemon_version": "stub-v1"})
				for {
					var frame stubFrame
					if decoder.Decode(&frame) != nil {
						return
					}
					frame.Connection = connectionID
					if frame.Kind != "" {
						result, _ := handle(frame)
						encode(map[string]any{"ok": true, "result": result})
						continue
					}
					if frame.ID == 0 {
						continue
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						result, fault := any(map[string]any{}), (*rpcError)(nil)
						if frame.Method != "initialize" {
							result, fault = handle(frame)
						}
						if fault != nil {
							encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": fault})
						} else {
							encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result})
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepted
		mu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
		_ = os.RemoveAll(dir)
	})
	return socket
}

func stubContent(value any) any {
	data, _ := json.Marshal(value)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(data)}}}
}

func TestStubDaemonSweepTargetsBusyAndMetrics(t *testing.T) {
	var mu sync.Mutex
	busy, invalid, external := 0, 0, 0
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		if frame.Kind == "status" {
			return map[string]any{"ready": true, "enriched": true}, nil
		}
		name, _ := frame.Params["name"].(string)
		args, _ := frame.Params["arguments"].(map[string]any)
		operation, _ := args["operation"].(string)
		target, _ := args["target"].(map[string]any)
		if name == "search" && busy < 3 {
			busy++
			return nil, &rpcError{Code: -32002, Message: "busy"}
		}
		if (name == "change" && operation == "verify" && args["source"] == nil) || (name == "relations" && operation == "dependents" && target["symbol"] == nil) {
			invalid++
			return map[string]any{"isError": true, "content": []any{map[string]any{"text": `{"error_code":"invalid_argument","message":"missing target"}`}}}, nil
		}
		if name == "read" && operation == "source" {
			if symbol, _ := target["symbol"].(string); len(symbol) >= 5 && symbol[:5] == "ext::" {
				external++
			}
		}
		return stubContent(map[string]any{"ok": true}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "/private/clone")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan, err := benchmarkPlan(sampleCorpus())
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	var all []Attempt
	for _, r := range plan {
		_, attempts, err := callTool(ctx, client, r, r.Arguments, "workload", 0, 0, 0, time.Second, func(_ context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, attempts...)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}) {
		t.Fatalf("busy sleep sequence: %v", sleeps)
	}
	metrics := summarize(all, 1, "workload")
	for _, name := range []string{"change.verify", "relations.dependents"} {
		if metrics[name].Latency.P50.Value == nil || len(metrics[name].Errors) != 0 {
			t.Fatalf("%s was not measured successfully: %+v", name, metrics[name])
		}
	}
	mu.Lock()
	if invalid != 0 || external != 0 || busy != 3 {
		t.Errorf("invalid=%d external=%d busy=%d", invalid, external, busy)
	}
	mu.Unlock()
	status, err := control(ctx, socket, "status", nil)
	if err != nil || string(status) != `{"enriched":true,"ready":true}` {
		t.Fatalf("control receipt: %s %v", status, err)
	}
}

func TestSocketConcurrentRoutingAndIndependentCancellation(t *testing.T) {
	release := make(chan struct{})
	blockedReceived := make(chan struct{})
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method == "blocked" {
			close(blockedReceived)
			<-release
		}
		if frame.Method == "slow" {
			time.Sleep(40 * time.Millisecond)
		}
		return map[string]any{"method": frame.Method}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	short, end := context.WithCancel(ctx)
	end()
	if _, err := c.call(short, "cancelled", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v", err)
	}
	var workers sync.WaitGroup
	for i := range 20 {
		method := "fast"
		if i%2 == 0 {
			method = "slow"
		}
		workers.Go(func() {
			data, err := c.call(ctx, method, nil)
			if err != nil || string(data) != `{"method":"`+method+`"}` {
				t.Errorf("reply routed incorrectly: %s %v", data, err)
			}
		})
	}
	workers.Wait()
	short, end = context.WithCancel(ctx)
	defer end()
	callDone := make(chan error, 1)
	go func() { _, err := c.call(short, "blocked", nil); callDone <- err }()
	select {
	case <-blockedReceived:
	case <-ctx.Done():
		t.Fatal("blocked request was not received")
	}
	for len(c.writeGate) != 0 {
		if waitContext(ctx, time.Millisecond) != nil {
			t.Fatal("request writer did not finish")
		}
	}
	end()
	err = <-callDone
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight call was not cancelled: %v", err)
	}
	if _, err := c.call(ctx, "fast", nil); err != nil {
		t.Fatalf("one cancellation closed the shared connection: %v", err)
	}
}

type blockingCaller struct{}

func (blockingCaller) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type delayedCancellationCaller struct{}

func (delayedCancellationCaller) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // simulate a delayed socket/scheduler return
	return nil, ctx.Err()
}

type lateSuccessCaller struct{}

func (lateSuccessCaller) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return json.RawMessage(`{"content":[{"text":"{}"}]}`), nil
}

func TestPhaseCensoringIsNotProductExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, attempts, _ := callTool(ctx, blockingCaller{}, request{Tool: "search"}, nil, "workload", 0, 0, 0, time.Second, waitContext)
	if len(attempts) != 1 || attempts[0].Outcome != "phase_censored" {
		t.Fatalf("phase censoring mislabeled: %+v", attempts)
	}
	_, attempts, _ = callTool(context.Background(), blockingCaller{}, request{Tool: "search"}, nil, "workload", 0, 0, 0, time.Millisecond, waitContext)
	if len(attempts) != 1 || attempts[0].Outcome != "product_deadline" {
		t.Fatalf("product deadline mislabeled: %+v", attempts)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, attempts, _ = callTool(ctx, delayedCancellationCaller{}, request{Tool: "search"}, nil, "workload", 0, 0, 0, 5*time.Millisecond, waitContext)
	if len(attempts) != 1 || attempts[0].Outcome != "phase_censored" {
		t.Fatalf("return after both deadlines lost declared clipping: %+v", attempts)
	}
	_, attempts, err := callTool(context.Background(), lateSuccessCaller{}, request{Tool: "search"}, nil, "workload", 0, 0, 0, time.Millisecond, waitContext)
	if !errors.Is(err, context.DeadlineExceeded) || attempts[0].Outcome != "product_deadline" {
		t.Fatalf("late successful receipt crossed deadline: %+v %v", attempts, err)
	}
}

func TestBlockedSocketWriterHonorsBudgetAndInvalidatesStream(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close() // peer deliberately never reads
	c := &socketClient{conn: local, reader: bufio.NewReader(local), pending: make(map[int64]chan wireReply), done: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	go c.readLoop()
	defer c.Close()
	writerCtx, cancelWriter := context.WithCancel(context.Background())
	defer cancelWriter()
	finished := make(chan error, 1)
	go func() {
		_, err := c.call(writerCtx, "blocked", map[string]string{"large": strings.Repeat("x", 1<<20)})
		finished <- err
	}()
	until := time.Now().Add(time.Second)
	for len(c.writeGate) == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(c.writeGate) == 0 {
		t.Fatal("writer did not start")
	}
	start := time.Now()
	queued, cancelQueued := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelQueued()
	if _, err := c.call(queued, "queued", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call ignored budget: %v", err)
	}
	advisory, cancelAdvisory := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelAdvisory()
	if err := c.send(advisory, map[string]any{"method": "notifications/cancelled"}, true); err != nil {
		t.Fatalf("busy advisory writer should be skipped: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("cancellation waited behind a wedged writer")
	}
	cancelWriter()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked writer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active writer did not stop on cancellation")
	}
	if _, err := c.call(context.Background(), "after-partial-write", nil); err == nil {
		t.Fatal("failed/partial stream was reused")
	}
}

func TestWireLimitAndStructuredErrors(t *testing.T) {
	if _, err := readWireLine(bufio.NewReaderSize(&repeatingReader{}, 4096)); err == nil {
		t.Fatal("unbounded daemon response accepted")
	}
	var remote *rpcError
	if err := checkToolResult(json.RawMessage(`{"isError":true,"content":[{"text":"{\"error_code\":\"invalid_argument\",\"message\":\"target\"}"}]}`)); !errors.As(err, &remote) || remote.Code != -32602 {
		t.Fatalf("structured tool error not classified: %v", err)
	}
	if err := checkToolResult(json.RawMessage(`{"isError":true,"content":[{"text":"source symbol not found"}]}`)); err == nil || !strings.Contains(err.Error(), "source symbol not found") {
		t.Fatalf("plain tool error was lost: %v", err)
	}
}

func TestMalformedRPCReceiptCannotBecomeSuccess(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	ch := make(chan wireReply, 1)
	c := &socketClient{conn: local, reader: bufio.NewReader(local), pending: map[int64]chan wireReply{1: ch}, done: make(chan struct{})}
	go c.readLoop()
	defer c.Close()
	if _, err := peer.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-ch:
		if reply.err == nil {
			t.Fatal("missing result/error accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("malformed response did not fail pending call")
	}
}

type repeatingReader struct{}

func (*repeatingReader) Read(data []byte) (int, error) {
	for i := range data {
		data[i] = 'a'
	}
	return len(data), nil
}

func TestInvalidationSubscriptionRecordsAvailabilityAndActualArguments(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "older-unavailable", true: "available"}[available], func(t *testing.T) {
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Params["name"] != "session" || args["operation"] != "subscribe" || args["channel"] != "graph_invalidated" {
					t.Errorf("wrong subscription: %+v", frame.Params)
				}
				if !available {
					return nil, &rpcError{Code: -32601, Message: "subscription unavailable"}
				}
				return stubContent(map[string]any{"subscribed": true}), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, err := openMCP(ctx, socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			phase := Phase{Gate: make(map[string]json.RawMessage)}
			subscribeInvalidations(ctx, client, "sample", time.Second, &phase)
			var receipt struct {
				Available bool   `json:"available"`
				Error     string `json:"error"`
			}
			if err := json.Unmarshal(phase.Gate["sample.graph_invalidated_subscription"], &receipt); err != nil || receipt.Available != available || (!available && receipt.Error == "") {
				t.Fatalf("availability receipt lost: %+v %v", receipt, err)
			}
			if len(phase.Attempts) != 1 || phase.Attempts[0].Role != "probe" {
				t.Fatal("subscription mixed into workload")
			}
		})
	}
}
