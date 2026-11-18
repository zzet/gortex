package graph

import (
	"testing"
)

// countingBelow counts the reads of the rows at a path the payload's
// comparisons make against the view below.
type countingBelow struct {
	*Graph
	fileNodeReads, recordedReads int
}

func (c *countingBelow) GetFileNodes(filePath string) []*Node {
	c.fileNodeReads++
	return c.Graph.GetFileNodes(filePath)
}

// RecordedEdgesAt answers the edges recorded at paths from the graph's
// edges, counting the read.
func (c *countingBelow) RecordedEdgesAt(paths []string) []*Edge {
	c.recordedReads++
	return recordedAt(c.Graph, paths)
}

func recordedAt(g *Graph, paths []string) []*Edge {
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var out []*Edge
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		if _, ok := want[e.FilePath]; ok {
			out = append(out, e)
		}
	}
	return out
}

// mapBelowRows serves the rows at a path from a map and counts its answers.
type mapBelowRows struct {
	nodes    map[string][]*Node
	recorded map[string][]*Edge
	served   int
}

func (m *mapBelowRows) BelowFileRows(path string) ([]*Node, []*Edge, bool) {
	n, ok := m.nodes[path]
	if !ok {
		return nil, nil, false
	}
	m.served++
	return n, m.recorded[path], true
}

// TestDeltaWriterPayloadComparesAgainstInstalledBelowRows: with a source of
// the view below's rows at a path installed, the payload's restated counts
// come from it (the same counts as reading the view below) and the view below
// is not read for that path again.
func TestDeltaWriterPayloadComparesAgainstInstalledBelowRows(t *testing.T) {
	f := newEdgeClaimFixture()
	edit := func(dw *DeltaWriter) {
		dw.EvictFile("cfg.go")
		nodes := make([]*Node, 0, len(f.cfgNodes))
		for _, n := range f.cfgNodes {
			c := *n
			nodes = append(nodes, &c)
		}
		edges := make([]*Edge, 0, len(f.cfgEdges))
		for _, e := range f.cfgEdges {
			c := *e
			if c.Kind == EdgeCalls {
				c.Line++ // the save moves one call
			}
			edges = append(edges, &c)
		}
		dw.AddBatch(nodes, edges)
	}
	fixed := map[string]struct{}{"cfg.go": {}}

	plain := &countingBelow{Graph: f.store()}
	want := NewDeltaWriter(plain, nil)
	edit(want)
	wp := want.Payload(fixed)
	if wp.RestatedNodes == 0 || wp.RestatedEdges == 0 || plain.recordedReads == 0 {
		t.Fatalf("the fixture restates nothing or reads nothing: %d/%d, %d recorded reads", wp.RestatedNodes, wp.RestatedEdges, plain.recordedReads)
	}

	ref := f.store()
	source := &mapBelowRows{
		nodes:    map[string][]*Node{"cfg.go": ref.GetFileNodes("cfg.go")},
		recorded: map[string][]*Edge{"cfg.go": recordedAt(ref, []string{"cfg.go"})},
	}
	below := &countingBelow{Graph: f.store()}
	dw := NewDeltaWriter(below, nil)
	dw.SetBelowFileRows(source)
	edit(dw)
	fileReads, recordedReads := below.fileNodeReads, below.recordedReads
	p := dw.Payload(fixed)
	if p.RestatedNodes != wp.RestatedNodes || p.RestatedEdges != wp.RestatedEdges {
		t.Fatalf("restated %d/%d with the source, %d/%d reading the view below", p.RestatedNodes, p.RestatedEdges, wp.RestatedNodes, wp.RestatedEdges)
	}
	if source.served == 0 || below.recordedReads != recordedReads || below.fileNodeReads != fileReads {
		t.Fatalf("the payload read the view below for cfg.go again: served=%d, recorded reads %d -> %d, file-node reads %d -> %d",
			source.served, recordedReads, below.recordedReads, fileReads, below.fileNodeReads)
	}
	if st := dw.DeltaStats(); st.BelowRowsServed == 0 {
		t.Fatal("the rows served from the source are not counted")
	}
}
