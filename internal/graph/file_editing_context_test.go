package graph

import (
	"fmt"
	"testing"
)

func TestFileEditingContextOrdersNeighborsBeforeSampleCap(t *testing.T) {
	g := New()
	const file = "repo/focus.go"
	const focus = file + "::Focus"
	g.AddNode(&Node{ID: file, Kind: KindFile, FilePath: file})
	g.AddNode(&Node{ID: focus, Kind: KindFunction, FilePath: file})
	// Insert in reverse order; neither insertion order nor an ID-map walk
	// should choose the first twenty callers/callees a tool displays.
	for i := 24; i >= 0; i-- {
		caller := fmt.Sprintf("repo/caller-%02d.go::Call", i)
		callee := fmt.Sprintf("repo/callee-%02d.go::Call", i)
		g.AddNode(&Node{ID: caller, Kind: KindFunction, FilePath: fmt.Sprintf("repo/caller-%02d.go", i)})
		g.AddNode(&Node{ID: callee, Kind: KindFunction, FilePath: fmt.Sprintf("repo/callee-%02d.go", i)})
		g.AddEdge(&Edge{From: caller, To: focus, Kind: EdgeCalls})
		g.AddEdge(&Edge{From: focus, To: callee, Kind: EdgeCalls})
	}
	for repeat := 0; repeat < 8; repeat++ {
		got := FileEditingContextOf(g, file, []NodeKind{KindFunction})
		if got == nil || len(got.CalledBy) != 25 || len(got.Calls) != 25 {
			t.Fatalf("neighbor projection = %+v", got)
		}
		for i := range 25 {
			wantCaller := fmt.Sprintf("repo/caller-%02d.go::Call", i)
			wantCallee := fmt.Sprintf("repo/callee-%02d.go::Call", i)
			if got.CalledBy[i].ID != wantCaller || got.Calls[i].ID != wantCallee {
				t.Fatalf("repeat %d position %d: caller=%q callee=%q, want %q/%q", repeat, i, got.CalledBy[i].ID, got.Calls[i].ID, wantCaller, wantCallee)
			}
		}
	}
}
