package graph

import (
	"reflect"
	"testing"
)

// A pathless stub whose last references sit in the replaced files is an
// orphan; one still referenced from an untouched file, one the payload still
// references, and a node with a path are not.
func TestOrphanedPathlessStubsNamesOnlyStubsTheLayerLeavesUnreferenced(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "repo/a.go"},
		{ID: "repo/b.go::B", Kind: KindFunction, Name: "B", FilePath: "repo/b.go"},
		{ID: "repo/c.go::C", Kind: KindFunction, Name: "C", FilePath: "repo/c.go"},
		{ID: "repo::builtin::go::len", Kind: KindBuiltin, Name: "len"},
		{ID: "repo::builtin::go::cap", Kind: KindBuiltin, Name: "cap"},
		{ID: "repo::builtin::go::copy", Kind: KindBuiltin, Name: "copy"},
	}, []*Edge{
		{From: "repo/a.go::A", To: "repo::builtin::go::len", Kind: EdgeCalls, FilePath: "repo/a.go", Line: 3},
		{From: "repo/a.go::A", To: "repo::builtin::go::cap", Kind: EdgeCalls, FilePath: "repo/a.go", Line: 4},
		{From: "repo/b.go::B", To: "repo::builtin::go::cap", Kind: EdgeCalls, FilePath: "repo/b.go", Line: 3},
		{From: "repo/a.go::A", To: "repo::builtin::go::copy", Kind: EdgeCalls, FilePath: "repo/a.go", Line: 5},
		{From: "repo/a.go::A", To: "repo/c.go::C", Kind: EdgeCalls, FilePath: "repo/a.go", Line: 6},
	})
	payloadNodes := []*Node{{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "repo/a.go"}}
	payloadEdges := []*Edge{{From: "repo/a.go::A", To: "repo::builtin::go::copy", Kind: EdgeCalls, FilePath: "repo/a.go", Line: 5}}
	got := OrphanedPathlessStubs(g, map[string]struct{}{"repo/a.go": {}}, payloadNodes, payloadEdges)
	if want := []string{"repo::builtin::go::len"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans = %v, want %v", got, want)
	}
	if got := OrphanedPathlessStubs(g, nil, nil, nil); got != nil {
		t.Fatalf("no covered path, orphans = %v", got)
	}
}
