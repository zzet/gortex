package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMCPRequestIDKeyFollowsActualWireTypes(t *testing.T) {
	conn, reader, _ := startMCPConnectionTest(t, time.Second, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		key, available := MCPRequestIDKeyForContext(ctx)
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": map[string]any{"key": key, "available": available}})
	})
	for _, item := range []struct{ frame, key string }{
		{`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read"}}`, "n:7"},
		{`{"jsonrpc":"2.0","id":"7","method":"tools/call","params":{"name":"read"}}`, "s:7"},
	} {
		writeMCPTestFrame(t, conn, item.frame)
		response := readMCPTestResponse(t, conn, reader)
		if response.Error != nil || response.Result["key"] != item.key || response.Result["available"] != true {
			t.Fatalf("wire identity: got %+v, want %s", response, item.key)
		}
	}
}

func TestMCPRequestIDKeyIsNotAPublisherIdentity(t *testing.T) {
	first := withMCPDispatchSlotLeaseForRequest(t.Context(), "n:7")
	second := withMCPDispatchSlotLeaseForRequest(t.Context(), "n:7")
	if mcpDispatchSlotLeaseForContext(first) == mcpDispatchSlotLeaseForContext(second) {
		t.Fatal("separate request leases were shared")
	}
	firstKey, firstOK := MCPRequestIDKeyForContext(first)
	secondKey, secondOK := MCPRequestIDKeyForContext(second)
	if !firstOK || !secondOK || firstKey != secondKey {
		t.Fatal("reused wire key was mistaken for a unique physical owner")
	}
}

func TestMCPRequestIDKeySurvivesGrantReplacementAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(withMCPDispatchSlotLeaseForRequest(t.Context(), "s:read-source"))
	defer cancel()
	lease := mcpDispatchSlotLeaseForContext(ctx)
	first := lease.attach(func() {})
	second := lease.attach(func() {})
	lease.detach(first)
	if !ReleaseMCPDispatchSlot(ctx) {
		t.Fatal("current grant was not released")
	}
	lease.detach(second)
	cancel()
	if key, available := MCPRequestIDKeyForContext(ctx); !available || key != "s:read-source" {
		t.Fatalf("request identity replaced by grant lifetime: %q %v", key, available)
	}
	if got := testing.AllocsPerRun(100, func() { _, _ = MCPRequestIDKeyForContext(ctx) }); got != 0 {
		t.Fatalf("identity lookup allocates: %v", got)
	}
}

func TestMCPRequestIDKeyIsUnavailableWithoutTrackedRequest(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background(), withMCPDispatchSlotLease(t.Context())} {
		if key, available := MCPRequestIDKeyForContext(ctx); available || key != "" {
			t.Fatalf("untracked request borrowed identity: %q %v", key, available)
		}
	}
}
