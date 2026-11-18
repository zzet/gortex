package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The SQLite funnel mirror of the in-memory materialization test: an edge
// targeting a builtin stub must leave a real KindBuiltin node behind it (no
// dangling calls), and a warm re-index of the same target must ride the
// seen-set instead of re-upserting the stub.
func TestSQLiteAddBatchMaterializesBuiltinStubs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "builtin.sqlite"))
	require.NoError(t, err)
	defer s.Close()

	caller := &graph.Node{ID: "web/src/a.ts::fn", Kind: graph.KindFunction, Name: "fn", FilePath: "web/src/a.ts", RepoPrefix: "web"}
	s.AddBatch([]*graph.Node{caller}, []*graph.Edge{
		{From: caller.ID, To: "web::builtin::ts::array::push", Kind: graph.EdgeCalls, FilePath: "web/src/a.ts", Line: 3},
		{From: caller.ID, To: "builtin::ts::string::split", Kind: graph.EdgeCalls, FilePath: "web/src/a.ts", Line: 4},
	})

	prefixed := s.GetNode("web::builtin::ts::array::push")
	require.NotNil(t, prefixed, "builtin edge target must exist as a node after AddBatch")
	assert.Equal(t, graph.KindBuiltin, prefixed.Kind)
	assert.Equal(t, "push", prefixed.Name)
	assert.Equal(t, "web", prefixed.RepoPrefix)

	solo := s.GetNode("builtin::ts::string::split")
	require.NotNil(t, solo, "solo-repo (unprefixed) stub form must materialize too")
	assert.Equal(t, graph.KindBuiltin, solo.Kind)
	assert.Equal(t, "", solo.RepoPrefix)

	// The edges must no longer read as orphans: every out-edge target
	// resolves to a live node.
	for _, e := range s.GetOutEdges(caller.ID) {
		require.NotNil(t, s.GetNode(e.To), "edge target %s must not dangle", e.To)
	}

	before := s.NodeCount()
	s.AddBatch(nil, []*graph.Edge{
		{From: caller.ID, To: "web::builtin::ts::array::push", Kind: graph.EdgeCalls, FilePath: "web/src/a.ts", Line: 9},
	})
	assert.Equal(t, before, s.NodeCount(), "re-adding an edge to a seen stub must not create nodes")
}

// Fallback materialization is insert-only. Reopening loses the in-memory
// seen set, and a pinned bulk writer must obey the same precedence as a
// pooled writer: explicit and stored sentinels keep their exact boundaries.
func TestSQLiteBuiltinFallbackPreservesExplicitAndStoredNodes(t *testing.T) {
	for _, jsonb := range []string{"0", "1"} {
		t.Run("jsonb="+jsonb, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_JSONB_INGEST", jsonb)
			path := filepath.Join(t.TempDir(), "builtin.sqlite")
			s, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			rich := &graph.Node{
				ID: "repo::builtin::go::len", Kind: graph.KindBuiltin, Name: "len", Language: "go",
				RepoPrefix: "repo", WorkspaceID: "custom-workspace", ProjectID: "custom-project",
				Meta: map[string]any{"builtin": true, "builtin_kind": "func", "other": "retained"},
			}
			edge := &graph.Edge{From: "repo/a.go::f", To: rich.ID, Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1}
			require.NoError(t, s.AddBatchChecked([]*graph.Node{rich}, []*graph.Edge{edge}))
			require.Equal(t, rich, s.GetNode(rich.ID))
			require.NoError(t, s.Close())
			s, err = Open(path)
			require.NoError(t, err)
			edge.Line = 2
			require.NoError(t, s.AddBatchChecked(nil, []*graph.Edge{edge}))
			require.Equal(t, rich, s.GetNode(rich.ID))

			// A real node update remains an ordinary upsert even when an edge
			// in the same batch asks for the generic fallback.
			updated := *rich
			updated.ProjectID = "changed-project"
			edge.Line = 3
			require.NoError(t, s.AddBatchChecked([]*graph.Node{&updated}, []*graph.Edge{edge}))
			require.Equal(t, &updated, s.GetNode(rich.ID))

			// Generation ownership is exact: an existing base sentinel cannot
			// suppress a missing sentinel in a different writable generation.
			other := s.AtGeneration(91)
			require.NoError(t, other.AddBatchChecked(nil, []*graph.Edge{edge}))
			fallback := other.GetNode(rich.ID)
			require.NotNil(t, fallback)
			require.Empty(t, fallback.WorkspaceID)
			require.Equal(t, &updated, s.GetNode(rich.ID))

			proxy := &graph.Node{ID: "repo::builtin::go::make", Stub: true, Origin: "remote"}
			require.NoError(t, s.AddBatchChecked([]*graph.Node{proxy}, []*graph.Edge{
				{From: edge.From, To: proxy.ID, Kind: graph.EdgeCalls, FilePath: edge.FilePath, Line: 4},
			}))
			require.Equal(t, graph.KindBuiltin, s.GetNode(proxy.ID).Kind, "filtered proxies cannot suppress durable fallback")
		})
	}
}

func TestSQLiteBuiltinFallbackPreservesPinnedWriterNodes(t *testing.T) {
	_, handle, id := reparseWindowFixture(t)
	rich := &graph.Node{
		ID: "repo::builtin::go::type::int", Kind: graph.KindBuiltin, Name: "int", Language: "go",
		RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
		Meta: map[string]any{"builtin": true, "builtin_kind": "type"},
	}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{rich}, nil))
	opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	t.Cleanup(func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) })
	require.NoError(t, handle.AddBatchChecked(nil, []*graph.Edge{
		{From: "repo/a.go::f", To: rich.ID, Kind: graph.EdgeTypedAs, FilePath: "repo/a.go", Line: 1},
	}))
	require.NoError(t, handle.EndGenerationBulkLoadFor(id))
	require.Equal(t, rich, handle.GetNode(rich.ID))
}

func TestSQLiteBuiltinFallbackReceiptKeepsStoredSourceIdentity(t *testing.T) {
	s := openMutationReceiptStore(t)
	stored := &graph.Node{ID: "repo::builtin::go::len", Kind: graph.KindBuiltin, Name: "len", FilePath: "repo/owner.go", RepoPrefix: "repo"}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{stored}, nil))
	token := s.BeginMutationReceipt()
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "repo/new.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "repo/new.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: "repo/new.go::f", To: stored.ID, Kind: graph.EdgeCalls, FilePath: "repo/new.go"},
		{From: stored.ID, To: "unresolved::other", Kind: graph.EdgeReferences},
	}))
	receipt := s.EndMutationReceipt(token)
	require.True(t, receipt.Complete)
	require.Equal(t, []string{"repo/owner.go"}, receipt.UnresolvedFiles)
	require.Equal(t, stored, s.GetNode(stored.ID))
}
