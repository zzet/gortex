package graph

import (
	"sort"
	"strings"
	"testing"
)

// recordingStore is a store at the bottom of a stack that answers recorded
// edges by file and counts the scans.
type recordingStore struct {
	*Graph
	scans int
}

func (s *recordingStore) RecordedEdgesAt(paths []string) []*Edge {
	s.scans++
	want := stringKeySet(paths)
	var out []*Edge
	for _, e := range s.AllEdges() {
		if _, ok := want[e.FilePath]; ok {
			out = append(out, cloneDeltaEdge(e))
		}
	}
	return out
}

// storeBelowRows is a below-rows source over the same rows.
type storeBelowRows struct {
	g     *recordingStore
	calls int
}

func (b *storeBelowRows) BelowFileRows(path string) ([]*Node, []*Edge, bool) {
	b.calls++
	var edges []*Edge
	for _, e := range b.g.AllEdges() {
		if e.FilePath == path {
			edges = append(edges, e)
		}
	}
	return b.g.GetFileNodes(path), edges, true
}

func renderDelta(dw *DeltaWriter, ids, paths []string) string {
	var rows []string
	for id, edges := range dw.GetOutEdgesByNodeIDs(ids) {
		rows = append(rows, id+":"+edgeSetRender(edges))
	}
	for id, edges := range dw.GetInEdgesByNodeIDs(ids) {
		rows = append(rows, "in "+id+":"+edgeSetRender(edges))
	}
	for _, p := range paths {
		for _, n := range dw.GetFileNodes(p) {
			rows = append(rows, "node "+n.ID)
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// A pristine delta covers the paths it evicts from the stack's below rows
// instead of scanning the stack for their recorded edges, and composes what
// the scanning delta composes, before and after further writes.
func TestDeltaWriterFirstEvictionCoversFromBelowRows(t *testing.T) {
	f := newEdgeClaimFixture()
	ids := []string{"u1.go::Use", "u2.go::Use", "cfg.go::Load"}
	paths := []string{"u1.go", "u2.go", "cfg.go"}
	edit := func(dw *DeltaWriter) {
		dw.EvictFiles([]string{"u2.go"})
		dw.EvictFiles([]string{"cfg.go"})
	}
	plain := NewDeltaWriter(&recordingStore{Graph: f.store()}, nil)
	edit(plain)
	want := renderDelta(plain, ids, paths)

	base := &recordingStore{Graph: f.store()}
	source := &storeBelowRows{g: base}
	dw := NewDeltaWriter(base, nil)
	dw.SetBelowFileRows(source)
	dw.EvictFiles([]string{"u2.go"})
	if base.scans != 0 || source.calls == 0 {
		t.Fatalf("first eviction: stack scans %d, below-rows reads %d; want it served from the below rows", base.scans, source.calls)
	}
	dw.EvictFiles([]string{"cfg.go"})
	if base.scans == 0 {
		t.Fatal("an eviction after the delta wrote was served from the below rows")
	}
	if got := renderDelta(dw, ids, paths); got != want {
		t.Fatalf("composed view:\n%s\nwant\n%s", got, want)
	}
}
