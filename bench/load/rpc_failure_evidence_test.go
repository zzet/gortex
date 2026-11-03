package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This uses the actual LOAD socket read/write path. The peer retains its
// literal frames, so evidence cannot be reconstructed from the declared plan.
func TestReadSourceFailureRetainsExactTransportFrames(t *testing.T) {
	left, right := net.Pipe()
	client := &socketClient{conn: left, reader: bufio.NewReader(left), writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply), done: make(chan struct{}), cwd: "/owned/clone"}
	go client.readLoop()
	type exchange struct {
		request, response []byte
		err               error
	}
	peer := make(chan exchange, 1)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		raw, err := readWireLine(bufio.NewReader(right))
		var request struct {
			ID int64 `json:"id"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &request)
		}
		response := []byte(fmt.Sprintf("{ \"jsonrpc\" : \"2.0\", \"id\" : %d, \"result\" : {\"isError\":true,\"_meta\":{\"gortex_facade\":{\"facade\":\"read\",\"operation\":\"source\",\"canonical_tool\":\"get_symbol_source\",\"surface_version\":\"facade-v1\"}},\"content\":[{\"type\":\"text\",\"text\":\"file: pending (1 files)\\nsymbol not found: gortex/internal/graph/graph.go::fnv1aEdge\"}] } }\n", request.ID))
		if err == nil {
			_, err = right.Write(response)
		}
		peer <- exchange{raw, response, err}
	}()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close(); <-client.done })
	ctx := withReadSourceFailureEvidence(t.Context(), true)
	declared := request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": "declared::different"}}}
	actual := map[string]any{"operation": "source", "target": map[string]any{"symbol": "internal/graph/graph.go::fnv1aEdge"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
	raw, attempts, callErr := callTool(ctx, client, declared, actual, "workload", 0, 0, 2, time.Second, waitContext)
	observed := <-peer
	<-peerDone
	// All peer work is joined before any assertion, including the expected RED.
	require.NoError(t, observed.err)
	require.ErrorContains(t, callErr, "symbol not found")
	require.Len(t, attempts, 1)
	require.Equal(t, -32000, attempts[0].Code)
	t.Logf("actual_request=%s actual_response=%s returned_result=%s normalized_error=%v", observed.request, observed.response, raw, callErr)
	encoded, err := json.Marshal(attempts[0])
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.NotEmpty(t, fields["failure_rpc"], "opt-in failed read.source must retain actual frames before error normalization discards them")
	var evidence struct {
		Status   string `json:"status"`
		Request  string `json:"request_wire_base64"`
		Response string `json:"response_wire_base64"`
		Present  bool   `json:"is_error_present"`
		IsError  *bool  `json:"is_error"`
	}
	require.NoError(t, json.Unmarshal(fields["failure_rpc"], &evidence))
	require.Equal(t, "exact", evidence.Status)
	requestBytes, err := base64.StdEncoding.DecodeString(evidence.Request)
	require.NoError(t, err)
	require.Equal(t, observed.request, requestBytes)
	responseBytes, err := base64.StdEncoding.DecodeString(evidence.Response)
	require.NoError(t, err)
	require.Equal(t, observed.response, responseBytes, "including original whitespace and line framing")
	require.True(t, evidence.Present)
	require.NotNil(t, evidence.IsError)
	require.True(t, *evidence.IsError)
	require.False(t, attempts[0].FailureRPC.OfferedProfileKnown, "manually constructed transport has no observed handshake")
	require.True(t, attempts[0].FailureRPC.ObservedFacadePresent)
	require.JSONEq(t, `{"facade":"read","operation":"source","canonical_tool":"get_symbol_source","surface_version":"facade-v1"}`, string(attempts[0].FailureRPC.ObservedFacade))
}

func TestReadSourceFailureCaptureDefaultAndHealthy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, failed bool
	}{{"disabled_error", false, true}, {"enabled_success", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			c := &socketClient{conn: left, reader: bufio.NewReader(left), writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply), done: make(chan struct{})}
			go c.readLoop()
			t.Cleanup(func() { _ = left.Close(); _ = right.Close(); <-c.done })
			peer := make(chan error, 1)
			go func() {
				line, err := readWireLine(bufio.NewReader(right))
				var frame struct {
					ID int64 `json:"id"`
				}
				if err == nil {
					err = json.Unmarshal(line, &frame)
				}
				if err == nil {
					_, err = fmt.Fprintf(right, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"isError\":%t,\"content\":[{\"type\":\"text\",\"text\":\"source result\"}]}}\n", frame.ID, tc.failed)
				}
				peer <- err
			}()
			ctx := withReadSourceFailureEvidence(t.Context(), tc.enabled)
			r := request{Tool: "read", Operation: "source"}
			_, attempts, err := callTool(ctx, c, r, map[string]any{"operation": "source"}, "workload", 0, 0, 0, time.Second, waitContext)
			peerErr := <-peer
			require.NoError(t, peerErr)
			if tc.failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, attempts, 1)
			require.Nil(t, attempts[0].FailureRPC)
			encoded, marshalErr := json.Marshal(attempts[0])
			require.NoError(t, marshalErr)
			require.NotContains(t, string(encoded), "failure_rpc")
			c.mu.Lock()
			allocated, live := c.capturePending != nil, len(c.capturePending)
			c.mu.Unlock()
			require.Zero(t, live)
			require.Equal(t, tc.enabled, allocated, "default off must not allocate the capture map")
			if recorder, _ := ctx.Value(readSourceFailureKey{}).(*readSourceFailureRecorder); recorder != nil {
				require.Zero(t, recorder.count)
			}
		})
	}
}

func TestReadSourceFailureCaptureCorrelatesConcurrentReplies(t *testing.T) {
	left, right := net.Pipe()
	c := &socketClient{conn: left, reader: bufio.NewReader(left), writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply), done: make(chan struct{}), offeredTools: loadWireTools, offeredMode: loadWireToolsMode, version: "actual-handshake-version"}
	go c.readLoop()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close(); <-c.done })
	type exchange struct {
		id     int64
		raw    []byte
		target string
	}
	type result struct {
		target   string
		attempts []Attempt
		err      error
	}
	peer := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(right)
		var frames []exchange
		for range 2 {
			line, err := readWireLine(reader)
			var frame struct {
				ID     int64 `json:"id"`
				Params struct {
					Arguments struct {
						Target string `json:"target"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if err == nil {
				err = json.Unmarshal(line, &frame)
			}
			if err != nil {
				peer <- err
				return
			}
			frames = append(frames, exchange{frame.ID, line, frame.Params.Arguments.Target})
		}
		for i := len(frames) - 1; i >= 0; i-- {
			frame := frames[i]
			if _, err := fmt.Fprintf(right, "{\"id\":%d,\"result\":{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", frame.id, frame.target); err != nil {
				peer <- err
				return
			}
		}
		peer <- nil
	}()
	ctx := withReadSourceFailureEvidence(t.Context(), true)
	results := make(chan result, 2)
	var calls sync.WaitGroup
	for _, target := range []string{"first::Target", "second::Other"} {
		calls.Go(func() {
			_, attempts, err := callTool(ctx, c, request{Tool: "read", Operation: "source"}, map[string]any{"operation": "source", "target": target}, "workload", 0, 0, 0, time.Second, waitContext)
			results <- result{target, attempts, err}
		})
	}
	calls.Wait()
	require.NoError(t, <-peer)
	close(results)
	ids := map[int64]bool{}
	for result := range results {
		require.ErrorContains(t, result.err, result.target)
		require.Len(t, result.attempts, 1)
		e := result.attempts[0].FailureRPC
		require.NotNil(t, e)
		require.Equal(t, "exact", e.Status)
		require.False(t, ids[e.WireID])
		ids[e.WireID] = true
		requestBytes, err := base64.StdEncoding.DecodeString(e.RequestBase64)
		require.NoError(t, err)
		responseBytes, err := base64.StdEncoding.DecodeString(e.ResponseBase64)
		require.NoError(t, err)
		require.Contains(t, string(requestBytes), result.target)
		require.Contains(t, string(responseBytes), result.target)
		require.True(t, e.OfferedProfileKnown)
		require.Equal(t, loadWireTools, e.OfferedTools)
		require.True(t, e.ObservedFacadePresenceKnown)
		require.False(t, e.ObservedFacadePresent, "offered profile must not invent response metadata")
	}
}

func TestReadSourceFailureCaptureBoundsAndPresence(t *testing.T) {
	complete := func(response string) *failureWireCapture {
		return &failureWireCapture{id: 7, request: []byte("request\n"), response: []byte(response), requestBytes: 8, responseBytes: len(response), requestStatus: "complete", responseStatus: "complete", writeCompleted: true}
	}
	for _, tc := range []struct {
		name, response string
		present        bool
		value          *bool
	}{
		{"absent", `{"result":{"content":[]}}`, false, nil},
		{"null", `{"result":{"isError":null}}`, true, nil},
		{"false", `{"result":{"isError":false}}`, true, new(bool)},
		{"rpc_error", `{"error":{"code":-32603,"message":"error"}}`, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := new(readSourceFailureRecorder).failure(complete(tc.response))
			require.Equal(t, "exact", e.Status)
			require.True(t, e.IsErrorPresenceKnown)
			require.Equal(t, tc.present, e.IsErrorPresent)
			require.Equal(t, tc.value, e.IsError)
		})
	}
	for _, tc := range []struct {
		name, status string
		mutate       func(*failureWireCapture)
	}{
		{"not_sent", "request_absent", func(c *failureWireCapture) {
			c.requestStatus = "absent"
			c.request = nil
			c.requestBytes = 0
			c.writeCompleted = false
		}},
		{"partial_write", "request_partial", func(c *failureWireCapture) { c.requestStatus = "partial"; c.writeCompleted = false }},
		{"no_response", "response_absent", func(c *failureWireCapture) { c.responseStatus = "absent"; c.response = nil; c.responseBytes = 0 }},
		{"unsupported", "unsupported_transport", func(c *failureWireCapture) { c.unsupported = true }},
		{"request_oversize", "oversize", func(c *failureWireCapture) {
			c.requestStatus = "oversize"
			c.request = nil
			c.requestBytes = readSourceFailureBytes + 1
		}},
		{"response_oversize", "oversize", func(c *failureWireCapture) {
			c.responseStatus = "oversize"
			c.response = nil
			c.responseBytes = readSourceFailureBytes + 1
		}},
		{"combined_oversize", "oversize", func(c *failureWireCapture) {
			c.request = []byte(strings.Repeat("q", readSourceFailureBytes))
			c.requestBytes = len(c.request)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := complete(`{"result":{"isError":true}}`)
			tc.mutate(capture)
			e := new(readSourceFailureRecorder).failure(capture)
			require.Equal(t, tc.status, e.Status)
			require.Empty(t, e.RequestBase64)
			require.Empty(t, e.ResponseBase64)
			require.False(t, e.IsErrorPresenceKnown)
		})
	}
	recorder := new(readSourceFailureRecorder)
	for i := range readSourceFailureRecords + 2 {
		e := recorder.failure(complete(`{"result":{"isError":true}}`))
		if i < readSourceFailureRecords {
			require.Equal(t, "exact", e.Status)
		} else {
			require.Equal(t, "cap_exhausted", e.Status)
			require.Empty(t, e.RequestBase64)
			require.Empty(t, e.ResponseBase64)
		}
	}
	require.Equal(t, readSourceFailureRecords, recorder.count)
}

func TestReadSourceFailureCaptureCanceledBeforeSend(t *testing.T) {
	left, right := net.Pipe()
	c := &socketClient{conn: left, writeGate: make(chan struct{}, 1), pending: make(map[int64]chan wireReply)}
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	ctx, cancel := context.WithCancel(withReadSourceFailureEvidence(t.Context(), true))
	cancel()
	_, attempts, err := callTool(ctx, c, request{Tool: "read", Operation: "source"}, map[string]any{"operation": "source"}, "workload", 0, 0, 0, time.Second, waitContext)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, attempts, 1)
	require.Equal(t, "phase_censored", attempts[0].Outcome)
	require.Equal(t, "request_absent", attempts[0].FailureRPC.Status)
	require.Zero(t, attempts[0].FailureRPC.WireID)
	require.Nil(t, c.capturePending)
}

func TestReadSourceFailureCaptureOptionsDoNotChangePlan(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--edit-ack=local"}
	plain, err := parseRun(base)
	require.NoError(t, err)
	enabled, err := parseRun(append(base, "--retain-read-source-failures"))
	require.NoError(t, err)
	require.False(t, plain.RetainReadSourceFailures)
	require.True(t, enabled.RetainReadSourceFailures)
	require.Equal(t, "local", enabled.EditACKMode)
	before, err := newRunReport(plain, []Corpus{sampleCorpus()}, strings.Repeat("a", 40), Host{}, "/private/output")
	require.NoError(t, err)
	after, err := newRunReport(enabled, []Corpus{sampleCorpus()}, strings.Repeat("a", 40), Host{}, "/private/output")
	require.NoError(t, err)
	require.Equal(t, before.Workload, after.Workload)
	require.Equal(t, before.RequestPlan, after.RequestPlan)
	require.Nil(t, before.FailureCapture)
	require.Equal(t, &readSourceFailureConfig{MaxRecords: 8, MaxBytes: 65536, Encoding: "base64"}, after.FailureCapture)
	data, err := json.Marshal(before)
	require.NoError(t, err)
	require.NotContains(t, string(data), "read_source_failure_capture")
	err = smokeCommand(t.Context(), []string{"--retain-read-source-failures", "--edit-ack=local"})
	require.ErrorContains(t, err, "version 2 smoke requires fixed two-build inputs")
	// An armed context is a report-local budget, not an inherited global cap.
	ctx := withReadSourceFailureEvidence(t.Context(), true)
	child := withReadSourceFailureEvidence(ctx, true)
	require.NotSame(t, ctx.Value(readSourceFailureKey{}), child.Value(readSourceFailureKey{}))
	_, capture := readSourceFailureCapture(ctx, request{Tool: "search"}, map[string]any{"operation": "source"})
	require.Nil(t, capture)
}

type unsupportedReadSourceCaller struct{ calls int }

func (c *unsupportedReadSourceCaller) call(context.Context, string, any) (json.RawMessage, error) {
	c.calls++
	return nil, &rpcError{Code: -32603, Message: "unchanged transport failure"}
}

func TestReadSourceFailureCaptureUnsupportedTransport(t *testing.T) {
	c := new(unsupportedReadSourceCaller)
	ctx := withReadSourceFailureEvidence(t.Context(), true)
	_, attempts, err := callTool(ctx, c, request{Tool: "read", Operation: "source"}, map[string]any{"operation": "source"}, "workload", 0, 0, 0, time.Second, waitContext)
	require.ErrorContains(t, err, "unchanged transport failure")
	require.Equal(t, 1, c.calls, "observation cannot introduce another call")
	require.Len(t, attempts, 1)
	require.Equal(t, -32603, attempts[0].Code)
	require.Equal(t, "unsupported_transport", attempts[0].FailureRPC.Status)
	require.Empty(t, attempts[0].FailureRPC.RequestBase64)
	require.False(t, attempts[0].FailureRPC.IsErrorPresenceKnown)
}
