package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The result really traverses the multiplexed net.Pipe client. One owner's
// rejected frame must not consume another owner's cap, and pending/in-flight
// accepted rows must be counted once and drained even after the refusal.
func TestEditProbeRawCapReportsKnownFrameAndKeepsOwnersIndependent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := searchSequenceClient(t, func(frame stubFrame) any {
		if frame.Params["large"] == true {
			return map[string]any{"blob": strings.Repeat("x", editProbeRawLimit)}
		}
		return map[string]any{"owner": "second"}
	})
	firstSink, secondSink := newEditProbeHeldSink(t), newEditProbeHeldSink(t)
	first, second := startHeldEditProbeSpool(t, firstSink), startHeldEditProbeSpool(t, secondSink)
	secondSink.unblock()
	if err := first.emit(map[string]string{"event": "held"}); err != nil {
		t.Fatal(err)
	}
	awaitEditProbeSpool(t, firstSink.started)
	if err := first.emit(map[string]string{"event": "queued"}); err != nil {
		t.Fatal(err)
	}
	first.sinkMu.Lock()
	accepted, written, queued := first.rawAccepted, first.rawBytes, len(first.rawPending)
	first.sinkMu.Unlock()
	if accepted == 0 || written != 0 || queued == 0 {
		t.Fatalf("missing actual held/queued boundary: accepted=%d written=%d queued=%d", accepted, written, queued)
	}
	runtime := &editProbeRuntime{origin: time.Now()}
	req := request{Tool: "search", Operation: "symbols"}
	caller := &editProbeCaller{runtime: runtime, state: first, client: client, purpose: "readback:search_visible", request: req}
	data, err := caller.call(ctx, "tools/call", map[string]any{"large": true})
	if !errors.Is(err, errEditProbeEvidenceIncomplete) {
		t.Fatalf("oversized real reply did not fail evidence: %v", err)
	}
	id := caller.last
	if id.ID == 0 || id.ReplyDelivered.IsZero() || len(data) <= editProbeRawLimit {
		t.Fatal("known oversized result did not traverse its actual request/reply channel")
	}
	encoded, encodeErr := json.Marshal(map[string]any{
		"event": "rpc", "purpose": caller.purpose, "tool": req.name(), "wire_id": id.ID,
		"call_start_ns": editProbeOffset(id.Started, runtime.origin), "write_start_ns": editProbeOffset(id.WriteStarted, runtime.origin),
		"write_end_ns": editProbeOffset(id.WriteFinished, runtime.origin), "reply_delivered_ns": editProbeOffset(id.ReplyDelivered, runtime.origin),
		"call_end_ns": editProbeOffset(id.Finished, runtime.origin), "output_mode": editProbeRequestOutput(req), "payload": data,
	})
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	for _, scalar := range []string{
		fmt.Sprintf("accepted_bytes=%d", accepted), fmt.Sprintf("written_bytes=%d", written), fmt.Sprintf("queued_bytes=%d", queued),
		fmt.Sprintf("attempted_encoded_bytes=%d", len(encoded)+1), fmt.Sprintf("payload_bytes=%d", len(data)),
		`event="rpc"`, `purpose="readback:search_visible"`, fmt.Sprintf("wire_id=%d", id.ID),
	} {
		if !strings.Contains(err.Error(), scalar) {
			t.Errorf("first-cap error omitted exact %s: %v", scalar, err)
		}
	}
	if sticky := first.emit(map[string]string{"event": "after_cap"}); sticky == nil || sticky.Error() != first.sinkErr.Error() {
		t.Fatal("first-cap diagnostics were replaced or cleared")
	}
	other := &editProbeCaller{runtime: runtime, state: second, client: client, purpose: "readback:text", request: request{Tool: "search", Operation: "text"}}
	otherData, otherErr := other.call(ctx, "tools/call", map[string]any{"large": false})
	if otherErr != nil || other.last.ID == id.ID || !strings.Contains(string(otherData), `"second"`) {
		t.Fatalf("first owner contaminated the second reply/cap: id=%d err=%v", other.last.ID, otherErr)
	}
	firstSink.unblock()
	first.finishRaw()
	second.finishRaw()
	if first.rawAccepted != accepted || first.rawBytes != accepted || firstSink.data.Len() != accepted {
		t.Fatalf("rejected frame counted or accepted bytes lost: accepted=%d written=%d retained=%d", first.rawAccepted, first.rawBytes, firstSink.data.Len())
	}
	if second.sinkErr != nil || second.rawAccepted == 0 || second.rawBytes != second.rawAccepted {
		t.Fatalf("second owner did not retain exactly its own frame: accepted=%d written=%d err=%v", second.rawAccepted, second.rawBytes, second.sinkErr)
	}
	if strings.Contains(firstSink.data.String(), `"payload"`) || !strings.Contains(secondSink.data.String(), `"second"`) {
		t.Fatal("rejected oversized payload retained or second-owner result missing")
	}
	t.Logf("first accepted/written=%d rejected encoded=%d payload=%d wire=%d; second accepted/written=%d wire=%d; unchanged cap=%d", accepted, len(encoded)+1, len(data), id.ID, second.rawBytes, other.last.ID, editProbeRawLimit)
}
