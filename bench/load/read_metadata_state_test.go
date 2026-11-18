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

func partialMetadataWire(t *testing.T, body string, extra ...string) json.RawMessage {
	t.Helper()
	content := []any{map[string]any{"type": "text", "text": body}}
	for _, note := range extra {
		content = append(content, map[string]any{"type": "text", "text": note})
	}
	raw, err := json.Marshal(map[string]any{"content": content})
	require.NoError(t, err)
	return raw
}

func partialMetadataFields(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	return fields
}

func TestPartialPendingRPCSuccessRetainsObservation(t *testing.T) {
	for _, operation := range []string{"summary", "editing_context"} {
		t.Run(operation, func(t *testing.T) {
			left, right := net.Pipe()
			client := &socketClient{conn: left, reader: bufio.NewReader(left), writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply), done: make(chan struct{}), cwd: "/owned/private"}
			go client.readLoop()
			wire := partialMetadataWire(t, `{"file":null,"defines":null,"freshness":{"graph_freshness":"pending","complete":false,"reason":"store_busy"}}`)
			var peer sync.WaitGroup
			peer.Add(1)
			requests := 0
			go func() {
				defer peer.Done()
				raw, err := readWireLine(bufio.NewReader(right))
				if err != nil {
					return
				}
				requests++
				var message struct {
					ID int64 `json:"id"`
				}
				if json.Unmarshal(raw, &message) != nil {
					return
				}
				frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": json.RawMessage(wire)})
				_, _ = right.Write(append(frame, '\n'))
			}()
			closeAndJoin := sync.OnceFunc(func() {
				_ = left.Close()
				_ = right.Close()
				<-client.done
				peer.Wait()
			})
			t.Cleanup(closeAndJoin)
			args := map[string]any{"operation": operation, "target": map[string]any{"file": "main.go"}, "output": map[string]any{"format": "json"}}
			r := request{Tool: "read", Operation: operation, Arguments: args}
			_, attempts, err := callTool(t.Context(), client, r, args, "workload", 0, 0, 0, time.Second, waitContext)
			closeAndJoin()
			require.NoError(t, err)
			require.Equal(t, 1, requests, "partial observation cannot authorize another RPC")
			require.Len(t, attempts, 1)
			require.Equal(t, "success", attempts[0].Outcome, "this remains RPC success, not complete metadata proof")
			require.Zero(t, attempts[0].Retry)
			require.Zero(t, attempts[0].Code)
			require.Empty(t, attempts[0].Error)
			t.Logf("REAL_PARTIAL_WIRE operation=%s requests=%d result=%s attempt=%v", operation, requests, wire, partialMetadataFields(t, attempts[0]))
			require.Equal(t, "partial_pending", partialMetadataFields(t, attempts[0])["read_metadata_state"])
			metrics := summarize(attempts, 1, "workload")[r.name()]
			require.Equal(t, 1, metrics.Successes)
			require.Equal(t, 1, metrics.Latency.Count)
			require.Equal(t, float64(1), partialMetadataFields(t, metrics)["partial_pending"])
		})
	}
}

func TestPartialMetadataObservationRequiresDeclaredFlags(t *testing.T) {
	const partial = `{"freshness":{"complete":false,"graph_freshness":"pending"}}`
	const note = "(Session note: graph read #6. Every location returned so far is real and citeable — if you are localizing or answering a question, consider writing your conclusion from what you already hold and fetching only what it still lacks. This note appears once.)"
	for _, tc := range []struct {
		name, body, note, tool, operation, role string
		strict, want                            bool
	}{
		{name: "explicit", body: partial, want: true},
		{name: "known_note", body: partial, note: note, want: true},
		{name: "unknown_note", body: partial, note: "pending metadata"},
		{name: "explicit_complete", body: `{"freshness":{"complete":true,"graph_freshness":"current"}}`},
		{name: "missing_complete", body: `{"freshness":{"graph_freshness":"pending"}}`},
		{name: "string_false", body: `{"freshness":{"complete":"false","graph_freshness":"pending"}}`},
		{name: "missing_pending", body: `{"freshness":{"complete":false}}`},
		{name: "wrong_state", body: `{"freshness":{"complete":false,"graph_freshness":"current"}}`},
		{name: "wrong_nesting", body: `{"complete":false,"graph_freshness":"pending"}`},
		{name: "message_only", body: "metadata pending complete:false"},
		{name: "other_tool", body: partial, tool: "search", operation: "symbols"},
		{name: "strict_probe", body: partial, role: "probe", strict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, operation, role := tc.tool, tc.operation, tc.role
			if tool == "" {
				tool = "read"
			}
			if operation == "" {
				operation = "editing_context"
			}
			if role == "" {
				role = "workload"
			}
			var notes []string
			if tc.note != "" {
				notes = append(notes, tc.note)
			}
			peer := &readFreshnessPolicyPeer{data: partialMetadataWire(t, tc.body, notes...)}
			args := map[string]any{"operation": operation}
			r := request{Tool: tool, Operation: operation, Arguments: args}
			var rows []Attempt
			var err error
			if tc.strict {
				_, rows, err = callEditProbeTool(context.Background(), peer, r, args, role, 0, 0, 0, time.Second, waitContext)
			} else {
				_, rows, err = callTool(context.Background(), peer, r, args, role, 0, 0, 0, time.Second, waitContext)
			}
			require.NoError(t, err, "observation must not rewrite existing RPC classification")
			require.Len(t, rows, 1)
			require.Equal(t, 1, peer.calls)
			state, observed := partialMetadataFields(t, rows[0])["read_metadata_state"]
			if tc.want {
				require.Equal(t, "partial_pending", state)
			} else {
				require.False(t, observed, "absence is UNOBSERVED, not complete metadata")
			}
		})
	}
	legacy := partialMetadataFields(t, ToolMetrics{})
	require.NotContains(t, legacy, "partial_pending", "nil historical metric stays unobserved")
}
