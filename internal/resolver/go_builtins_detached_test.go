package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGoBuiltinDetachedAttributionWholeAndFileAgree(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			makeStore := func() graph.Store {
				if backend == "memory" {
					return graph.New()
				}
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				return s
			}
			const file = "repo/a.go"
			const owner = file + "::f"
			seed := func(store graph.Store) {
				store.AddBatch([]*graph.Node{
					{ID: file, Kind: graph.KindFile, Name: "a.go", FilePath: file, Language: "go", RepoPrefix: "repo", WorkspaceID: "custom-workspace", ProjectID: "custom-project"},
					{ID: owner, Kind: graph.KindFunction, Name: "f", FilePath: file, Language: "go", RepoPrefix: "repo", WorkspaceID: "custom-workspace", ProjectID: "custom-project"},
				}, []*graph.Edge{
					{From: owner, To: "unresolved::len", Kind: graph.EdgeCalls, FilePath: file, Line: 10},
					{From: "repo/unresolved::len", To: "unresolved::make", Kind: graph.EdgeArgOf, FilePath: file, Line: 10, Meta: map[string]any{"kept": "payload"}},
					{From: "repo/unresolved::len", To: owner, Kind: graph.EdgeValueFlow, FilePath: file, Line: 11},
				})
			}
			whole, scoped := makeStore(), makeStore()
			seed(whole)
			seed(scoped)
			require.True(t, New(whole).graphHasLanguage("go"), "real named Go nodes must admit the attribution pass")
			require.True(t, New(scoped).graphHasLanguage("go"))
			New(whole).attributeGoBuiltins()
			New(scoped).attributeGoBuiltinsForFile(file)
			for _, store := range []graph.Store{whole, scoped} {
				resolved := store.GetOutEdges("repo::builtin::go::len")
				require.Len(t, resolved, 1, "only the reference's exact recording site may move")
				require.Equal(t, "repo::builtin::go::make", resolved[0].To)
				require.Equal(t, map[string]any{"kept": "payload"}, resolved[0].Meta)
				unmoved := store.GetOutEdges("repo/unresolved::len")
				require.Len(t, unmoved, 1)
				require.Equal(t, 11, unmoved[0].Line)
				for _, id := range []string{"repo::builtin::go::len", "repo::builtin::go::make"} {
					node := store.GetNode(id)
					require.NotNil(t, node)
					require.Equal(t, "custom-workspace", node.WorkspaceID)
					require.Equal(t, "custom-project", node.ProjectID)
				}
			}
			require.Equal(t, whole.GetNode("repo::builtin::go::make"), scoped.GetNode("repo::builtin::go::make"))
			// Replay must not duplicate or resurrect the detached source.
			New(scoped).attributeGoBuiltinsForFile(file)
			require.Equal(t, whole.EdgeCount(), scoped.EdgeCount())
		})
	}
}

func TestGoBuiltinDetachedFileScopeKeepsOtherSitesAndForeignBoundary(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.AddBatch([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p"},
	}, []*graph.Edge{
		{From: "repo/unresolved::value", To: "unresolved::int32", Kind: graph.EdgeArgOf, FilePath: "repo/a.go", Line: 1},
		{From: "repo/unresolved::value", To: "unresolved::int32", Kind: graph.EdgeArgOf, FilePath: "repo/b.go", Line: 1},
		{From: "other/unresolved::value", To: "unresolved::int32", Kind: graph.EdgeArgOf, FilePath: "repo/a.go", Line: 2},
	})
	require.True(t, New(s).graphHasLanguage("go"), "named Go fixture must enter the attribution pass")
	New(s).attributeGoBuiltinsForFile("repo/a.go")
	edges := s.GetOutEdges("repo/unresolved::value")
	require.Len(t, edges, 2)
	for _, edge := range edges {
		if edge.FilePath == "repo/a.go" {
			require.Equal(t, "repo::builtin::go::type::int32", edge.To)
		} else {
			require.Equal(t, "unresolved::int32", edge.To)
		}
	}
	foreign := s.GetOutEdges("other/unresolved::value")
	require.Len(t, foreign, 1)
	require.Equal(t, "unresolved::int32", foreign[0].To)
	require.Nil(t, s.GetNode("other::builtin::go::type::int32"))
}
