package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// recordedReadCounter is a store whose recorded-edge reads are counted.
type recordedReadCounter struct {
	*Graph
	reads int
}

func (c *recordedReadCounter) RecordedEdgesAt(paths []string) []*Edge {
	c.reads++
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var out []*Edge
	for _, e := range c.AllEdges() {
		if _, ok := want[e.FilePath]; ok {
			out = append(out, e)
		}
	}
	return out
}

func renderRecorded(edges []*Edge) string {
	rows := make([]string, 0, len(edges))
	for _, e := range edges {
		rows = append(rows, fmt.Sprintf("%s %s>%s %s %d", e.FilePath, e.From, e.To, e.Kind, e.Line))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// A delta's recorded-edge reads are answered over the stack's cache: every
// delta's answer equals the uncached composition's (its eviction's claims
// and its own rows at paths it does not cover applied per delta), the second
// delta's reads over the stack read no recorded edge below, and a caller's change
// to an answer does not reach the cache.
func TestDeltaWriterRecordedEdgeReadsAreKeptPerStack(t *testing.T) {
	f := newEdgeClaimFixture()
	paths := []string{"cfg.go", "other.go", "u1.go", "u2.go", "u3.go", "missing.go"}
	edit := func(dw *DeltaWriter) {
		dw.EvictFiles([]string{"cfg.go", "u2.go"})
		dw.AddBatch([]*Node{{ID: "cfg.go::Load", Name: "Load", Kind: KindFunction, FilePath: "cfg.go", Language: "go"}},
			[]*Edge{{From: "cfg.go::Load", To: "other.go::F1", Kind: EdgeCalls, FilePath: "cfg.go", Line: 8}})
	}
	plainBelow := &recordedReadCounter{Graph: f.store()}
	plain := NewDeltaWriter(plainBelow, nil)
	edit(plain)
	plainReader, ok := RecordedEdgesOf(plain)
	if !ok {
		t.Fatal("the plain delta serves no recorded edges")
	}
	want := renderRecorded(plainReader.RecordedEdgesAt(paths))
	if !strings.Contains(want, "cfg.go cfg.go::Load>other.go::F1") || strings.Contains(want, "cfg.go::Load>other.go::F0") ||
		!strings.Contains(want, "u1.go") || strings.Contains(want, "u2.go") {
		t.Fatalf("fixture precondition:\n%s", want)
	}
	below := &recordedReadCounter{Graph: f.store()}
	cache := NewBaseProjectionCache()
	var reads []int
	for delta := 0; delta < 2; delta++ {
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		readsHere := 0
		reader, ok := RecordedEdgesOf(dw)
		if !ok {
			t.Fatal("the cached delta serves no recorded edges")
		}
		before := below.reads
		first := reader.RecordedEdgesAt([]string{"u1.go", "u3.go"})
		readsHere += below.reads - before
		if len(first) == 0 {
			t.Fatalf("delta %d: the untouched read answered nothing", delta)
		}
		for _, e := range first {
			e.Kind = "mutated"
		}
		edit(dw)
		reader, _ = RecordedEdgesOf(dw)
		before = below.reads
		got := renderRecorded(reader.RecordedEdgesAt(paths))
		readsHere += below.reads - before
		if got != want {
			t.Fatalf("delta %d:\n%s\nwant\n%s", delta, got, want)
		}
		reads = append(reads, readsHere)
	}
	if reads[0] == 0 || reads[1] != 0 {
		t.Fatalf("recorded-edge reads below per delta = %v; want some on the first and none on the second", reads)
	}
	if hits, misses := cache.StackRecordedEdgeStats(); hits == 0 || misses == 0 {
		t.Fatalf("cache stats hits=%d misses=%d", hits, misses)
	}
}
