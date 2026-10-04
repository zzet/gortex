package graph

import (
	"sync"
	"testing"
	"time"
)

type heldFileNodeReader struct {
	Reader
	entered, release chan struct{}
	once             sync.Once
}

func (r *heldFileNodeReader) GetFileNodes(path string) []*Node {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return r.Reader.GetFileNodes(path)
}

// A delta counts its file-node reads, each read once, whatever path it
// takes, so a caller can split a step into those reads and the rest.
func TestDeltaWriterCountsItsFileNodeReads(t *testing.T) {
	f := newEdgeClaimFixture()
	reader := &heldFileNodeReader{Reader: f.store(), entered: make(chan struct{}), release: make(chan struct{})}
	dw := NewDeltaWriter(reader, nil)
	if reads, wall := dw.FileNodeReadStats(); reads != 0 || wall != 0 {
		t.Fatalf("a fresh delta reports %d reads, %v", reads, wall)
	}
	done := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(reader.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("file-node read did not join after its reader was released")
		}
	})
	go func() {
		dw.GetFileNodes("cfg.go")
		close(done)
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("file-node instrumentation did not enter the held reader")
	}
	// Hold a real reader long enough to observe a duration even on a coarse
	// clock. The counter must include the measured hold, not invent a tick.
	started := time.Now()
	time.Sleep(25 * time.Millisecond)
	held := time.Since(started)
	release.Do(func() { close(reader.release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("file-node read did not finish after its reader was released")
	}
	dw.GetFileNodesByPaths([]string{"cfg.go", "u1.go"})
	dw.NodesInFilesByKind([]string{"cfg.go"}, []NodeKind{KindType})
	if reads, wall := dw.FileNodeReadStats(); reads != 3 || held <= 0 || wall < held {
		t.Fatalf("after three file-node reads the delta reports %d reads, %v", reads, wall)
	}
	var _ FileNodeReadCounter = dw
}
