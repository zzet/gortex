package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type crossRepoScopeCountingStore struct {
	graph.Store
	scans int
}

func (s *crossRepoScopeCountingStore) CrossRepoCandidatesForRepos(kinds []graph.EdgeKind, repos []string) []graph.CrossRepoCandidateRow {
	s.scans++
	return s.Store.(graph.ScopedCrossRepoCandidates).CrossRepoCandidatesForRepos(kinds, repos)
}

func (s *crossRepoScopeCountingStore) CrossRepoCandidatesForFiles(kinds []graph.EdgeKind, files []string) []graph.CrossRepoCandidateRow {
	return s.Store.(graph.ScopedCrossRepoCandidates).CrossRepoCandidatesForFiles(kinds, files)
}

// Given a call bound across repositories while tracking, when the caller's
// publication tail runs, then it does not rescan every relationship incident
// to the repository: the pass that bound the call already gave it a parallel.
func TestGoModulePublishTailSkipsRepositoryParallelScan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &crossRepoScopeCountingStore{Store: db}
	provider, consumer := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(provider, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(provider, "lib.go"), "package library\nfunc Hello() int { return 1 }\n")
	writeFile(t, filepath.Join(consumer, "go.mod"), "module example.org/app\n")
	writeFile(t, filepath.Join(consumer, "main.go"), "package app\nimport library \"example.org/library\"\nfunc Caller() int { return library.Hello() }\n")
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, entry := range []config.RepoEntry{{Path: provider, Name: "b", Workspace: "shared"}, {Path: consumer, Name: "a", Workspace: "shared"}} {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	want := []string{"b/lib.go::Hello"}
	require.Equal(t, want, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
	g.scans = 0
	require.NoError(t, mi.postPublishGoModules(t.Context(), mi.GetIndexer("a"), false))
	assert.Zero(t, g.scans, "the publication tail rescanned the repository for parallels")
	assert.Equal(t, want, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
}
