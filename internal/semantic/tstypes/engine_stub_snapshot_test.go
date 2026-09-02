package tstypes

// Unit pins for buildIndex's stub-snapshot admission (issue #729 items
// 2 and 3): the calls-facts gate that keeps the snapshot out of the
// phases that never read it, and the Line == 0 skip that keeps lineless
// edges out of a line-keyed map.

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// stubSnapshotApplier builds a one-node, one-edge graph and an applier
// with its adjacency preloaded, returning the applier and the owner
// node's span so each pin can shape it.
func stubSnapshotApplier(t *testing.T, startLine, endLine, edgeLine int) *applier {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "X.cs::App.Tick", Name: "Tick", Kind: graph.KindField,
		FilePath: "X.cs", StartLine: startLine, EndLine: endLine,
	})
	g.AddEdge(&graph.Edge{
		From: "X.cs::App.Tick", To: "unresolved::*.Run",
		Kind: graph.EdgeCalls, FilePath: "X.cs", Line: edgeLine,
	})
	return newApplier(g, CSharpSpec(), "csharp-types")
}

// stubsByLine is read by applyCall alone, so buildIndex snapshots it
// only for files whose facts carry calls — every other phase/file
// combination built a map nothing could ever read.
func TestBuildIndex_SnapshotsStubsOnlyForFilesWithCallFacts(t *testing.T) {
	withCalls := &fileFacts{file: "X.cs", calls: []callFact{{line: 4, method: "Run"}}}
	a := stubSnapshotApplier(t, 3, 5, 4)
	a.preload([]*fileFacts{withCalls})
	if idx := a.buildIndex(withCalls); len(idx.stubsByLine[4]) != 1 {
		t.Errorf("calls-carrying file lost its stub snapshot: %v", idx.stubsByLine)
	}

	withoutCalls := &fileFacts{file: "X.cs"}
	a = stubSnapshotApplier(t, 3, 5, 4)
	a.preload([]*fileFacts{withoutCalls})
	if idx := a.buildIndex(withoutCalls); len(idx.stubsByLine) != 0 {
		t.Errorf("file with no call facts built a stub snapshot nothing reads: %v", idx.stubsByLine)
	}
}

// The Line == 0 skip: an edge with no line evidence must not enter the
// line-keyed snapshot. For any owner with a real span the extent guard
// below it also rejects Line 0, but an owner whose span was never set
// (StartLine == EndLine == 0) admits it — the skip is what keeps a
// lineless edge from becoming an owner claim at "line 0".
func TestBuildIndex_LinelessStubNotAdmitted(t *testing.T) {
	facts := &fileFacts{file: "X.cs", calls: []callFact{{line: 0, method: "Run"}}}
	a := stubSnapshotApplier(t, 0, 0, 0)
	a.preload([]*fileFacts{facts})
	if idx := a.buildIndex(facts); len(idx.stubsByLine) != 0 {
		t.Errorf("lineless calls-edge admitted to the stub snapshot: %v", idx.stubsByLine)
	}
}
