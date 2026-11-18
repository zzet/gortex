package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type blockingSearchSubstringReader struct {
	graph.Reader
	started     chan struct{}
	contextRuns atomic.Int32
	legacyRuns  atomic.Int32
}

func (r *blockingSearchSubstringReader) GetNodesByIDs([]string) map[string]*graph.Node {
	return map[string]*graph.Node{}
}

func (*blockingSearchSubstringReader) FindNodesByNameContext(context.Context, string) ([]*graph.Node, error) {
	return nil, nil
}

func (r *blockingSearchSubstringReader) FindNodesByName(string) []*graph.Node {
	r.legacyRuns.Add(1)
	return nil
}

func (r *blockingSearchSubstringReader) FindNodesByNameContaining(string, int) []*graph.Node {
	r.legacyRuns.Add(1)
	return nil
}

func (r *blockingSearchSubstringReader) FindNodesByNameContainingContext(ctx context.Context, _ string, _ int) ([]*graph.Node, error) {
	r.contextRuns.Add(1)
	close(r.started)
	<-ctx.Done()
	return []*graph.Node{{ID: "partial", Name: "partial", Kind: graph.KindFunction}}, ctx.Err()
}

func TestSearchSymbolsCancellationStopsSubstringGraphLookup(t *testing.T) {
	base := graph.New()
	reader := &blockingSearchSubstringReader{started: make(chan struct{})}
	eng := query.NewEngine(base).WithReader(reader)
	eng.SetSearch(&searchSymbolsContextBackend{bundles: []search.SymbolBundle{}})
	eng.SetRerank(nil)
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := callSearchSymbolsContext(srv, ctx, searchSymbolsContextRequest("substringOnly"))
	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("search_symbols did not enter the context-aware substring lookup")
	}
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result != nil {
			t.Fatalf("canceled substring search = result %#v, error %v; want nil/context.Canceled", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search_symbols did not return after substring cancellation")
	}
	if reader.contextRuns.Load() != 1 || reader.legacyRuns.Load() != 0 {
		t.Fatalf("substring calls context/legacy = %d/%d, want 1/0", reader.contextRuns.Load(), reader.legacyRuns.Load())
	}
}
