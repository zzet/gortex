package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type editProbeHeldSink struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	unblockOnce sync.Once
	mu          sync.Mutex
	data        bytes.Buffer
	closed      bool
	writeErr    error
	short       bool
}

func newEditProbeHeldSink(t *testing.T) *editProbeHeldSink {
	t.Helper()
	sink := &editProbeHeldSink{started: make(chan struct{}), release: make(chan struct{})}
	return sink
}

func startHeldEditProbeSpool(t *testing.T, sink *editProbeHeldSink) *editProbeState {
	t.Helper()
	state := &editProbeState{sink: sink}
	state.startRaw()
	t.Cleanup(func() {
		sink.unblock()
		state.finishRaw()
		_ = sink.Close()
	})
	return state
}
func (s *editProbeHeldSink) unblock() { s.unblockOnce.Do(func() { close(s.release) }) }
func (s *editProbeHeldSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if s.short {
		n, _ := s.data.Write(p[:len(p)-1])
		return n, nil
	}
	return s.data.Write(p)
}
func (s *editProbeHeldSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
func awaitEditProbeSpool(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned spool boundary did not complete")
	}
}

func TestEditProbeSpoolAcceptsMoreThan32RowsWithoutWriterBackpressure(t *testing.T) {
	sink := newEditProbeHeldSink(t)
	state := startHeldEditProbeSpool(t, sink)
	if err := state.emit(map[string]int{"ordinal": -1}); err != nil {
		t.Fatal(err)
	}
	awaitEditProbeSpool(t, sink.started)
	emitted := make(chan struct{})
	var emitErr error
	go func() {
		defer close(emitted)
		for i := 0; i < 1024; i++ {
			if err := state.emit(map[string]int{"ordinal": i}); err != nil {
				emitErr = err
				return
			}
		}
	}()
	awaitEditProbeSpool(t, emitted) // The actual writer remains held throughout.
	if emitErr != nil {
		t.Fatal(emitErr)
	}
	sink.unblock()
	state.finishRaw()
	if state.sinkErr != nil || state.rawBytes != state.rawAccepted || state.rawBytes >= editProbeRawLimit {
		t.Fatalf("lost accepted rows: bytes%d accepted%d err%v", state.rawBytes, state.rawAccepted, state.sinkErr)
	}
	decoder := json.NewDecoder(bytes.NewReader(sink.data.Bytes()))
	for i := -1; i < 1024; i++ {
		var row struct {
			Ordinal int `json:"ordinal"`
		}
		if err := decoder.Decode(&row); err != nil || row.Ordinal != i {
			t.Fatalf("row%d: %+v %v", i, row, err)
		}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("extra or malformed retained row")
	}
	if err := state.emit(nil); !errors.Is(err, errEditProbeEvidenceIncomplete) {
		t.Fatal("post-close emit accepted")
	}
}

func TestEditProbeSpoolByteCapCountsInFlightBytesAndStillDrainsAcceptedData(t *testing.T) {
	sink := newEditProbeHeldSink(t)
	state := startHeldEditProbeSpool(t, sink)
	if err := state.emit(strings.Repeat("x", editProbeRawLimit-3)); err != nil {
		t.Fatal(err)
	}
	awaitEditProbeSpool(t, sink.started)
	if err := state.emit(nil); !errors.Is(err, errEditProbeEvidenceIncomplete) || !strings.Contains(err.Error(), "evidence limit") {
		t.Fatal("in-flight bytes escaped total cap")
	}
	sink.unblock()
	state.finishRaw()
	if state.rawAccepted != editProbeRawLimit || state.rawBytes != editProbeRawLimit || !errors.Is(state.sinkErr, errEditProbeEvidenceIncomplete) {
		t.Fatalf("cap/drain: accepted%d bytes%d err%v", state.rawAccepted, state.rawBytes, state.sinkErr)
	}
}

func TestEditProbeSpoolRetainsWriteAndShortWriteFailures(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "write_error", true: "short_write"}[short], func(t *testing.T) {
			sink := newEditProbeHeldSink(t)
			sink.short = short
			if !short {
				sink.writeErr = errors.New("owned sink failure")
			}
			state := startHeldEditProbeSpool(t, sink)
			if err := state.emit(map[string]bool{"required": true}); err != nil {
				t.Fatal(err)
			}
			awaitEditProbeSpool(t, sink.started)
			sink.unblock()
			state.finishRaw()
			if !errors.Is(state.sinkErr, errEditProbeEvidenceIncomplete) || state.rawBytes >= state.rawAccepted {
				t.Fatalf("failed bytes credited: %d/%d %v", state.rawBytes, state.rawAccepted, state.sinkErr)
			}
			if err := state.emit(nil); !errors.Is(err, errEditProbeEvidenceIncomplete) {
				t.Fatal("write failure cleared")
			}
		})
	}
}

func TestEditProbeSpoolCancellationJoinsProducerThenDrainsBeforeClose(t *testing.T) {
	sink := newEditProbeHeldSink(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state := startHeldEditProbeSpool(t, sink)
	state.cancel = cancel
	if err := state.emit(map[string]string{"event": "before_cancel"}); err != nil {
		t.Fatal(err)
	}
	awaitEditProbeSpool(t, sink.started)
	producerDone := make(chan struct{})
	var emitErr error
	state.probes.Go(func() {
		defer close(producerDone)
		<-ctx.Done()
		emitErr = state.emit(map[string]string{"event": "phase_censored"})
	})
	runtime := &editProbeRuntime{finalized: true, states: []*editProbeState{state}}
	closed := make(chan struct{})
	var closeErr error
	go func() { defer close(closed); closeErr = runtime.close() }()
	awaitEditProbeSpool(t, producerDone)
	if emitErr != nil {
		t.Fatal(emitErr)
	}
	sink.unblock()
	awaitEditProbeSpool(t, closed)
	if closeErr != nil || !sink.closed || state.rawBytes != state.rawAccepted || !bytes.Contains(sink.data.Bytes(), []byte("phase_censored")) {
		t.Fatalf("cancel/close lost final evidence: close%v bytes%d/%d", closeErr, state.rawBytes, state.rawAccepted)
	}
	state.finishRaw() // Repeated shutdown joins the same completed writer.
}
