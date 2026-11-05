package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Actual framed transport, not a fabricated last-response cache. This test
// retains the first refused frame separately from the final successful frame.
func TestReadOnlyFreshnessRetryKeepsFirstWireAndLogicalCost(t *testing.T) {
	for _, tc := range []struct{ operation, code string }{{"editing_context", "projection_superseded"}, {"editing_context", "file_view_pending"}, {"source", "file_view_pending"}} {
		code := tc.code
		t.Run(tc.operation+"/"+code, func(t *testing.T) {
			left, right := net.Pipe()
			client := &socketClient{conn: left, reader: bufio.NewReader(left), writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply), done: make(chan struct{}), cwd: "/owned/private"}
			go client.readLoop()
			var peer sync.WaitGroup
			peer.Add(1)
			var requests, responses [][]byte
			go func() {
				defer peer.Done()
				reader := bufio.NewReader(right)
				for i := 0; i < 2; i++ {
					raw, err := readWireLine(reader)
					if err != nil {
						return
					}
					requests = append(requests, raw)
					var request struct {
						ID int64 `json:"id"`
					}
					if json.Unmarshal(raw, &request) != nil {
						return
					}
					result := map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"file\":{\"id\":\"main.go\"},\"defines\":[]}"}}}
					if i == 0 {
						body, _ := json.Marshal(map[string]any{"error_code": code, "message": "transient selected read unavailable", "retriable": true, "data": map[string]any{"retry_scope": "read_only", "delivery_unknown": false, "reason": "projection_deferred"}})
						result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": string(body)}}}
					}
					frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
					frame = append(frame, '\n')
					responses = append(responses, frame)
					if _, err := right.Write(frame); err != nil {
						return
					}
				}
			}()
			closed := false
			closeAndJoin := func() {
				if !closed {
					closed = true
					_ = left.Close()
					_ = right.Close()
					<-client.done
					peer.Wait()
				}
			}
			t.Cleanup(closeAndJoin)
			args := map[string]any{"operation": tc.operation, "target": map[string]any{"file": "main.go"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
			r := request{Tool: "read", Operation: tc.operation, Arguments: args}
			started := time.Now()
			result, attempts, err := callTool(t.Context(), client, r, args, "workload", 3, 0, 1, time.Second, waitContext)
			closeAndJoin()
			require.NotEmpty(t, responses)
			require.Contains(t, string(responses[0]), code)
			require.Contains(t, string(responses[0]), "isError")
			t.Logf("ACTUAL_FRAMED_TRANSIENT_WITNESS code=%s requests=%d responses=%d original_errors_retained=%+v", code, len(requests), len(responses), attempts)
			require.NoError(t, err, "explicit read-only contract must allow a bounded fresh RPC within the original logical deadline")
			require.Len(t, requests, 2)
			require.Len(t, responses, 2)
			require.Len(t, attempts, 2)
			require.Equal(t, "retryable", attempts[0].Outcome)
			require.Equal(t, -32000, attempts[0].Code)
			require.NotEmpty(t, attempts[0].Error)
			require.Equal(t, "success", attempts[1].Outcome)
			require.Equal(t, 1, attempts[1].Retry)
			require.Equal(t, attempts[0].Request, attempts[1].Request)
			require.GreaterOrEqual(t, attempts[1].LogicalMS, attempts[0].DurationMS)
			require.Less(t, time.Since(started), time.Second)
			require.Contains(t, string(responses[0]), "isError")
			require.NotEmpty(t, result)
			metric := summarize(attempts, 1, "workload")["read."+tc.operation]
			require.Equal(t, 1, metric.RetryableRefusals)
			require.Equal(t, 1, metric.Successes)
		})
	}
}

type readFreshnessPolicyPeer struct {
	calls        int
	data         json.RawMessage
	deadlines    []time.Time
	successAfter int
}

func (p *readFreshnessPolicyPeer) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	p.calls++
	deadline, _ := ctx.Deadline()
	p.deadlines = append(p.deadlines, deadline)
	if p.successAfter > 0 && p.calls >= p.successAfter {
		return json.RawMessage(`{"content":[{"type":"text","text":"{\"ok\":true}"}]}`), nil
	}
	return p.data, nil
}
func readFreshnessTestWire(code string, retriable any, data map[string]any) json.RawMessage {
	fields := map[string]any{"error_code": code, "message": "selected read unavailable", "data": data}
	if retriable != nil {
		fields["retriable"] = retriable
	}
	body, _ := json.Marshal(fields)
	raw, _ := json.Marshal(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": string(body)}}})
	return raw
}
func TestReadFreshnessRetryRejectsUnknownAndFirstProbeAuthority(t *testing.T) {
	for _, name := range []string{"plain_generic", "unknown_code", "false_retriable", "missing_retriable", "missing_scope", "missing_delivery", "unknown_delivery", "wrong_scope", "write", "compressed", "wrong_actual_operation", "probe", "strict_probe_path"} {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{"retry_scope": "read_only", "delivery_unknown": false}
			code := "projection_superseded"
			var retriable any = true
			r := request{Tool: "read", Operation: "editing_context"}
			args := map[string]any{"operation": "editing_context"}
			role := "workload"
			switch name {
			case "unknown_code":
				code = "unknown"
			case "false_retriable":
				retriable = false
			case "missing_retriable":
				retriable = nil
			case "missing_scope":
				delete(data, "retry_scope")
			case "missing_delivery":
				delete(data, "delivery_unknown")
			case "unknown_delivery":
				data["delivery_unknown"] = true
			case "wrong_scope":
				data["retry_scope"] = "write"
			case "write":
				r.Tool = "edit"
				r.Operation = "file"
				args["operation"] = "file"
			case "compressed":
				args["options"] = map[string]any{"compress_bodies": true}
			case "wrong_actual_operation":
				args["operation"] = "source"
			case "probe":
				role = "probe"
			}
			wire := readFreshnessTestWire(code, retriable, data)
			if name == "plain_generic" {
				wire = json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"projection not published: superseded"}]}`)
			}
			peer := &readFreshnessPolicyPeer{data: wire, successAfter: 2}
			call := callTool
			if name == "strict_probe_path" {
				call = callEditProbeTool
			}
			_, rows, err := call(t.Context(), peer, r, args, role, 0, 0, 0, time.Second, waitContext)
			require.Error(t, err)
			require.Equal(t, 1, peer.calls)
			require.Len(t, rows, 1)
			require.Equal(t, "error", rows[0].Outcome)
		})
	}
}
func TestReadFreshnessRetryCapCancellationAndOneDeadline(t *testing.T) {
	wire := readFreshnessTestWire("file_view_pending", true, map[string]any{"retry_scope": "read_only", "delivery_unknown": false})
	r := request{Tool: "read", Operation: "summary"}
	args := map[string]any{"operation": "summary"}
	immediate := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	capped := &readFreshnessPolicyPeer{data: wire}
	_, rows, err := callTool(t.Context(), capped, r, args, "workload", 0, 0, 0, time.Second, immediate)
	require.Error(t, err)
	require.Equal(t, 3, capped.calls)
	require.Len(t, rows, 3)
	require.Equal(t, "error", rows[2].Outcome)
	metric := summarize(rows, 1, "workload")["read.summary"]
	require.Equal(t, 2, metric.RetryableRefusals)
	require.NotEmpty(t, metric.Errors)
	for _, deadline := range capped.deadlines {
		require.Equal(t, capped.deadlines[0], deadline, "fresh requests cannot reset the logical deadline")
	}
	caller, cancel := context.WithCancel(t.Context())
	cancelled := &readFreshnessPolicyPeer{data: wire, successAfter: 2}
	_, rows, err = callTool(caller, cancelled, r, args, "workload", 0, 0, 0, time.Second, func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() })
	require.Error(t, err)
	require.Equal(t, 1, cancelled.calls)
	require.Len(t, rows, 2)
	require.Equal(t, "retryable", rows[0].Outcome)
	require.Equal(t, "phase_censored", rows[1].Outcome)
	deadlinePeer := &readFreshnessPolicyPeer{data: wire, successAfter: 2}
	_, rows, err = callTool(t.Context(), deadlinePeer, r, args, "workload", 0, 0, 0, 5*time.Millisecond, func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() })
	require.Error(t, err)
	require.Equal(t, 1, deadlinePeer.calls)
	require.Equal(t, "product_deadline", rows[len(rows)-1].Outcome)
}
