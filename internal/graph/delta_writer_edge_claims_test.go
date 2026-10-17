package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// edgeClaimFixture is a configuration file three users call into, each user
// with a wide outgoing set of its own (the shape that made an eviction claim
// thousands of sources whole).
type edgeClaimFixture struct {
	nodes []*Node
	edges []*Edge
	// cfgNodes / cfgEdges are cfg.go's rows; incoming the users' edges into it.
	cfgNodes []*Node
	cfgEdges []*Edge
	incoming []*Edge
}

const edgeClaimFanout = 20

func newEdgeClaimFixture() edgeClaimFixture {
	var f edgeClaimFixture
	node := func(id, path string, kind NodeKind) *Node {
		name := id
		if i := strings.LastIndex(id, "::"); i >= 0 {
			name = id[i+2:]
		}
		return &Node{ID: id, Name: name, Kind: kind, FilePath: path, Language: "go", RepoPrefix: ""}
	}
	f.cfgNodes = []*Node{
		node("cfg.go", "cfg.go", KindFile),
		node("cfg.go::Load", "cfg.go", KindFunction),
		node("cfg.go::Opt", "cfg.go", KindType),
	}
	f.nodes = append(f.nodes, f.cfgNodes...)
	f.nodes = append(f.nodes, node("other.go", "other.go", KindFile))
	for k := 0; k < edgeClaimFanout; k++ {
		f.nodes = append(f.nodes, node(fmt.Sprintf("other.go::F%d", k), "other.go", KindFunction))
	}
	f.cfgEdges = []*Edge{
		{From: "cfg.go::Load", To: "other.go::F0", Kind: EdgeCalls, FilePath: "cfg.go", Line: 7},
		{From: "cfg.go", To: "cfg.go::Load", Kind: EdgeDefines, FilePath: "cfg.go", Line: 1},
	}
	f.edges = append(f.edges, f.cfgEdges...)
	for u := 1; u <= 3; u++ {
		path := fmt.Sprintf("u%d.go", u)
		use := path + "::Use"
		f.nodes = append(f.nodes, node(path, path, KindFile), node(use, path, KindFunction))
		f.incoming = append(f.incoming,
			&Edge{From: use, To: "cfg.go::Load", Kind: EdgeCalls, FilePath: path, Line: 3, Origin: "ast_resolved"},
			&Edge{From: use, To: "cfg.go::Load", Kind: EdgeCalls, FilePath: path, Line: 9, Origin: "ast_resolved"},
			&Edge{From: use, To: "cfg.go::Opt", Kind: EdgeReferences, FilePath: path, Line: 4},
			&Edge{From: path, To: "cfg.go", Kind: EdgeImports, FilePath: path, Line: 1},
		)
		for k := 0; k < edgeClaimFanout; k++ {
			f.edges = append(f.edges, &Edge{From: use, To: fmt.Sprintf("other.go::F%d", k), Kind: EdgeCalls, FilePath: path, Line: 10 + k})
		}
	}
	f.edges = append(f.edges, f.incoming...)
	return f
}

func (f edgeClaimFixture) store() *Graph {
	g := New()
	g.AddBatch(cloneDeltaNodes(f.nodes), cloneDeltaEdges(f.edges))
	return g
}

// edgeSetRender renders a set of edges order-independently.
func edgeSetRender(edges []*Edge) string {
	rows := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			rows = append(rows, deltaEdgeRender(e))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// requireSameEdges compares every adjacency read of the delta, through both
// composition paths (the view, and the stack-composed batches), with a store
// that took the same writes.
func requireSameEdges(t *testing.T, label string, dw *DeltaWriter, ref *Graph, ids []string) {
	t.Helper()
	outBatch := dw.GetOutEdgesByNodeIDs(ids)
	inBatch := dw.GetInEdgesByNodeIDs(ids)
	for _, id := range ids {
		want := edgeSetRender(ref.GetOutEdges(id))
		if got := edgeSetRender(dw.GetOutEdges(id)); got != want {
			t.Fatalf("%s: out-edges of %s\n got: %s\nwant: %s", label, id, got, want)
		}
		if got := edgeSetRender(outBatch[id]); got != want {
			t.Fatalf("%s: batched out-edges of %s\n got: %s\nwant: %s", label, id, got, want)
		}
		wantIn := edgeSetRender(ref.GetInEdges(id))
		if got := edgeSetRender(dw.GetInEdges(id)); got != wantIn {
			t.Fatalf("%s: in-edges of %s\n got: %s\nwant: %s", label, id, got, wantIn)
		}
		if got := edgeSetRender(inBatch[id]); got != wantIn {
			t.Fatalf("%s: batched in-edges of %s\n got: %s\nwant: %s", label, id, got, wantIn)
		}
	}
	if got, want := edgeSetRender(dw.AllEdges()), edgeSetRender(ref.AllEdges()); got != want {
		t.Fatalf("%s: all edges differ\n got: %s\nwant: %s", label, got, want)
	}
}

func (f edgeClaimFixture) ids() []string {
	out := make([]string, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n.ID)
	}
	return out
}

// TestDeltaWriterEvictionClaimsOnlyTheIncidentEdges pins the eviction's cost
// and its answer: evicting a file other files call into claims the incident
// edges alone (no source is claimed whole, no source's other edges are read
// into the working graph), the composed view answers exactly what a store
// taking the same writes answers, and a re-statement of the incident edges
// publishes nothing for their sources.
func TestDeltaWriterEvictionClaimsOnlyTheIncidentEdges(t *testing.T) {
	f := newEdgeClaimFixture()
	below := f.store()
	ref := f.store()
	dw := NewDeltaWriter(below, nil)

	dw.EvictFiles([]string{"cfg.go"})
	ref.EvictFile("cfg.go")
	stats := dw.DeltaStats()
	if stats.ClaimedSources != 0 {
		t.Fatalf("eviction claimed %d sources whole; want 0", stats.ClaimedSources)
	}
	if want := len(f.cfgEdges); stats.MaterializedEdges != want {
		t.Fatalf("eviction materialized %d edges; want only cfg.go's %d recorded edges", stats.MaterializedEdges, want)
	}
	// Three users × (calls Load, references Opt, imports cfg.go): the two
	// call rows share one tuple.
	if stats.EdgeClaims != 9 || stats.EdgeClaimSources != 6 {
		t.Fatalf("edge claims = %d over %d sources; want 9 over 6", stats.EdgeClaims, stats.EdgeClaimSources)
	}
	requireSameEdges(t, "after eviction", dw, ref, f.ids())

	restate := append(cloneDeltaEdges(f.cfgEdges), cloneDeltaEdges(f.incoming)...)
	dw.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	ref.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	requireSameEdges(t, "after re-statement", dw, ref, f.ids())
	if got := dw.DeltaStats().ClaimedSources; got != 0 {
		t.Fatalf("the re-statement claimed %d sources whole; want 0", got)
	}

	payload := dw.Payload(map[string]struct{}{"cfg.go": {}})
	if len(payload.EdgeSources) != 0 {
		t.Fatalf("a restated eviction published edge sources %v", payload.EdgeSources)
	}
	if payload.DroppedSources != 6 {
		t.Fatalf("dropped sources = %d; want the 6 edge-claimed sources", payload.DroppedSources)
	}
	for _, e := range payload.Edges {
		if e.FilePath != "cfg.go" {
			t.Fatalf("payload carries %s -> %s recorded at %s; only cfg.go's rows belong in it", e.From, e.To, e.FilePath)
		}
	}
}

// TestDeltaWriterEdgeClaimThatChangesPublishesTheWholeSource: an incident edge
// the engine does not re-state stays removed, in the view and in the payload,
// which then carries the source's whole outgoing set under a marker exactly
// as a source claimed at eviction does.
func TestDeltaWriterEdgeClaimThatChangesPublishesTheWholeSource(t *testing.T) {
	f := newEdgeClaimFixture()
	below := f.store()
	ref := f.store()
	dw := NewDeltaWriter(below, nil)
	dw.EvictFiles([]string{"cfg.go"})
	ref.EvictFile("cfg.go")

	var restate []*Edge
	restate = append(restate, cloneDeltaEdges(f.cfgEdges)...)
	for _, e := range f.incoming {
		// u2 stops referencing Opt; the call at line 9 of u3 moves to line 11.
		if e.From == "u2.go::Use" && e.Kind == EdgeReferences {
			continue
		}
		c := cloneDeltaEdge(e)
		if c.From == "u3.go::Use" && c.Line == 9 {
			c.Line = 11
		}
		restate = append(restate, c)
	}
	dw.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	ref.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	requireSameEdges(t, "after a partial re-statement", dw, ref, f.ids())

	payload := dw.Payload(map[string]struct{}{"cfg.go": {}})
	if got, want := strings.Join(payload.EdgeSources, ","), "u2.go::Use,u3.go::Use"; got != want {
		t.Fatalf("edge sources = %s; want %s", got, want)
	}
	for _, id := range payload.EdgeSources {
		var carried []*Edge
		for _, e := range payload.Edges {
			if e.From == id {
				carried = append(carried, e)
			}
		}
		if got, want := edgeSetRender(carried), edgeSetRender(ref.GetOutEdges(id)); got != want {
			t.Fatalf("payload rows of marked source %s\n got: %s\nwant: %s", id, got, want)
		}
	}
	if payload.DroppedSources != 4 {
		t.Fatalf("dropped sources = %d; want 4 (u1 and the three importers)", payload.DroppedSources)
	}
}

// TestDeltaWriterEdgeClaimThenWholeClaimKeepsTheAnswer: a write that needs an
// edge-claimed source's whole set claims it whole, and the rows under its edge
// claims (the removed and the re-stated) keep their answer.
func TestDeltaWriterEdgeClaimThenWholeClaimKeepsTheAnswer(t *testing.T) {
	f := newEdgeClaimFixture()
	below := f.store()
	ref := f.store()
	dw := NewDeltaWriter(below, nil)
	dw.EvictFiles([]string{"cfg.go"})
	ref.EvictFile("cfg.go")
	// Load is renamed: its callers are not re-stated; Opt's references are.
	renamed := []*Node{f.cfgNodes[0], {ID: "cfg.go::Load2", Name: "Load2", Kind: KindFunction, FilePath: "cfg.go", Language: "go"}, f.cfgNodes[2]}
	var restate []*Edge
	for _, e := range f.incoming {
		if e.To == "cfg.go::Opt" {
			restate = append(restate, cloneDeltaEdge(e))
		}
	}
	dw.AddBatch(cloneDeltaNodes(renamed), cloneDeltaEdges(restate))
	ref.AddBatch(cloneDeltaNodes(renamed), cloneDeltaEdges(restate))
	ids := append(f.ids(), "cfg.go::Load2")
	requireSameEdges(t, "after a rename", dw, ref, ids)

	if !dw.RemoveEdge("u1.go::Use", "other.go::F5", EdgeCalls) || !ref.RemoveEdge("u1.go::Use", "other.go::F5", EdgeCalls) {
		t.Fatal("remove u1 -> F5")
	}
	if got := dw.DeltaStats().ClaimedSources; got != 1 {
		t.Fatalf("claimed sources = %d; want u1 claimed whole", got)
	}
	requireSameEdges(t, "after a whole claim of an edge-claimed source", dw, ref, ids)
	// The old target comes back (an undo): the removed rows stay removed.
	dw.AddBatch([]*Node{cloneDeltaNode(f.cfgNodes[1])}, nil)
	ref.AddBatch([]*Node{cloneDeltaNode(f.cfgNodes[1])}, nil)
	requireSameEdges(t, "after the target returns without its callers", dw, ref, ids)
}

// namesProjectingStore is a bottom store with a name-only read, counting it.
type namesProjectingStore struct {
	*Graph
	nameReads int
	asked     []string
}

func (s *namesProjectingStore) NodeNamesByIDs(ids []string) map[string]string {
	s.nameReads++
	s.asked = append(s.asked, ids...)
	out := make(map[string]string, len(ids))
	for id, n := range s.GetNodesByIDs(ids) {
		if n != nil {
			out[id] = n.Name
		}
	}
	return out
}

// TestDeltaWriterNodeNamesComposeTheStack: the name-only read answers what
// the full composed read answers — an identity a layer covers, carries or hid
// from that layer, every other one from the bottom store's name-only read.
func TestDeltaWriterNodeNamesComposeTheStack(t *testing.T) {
	f := newEdgeClaimFixture()
	base := &namesProjectingStore{Graph: f.store()}
	// A generation below the delta re-emits other.go with F1 renamed.
	layer := NewOverlayLayer()
	layer.MarkFile("other.go", false)
	for _, n := range f.nodes {
		if n.FilePath != "other.go" {
			continue
		}
		c := cloneDeltaNode(n)
		if c.ID == "other.go::F1" {
			c.Name = "F1renamed"
		}
		if c.ID == "other.go::F2" {
			continue // gone in that generation
		}
		layer.AddNode("other.go", c)
	}
	dw := NewDeltaWriter(NewOverlaidView(base, layer), nil)
	dw.EvictFiles([]string{"cfg.go"})
	dw.AddBatch([]*Node{cloneDeltaNode(f.cfgNodes[0]), {ID: "cfg.go::Load2", Name: "Load2", Kind: KindFunction, FilePath: "cfg.go"}}, nil)

	ids := append(f.ids(), "cfg.go::Load2", "missing::X", "")
	want := map[string]string{}
	for id, n := range dw.GetNodesByIDs(ids) {
		if n != nil {
			want[id] = n.Name
		}
	}
	base.nameReads, base.asked = 0, nil
	got := dw.NodeNamesByIDs(ids)
	if len(got) != len(want) {
		t.Fatalf("names = %d ids, full rows = %d ids\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for id, name := range want {
		if got[id] != name {
			t.Fatalf("name of %s = %q; the composed row says %q", id, got[id], name)
		}
	}
	if base.nameReads != 1 {
		t.Fatalf("bottom name-only reads = %d; want one batch", base.nameReads)
	}
	for _, id := range base.asked {
		if strings.HasPrefix(id, "cfg.go") || strings.HasPrefix(id, "other.go") {
			t.Fatalf("the bottom store was asked for %s, which a layer above speaks for", id)
		}
	}
}
