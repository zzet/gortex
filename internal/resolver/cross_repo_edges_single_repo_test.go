package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type crossRepoCandidateSpy struct {
	graph.Store
	candidateCalls int
	prefixes       []string
}

func (s *crossRepoCandidateSpy) RepoPrefixes() []string {
	if s.prefixes != nil {
		return s.prefixes
	}
	return s.Store.RepoPrefixes()
}

func (s *crossRepoCandidateSpy) CrossRepoCandidates(kinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	s.candidateCalls++
	return s.Store.(graph.CrossRepoCandidates).CrossRepoCandidates(kinds)
}

func (s *crossRepoCandidateSpy) MarkEdgesCrossRepo(edges []*graph.Edge) int {
	if marker, ok := s.Store.(graph.CrossRepoFlagMarker); ok {
		return marker.MarkEdgesCrossRepo(edges)
	}
	return 0
}

func newCrossRepoCandidateStore(t *testing.T, backend string) *crossRepoCandidateSpy {
	t.Helper()
	if backend == "memory" {
		return &crossRepoCandidateSpy{Store: graph.New()}
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return &crossRepoCandidateSpy{Store: store}
}

func TestDetectCrossRepoEdgesSingleRepoSkipsCandidates(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, unstamped := range []bool{false, true} {
			name := "stamped"
			if unstamped {
				name = "with_empty_prefix"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				store := newCrossRepoCandidateStore(t, backend)
				store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
				prefix := "repo"
				if unstamped {
					prefix = ""
				}
				store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: prefix})
				store.AddEdge(&graph.Edge{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1})
				edges, skipped := DetectCrossRepoEdgesWithSkip(store)
				assert.Zero(t, edges)
				assert.True(t, skipped)
				assert.Zero(t, store.candidateCalls)
				require.Len(t, store.AllEdges(), 1)
				assert.False(t, store.AllEdges()[0].CrossRepo)
			})
		}
		t.Run(backend+"/duplicate_prefixes", func(t *testing.T) {
			store := newCrossRepoCandidateStore(t, backend)
			store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
			store.prefixes = []string{"", "repo", "repo", ""}
			assert.Zero(t, DetectCrossRepoEdges(store))
			assert.Zero(t, store.candidateCalls)
		})
		t.Run(backend+"/empty", func(t *testing.T) {
			store := newCrossRepoCandidateStore(t, backend)
			assert.Zero(t, DetectCrossRepoEdges(store))
			assert.Zero(t, store.candidateCalls)
		})
	}
}

func TestDetectCrossRepoEdgesSecondPrefixFileRunsCandidates(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := newCrossRepoCandidateStore(t, backend)
			store.AddNode(&graph.Node{ID: "a/a.go::A", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"})
			store.AddNode(&graph.Node{ID: "b/b.go", Kind: graph.KindFile, FilePath: "b/b.go", RepoPrefix: "b"})
			assert.Zero(t, DetectCrossRepoEdges(store))
			assert.Equal(t, 1, store.candidateCalls)
		})
	}
}

func TestDetectCrossRepoEdgesSingleRepoIgnoresOtherGeneration(t *testing.T) {
	store := newCrossRepoCandidateStore(t, "sqlite")
	sqlite := store.Store.(*store_sqlite.Store)
	sqlite.AddNode(&graph.Node{ID: "old/b.go", Kind: graph.KindFile, FilePath: "old/b.go", RepoPrefix: "old"})
	store.Store = sqlite.AtGeneration(1)
	store.AddNode(&graph.Node{ID: "current/a.go::A", Kind: graph.KindFunction, FilePath: "current/a.go", RepoPrefix: "current"})
	require.Equal(t, []string{"current"}, store.RepoPrefixes())
	assert.Zero(t, DetectCrossRepoEdges(store))
	assert.Zero(t, store.candidateCalls)
}

func seedCrossRepoDifferentialFixture(store graph.Store) {
	store.AddBatch([]*graph.Node{
		{ID: "a/a.go::Caller", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go::Callee", Kind: graph.KindFunction, FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "a/a.go::Impl", Kind: graph.KindType, FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go::Iface", Kind: graph.KindInterface, FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "a/a.go::Child", Kind: graph.KindType, FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go::Parent", Kind: graph.KindType, FilePath: "b/b.go", RepoPrefix: "b"},
	}, []*graph.Edge{
		{From: "a/a.go::Caller", To: "b/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 1},
		{From: "a/a.go::Impl", To: "b/b.go::Iface", Kind: graph.EdgeImplements, FilePath: "a/a.go", Line: 2},
		{From: "a/a.go::Child", To: "b/b.go::Parent", Kind: graph.EdgeExtends, FilePath: "a/a.go", Line: 3},
		{From: "a/a.go::Caller", To: "a/a.go::Impl", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 4},
	})
}

func TestDetectCrossRepoEdgesMultiRepoMatchesUnguardedPass(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			guarded := newCrossRepoCandidateStore(t, backend)
			baseline := newCrossRepoCandidateStore(t, backend)
			seedCrossRepoDifferentialFixture(guarded)
			seedCrossRepoDifferentialFixture(baseline)
			// The parent commit's complete implementation, without the prefix guard.
			want := materializeCrossRepoCandidates(baseline, crossRepoCandidates(baseline))
			require.Equal(t, 3, want)
			edges, skipped := DetectCrossRepoEdgesWithSkip(guarded)
			assert.Equal(t, want, edges)
			assert.False(t, skipped)
			require.Len(t, guarded.AllEdges(), 7)
			// Includes every identity field (from, to, kind, file, line) and CrossRepo,
			// on both the base and parallel edges, not only the emitted count.
			assert.ElementsMatch(t, baseline.AllEdges(), guarded.AllEdges())
			assert.Equal(t, 1, baseline.candidateCalls)
			assert.Equal(t, baseline.candidateCalls, guarded.candidateCalls)
			assert.Equal(t, want, DetectCrossRepoEdges(guarded))
			assert.Equal(t, 2, guarded.candidateCalls)
			assert.ElementsMatch(t, baseline.AllEdges(), guarded.AllEdges())
		})
	}
}
