package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Complete rows crossing the former artifact cap must survive, including when
// the first row is still in flight. These are the two observed overrun sizes;
// the payloads are synthetic and do not reclassify any historical evidence.
func TestEditProbeRawCaptureRetainsRowsCrossingEightMiB(t *testing.T) {
	const oldLimit = 8 << 20
	for _, overrun := range []int{309, 5204} {
		t.Run(fmt.Sprint(overrun), func(t *testing.T) {
			sink := newEditProbeHeldSink(t)
			state := startHeldEditProbeSpool(t, sink)
			rows := []string{strings.Repeat("x", oldLimit-4099), strings.Repeat("y", 4093+overrun)}
			if err := state.emit(rows[0]); err != nil {
				t.Fatal(err)
			}
			awaitEditProbeSpool(t, sink.started)
			if err := state.emit(rows[1]); err != nil {
				t.Fatalf("complete row crossing the old cap was refused: %v", err)
			}
			sink.unblock()
			state.finishRaw()
			wantBytes := oldLimit + overrun
			if state.sinkErr != nil || state.rawAccepted != wantBytes || state.rawBytes != wantBytes || sink.data.Len() != wantBytes {
				t.Fatalf("raw rows lost: accepted=%d written=%d retained=%d want=%d err=%v", state.rawAccepted, state.rawBytes, sink.data.Len(), wantBytes, state.sinkErr)
			}
			decoder := json.NewDecoder(bytes.NewReader(sink.data.Bytes()))
			for i, want := range rows {
				var got string
				if err := decoder.Decode(&got); err != nil || got != want {
					t.Fatalf("retained row %d differs: %v", i, err)
				}
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("extra or malformed retained row: %v", err)
			}
		})
	}
}
