package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
)

// repoTopologyFixture tracks two Go repositories in one SQLite store with no
// edge between them: "lib" (module example.org/library) and "app".
func repoTopologyFixture(t *testing.T) (*MultiIndexer, graph.Store, map[string]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	lib, app := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(lib, "lib.go"), "package library\nfunc Hello() {}\nfunc Local() { Hello() }\n")
	writeFile(t, filepath.Join(app, "go.mod"), "module example.org/app\n")
	writeFile(t, filepath.Join(app, "main.go"), "package app\nfunc Run() {}\nfunc Start() { Run() }\n")

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, entry := range []config.RepoEntry{
		{Path: lib, Name: "lib", Workspace: "shared"},
		{Path: app, Name: "app", Workspace: "shared"},
	} {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	return mi, s, map[string]string{"lib": lib, "app": app}
}

func reachIDs(entries []reach.Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// A patch in one repository that adds an edge into another must retire the
// other repository's reach records too, or impact keeps answering from a
// record that misses the new caller.
func TestCrossRepoPatchInvalidatesBothRepos(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	reach.BuildIndex(g)
	d1, _, _, hit, _ := reach.LookupCached(g, "lib/lib.go::Hello")
	require.True(t, hit, "fixture must start from a published record")
	require.Equal(t, []string{"lib/lib.go::Local"}, reachIDs(d1))

	main := filepath.Join(roots["app"], "main.go")
	writeFile(t, main, "package app\nimport library \"example.org/library\"\nfunc Run() {}\nfunc Start() { Run(); library.Hello() }\n")
	_, err := mi.IncrementalReindexRepo("app", []string{main})
	require.NoError(t, err)
	var bound bool
	for _, e := range g.GetOutEdges("app/main.go::Start") {
		bound = bound || e.To == "lib/lib.go::Hello"
	}
	require.True(t, bound, "the patch must bind the cross-repository call")

	d1, _, _, hit, truncated := reach.LookupContext(t.Context(), g, "lib/lib.go::Hello")
	require.True(t, hit)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{"lib/lib.go::Local", "app/main.go::Start"}, reachIDs(d1),
		"the record published before the patch was served")
}
