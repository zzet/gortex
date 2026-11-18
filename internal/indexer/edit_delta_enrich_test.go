package indexer

import (
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// An edge the enrichment stage writes after the delta's claims, recorded at a
// path the delta does not replace and out of a source it does not mark, would
// be served next to the view below's copy of the same identity. The settle
// step removes exactly those restatements and keeps everything the delta
// owns and every genuinely new edge.
func TestEditDeltaEnrichmentCannotShadowARowBelow(t *testing.T) {
	store := builderOpenStore(t, "enrich-settle")
	const (
		edited = "repo/a.go"
		other  = "repo/b.go"
		marked = "repo/c.go::Marked"
	)
	below := []*graph.Edge{
		{From: "repo/b.go::B", To: "repo/d.go::D", Kind: graph.EdgeCalls, FilePath: other, Line: 3},
		{From: marked, To: "repo/d.go::D", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 4},
	}
	store.AddBatch([]*graph.Node{
		{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: other, RepoPrefix: "repo"},
		{ID: marked, Kind: graph.KindFunction, Name: "Marked", FilePath: "repo/c.go", RepoPrefix: "repo"},
		{ID: "repo/d.go::D", Kind: graph.KindFunction, Name: "D", FilePath: "repo/d.go", RepoPrefix: "repo"},
	}, below)

	handle := builderContextDerivedHandle(t, store)
	written := []*graph.Edge{
		// Owned by the replaced path: kept.
		{From: "repo/a.go::A", To: "repo/d.go::D", Kind: graph.EdgeCalls, FilePath: edited, Line: 1},
		// A restatement of the row below at an unowned path: removed.
		{From: "repo/b.go::B", To: "repo/d.go::D", Kind: graph.EdgeCalls, FilePath: other, Line: 3},
		// A new edge at an unowned path: kept.
		{From: "repo/b.go::B", To: "repo/d.go::D", Kind: graph.EdgeImplements, FilePath: other, Line: 3},
		// Out of a marked source: the generation owns its whole set, kept.
		{From: marked, To: "repo/d.go::D", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 4},
	}
	handle.AddBatch(nil, written)

	own := editDeltaOwnership{
		paths:   map[string]struct{}{edited: {}},
		sources: map[string]struct{}{marked: {}},
	}
	if removed := editDeltaSettleEnrichment(handle, store.AtGeneration(0), own); removed != 1 {
		t.Fatalf("settle removed %d rows, want the 1 restatement", removed)
	}
	var got []string
	for _, e := range handle.AllEdgesLight() {
		got = append(got, e.From+" "+string(e.Kind)+" "+e.FilePath)
	}
	sort.Strings(got)
	want := []string{
		"repo/a.go::A calls repo/a.go",
		"repo/b.go::B implements repo/b.go",
		"repo/c.go::Marked calls repo/c.go",
	}
	if len(got) != len(want) {
		t.Fatalf("generation keeps %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("generation keeps %v, want %v", got, want)
		}
	}
	if removed := editDeltaSettleEnrichment(handle, store.AtGeneration(0), own); removed != 0 {
		t.Fatalf("a second settle removed %d rows", removed)
	}
}
