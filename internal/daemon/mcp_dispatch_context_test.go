package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDispatchEnvelopeLeavesMalformedToolArgumentsToPublicValidation(t *testing.T) {
	for _, value := range []string{`[]`, `"invalid"`, `42`, `null`} {
		frame := []byte(`{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"read","arguments":` + value + `}}`)
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatalf("admission consumed handler validation for arguments=%s: %v", value, err)
		}
		if string(envelope.ID) != "17" || string(envelope.Params.Arguments) != value {
			t.Fatalf("envelope identity/payload changed: %+v", envelope)
		}
	}
}

func TestSharedDispatchEntryReplayDoesNotAcquireAnotherSlot(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	ctx := withMCPDispatchQueueWait(withMCPDispatchSlots(t.Context(), slots), 0)
	sess := &Session{ID: "same-session", LogicalSessionID: "same-logical-session"}
	frame := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read_file"}}`)
	calls := 0
	dispatcher := testMCPDispatcherFunc(func(context.Context, *Session, []byte) ([]byte, error) {
		calls++
		return []byte(`{"jsonrpc":"2.0","id":9,"result":{"ok":true}}`), nil
	})
	first, err := DispatchMCPFrame(ctx, sess, frame, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	holder := takeScheduled(t, mcpSchedulerFor(slots), MCPDispatchMutating, "holder")
	defer holder.complete()
	replay, err := DispatchMCPFrame(ctx, sess, frame, dispatcher)
	if err != nil || string(replay) != string(first) || calls != 1 || len(slots) != 1 {
		t.Fatalf("cached replay double-admitted: err=%v calls=%d held=%d first=%s replay=%s", err, calls, len(slots), first, replay)
	}
}

func TestDiscoveryUsesProtocolCapacityWithoutLegacyNotificationBarrier(t *testing.T) {
	slots := newMCPDispatchSlots(2)
	var zero time.Duration
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	conn, reader, _ := startMCPConnectionWithHooksTest(t, time.Second, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if envelope.Method == "server/discover" {
			if !mcpDispatchRequestForContext(ctx).protocol {
				return nil, errors.New("discovery was not admitted as protocol negotiation")
			}
			close(started)
			<-release
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"ok":true}}`), nil
	}, &mcpConnectionHooks{dispatchSlots: slots, dispatchQueueWait: &zero})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)
	awaitMCPTestSignal(t, started, "modern discovery did not reach protocol dispatch")
	// Modern negotiation is stateless: no initialize/initialized barrier is
	// synthesized. Its protocol slot must not consume the only shared read slot.
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file"}}`)
	read := readMCPTestResponse(t, conn, reader)
	if string(read.ID) != "2" || read.Error != nil || read.Result["ok"] != true {
		t.Fatalf("tool after modern discovery: %+v", read)
	}
	unblock()
	discovery := readMCPTestResponse(t, conn, reader)
	if string(discovery.ID) != "1" || discovery.Error != nil {
		t.Fatalf("discovery response: %+v", discovery)
	}
}
