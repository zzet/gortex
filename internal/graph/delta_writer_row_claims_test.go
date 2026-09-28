package graph

import (
	"context"
	"testing"
)

// fixtureRow returns a copy of u1.go::Use's call to other.go::F<k>.
func fixtureRow(f edgeClaimFixture, from, to string) *Edge {
	for _, e := range f.edges {
		if e.From == from && e.To == to {
			return cloneDeltaEdge(e)
		}
	}
	return nil
}

// TestDeltaWriterRetargetClaimsTheRowNotItsSource: the resolver retargets one
// row of a wide foreign source. The delta claims that row's old and new
// identities alone (one row materialized, no source claimed whole), every
// read equals a store taking the same write, and the payload publishes the
// changed source whole; a retarget back publishes nothing.
func TestDeltaWriterRetargetClaimsTheRowNotItsSource(t *testing.T) {
	f := newEdgeClaimFixture()
	dw := NewDeltaWriter(f.store(), nil)
	ref := f.store()
	const from = "u1.go::Use"
	e := fixtureRow(f, from, "other.go::F3")
	e.To = "unresolved::F3"
	dw.ReindexEdges([]EdgeReindex{{Edge: cloneDeltaEdge(e), OldTo: "other.go::F3"}})
	re := fixtureRow(f, from, "other.go::F3")
	for _, stored := range ref.GetOutEdges(from) {
		if stored.To == "other.go::F3" {
			re = stored
		}
	}
	re.To = "unresolved::F3"
	ref.ReindexEdge(re, "other.go::F3")
	st := dw.DeltaStats()
	if st.ClaimedSources != 0 || st.MaterializedEdges != 1 || st.IdentityClaims != 1 {
		t.Fatalf("claimed %d sources whole, materialized %d edges, %d identity claims; want 0, 1, 1", st.ClaimedSources, st.MaterializedEdges, st.IdentityClaims)
	}
	requireSameEdges(t, "after a retarget", dw, ref, append(f.ids(), "unresolved::F3"))
	p := dw.Payload(nil)
	if len(p.EdgeSources) != 1 || p.EdgeSources[0] != from {
		t.Fatalf("edge sources = %v; want the changed source", p.EdgeSources)
	}
	var carried []*Edge
	for _, pe := range p.Edges {
		if pe.From == from {
			carried = append(carried, pe)
		}
	}
	if got, want := edgeSetRender(carried), edgeSetRender(ref.GetOutEdges(from)); got != want {
		t.Fatalf("payload rows of %s\n got: %s\nwant: %s", from, got, want)
	}

	back := NewDeltaWriter(f.store(), nil)
	e2 := fixtureRow(f, from, "other.go::F3")
	e2.To = "unresolved::F3"
	back.ReindexEdges([]EdgeReindex{{Edge: cloneDeltaEdge(e2), OldTo: "other.go::F3"}})
	e2.To = "other.go::F3"
	back.ReindexEdges([]EdgeReindex{{Edge: cloneDeltaEdge(e2), OldTo: "unresolved::F3"}})
	if p := back.Payload(nil); len(p.EdgeSources) != 0 || len(p.Edges) != 0 {
		t.Fatalf("a retarget and its undo published %v / %d edges", p.EdgeSources, len(p.Edges))
	}
	if got := back.DeltaStats().ClaimedSources; got != 0 {
		t.Fatalf("a retarget and its undo claimed %d sources whole", got)
	}
}

// TestDeltaWriterRetargetOntoAnExistingRowClaimsTheSource: a retarget whose
// new identity the view below already holds cannot be expressed by row; the
// source is claimed whole and the answer stays the store's.
func TestDeltaWriterRetargetOntoAnExistingRowClaimsTheSource(t *testing.T) {
	f := newEdgeClaimFixture()
	base := f.store()
	dup := &Edge{From: "u1.go::Use", To: "other.go::F4", Kind: EdgeCalls, FilePath: "u1.go", Line: 13}
	base.AddEdge(cloneDeltaEdge(dup))
	ref := f.store()
	ref.AddEdge(cloneDeltaEdge(dup))
	dw := NewDeltaWriter(base, nil)
	e := fixtureRow(f, "u1.go::Use", "other.go::F3") // line 13
	e.To = "other.go::F4"
	dw.ReindexEdges([]EdgeReindex{{Edge: cloneDeltaEdge(e), OldTo: "other.go::F3"}})
	for _, stored := range ref.GetOutEdges("u1.go::Use") {
		if stored.To == "other.go::F3" {
			stored.To = "other.go::F4"
			ref.ReindexEdge(stored, "other.go::F3")
			break
		}
	}
	if got := dw.DeltaStats().ClaimedSources; got != 1 {
		t.Fatalf("claimed %d sources whole; want the source of the colliding retarget", got)
	}
	requireSameEdges(t, "after a colliding retarget", dw, ref, f.ids())
}

// TestDeltaWriterKindEvictionClaimsTheKindNotTheSource: a derived pass evicts
// one kind out of wide foreign sources and re-adds it. The delta claims the
// kind's rows alone, answers as the store does, and a re-derivation equal to
// what was there publishes nothing; a different one publishes the source.
func TestDeltaWriterKindEvictionClaimsTheKindNotTheSource(t *testing.T) {
	f := newEdgeClaimFixture()
	var capability []*Edge
	for u := 1; u <= 3; u++ {
		from := "u" + string(rune('0'+u)) + ".go::Use"
		capability = append(capability,
			&Edge{From: from, To: "other.go::F1", Kind: EdgeAccessesField, FilePath: "u" + string(rune('0'+u)) + ".go", Line: 40, Origin: "ast_resolved"})
	}
	build := func() *Graph {
		g := f.store()
		g.AddBatch(nil, cloneDeltaEdges(capability))
		return g
	}
	sources := []string{"u1.go::Use", "u2.go::Use", "u3.go::Use"}
	kinds := []EdgeKind{EdgeAccessesField, EdgeReadsEnv}

	dw := NewDeltaWriter(build(), nil)
	ref := build()
	if _, err := dw.EvictEdgesFromSourcesByKinds(context.Background(), sources, kinds); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.EvictEdgesFromSourcesByKinds(context.Background(), sources, kinds); err != nil {
		t.Fatal(err)
	}
	st := dw.DeltaStats()
	if st.ClaimedSources != 0 || st.MaterializedEdges != 0 || st.KindClaimRows != 3 {
		t.Fatalf("claimed %d sources whole, materialized %d, kind rows %d; want 0, 0, 3", st.ClaimedSources, st.MaterializedEdges, st.KindClaimRows)
	}
	requireSameEdges(t, "after the kind eviction", dw, ref, f.ids())
	// Re-derive: u1 and u2 as before, u3 to a different field.
	readd := cloneDeltaEdges(capability)
	readd[2].To = "other.go::F2"
	dw.AddBatch(nil, cloneDeltaEdges(readd))
	ref.AddBatch(nil, cloneDeltaEdges(readd))
	requireSameEdges(t, "after the re-derivation", dw, ref, f.ids())
	if got := dw.DeltaStats().ClaimedSources; got != 0 {
		t.Fatalf("the re-derivation claimed %d sources whole", got)
	}
	p := dw.Payload(nil)
	if len(p.EdgeSources) != 1 || p.EdgeSources[0] != "u3.go::Use" {
		t.Fatalf("edge sources = %v; want only the source whose derivation changed", p.EdgeSources)
	}
}

// TestDeltaWriterProvenanceAndExactRemovalClaimRows: a provenance update and
// an exact removal of foreign rows answer as the store does without claiming
// the sources whole.
func TestDeltaWriterProvenanceAndExactRemovalClaimRows(t *testing.T) {
	f := newEdgeClaimFixture()
	dw := NewDeltaWriter(f.store(), nil)
	ref := f.store()
	e := fixtureRow(f, "u2.go::Use", "other.go::F7")
	dw.SetEdgeProvenance(cloneDeltaEdge(e), "ast_inferred")
	for _, stored := range ref.GetOutEdges("u2.go::Use") {
		if stored.To == "other.go::F7" {
			ref.SetEdgeProvenance(stored, "ast_inferred")
		}
	}
	gone := fixtureRow(f, "u3.go::Use", "other.go::F9")
	if n := dw.RemoveEdgesExact([]*Edge{cloneDeltaEdge(gone)}); n != 1 {
		t.Fatalf("removed %d rows; want 1", n)
	}
	ref.RemoveEdgesExact([]*Edge{cloneDeltaEdge(gone)})
	if got := dw.DeltaStats().ClaimedSources; got != 0 {
		t.Fatalf("claimed %d sources whole", got)
	}
	requireSameEdges(t, "after a provenance update and an exact removal", dw, ref, f.ids())
}

// TestDeltaWriterMoveBetweenSourcesClaimsRows: a dataflow row keyed from a
// placeholder moves to the declaration the reference bound to (a refresh that
// changes the row's source). Both sources keep their other rows below.
func TestDeltaWriterMoveBetweenSourcesClaimsRows(t *testing.T) {
	f := newEdgeClaimFixture()
	flow := &Edge{From: "unresolved::Helper", To: "u2.go::Use", Kind: EdgeValueFlow, FilePath: "u2.go", Line: 50}
	build := func() *Graph {
		g := f.store()
		g.AddBatch(nil, []*Edge{cloneDeltaEdge(flow)})
		return g
	}
	dw := NewDeltaWriter(build(), nil)
	ref := build()
	moved := cloneDeltaEdge(flow)
	moved.From = "u1.go::Use"
	dw.ReindexEdges([]EdgeReindex{{Edge: cloneDeltaEdge(moved), OldFrom: flow.From, OldTo: flow.To,
		RefreshIdentity: true, OldFilePath: flow.FilePath, OldLine: flow.Line}})
	for _, stored := range ref.GetOutEdges(flow.From) {
		stored.From = "u1.go::Use"
		ref.ReindexEdges([]EdgeReindex{{Edge: stored, OldFrom: flow.From, OldTo: flow.To,
			RefreshIdentity: true, OldFilePath: flow.FilePath, OldLine: flow.Line}})
		break
	}
	if got := dw.DeltaStats().ClaimedSources; got != 0 {
		t.Fatalf("a move claimed %d sources whole", got)
	}
	var movedThere bool
	for _, e := range dw.GetOutEdges("u1.go::Use") {
		movedThere = movedThere || e.Kind == EdgeValueFlow
	}
	if !movedThere || len(dw.GetOutEdges(flow.From)) != 0 {
		t.Fatal("the row did not move")
	}
	requireSameEdges(t, "after a move between sources", dw, ref, append(f.ids(), flow.From))
}
