package query

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// countingBaseBackend counts every base-corpus lane the engine could ask.
type countingBaseBackend struct {
	inner *search.SymbolSearcherBackend
	calls int
}

func (b *countingBaseBackend) Add(id string, fields ...string) { b.inner.Add(id, fields...) }
func (b *countingBaseBackend) Remove(id string)                { b.inner.Remove(id) }
func (b *countingBaseBackend) Search(q string, limit int) []search.SearchResult {
	b.calls++
	return b.inner.Search(q, limit)
}
func (b *countingBaseBackend) Count() int { return b.inner.Count() }
func (b *countingBaseBackend) Close()     {}
func (b *countingBaseBackend) SearchSymbolBundles(q string, limit int) []search.SymbolBundle {
	b.calls++
	return b.inner.SearchSymbolBundles(q, limit)
}

// TestComposedViewWithoutTheBaseCorpusNeverAsksIt pins the generation-zero
// skip: a stack that does not compose the indexed corpus enumerates its own
// generations only, and still finds what they hold.
func TestComposedViewWithoutTheBaseCorpusNeverAsksIt(t *testing.T) {
	stack := newViewStack(t)
	backend := &countingBaseBackend{inner: search.NewSymbolSearcherBackend(stack.store)}
	base := NewEngine(stack.store)
	base.SetSearch(backend)
	base.SetRerank(nil)

	withBase := candidateIDs(base.WithComposedView(stack.reader, stack.layers, context.Background(), false).
		GatherSymbolCandidates(viewProseQuery, 20, QueryOptions{}, nil))
	if backend.calls == 0 || !containsID(withBase, viewStayerID) {
		t.Fatalf("the composing view never asked the base corpus (calls=%d, got %v); the fixture proves nothing", backend.calls, withBase)
	}

	backend.calls = 0
	got := candidateIDs(base.WithComposedView(stack.reader, stack.layers, context.Background(), true).
		GatherSymbolCandidates(viewProseQuery, 20, QueryOptions{}, nil))
	if backend.calls != 0 {
		t.Fatalf("a view that does not compose the base corpus asked it %d times", backend.calls)
	}
	for _, want := range []string{viewDirtyID, viewFreshID} {
		if !containsID(got, want) {
			t.Fatalf("the stack's own symbol %s is missing; got %v", want, got)
		}
	}

	// Without layers there is nothing else to ask: the flag is inert.
	if e := base.WithComposedView(stack.reader, nil, context.Background(), true); e.viewExcludesBase {
		t.Fatal("a layerless clone excluded the base corpus")
	}
	if e := base.WithComposedView(stack.reader, stack.layers, context.Background(), true).WithReader(stack.store); e.viewExcludesBase {
		t.Fatal("a reader swap kept the base-corpus exclusion")
	}
}
