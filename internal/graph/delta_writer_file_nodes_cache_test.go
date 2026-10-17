package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fileNodeReadCounter counts the bottom store's per-path file-node batches.
type fileNodeReadCounter struct {
	*Graph
	reads int
}

func (c *fileNodeReadCounter) GetFileNodesByPaths(paths []string) map[string][]*Node {
	c.reads++
	return c.Graph.GetFileNodesByPaths(paths)
}

func renderFileNodes(m map[string][]*Node) string {
	var rows []string
	for p, nodes := range m {
		for _, n := range nodes {
			rows = append(rows, fmt.Sprintf("%s %s %s", p, n.ID, n.Kind))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// A delta's per-path file-node reads are answered from the stack's cache:
// every delta's answer equals the uncached composition's (the delta's own
// eviction and re-statement applied per delta), and the second delta over the
// stack reads no file node below.
func TestDeltaWriterFileNodeReadsAreKeptPerStack(t *testing.T) {
	f := newEdgeClaimFixture()
	paths := []string{"cfg.go", "other.go", "u1.go", "u2.go", "u3.go", "moved.go", "missing.go"}
	edit := func(dw *DeltaWriter) {
		dw.EvictFiles([]string{"cfg.go", "u2.go"})
		dw.AddBatch([]*Node{{ID: "cfg.go::Load", Name: "Load", Kind: KindFunction, FilePath: "cfg.go", Language: "go"}}, nil)
		// An identity the delta re-homes: the row below at other.go is hidden
		// though the delta does not cover other.go.
		dw.AddBatch([]*Node{{ID: "other.go::F1", Name: "F1", Kind: KindFunction, FilePath: "moved.go", Language: "go"}}, nil)
	}
	plain := NewDeltaWriter(f.store(), nil)
	edit(plain)
	want := renderFileNodes(plain.GetFileNodesByPaths(paths))
	if !strings.Contains(want, "cfg.go cfg.go::Load") || strings.Contains(want, "u2.go") || !strings.Contains(want, "other.go") ||
		strings.Contains(want, "other.go other.go::F1 ") {
		t.Fatalf("fixture precondition:\n%s", want)
	}
	below := &fileNodeReadCounter{Graph: f.store()}
	cache := NewBaseProjectionCache()
	var reads []int
	for delta := 0; delta < 2; delta++ {
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		before := below.reads
		// Read once before the delta writes (the prior graph) and once after.
		if got := renderFileNodes(dw.GetFileNodesByPaths([]string{"u1.go", "other.go"})); got == "" {
			t.Fatalf("delta %d: the untouched read answered nothing", delta)
		}
		edit(dw)
		if got := renderFileNodes(dw.GetFileNodesByPaths(paths)); got != want {
			t.Fatalf("delta %d:\n%s\nwant\n%s", delta, got, want)
		}
		reads = append(reads, below.reads-before)
	}
	if reads[0] == 0 || reads[1] != 0 {
		t.Fatalf("file-node reads below per delta = %v; want some on the first and none on the second", reads)
	}
}
