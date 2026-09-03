package query

import (
	"fmt"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

type rankedViewItem struct {
	ID      string
	Payload string
}

func TestMergeRankedSourcesOnlyPrioritizesOverlayForDuplicates(t *testing.T) {
	sources := [][]rankedViewItem{
		{
			{ID: "a-base", Payload: "base"},
			{ID: "shared", Payload: "base"},
			{ID: "d-base", Payload: "base"},
		},
		{
			{ID: "z-overlay", Payload: "overlay"},
			{ID: "b-overlay", Payload: "overlay"},
			{ID: "shared", Payload: "overlay"},
		},
	}

	got := MergeRankedSources(sources, func(item rankedViewItem) string { return item.ID })
	gotIDs := make([]string, 0, len(got))
	for _, item := range got {
		gotIDs = append(gotIDs, item.ID)
		if item.ID == "shared" && item.Payload != "overlay" {
			t.Fatalf("shared payload = %q, want the highest layer", item.Payload)
		}
	}
	wantIDs := []string{"a-base", "z-overlay", "b-overlay", "d-base", "shared"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("merged IDs = %v, want %v", gotIDs, wantIDs)
	}
}

type prefixSearchBackend struct {
	hits      []search.SearchResult
	calls     int
	lastLimit int
}

func (b *prefixSearchBackend) Add(string, ...string) {}
func (b *prefixSearchBackend) Remove(string)         {}
func (b *prefixSearchBackend) Count() int            { return len(b.hits) }
func (b *prefixSearchBackend) Close()                {}

func (b *prefixSearchBackend) Search(_ string, limit int) []search.SearchResult {
	b.calls++
	b.lastLimit = limit
	if limit <= 0 || limit > len(b.hits) {
		limit = len(b.hits)
	}
	return append([]search.SearchResult(nil), b.hits[:limit]...)
}

func TestViewTextCandidatesRefillsBelowMaskedTopK(t *testing.T) {
	allBase := []search.SearchResult{
		{ID: "repo/hidden.go::One"},
		{ID: "repo/hidden.go::Two"},
		{ID: "repo/hidden.go::Three"},
		{ID: "repo/visible-one.go::One"},
		{ID: "repo/visible-two.go::Two"},
		{ID: "repo/visible-three.go::Three"},
	}
	base := &prefixSearchBackend{hits: allBase}
	mask := graph.NewOverlayLayer()
	mask.MarkFile("repo/hidden.go", true)
	engine := &Engine{viewLayers: []ViewLayerSource{{Layer: mask}}}

	got := engine.viewTextCandidates("needle", 3, allBase[:3], func(limit int) []search.SearchResult { return base.Search("needle", limit) })
	gotIDs := make([]string, 0, len(got))
	for _, hit := range got {
		gotIDs = append(gotIDs, hit.ID)
	}
	wantIDs := []string{
		"repo/visible-one.go::One",
		"repo/visible-two.go::Two",
		"repo/visible-three.go::Three",
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("refilled IDs = %v, want %v", gotIDs, wantIDs)
	}
	if base.calls != 1 || base.lastLimit != 6 {
		t.Fatalf("base refill calls=%d last_limit=%d, want one call at width 6", base.calls, base.lastLimit)
	}
}

var benchmarkRankedViewSink []rankedViewItem
var benchmarkViewTextSink []search.SearchResult

func BenchmarkMergeRankedSources(b *testing.B) {
	sources := make([][]rankedViewItem, 4)
	for source := range sources {
		sources[source] = make([]rankedViewItem, 256)
		for rank := range sources[source] {
			id := fmt.Sprintf("source-%d-%04d", source, rank)
			if rank%32 == 0 {
				id = fmt.Sprintf("shared-%04d", rank)
			}
			sources[source][rank] = rankedViewItem{ID: id, Payload: fmt.Sprintf("source-%d", source)}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRankedViewSink = MergeRankedSources(sources, func(item rankedViewItem) string { return item.ID })
	}
}

func BenchmarkViewTextCandidates(b *testing.B) {
	allBase := make([]search.SearchResult, 512)
	for i := range allBase {
		if i < 256 {
			allBase[i] = search.SearchResult{ID: fmt.Sprintf("repo/hidden.go::Hidden%03d", i)}
		} else {
			allBase[i] = search.SearchResult{ID: fmt.Sprintf("repo/visible-%03d.go::Visible", i)}
		}
	}
	overlayHits := make([]search.SearchResult, 32)
	for i := range overlayHits {
		overlayHits[i] = search.SearchResult{ID: fmt.Sprintf("repo/overlay.go::Added%03d", i)}
	}

	base := &prefixSearchBackend{hits: allBase}
	overlay := &prefixSearchBackend{hits: overlayHits}
	mask := graph.NewOverlayLayer()
	mask.MarkFile("repo/hidden.go", true)
	engine := &Engine{viewLayers: []ViewLayerSource{{Search: overlay, Layer: mask}}}
	initial := append([]search.SearchResult(nil), allBase[:256]...)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkViewTextSink = engine.viewTextCandidates("needle", 256, initial, func(limit int) []search.SearchResult { return base.Search("needle", limit) })
		if len(benchmarkViewTextSink) != 256 {
			b.Fatalf("candidate count = %d, want 256", len(benchmarkViewTextSink))
		}
	}
}
