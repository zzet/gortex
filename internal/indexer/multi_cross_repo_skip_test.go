package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type globalCrossRepoCandidateSpy struct {
	graph.Store
	candidateCalls int
	scopedCalls    int
}

func (s *globalCrossRepoCandidateSpy) CrossRepoCandidates(kinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	s.candidateCalls++
	return s.Store.(graph.CrossRepoCandidates).CrossRepoCandidates(kinds)
}

func (s *globalCrossRepoCandidateSpy) CrossRepoCandidatesForRepos(kinds []graph.EdgeKind, prefixes []string) []graph.CrossRepoCandidateRow {
	s.scopedCalls++
	return s.Store.(graph.ScopedCrossRepoCandidates).CrossRepoCandidatesForRepos(kinds, prefixes)
}

func (s *globalCrossRepoCandidateSpy) CrossRepoCandidatesForFiles(kinds []graph.EdgeKind, files []string) []graph.CrossRepoCandidateRow {
	return s.Store.(graph.ScopedCrossRepoCandidates).CrossRepoCandidatesForFiles(kinds, files)
}

func (s *globalCrossRepoCandidateSpy) EvictRepoAllGenerations(prefix string) (int, int) {
	return s.Store.(graph.AllGenerationsRepoEvicter).EvictRepoAllGenerations(prefix)
}

func (s *globalCrossRepoCandidateSpy) MarkEdgesCrossRepo(edges []*graph.Edge) int {
	if marker, ok := s.Store.(graph.CrossRepoFlagMarker); ok {
		return marker.MarkEdgesCrossRepo(edges)
	}
	return 0
}

func newGlobalCrossRepoCandidateStore(t *testing.T, backend string) *globalCrossRepoCandidateSpy {
	t.Helper()
	if backend == "memory" {
		return &globalCrossRepoCandidateSpy{Store: graph.New()}
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return &globalCrossRepoCandidateSpy{Store: store}
}

func assertSingleRepoCrossRepoSkip(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	entries := logs.FilterMessage("global pass: cross-repo edges").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "single_repo", fields["skipped"])
	assert.EqualValues(t, 0, fields["edges"])
	assert.Contains(t, fields, "elapsed")
}

func TestMultiIndexerSingleRepoGlobalPassSkipsCrossRepoCandidates(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, fallback := range []bool{false, true} {
			name := "full_coverage"
			if fallback {
				name = "deferred_fallback"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				store := newGlobalCrossRepoCandidateStore(t, backend)
				core, logs := observer.New(zap.InfoLevel)
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.New(core))
				repo := setupRepoDir(t, "repo-a")
				_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
				require.NoError(t, err)
				require.Equal(t, []string{"repo-a"}, store.RepoPrefixes())
				store.candidateCalls = 0
				logs.TakeAll()
				if fallback {
					mode, complete := mi.resolveDeferredMutations(nil, true, nil, false)
					assert.Equal(t, deferredResolveFallback, mode)
					require.True(t, complete)
					assert.Zero(t, store.candidateCalls)
					// The fallback is not a global pass and keeps its silent call.
					assert.Empty(t, logs.FilterMessage("global pass: cross-repo edges").All())
					return
				}
				mi.runGlobalGraphPasses(context.Background(), nil, false)
				assert.Zero(t, store.candidateCalls)
				assertSingleRepoCrossRepoSkip(t, logs)
			})
		}
	}
}

func TestMultiIndexerMultiRepoGlobalPassKeepsCrossRepoCandidateCount(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := newGlobalCrossRepoCandidateStore(t, backend)
			store.AddBatch([]*graph.Node{
				{ID: "a/a.go::A", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"},
				{ID: "b/b.go::B", Kind: graph.KindFunction, FilePath: "b/b.go", RepoPrefix: "b"},
			}, []*graph.Edge{{From: "a/a.go::A", To: "b/b.go::B", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 1}})
			core, logs := observer.New(zap.InfoLevel)
			mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.New(core))
			mi.runGlobalGraphPasses(context.Background(), nil, false)
			assert.Equal(t, 1, store.candidateCalls)
			require.Len(t, logs.FilterMessage("global pass: cross-repo edges").All(), 1)
			assert.NotContains(t, logs.FilterMessage("global pass: cross-repo edges").All()[0].ContextMap(), "skipped")
			assert.NotNil(t, findEdge(store, graph.EdgeCrossRepoCalls, "a/a.go::A", "b/b.go::B"))
			store.candidateCalls = 0
			mode, complete := mi.resolveDeferredMutations(nil, true, nil, false)
			assert.Equal(t, deferredResolveFallback, mode)
			require.True(t, complete)
			assert.Equal(t, 1, store.candidateCalls)
		})
	}
}

func TestMultiIndexerCrossRepoTrackThenUntrackSkipsLaterFullPass(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := newGlobalCrossRepoCandidateStore(t, backend)
			core, logs := observer.New(zap.InfoLevel)
			mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.New(core))
			ctx := context.Background()
			repoA := setupRepoDir(t, "repo-a")
			repoB := setupRepoDir(t, "repo-b")
			_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: repoA, Name: "repo-a"})
			require.NoError(t, err)
			require.Equal(t, []string{"repo-a"}, store.RepoPrefixes())
			_, err = mi.TrackRepoCtx(ctx, config.RepoEntry{Path: repoB, Name: "repo-b"})
			require.NoError(t, err)
			from, to := "repo-a/main.go::Hello", "repo-b/main.go::Hello"
			require.NotNil(t, store.GetNode(from))
			require.NotNil(t, store.GetNode(to))
			// A resolved cross-repo call arriving with the second repository's
			// enrichment must be materialised by that repository's scoped pass.
			store.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: "repo-a/main.go", Line: 20})
			store.candidateCalls, store.scopedCalls = 0, 0
			mi.runGlobalGraphPasses(ctx, map[string]struct{}{"repo-b": {}}, false)
			assert.Zero(t, store.candidateCalls)
			assert.Equal(t, 1, store.scopedCalls)
			edge := findEdge(store, graph.EdgeCrossRepoCalls, from, to)
			require.NotNil(t, edge)
			assert.True(t, edge.CrossRepo)
			base := findEdge(store, graph.EdgeCalls, from, to)
			require.NotNil(t, base)
			assert.True(t, base.CrossRepo)
			nodesRemoved, edgesRemoved := mi.UntrackRepo("repo-b")
			require.Positive(t, nodesRemoved)
			require.Positive(t, edgesRemoved)
			assert.Nil(t, store.GetNode(to))
			assert.Nil(t, findEdge(store, graph.EdgeCrossRepoCalls, from, to))
			assert.Zero(t, countEdges(store, graph.EdgeCrossRepoCalls))
			require.Equal(t, []string{"repo-a"}, store.RepoPrefixes())
			store.candidateCalls = 0
			logs.TakeAll()
			mi.runGlobalGraphPasses(ctx, nil, false)
			assert.Zero(t, store.candidateCalls)
			assertSingleRepoCrossRepoSkip(t, logs)
		})
	}
}
