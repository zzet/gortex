package graph

import (
	"iter"
	"sort"
	"strings"
	"testing"
)

// scopedCountingStore answers file-scoped edge rows with fresh copies, as a
// database-backed store does, and counts the scans.
type scopedCountingStore struct {
	*Graph
	scans int
}

func (s *scopedCountingStore) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...EdgeKind) iter.Seq[ScopedEdgeRow] {
	s.scans++
	files := stringKeySet(filePaths)
	want := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	return func(yield func(ScopedEdgeRow) bool) {
		for _, n := range s.AllNodes() {
			if _, ok := files[n.FilePath]; !ok {
				continue
			}
			for _, e := range s.GetOutEdges(n.ID) {
				if _, ok := want[e.Kind]; !ok {
					continue
				}
				if !yield(ScopedEdgeRow{Edge: cloneDeltaEdge(e), Source: n}) {
					return
				}
			}
		}
	}
}

func (s *scopedCountingStore) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...NodeKind) iter.Seq[*Node] {
	return func(func(*Node) bool) {}
}

func (s *scopedCountingStore) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*Node] {
	return func(func(*Node) bool) {}
}

func renderScopedRows(dw *DeltaWriter, files []string) (string, []*Edge) {
	var rows []string
	var edges []*Edge
	for row := range dw.EdgesInScopeSeq(nil, files, EdgeCalls) {
		rows = append(rows, deltaEdgeRender(row.Edge))
		edges = append(edges, row.Edge)
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n"), edges
}

// A delta's scoped edge rows take the bottom store's from the stack's cache:
// every delta's answer equals the uncached composition's (the delta's own
// eviction hides the rows of the file it re-derives), the second delta over
// the stack does not scan the bottom store, and a row one delta rebinds is
// not what the next delta reads.
func TestDeltaWriterBottomScopedRowsAreKeptPerStack(t *testing.T) {
	f := newEdgeClaimFixture()
	files := []string{"u1.go", "u2.go"}
	edit := func(dw *DeltaWriter) { dw.EvictFiles([]string{"u1.go"}) }
	plainStore := &scopedCountingStore{Graph: f.store()}
	plain := NewDeltaWriter(plainStore, nil)
	edit(plain)
	want, _ := renderScopedRows(plain, files)
	if strings.Contains(want, "u1.go::Use") || !strings.Contains(want, "u2.go::Use") {
		t.Fatalf("fixture precondition:\n%s", want)
	}
	below := &scopedCountingStore{Graph: f.store()}
	cache := NewBaseProjectionCache()
	var scans []int
	for delta := 0; delta < 2; delta++ {
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		edit(dw)
		before := below.scans
		got, edges := renderScopedRows(dw, files)
		if got != want {
			t.Fatalf("delta %d:\n%s\nwant\n%s", delta, got, want)
		}
		scans = append(scans, below.scans-before)
		for _, e := range edges {
			e.To = "rebound::" + e.To
		}
	}
	if scans[0] == 0 || scans[1] != 0 {
		t.Fatalf("bottom scoped scans per delta = %v; want some on the first and none on the second", scans)
	}
	if hits, _ := cache.StackBaseScopedStats(); hits == 0 {
		t.Fatal("the second delta's scoped read was not served by the stack's cache")
	}
}
