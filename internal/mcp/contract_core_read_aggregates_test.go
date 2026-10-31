package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractCoreScopedImportersAndCountsKeepSelectedAdjacency(t *testing.T) {
	s := openContractCoreCapStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "a/file.go", Kind: graph.KindFile, RepoPrefix: "a", FilePath: "a/file.go"},
		{ID: "a/file.go::F", Name: "F", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/file.go"},
		{ID: "b/visible.go", Kind: graph.KindFile, RepoPrefix: "b", FilePath: "b/visible.go"},
		{ID: "b/bean.go", Kind: graph.KindFile, RepoPrefix: "b", FilePath: "b/bean.go"},
		{ID: "b/test_test.go", Kind: graph.KindFile, RepoPrefix: "b", FilePath: "b/test_test.go"},
		{ID: "contract", Kind: graph.KindContract, FilePath: "a/file.go"},
		{ID: "alias.go", Kind: graph.KindFile, FilePath: "different.go"},
	}, []*graph.Edge{
		{From: "b/visible.go", To: "a/file.go", Kind: graph.EdgeImports},
		{From: "b/visible.go", To: "a/file.go::F", Kind: graph.EdgeImports},
		{From: "b/test_test.go", To: "a/file.go", Kind: graph.EdgeImports},
		{From: "contract", To: "a/file.go", Kind: graph.EdgeImports},
		{From: "b/visible.go", To: "contract", Kind: graph.EdgeImports},
		{From: "b/bean.go", To: "a/file.go", Kind: graph.EdgeImports, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "b/visible.go", To: "alias.go", Kind: graph.EdgeImports},
		{From: "b/visible.go", To: "a/file.go::F", Kind: graph.EdgeCalls},
		{From: "contract", To: "a/file.go::F", Kind: graph.EdgeCalls},
	})
	var baselineRows int
	for pass := range 2 {
		spy := &kindAdjacencySpy{Reader: s}
		ctx := withContractCoreReadErrors(t.Context())
		reader := newContractCoreEdges(spy, ctx, nil)
		importers, ok := reader.(graph.FileImporters)
		require.True(t, ok)
		counts, ok := reader.(graph.InEdgeCounter)
		require.True(t, ok)
		rows := importers.FileImporters("a/file.go")
		require.Len(t, rows, 3, "real foreign imports count; hidden endpoints and bean metadata do not")
		got := collectImportingFiles(reader, &graph.Node{FilePath: "a/file.go"}, true)
		require.Equal(t, []string{"b/visible.go"}, got)
		// The capability-less selected-reader path must answer the same file
		// projection without falling back to a whole-generation edge read.
		fallback, err := collectImportingFilesContext(ctx, legacyOutlineReader{reader}, &graph.Node{FilePath: "a/file.go"}, true)
		require.NoError(t, err)
		require.Equal(t, got, fallback)
		require.Len(t, importers.FileImporters("alias.go"), 1, "target ID match preserves the legacy OR")
		if pass == 0 {
			baselineRows = spy.rows
		} else {
			require.Equal(t, baselineRows, spy.rows, "unrelated imports increased file projection rows")
		}
		wantCounts := make(map[string]int)
		for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports} {
			for edge := range reader.EdgesByKind(kind) {
				wantCounts[edge.To]++
			}
		}
		require.Equal(t, wantCounts, counts.InEdgeCountsByKind([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports, graph.EdgeCalls}))
		require.Zero(t, spy.allEdges)
		require.Zero(t, spy.allNodes)
		require.NoError(t, contractCoreReadError(ctx))
		if pass == 0 {
			for i := range 5 {
				prefix := fmt.Sprintf("unrelated-%d", i)
				s.AddBatch([]*graph.Node{{ID: prefix + "/from", Kind: graph.KindFile, FilePath: prefix + "/from.go"}, {ID: prefix + "/to", Kind: graph.KindFile, FilePath: prefix + "/to.go"}}, []*graph.Edge{{From: prefix + "/from", To: prefix + "/to", Kind: graph.EdgeImports}})
			}
		}
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile("b/visible.go", true)
	selected := newContractCoreEdges(graph.NewOverlaidView(s, layer), t.Context(), nil)
	require.Equal(t, []string{"b/test_test.go"}, collectImportingFiles(selected, &graph.Node{FilePath: "a/file.go"}, false))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fileImportersFromReader(ctx, selected, "a/file.go")
	require.ErrorIs(t, err, context.Canceled)
}
