package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

type sourceProjectionCarryBudgetStore struct {
	*store_sqlite.Store
	carryDeadline chan time.Time
}

func (s *sourceProjectionCarryBudgetStore) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	if _, observesSource := ctx.Value(checkedProjectionReadObserverKey{}).(checkedProjectionReadObserver); !observesSource {
		return s.Store.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
	}
	deadline, _ := ctx.Deadline()
	select {
	case s.carryDeadline <- deadline:
	default:
	}
	<-ctx.Done()
	return nil, false, ctx.Err()
}

func TestRegisteredSourceKeepsCheckedBytesWhenRelationCarryWouldExpire(t *testing.T) {
	selectedProjectionRegistry(t)
	s, store, path := canonicalPromotionReadFixture(t)
	// Only the scenario source request can stall the optional outgoing-edge
	// read. Inventory, ownership, physical bytes and hydration use real data.
	carry := &sourceProjectionCarryBudgetStore{Store: store, carryDeadline: make(chan time.Time, 1)}
	s.graph = carry
	pending := make(chan indexer.MutationResult)
	var receipt *mutationReceipt
	t.Cleanup(func() {
		close(pending)
		if receipt != nil {
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("owned ACK ticket did not join")
			}
		}
	})
	s.SetWatcher(mutationTestWatcher{done: pending})

	const session = "source-projection-carry-budget"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	base := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	const (
		id     = "main.go::SourceCarryTarget"
		anchor = "func Target() int { return 1 }"
		body   = "func SourceCarryTarget() int { return Target() }"
	)
	output := map[string]any{"format": "json", "max_bytes": 0}
	ack, err := externalVersionRegisteredCall(s, base, 2, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "main.go"},
		"match": anchor, "replacement": anchor + "\n\n" + body,
		"options": map[string]any{"mutation_id": "source-carry-budget"},
		"output":  output,
	})
	require.NoError(t, err)
	require.False(t, ack.IsError, resultText(ack))
	ackFields := extractTextResult(t, ack)
	require.Equal(t, "local", ackFields["ack_mode"])
	require.Equal(t, "committed", ackFields["disk_status"])
	receiptID, ok := ackFields["reindex_receipt"].(string)
	require.True(t, ok)
	value, found := s.mutationReceipts.Load(receiptID)
	require.True(t, found)
	receipt = value.(*mutationReceipt)
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.Positive(t, entry.commitSeq)
	require.True(t, liveProjectionForAcknowledgement(entry))
	require.Nil(t, store.GetNode(id), "only the acknowledged projection owns the new symbol")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), body)

	carryCandidate := false
	for edge := range entry.layer.Edges() {
		if edge.Kind == graph.EdgeCalls && edge.From == id && edge.FilePath == "main.go" && graph.IsUnresolvedTarget(edge.To) && !graph.IsFnValuePlaceholder(edge.To) {
			carryCandidate = true
			break
		}
	}
	require.True(t, carryCandidate, "the ACK projection must contain an unresolved call that admits optional relation carry")

	var checked atomic.Pointer[projectionEntry]
	caller, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	caller = context.WithValue(caller, checkedProjectionReadObserverKey{}, checkedProjectionReadObserver(func(_ context.Context, selected *projectionEntry) {
		checked.Store(selected)
	}))
	result, err := externalVersionRegisteredCall(s, caller, 3, "read", map[string]any{
		"operation": "source", "target": map[string]any{"symbol": id},
		"context": map[string]any{"context_lines": 0}, "output": output,
	})
	require.NoError(t, err)
	require.NoError(t, caller.Err(), "private relation work cannot cancel the caller")
	require.NotNil(t, result)
	require.Same(t, entry, checked.Load(), "source selection must check the exact existing ACK owner against current disk bytes")
	require.Same(t, entry, liveEntry(pendingFileProjections, path), "no competing physical owner")
	require.Nil(t, store.GetNode(id), "source consumption cannot publish canonical state")
	if result.IsError {
		select {
		case deadline := <-carry.carryDeadline:
			require.False(t, deadline.IsZero())
			require.False(t, time.Now().Before(deadline), "the unchanged private allowance must have expired")
		default:
			t.Fatal("the refusal did not reach optional relation carry")
		}
		requireReadFreshnessContract(t, result, "file_view_pending")
		var failure StructuredError
		require.NoError(t, json.Unmarshal([]byte(resultText(result)), &failure))
		require.Equal(t, "projection_deferred", failure.Data["reason"])
		t.Fatalf("optional relation carry expired after current ACK bytes and owner were checked: %s", resultText(result))
	}
	source := extractTextResult(t, result)
	require.Equal(t, id, source["id"])
	require.Equal(t, body, source["source"])
	require.Equal(t, true, source["freshness"].(map[string]any)["readback_exact"])
	require.Empty(t, carry.carryDeadline, "plain source does not need optional relation carry")
}
