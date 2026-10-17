package graph

import (
	"strings"
	"testing"
)

// adjacencyReadCounter counts the bottom store's adjacency batches.
type adjacencyReadCounter struct {
	*Graph
	reads int
}

func (c *adjacencyReadCounter) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	c.reads++
	return c.Graph.GetOutEdgesByNodeIDs(ids)
}

func (c *adjacencyReadCounter) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	c.reads++
	return c.Graph.GetInEdgesByNodeIDs(ids)
}

// A delta's adjacency reads take the bottom store's rows from the stack's
// cache: every delta's answer equals the uncached composition's (the delta's
// own eviction hides the rows of a file it re-derives), and the second delta
// over the stack answers them without reading the bottom store's adjacency.
func TestDeltaWriterBottomAdjacencyIsKeptPerStack(t *testing.T) {
	f := newEdgeClaimFixture()
	ids := []string{"u1.go::Use", "u2.go::Use", "cfg.go::Load"}
	edit := func(dw *DeltaWriter) {
		dw.EvictFiles([]string{"u2.go"})
	}
	render := func(dw *DeltaWriter) string {
		var rows []string
		for id, edges := range dw.GetOutEdgesByNodeIDs(ids) {
			rows = append(rows, id+":"+edgeSetRender(edges))
		}
		for id, edges := range dw.GetInEdgesByNodeIDs(ids) {
			rows = append(rows, "in "+id+":"+edgeSetRender(edges))
		}
		sortStrings(rows)
		return strings.Join(rows, "\n")
	}
	plain := NewDeltaWriter(f.store(), nil)
	edit(plain)
	want := render(plain)
	if strings.Contains(want, "u2.go::Use:") || !strings.Contains(want, "u1.go::Use:") {
		t.Fatalf("fixture precondition:\n%s", want)
	}
	below := &adjacencyReadCounter{Graph: f.store()}
	cache := NewBaseProjectionCache()
	var reads []int
	for delta := 0; delta < 2; delta++ {
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		edit(dw)
		before := below.reads
		if got := render(dw); got != want {
			t.Fatalf("delta %d:\n%s\nwant\n%s", delta, got, want)
		}
		reads = append(reads, below.reads-before)
	}
	if reads[0] == 0 || reads[1] != 0 {
		t.Fatalf("bottom adjacency reads per delta = %v; want some on the first and none on the second", reads)
	}
}

func sortStrings(rows []string) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j] < rows[j-1]; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// copyingAdjacency answers the bottom store's adjacency with fresh rows, as a
// database-backed store does: a caller that changes a row it read changes
// only its copy.
type copyingAdjacency struct{ *Graph }

func copyEdgeRows(in map[string][]*Edge) map[string][]*Edge {
	out := make(map[string][]*Edge, len(in))
	for id, edges := range in {
		for _, e := range edges {
			c := *e
			if e.Meta != nil {
				c.Meta = make(map[string]any, len(e.Meta))
				for k, v := range e.Meta {
					c.Meta[k] = v
				}
			}
			out[id] = append(out[id], &c)
		}
	}
	return out
}

func (c copyingAdjacency) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return copyEdgeRows(c.Graph.GetOutEdgesByNodeIDs(ids))
}

func (c copyingAdjacency) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return copyEdgeRows(c.Graph.GetInEdgesByNodeIDs(ids))
}

// A pass rebinds rows it read in place (a resolved target, a via stamp). A row
// the stack's cache keeps is the bottom store's, not the pass's: a later
// delta over the same stack reads it as the store holds it.
func TestDeltaWriterBottomAdjacencyIsNotChangedByAPassRebindingARow(t *testing.T) {
	f := newEdgeClaimFixture()
	below := copyingAdjacency{Graph: f.store()}
	cache := NewBaseProjectionCache()
	id := "u1.go::Use"
	first := NewDeltaWriter(below, nil)
	first.SetBaseProjectionCache(cache)
	first.EvictFiles([]string{"u2.go"})
	rows := first.GetOutEdgesByNodeIDs([]string{id})[id]
	if len(rows) == 0 {
		t.Fatal("fixture: no row out of u1.go::Use")
	}
	want := edgeSetRender(rows)
	for _, e := range rows {
		e.To = "rebound::" + e.To
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["via"] = "rebound"
	}
	second := NewDeltaWriter(below, nil)
	second.SetBaseProjectionCache(cache)
	second.EvictFiles([]string{"u2.go"})
	if got := edgeSetRender(second.GetOutEdgesByNodeIDs([]string{id})[id]); got != want {
		t.Fatalf("the next delta read the rebound rows:\n%s\nwant\n%s", got, want)
	}
	for _, e := range second.GetOutEdgesByNodeIDs([]string{id})[id] {
		if e.Meta["via"] == "rebound" {
			t.Fatal("the next delta read a row's rebound metadata")
		}
	}
}
