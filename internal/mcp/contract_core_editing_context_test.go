package mcp

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// newContractCoreEditingStore holds one edited file whose function is wired
// to visible neighbours and to edges the contract-core filter hides: a
// contract endpoint (calls in both directions, imported by the file) and a
// spring.Bean injection call.
func newContractCoreEditingStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "editing.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	node := func(id, name string, kind graph.NodeKind, path string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: kind, FilePath: path, RepoPrefix: "repo", Language: "go", StartLine: 1}
	}
	store.AddBatch([]*graph.Node{
		node("repo/a.go", "a.go", graph.KindFile, "repo/a.go"),
		node("repo/a.go::F", "F", graph.KindFunction, "repo/a.go"),
		node("repo/b.go::G", "G", graph.KindFunction, "repo/b.go"),
		node("repo/e.go::K", "K", graph.KindFunction, "repo/e.go"),
		node("repo/z.go", "z.go", graph.KindFile, "repo/z.go"),
		node("repo/api.yaml::C", "C", graph.KindContract, "repo/api.yaml"),
		node("repo/d.go::H", "H", graph.KindFunction, "repo/d.go"),
	}, []*graph.Edge{
		{From: "repo/b.go::G", To: "repo/a.go::F", Kind: graph.EdgeCalls},
		{From: "repo/a.go::F", To: "repo/e.go::K", Kind: graph.EdgeCalls},
		{From: "repo/a.go", To: "repo/z.go", Kind: graph.EdgeImports},
		{From: "repo/api.yaml::C", To: "repo/a.go::F", Kind: graph.EdgeCalls},
		{From: "repo/a.go::F", To: "repo/api.yaml::C", Kind: graph.EdgeCalls},
		{From: "repo/a.go", To: "repo/api.yaml::C", Kind: graph.EdgeImports},
		{From: "repo/d.go::H", To: "repo/a.go::F", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
	})
	return store
}

func editingContextIDs(res *graph.FileEditingContextResult) (calledBy, calls, imports []string) {
	for _, n := range res.CalledBy {
		calledBy = append(calledBy, n.ID)
	}
	for _, n := range res.Calls {
		calls = append(calls, n.ID)
	}
	for _, e := range res.Imports {
		imports = append(imports, e.To)
	}
	return calledBy, calls, imports
}

// The wrapper's editing-context fast path answers exactly what the filtered
// adjacency answers: never a contract endpoint or an edge the filter hides.
func TestContractCoreEditingContextFiltersHiddenEdges(t *testing.T) {
	store := newContractCoreEditingStore(t)
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}

	calledBy, calls, imports := editingContextIDs(store.FileEditingContext("repo/a.go", kinds))
	require.ElementsMatch(t, []string{"repo/b.go::G", "repo/api.yaml::C", "repo/d.go::H"}, calledBy)
	require.ElementsMatch(t, []string{"repo/e.go::K", "repo/api.yaml::C"}, calls)
	require.ElementsMatch(t, []string{"repo/z.go", "repo/api.yaml::C"}, imports)

	wrapped := newContractCoreEdges(store, t.Context(), nil)
	fc, ok := fileEditingContextFor(wrapped)
	require.True(t, ok, "the wrapper keeps the fast path of a store that has one")
	res := fc.FileEditingContext("repo/a.go", kinds)
	require.Equal(t, "repo/a.go", res.FileNode.ID)
	require.Len(t, res.Defines, 1)
	calledBy, calls, imports = editingContextIDs(res)
	require.ElementsMatch(t, []string{"repo/b.go::G"}, calledBy)
	require.ElementsMatch(t, []string{"repo/e.go::K"}, calls)
	require.ElementsMatch(t, []string{"repo/z.go"}, imports)

	// A selected reader without the capability keeps the engine walk.
	_, ok = fileEditingContextFor(newContractCoreEdges(struct{ graph.Reader }{store}, t.Context(), nil))
	require.False(t, ok)
}

// contractCoreEditingSpy counts the adjacency reads that tell the batched
// fast path from the engine's per-node caller walk.
type contractCoreEditingSpy struct {
	*store_sqlite.Store
	inEdges, inEdgeBatches int
}

func (s *contractCoreEditingSpy) GetInEdges(id string) []*graph.Edge {
	s.inEdges++
	return s.Store.GetInEdges(id)
}

func (s *contractCoreEditingSpy) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inEdgeBatches++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

// get_editing_context on a primary session takes the fast path under the
// runtime and shows only what the filtered adjacency shows; without the
// runtime the store answers raw.
func TestGetEditingContextUnderContractRuntime(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime_installed_%v", installed), func(t *testing.T) {
			store := &contractCoreEditingSpy{Store: newContractCoreEditingStore(t)}
			srv := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil)
			if installed {
				installContractCoreKindTestRuntime(t, srv)
			}
			fc, ok := fileEditingContextFor(srv.readerFor(t.Context()))
			require.True(t, ok, "the handler's fast-path probe succeeds")
			if installed {
				require.IsType(t, contractCoreEditingContext{}, fc)
			}
			store.inEdges, store.inEdgeBatches = 0, 0
			out := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{"path": "repo/a.go", "format": "json"}))
			ids := func(key, field string) []string {
				var got []string
				rows, _ := out[key].([]any)
				for _, row := range rows {
					got = append(got, row.(map[string]any)[field].(string))
				}
				return got
			}
			calledBy, calls, imports := ids("called_by", "id"), ids("calls", "id"), ids("imports", "id")
			if installed {
				require.Zero(t, store.inEdges, "the handler walked callers per node instead of taking the fast path")
				require.Equal(t, 1, store.inEdgeBatches, "the fast path reads callers in one batch")
				require.ElementsMatch(t, []string{"repo/b.go::G"}, calledBy)
				require.ElementsMatch(t, []string{"repo/e.go::K"}, calls)
				require.ElementsMatch(t, []string{"repo/z.go"}, imports)
				return
			}
			require.Contains(t, calledBy, "repo/api.yaml::C")
			require.Contains(t, calls, "repo/api.yaml::C")
			require.Contains(t, imports, "repo/api.yaml::C")
		})
	}
}
