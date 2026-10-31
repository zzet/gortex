package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func decodeAcknowledgementResult(t *testing.T, result *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &payload))
	return payload
}

func ackWireMilliseconds(value float64) *float64 { return &value }

func TestAcknowledgementWirePartialSubtotalHasSeparateDenominator(t *testing.T) {
	timings := (indexer.PublicationPhaseSnapshot{}).EditTimings()
	timings.Milliseconds["gate_wait"], timings.States["gate_wait"] = ackWireMilliseconds(2), "measured"
	timings.Window.PatchWallMS, timings.Window.PublicationWallMS = ackWireMilliseconds(30), ackWireMilliseconds(100)
	timings.Window.PublicationUnattributedWallMS = ackWireMilliseconds(50)
	payload := map[string]any{"phases_ms": timings.Milliseconds, "phase_states": timings.States, "phase_timing": timings.Window}
	snapshot := editAcknowledgementSnapshot{
		RPCID: 3, ReplyWallMS: ackWireMilliseconds(20), ReplyState: "measured", ActualState: "measured",
		LapsDisjoint: true, ParseGate: editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(1), AckMS: ackWireMilliseconds(1), State: "measured", Started: 1, Completed: 1},
		DiskWrite:       editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(2), AckMS: ackWireMilliseconds(2), State: "measured", Started: 1, Completed: 1},
		TicketAdmission: editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(3), AckMS: ackWireMilliseconds(3), State: "measured", Started: 1, Completed: 1},
		ReindexWait:     editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(4), AckMS: ackWireMilliseconds(4), State: "measured", Started: 1, Completed: 1},
	}
	attachAcknowledgementPayload(payload, &snapshot, "observed")
	ack := payload["acknowledgement"].(acknowledgementPayload)
	require.Equal(t, "rpc-3", *ack.RPCID)
	require.Equal(t, "shared_rpc_mcp_bound_call", ack.Scope)
	require.Equal(t, "partial", ack.SumState)
	require.Equal(t, 10.0, *ack.SubtotalMS)
	require.Equal(t, 10.0, *ack.UnattributedMS)
	require.Greater(t, *ack.UnattributedMS, 0.05*(*ack.ReplyWallMS), "a large real gap is not assigned to a phase to force the 5% gate")
	require.Equal(t, "not_additive_overlapping_lifetimes", ack.CombinedAckPatchSumState)
	window := payload["phase_timing"].(indexer.EditTimingWindow)
	require.Equal(t, 20.0, *window.AcknowledgementWallMS)
	require.Equal(t, 30.0, *window.PatchWallMS)
	require.Equal(t, 100.0, *window.PublicationWallMS)
	require.Nil(t, window.PublicationUnattributedWallMS, "do not subtract shared-RPC values from a per-file interval")
	require.Equal(t, 2.0, *payload["phases_ms"].(map[string]*float64)["gate_wait"])
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.Len(t, wire["phases_ms"], 19) // original required keys plus four explicit measured additions
	wireAck := wire["acknowledgement"].(map[string]any)
	phases := wireAck["phases_ms"].(map[string]any)
	require.Len(t, phases, 11)
	for _, name := range []string{"dispatcher_queue", "dispatcher_preamble", "middleware", "edit_control", "reply_prepare", "caller_result", "transport"} {
		require.Contains(t, phases, name)
		require.Nil(t, phases[name], name)
	}
}

func TestAcknowledgementWireRejectsInvalidSumsAndLateClipping(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		snapshot := editAcknowledgementSnapshot{RPCID: 1, ReplyWallMS: ackWireMilliseconds(1), ReplyState: "measured", LapsDisjoint: !overlap,
			DiskWrite: editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(2), AckMS: ackWireMilliseconds(2), State: "measured", Started: 1, Completed: 1}}
		ack := buildAcknowledgementPayload(&snapshot, "observed")
		require.Nil(t, ack.SubtotalMS)
		require.Nil(t, ack.UnattributedMS)
		require.Contains(t, []string{"invalid_total", "invalid_overlap"}, ack.SumState)
	}
	start := time.Now()
	record := &editAcknowledgement{entered: start}
	lap := record.beginLapAt(editAckDiskWrite, start.Add(time.Millisecond))
	record.finishReplyAt(start.Add(2*time.Millisecond), "go_error")
	lap.finishAt(start.Add(5 * time.Millisecond))
	record.finishActualAt(start.Add(6 * time.Millisecond))
	snapshot := record.snapshot()
	ack := buildAcknowledgementPayload(&snapshot, "observed")
	require.Nil(t, ack.PhasesMS["disk_write"])
	require.Equal(t, 4.0, *ack.ObservedExecutionMS["disk_write"])
	require.Equal(t, "outside_reply_window", ack.PhaseStates["disk_write"])
	require.Nil(t, ack.SubtotalMS)
	invalid := editAcknowledgementSnapshot{ReplyWallMS: ackWireMilliseconds(10), ReplyState: "measured", LapsDisjoint: true,
		DiskWrite: editAcknowledgementLapSnapshot{ObservedMS: ackWireMilliseconds(2), AckMS: ackWireMilliseconds(2), State: "measured", Started: 2, Completed: 1}}
	bad := buildAcknowledgementPayload(&invalid, "observed")
	require.Equal(t, "invalid_count", bad.PhaseStates["disk_write"])
	require.Nil(t, bad.PhasesMS["disk_write"])
	require.Nil(t, bad.SubtotalMS)
}

func TestMutationStatusAcknowledgementSharesRPCAndPreservesPublication(t *testing.T) {
	s := newMutationServer()
	s.ToolCallTimeout = -1
	s.watcher = &canonicalPublicationWatcher{}
	root := t.TempDir()
	var records []*mutationCommitRecord
	h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		for _, name := range []string{"a.go", "b.go"} {
			path := filepath.Join(root, name)
			record, err := s.commitFileMutation(ctx, "write_file", "", "", name, path, []byte("package p\n"), 0o600)
			if err != nil {
				return nil, err
			}
			record.recordGraph(s.mutationReindexState(ctx, path))
			records = append(records, record)
		}
		return mcp.NewToolResultText("done"), nil
	})
	_, err := h(canonicalTimingContext(), editOperationRequest("write_file", ""))
	require.NoError(t, err)
	require.Len(t, records, 2)
	var firstRPC string
	for _, record := range records {
		timeline, _ := retainedMutationPublication(record)
		before := timeline.Snapshot()
		payload := s.mutationStatusPayload(record)
		require.Equal(t, before, payload["publication_phases"], "legacy first-wins origin and offsets remain unchanged")
		ack := payload["acknowledgement"].(acknowledgementPayload)
		require.Equal(t, "observed", ack.State)
		if firstRPC == "" {
			firstRPC = *ack.RPCID
		}
		require.Equal(t, firstRPC, *ack.RPCID)
		require.EqualValues(t, 2, ack.Counts["disk_write"].Completed)
		require.EqualValues(t, 2, ack.Counts["publish"].Completed)
		require.EqualValues(t, 2, ack.Counts["reindex_wait"].Completed)
		require.Equal(t, "partial", ack.SumState)
		phases := payload["phases_ms"].(map[string]*float64)
		original := *phases["disk_write"]
		*phases["disk_write"] = 999
		require.Equal(t, original, *s.mutationStatusPayload(record)["phases_ms"].(map[string]*float64)["disk_write"])
	}
	ack := retainedMutationAcknowledgement(records[0])
	for i := range maxMutationCommits {
		s.mutationCommits.put(&mutationCommitRecord{id: fmt.Sprintf("wire-cap-%d", i), startedAt: time.Now()})
	}
	ack.finishActualAt(time.Now()) // first-wins completion cannot resurrect history
	missing := s.mutationStatusPayload(records[0])["acknowledgement"].(acknowledgementPayload)
	require.Equal(t, "unavailable_recording_or_evicted", missing.State)
	require.Nil(t, missing.RPCID)
	require.Nil(t, missing.ReplyWallMS)
}

func TestAtomicBatchResponseAndStatusExposeOnlyBoundedTimingReference(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	s.ToolCallTimeout = -1
	file := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"format": "json", "transaction_id": "wire-batch", "edits": []any{
		map[string]any{"op": "edit_file", "path": file, "old_string": "before", "new_string": "after"},
	}}
	h := s.boundToolHandler(s.handleAtomicBatchEdit)
	result, err := h(t.Context(), req)
	first := decodeAcknowledgementResult(t, result, err)
	id := first["acknowledgement_receipt"].(string)
	status := editOperationRequest("mutation_status", "")
	status.Params.Arguments = map[string]any{"receipt": id, "format": "json"}
	result, err = s.handleMutationStatus(t.Context(), status)
	timing := decodeAcknowledgementResult(t, result, err)
	require.Equal(t, "batch_acknowledgement", timing["receipt_kind"])
	require.Equal(t, true, timing["found"])
	require.NotContains(t, timing, "disk_status")
	require.NotContains(t, timing, "graph_status")
	result, err = h(t.Context(), req)
	duplicate := decodeAcknowledgementResult(t, result, err)
	require.Equal(t, id, duplicate["acknowledgement_receipt"])
	result, err = s.handleMutationStatus(t.Context(), status)
	require.Equal(t, timing["acknowledgement"], decodeAcknowledgementResult(t, result, err)["acknowledgement"])
	manifest, err := os.ReadFile(batchManifestPath("wire-batch"))
	require.NoError(t, err)
	require.NotContains(t, string(manifest), id)
	require.NotContains(t, string(manifest), "acknowledgement")
	compact := editOperationRequest("batch_edit", "")
	compact.Params.Arguments = map[string]any{"transaction_id": "wire-batch", "status_only": true, "compact": true}
	result, err = h(t.Context(), compact)
	require.NoError(t, err)
	require.Contains(t, result.Content[0].(mcp.TextContent).Text, "acknowledgement_receipt="+id)
	status.Params.Arguments = map[string]any{"receipt": id, "format": "toon"}
	result, err = s.handleMutationStatus(t.Context(), status)
	require.NoError(t, err)
	require.Contains(t, result.Content[0].(mcp.TextContent).Text, "shared_rpc_mcp_bound_call")
	for i := range maxMutationCommits {
		s.mutationCommits.put(&mutationCommitRecord{id: fmt.Sprintf("wire-batch-cap-%d", i), startedAt: time.Now()})
	}
	status.Params.Arguments = map[string]any{"receipt": id, "format": "json"}
	result, err = s.handleMutationStatus(t.Context(), status)
	unavailable := decodeAcknowledgementResult(t, result, err)
	require.Equal(t, false, unavailable["found"])
	require.Equal(t, "unavailable_ledger_entry_or_scope", unavailable["acknowledgement"].(map[string]any)["state"])
	require.NotContains(t, unavailable, "disk_status")
}

func TestBatchAcknowledgementStatusHonoursCheckoutIncarnationAndRestart(t *testing.T) {
	s := newMutationServer()
	ctx, _, _ := s.withEditAcknowledgement(t.Context())
	state := &batchTransactionState{receipt: batchTransactionReceipt{TransactionID: "scoped", CheckoutID: "checkout-a", Incarnation: "inc-a"}}
	s.bindBatchAcknowledgement(ctx, state)
	id := state.acknowledgementReceipt
	for _, tt := range []struct {
		checkout, incarnation string
		found                 bool
	}{
		{"checkout-a", "inc-a", true}, {"checkout-a", "inc-b", false}, {"checkout-b", "inc-a", false}, {"", "", false},
	} {
		scope := context.WithValue(t.Context(), checkoutMutationContextKey{}, &checkoutMutationState{checkoutID: tt.checkout, incarnation: tt.incarnation})
		payload, handled := s.batchAcknowledgementReceiptPayload(scope, id)
		require.True(t, handled)
		require.Equal(t, tt.found, payload["found"])
		if !tt.found {
			require.Nil(t, payload["acknowledgement"].(acknowledgementPayload).RPCID)
		}
		require.NotContains(t, payload, "disk_status")
		require.NotContains(t, payload, "graph_status")
	}
	restarted := &Server{}
	payload, handled := restarted.batchAcknowledgementReceiptPayload(t.Context(), id)
	require.True(t, handled)
	require.Equal(t, false, payload["found"])
	require.Equal(t, "unavailable_ledger_entry_or_scope", payload["acknowledgement"].(acknowledgementPayload).State)
}
