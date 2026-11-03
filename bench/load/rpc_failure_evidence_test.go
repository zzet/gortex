package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
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
}
