package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// cancelOnFirstEdgeRead ends the request the moment the bounded build asks for
// its first adjacency batch, and counts every batch read that follows.
type cancelOnFirstEdgeRead struct {
	graph.Store
	cancel        context.CancelFunc
	edgeReads     int
	readsAfterEnd int
	requestEnded  bool
}

func (r *cancelOnFirstEdgeRead) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	r.edgeReads++
	if r.requestEnded {
		r.readsAfterEnd++
	}
	out := r.Store.GetOutEdgesByNodeIDs(ids)
	r.cancel()
	r.requestEnded = true
	return out
}

func (r *cancelOnFirstEdgeRead) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if r.requestEnded {
		r.readsAfterEnd++
	}
	return r.Store.GetNodesByIDs(ids)
}

// An abandoned request's bounded adjacency build stops reading at the
// request's end and leaves nothing in the shared walk cache. The fixture is a
// two-hop chain, so an unbounded build reads a second level after the first.
func TestBoundedCentralityStopsReadingWhenTheRequestEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelOnFirstEdgeRead{Store: walkTestGraph(t), cancel: cancel}
	srv := &Server{graph: reader, pprCache: newPPRWalkCache()}

	got := srv.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A"})

	assert.Empty(t, got.Scores, "a request that ended mid-build is answered with nothing")
	assert.Equal(t, 1, reader.edgeReads, "no adjacency batch may be read after the request ended")
	assert.Zero(t, reader.readsAfterEnd, "no node or edge batch may be read after the request ended")
	_, _, size, _, _ := srv.pprCache.stats()
	assert.Zero(t, size, "a partial snapshot's walk must not reach the shared cache")
}

// A request that is still live gets exactly the answer it got before the
// build read through the request-bound reader — including on the SQLite
// store, where the bound reader switches to the store's cancellable batch
// reads.
func TestBoundedCentralityThroughALiveRequestMatchesTheUnboundBuild(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "centrality.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	for _, id := range []string{"a.go::A", "b.go::B", "c.go::C", "d.go::D"} {
		s.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id[len(id)-1:], FilePath: id[:4], Language: "go"})
	}
	s.AddEdge(&graph.Edge{From: "a.go::A", To: "b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})
	s.AddEdge(&graph.Edge{From: "a.go::A", To: "d.go::D", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 3})
	s.AddEdge(&graph.Edge{From: "b.go::B", To: "c.go::C", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2})
	s.AddEdge(&graph.Edge{From: "d.go::D", To: "c.go::C", Kind: graph.EdgeCalls, FilePath: "d.go", Line: 4})

	seeds := []string{"a.go::A"}
	candidates := []string{"a.go::A", "d.go::D"}
	unbound := (&Server{graph: s}).boundedCentralityForRequest(context.Background(), seeds, candidates)
	require.NotEmpty(t, unbound.Scores)

	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound := (&Server{graph: s}).boundedCentralityForRequest(live, seeds, candidates)
	assert.Equal(t, unbound, bound, "the request-bound reader must not change a live request's answer")
}
