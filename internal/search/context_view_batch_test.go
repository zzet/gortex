package search

import (
	"context"
	"testing"
)

type sequentialOnlyBackend struct{ searches int }

func (b *sequentialOnlyBackend) Add(string, ...string) {}
func (b *sequentialOnlyBackend) Remove(string)         {}
func (b *sequentialOnlyBackend) Search(string, int) []SearchResult {
	b.searches++
	return []SearchResult{{ID: "sequential"}}
}
func (b *sequentialOnlyBackend) Count() int { return 0 }
func (b *sequentialOnlyBackend) Close()     {}
func (b *sequentialOnlyBackend) SearchContext(context.Context, string, int) []SearchResult {
	b.searches++
	return []SearchResult{{ID: "sequential-context"}}
}

func TestHybridAndSwappableKeepSequentialViewFallback(t *testing.T) {
	hybridInner := &sequentialOnlyBackend{}
	hybrid := &HybridBackend{text: hybridInner}
	if _, ok := Backend(hybrid).(ContextViewBatchSearcherBackend); ok {
		t.Fatal("HybridBackend unexpectedly exposes view batching")
	}
	if got := hybrid.SearchContext(context.Background(), "query", 2); len(got) != 1 || got[0].ID != "sequential-context" {
		t.Fatalf("hybrid sequential result = %#v", got)
	}
	if hybridInner.searches != 1 {
		t.Fatalf("hybrid sequential calls = %d, want 1", hybridInner.searches)
	}

	swappableInner := &sequentialOnlyBackend{}
	swappable := &Swappable{inner: swappableInner}
	if _, ok := Backend(swappable).(ContextViewBatchSearcherBackend); ok {
		t.Fatal("Swappable unexpectedly exposes view batching")
	}
	if got := swappable.SearchContext(context.Background(), "query", 2); len(got) != 1 || got[0].ID != "sequential-context" {
		t.Fatalf("swappable sequential result = %#v", got)
	}
	if swappableInner.searches != 1 {
		t.Fatalf("swappable sequential calls = %d, want 1", swappableInner.searches)
	}
}
