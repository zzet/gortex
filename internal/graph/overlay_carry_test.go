package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCarryBaseEdgesKeepsEditedCallerRelations re-parses a caller whose body
// was edited without touching its calls to Save: the edit adds two lines above
// both calls and drops the call to Load. Through the overlay the caller must
// keep calling Save, provisionally and at the calls' new lines, and stay among
// Save's callers; the dropped call is not carried.
func TestCarryBaseEdgesKeepsEditedCallerRelations(t *testing.T) {
	base := New()
	run := &Node{ID: "a.go::Run", Name: "Run", Kind: KindFunction, FilePath: "a.go", StartLine: 3, EndLine: 8}
	save := &Node{ID: "b.go::Save", Name: "Save", Kind: KindFunction, FilePath: "b.go", StartLine: 1, EndLine: 1}
	load := &Node{ID: "b.go::Load", Name: "Load", Kind: KindFunction, FilePath: "b.go", StartLine: 2, EndLine: 2}
	for _, n := range []*Node{run, save, load} {
		base.AddNode(n)
	}
	base.AddEdge(&Edge{From: run.ID, To: save.ID, Kind: EdgeCalls, FilePath: "a.go", Line: 4})
	base.AddEdge(&Edge{From: run.ID, To: load.ID, Kind: EdgeCalls, FilePath: "a.go", Line: 5})
	base.AddEdge(&Edge{From: run.ID, To: save.ID, Kind: EdgeCalls, FilePath: "a.go", Line: 6})

	layer := NewOverlayLayer()
	layer.MarkFile("a.go", false)
	edited := *run
	edited.EndLine = 9
	layer.AddNode("a.go", &edited)
	reparsed := []*Edge{
		{From: run.ID, To: UnresolvedMarker + "Save", Kind: EdgeCalls, FilePath: "a.go", Line: 8},
		{From: run.ID, To: UnresolvedMarker + "*.Save", Kind: EdgeCalls, FilePath: "a.go", Line: 6},
	}
	for _, e := range reparsed {
		layer.AddEdge(e)
	}
	if got := NewOverlaidView(base, layer).GetInEdges(save.ID); len(got) != 0 {
		t.Fatalf("a covered file still serves its recorded calls: %+v", got)
	}

	carried := CarryBaseEdges(base, "a.go", reparsed)
	for _, e := range carried {
		layer.AddEdge(e)
	}
	view := NewOverlaidView(base, layer)

	var lines []int
	for _, e := range view.GetInEdges(save.ID) {
		if e.From != run.ID || !e.Provisional || e.FilePath != "a.go" {
			t.Fatalf("Save's caller edge = %+v, want a provisional call from Run in a.go", e)
		}
		lines = append(lines, e.Line)
	}
	if len(lines) != 2 || lines[0] != 6 || lines[1] != 8 {
		t.Fatalf("carried Save calls at lines %v, want [6 8]", lines)
	}
	var callees []string
	for _, e := range view.GetOutEdges(run.ID) {
		if !IsUnresolvedTarget(e.To) {
			callees = append(callees, e.To)
		}
	}
	if strings.Join(callees, ",") != save.ID+","+save.ID {
		t.Fatalf("Run's resolved callees = %v, want Save twice and no Load", callees)
	}

	plain := *carried[0]
	plain.Provisional = false
	if plain.IdentityHash() != carried[0].IdentityHash() {
		t.Fatal("Provisional must not change an edge's identity")
	}
	raw, err := json.Marshal(carried[0])
	if err != nil || !strings.Contains(string(raw), `"provisional":true`) {
		t.Fatalf("carried edge JSON = %s, %v; want provisional:true", raw, err)
	}
}
