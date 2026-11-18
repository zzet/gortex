package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourcePendingRetryRequiresExactContractAndWorkloadAuthority(t *testing.T) {
	for _, name := range []string{"missing_reason", "wrong_reason", "superseded", "generic", "probe", "strict_probe", "write", "compressed", "malformed_compression", "wrong_operation", "unknown_delivery", "false_retriable"} {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{"retry_scope": "read_only", "delivery_unknown": false, "reason": "projection_deferred"}
			code, retriable := "file_view_pending", true
			r := request{Tool: "read", Operation: "source"}
			args := map[string]any{"operation": "source"}
			role := "workload"
			call := callTool
			switch name {
			case "missing_reason":
				delete(data, "reason")
			case "wrong_reason":
				data["reason"] = "store_busy"
			case "superseded":
				code = "projection_superseded"
			case "generic":
				code = "symbol_not_found"
			case "probe":
				role = "probe"
			case "strict_probe":
				call = callEditProbeTool
			case "write":
				r.Tool = "edit"
			case "compressed":
				args["options"] = map[string]any{"compress_bodies": true}
			case "malformed_compression":
				args["compress_bodies"] = "false"
			case "wrong_operation":
				args["operation"] = "summary"
			case "unknown_delivery":
				data["delivery_unknown"] = true
			case "false_retriable":
				retriable = false
			}
			peer := &readFreshnessPolicyPeer{data: readFreshnessTestWire(code, retriable, data), successAfter: 2}
			_, rows, err := call(t.Context(), peer, r, args, role, 0, 0, 0, time.Second, waitContext)
			require.Error(t, err)
			require.Equal(t, 1, peer.calls)
			require.Len(t, rows, 1)
			require.Equal(t, "error", rows[0].Outcome)
		})
	}
}

func TestSourcePendingRetryKeepsCapCancellationAndOriginalDeadline(t *testing.T) {
	wire := readFreshnessTestWire("file_view_pending", true, map[string]any{"retry_scope": "read_only", "delivery_unknown": false, "reason": "projection_deferred"})
	r := request{Tool: "read", Operation: "source"}
	args := map[string]any{"operation": "source"}
	peer := &readFreshnessPolicyPeer{data: wire}
	_, rows, err := callTool(t.Context(), peer, r, args, "workload", 0, 0, 0, time.Second, func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	require.Error(t, err)
	require.Equal(t, 3, peer.calls)
	require.Len(t, rows, 3)
	require.Equal(t, "error", rows[2].Outcome)
	require.Equal(t, 2, summarize(rows, 1, "workload")["read.source"].RetryableRefusals)
	for _, deadline := range peer.deadlines {
		require.Equal(t, peer.deadlines[0], deadline)
	}
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	peer = &readFreshnessPolicyPeer{data: wire, successAfter: 2}
	_, rows, err = callTool(caller, peer, r, args, "workload", 0, 0, 0, time.Second, func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() })
	require.Error(t, err)
	require.Equal(t, 1, peer.calls)
	require.Equal(t, "phase_censored", rows[len(rows)-1].Outcome)
	peer = &readFreshnessPolicyPeer{data: wire, successAfter: 2}
	_, rows, err = callTool(t.Context(), peer, r, args, "workload", 0, 0, 0, 5*time.Millisecond, func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() })
	require.Error(t, err)
	require.Equal(t, 1, peer.calls)
	require.Equal(t, "product_deadline", rows[len(rows)-1].Outcome)
}
