package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGoBuiltinRecordedCandidatesThroughPublicFileTails(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, entry := range []string{"point", "incoming", "outgoing"} {
			t.Run(backend+"/"+entry, func(t *testing.T) {
				store := builtinFrontierStore(t, backend)
				const file = "repo/a.go"
				store.AddBatch([]*graph.Node{
					{ID: file, Kind: graph.KindFile, Name: "a.go", FilePath: file, Language: "go", RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p"},
					{ID: file + "::F", Kind: graph.KindFunction, Name: "F", FilePath: file, Language: "go", RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p"},
					{ID: "repo::builtin::go::len", Kind: graph.KindBuiltin, Name: "len", Language: "go", RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p"},
				}, []*graph.Edge{
					{From: file + "::F", To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: file, Line: 8},
					// This source is a persisted node, but has no file membership.
					// Only recording-file discovery reaches its ArgOf reference.
					{From: "repo::builtin::go::len", To: "unresolved::make", Kind: graph.EdgeArgOf, FilePath: file, Line: 10, Meta: map[string]any{"kept": "payload"}},
					{From: "repo::builtin::go::len", To: "unresolved::make", Kind: graph.EdgeArgOf, FilePath: "repo/b.go", Line: 10},
					{From: "other/unresolved::value", To: "unresolved::int32", Kind: graph.EdgeArgOf, FilePath: file, Line: 12},
				})
				r := New(store)
				builtinFrontierResolve(r, entry, file)
				var own, other *graph.Edge
				for _, edge := range store.GetOutEdges("repo::builtin::go::len") {
					if edge.FilePath == file {
						own = edge
					} else {
						other = edge
					}
				}
				require.NotNil(t, own)
				require.Equal(t, "repo::builtin::go::make", own.To)
				require.Equal(t, map[string]any{"kept": "payload"}, own.Meta)
				require.NotNil(t, other)
				require.Equal(t, "unresolved::make", other.To, "recording-file scope must remain exact")
				foreign := store.GetOutEdges("other/unresolved::value")
				require.Len(t, foreign, 1)
				require.Equal(t, "unresolved::int32", foreign[0].To)
				node := store.GetNode("repo::builtin::go::make")
				require.NotNil(t, node)
				require.Equal(t, "w", node.WorkspaceID)
				require.Equal(t, "p", node.ProjectID)
				count := store.EdgeCount()
				builtinFrontierResolve(r, entry, file)
				require.Equal(t, count, store.EdgeCount(), "replay is idempotent")
			})
		}
	}
}

func TestGoBuiltinSkippedFrontierStillAttributesWithoutGenericPending(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, entry := range []string{"point", "incoming", "outgoing"} {
			t.Run(backend+"/"+entry, func(t *testing.T) {
				store := builtinFrontierStore(t, backend)
				const file = "repo/a.go"
				const owner = file + "::F"
				edge := &graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeCalls, FilePath: file, Line: 4, Meta: map[string]any{"kept": "payload"}}
				store.AddBatch([]*graph.Node{
					{ID: file, Kind: graph.KindFile, Name: "a.go", FilePath: file, Language: "go", RepoPrefix: "repo"},
					{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: file, Language: "go", RepoPrefix: "repo"},
				}, []*graph.Edge{edge})
				r := New(store)
				r.SetIncrementalSkip([]*graph.Edge{edge})
				frontier := collectOutgoingFileFrontier(store, []string{file}, r.incrementalSkipped)
				require.Empty(t, frontier.pending, "exercise the actual no-pending entry guard")
				builtinFrontierResolve(r, entry, file)
				edges := store.GetOutEdges(owner)
				require.Len(t, edges, 1)
				require.Equal(t, "repo::builtin::go::len", edges[0].To)
				require.Equal(t, map[string]any{"kept": "payload"}, edges[0].Meta)
			})
		}
	}
}

func builtinFrontierStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func builtinFrontierResolve(r *Resolver, entry, file string) {
	switch entry {
	case "point":
		r.ResolveFileAndIncoming(file)
	case "incoming":
		r.ResolveFilesAndIncoming([]string{file})
	case "outgoing":
		r.ResolveFilesOutgoing([]string{file})
	}
}
