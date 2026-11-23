package resolver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type refusingReindexStore struct {
	*store_sqlite.Store
	refuseNext bool
	refused    int
}

func (s *refusingReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *refusingReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	if s.refuseNext {
		s.refuseNext = false
		s.refused++
		return false
	}
	return s.Store.ReindexEdgesApplied(batch)
}

func openRefusingReindexStore(t *testing.T) *refusingReindexStore {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return &refusingReindexStore{Store: s, refuseNext: true}
}

func TestResolveAllContextReturnsErrorOnRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "target.ts", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2,
	})

	r := New(store)
	computeDone := false
	r.OnComputeDone = func() { computeDone = true }

	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.False(t, computeDone, "compute readiness must not publish after a refused page write")
	edges := store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::helper", edges[0].To)

	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	assert.True(t, computeDone)
	edges = store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "target.ts::helper", edges[0].To)
}

func TestResolveFilesAndIncomingFlagsRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "target.ts", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2,
	})

	r := New(store)

	stats := r.ResolveFilesAndIncoming([]string{"caller.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	edges := store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::helper", edges[0].To)

	stats = r.ResolveFilesAndIncoming([]string{"caller.ts"})
	assert.False(t, stats.WriteRefused)
	edges = store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "target.ts::helper", edges[0].To)
}

func TestCrossRepoResolveAllContextReturnsErrorOnRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "a/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "b/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "a/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "b/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "a/caller.ts", To: "b/target.ts",
		Kind: graph.EdgeImports, FilePath: "a/caller.ts", Line: 1, CrossRepo: true,
	})
	store.AddEdge(&graph.Edge{
		From: "a/caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 2,
	})

	cr := NewCrossRepo(store)

	stats, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	for _, e := range store.GetOutEdges("a/caller.ts::run") {
		assert.NotEqual(t, graph.EdgeCrossRepoCalls, e.Kind, "derived cross-repo parallel must not publish after a refused write")
	}

	stats, err = cr.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	var sawBound bool
	for _, e := range store.GetOutEdges("a/caller.ts::run") {
		if e.Kind == graph.EdgeCalls && e.To == "b/target.ts::helper" {
			sawBound = true
		}
	}
	assert.True(t, sawBound)
}
