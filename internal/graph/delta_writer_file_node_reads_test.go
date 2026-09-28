package graph

import "testing"

// A delta counts its file-node reads, each read once, whatever path it
// takes, so a caller can split a step into those reads and the rest.
func TestDeltaWriterCountsItsFileNodeReads(t *testing.T) {
	f := newEdgeClaimFixture()
	dw := NewDeltaWriter(f.store(), nil)
	if reads, wall := dw.FileNodeReadStats(); reads != 0 || wall != 0 {
		t.Fatalf("a fresh delta reports %d reads, %v", reads, wall)
	}
	dw.GetFileNodes("cfg.go")
	dw.GetFileNodesByPaths([]string{"cfg.go", "u1.go"})
	dw.NodesInFilesByKind([]string{"cfg.go"}, []NodeKind{KindType})
	if reads, wall := dw.FileNodeReadStats(); reads != 3 || wall <= 0 {
		t.Fatalf("after three file-node reads the delta reports %d reads, %v", reads, wall)
	}
	var _ FileNodeReadCounter = dw
}
