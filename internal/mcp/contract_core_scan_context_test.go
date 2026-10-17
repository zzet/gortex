package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type contractCoreScanSpy struct {
	graph.Reader
	scans, lightReads, fullReads, pageSize int
	seenContext                            context.Context
}

func (s *contractCoreScanSpy) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]graph.NodeSearchKey) bool) error {
	s.scans++
	s.pageSize, s.seenContext = pageSize, ctx
	return graph.ScanNodeSearchKeys(ctx, s.Reader, pageSize, yield)
}
func (s *contractCoreScanSpy) AllNodesLight() []*graph.Node {
	s.lightReads++
	return graph.AllNodesLight(s.Reader)
}
func (s *contractCoreScanSpy) AllNodes() []*graph.Node {
	s.fullReads++
	return s.Reader.AllNodes()
}

func TestContractCoreScanRetainsCompactSelectedPagingAndCallbackReentry(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Old", Name: "Old", Kind: graph.KindFunction, FilePath: "repo/a.go"},
		{ID: "repo/b.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "repo/b.go"},
		{ID: "repo/c.go::C", Name: "C", Kind: graph.KindFunction, FilePath: "repo/c.go"},
	}, nil)
	layer := graph.NewOverlayLayer()
	current := &graph.Node{ID: "repo/a.go::Current", Name: "Current", Kind: graph.KindFunction, FilePath: "repo/a.go"}
	layer.AddNode(current.FilePath, current)
	selected := graph.NewOverlaidViewWithLayer(base, layer)
	spy := &contractCoreScanSpy{Reader: selected}
	reader := newContractCoreEdges(spy, t.Context(), nil)
	var ids []string
	err := graph.ScanNodeSearchKeys(t.Context(), reader, 1, func(page []graph.NodeSearchKey) bool {
		require.Len(t, page, 1)
		for _, key := range page {
			ids = append(ids, key.ID)
			// The compact reader must release its read ownership before the
			// callback re-enters the selected reader for full evidence.
			require.NotNil(t, reader.GetNode(key.ID))
		}
		return true
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{current.ID, "repo/b.go::B", "repo/c.go::C"}, ids)
	require.Equal(t, 1, spy.scans)
	require.Equal(t, 1, spy.pageSize)
	require.Equal(t, t.Context(), spy.seenContext)
	require.Zero(t, spy.fullReads)
	require.Zero(t, spy.lightReads, "compact scan must not fall back to materializing all light rows")
	var calls int
	require.NoError(t, graph.ScanNodeSearchKeys(t.Context(), reader, 1, func([]graph.NodeSearchKey) bool { calls++; return false }))
	require.Equal(t, 1, calls, "callback stop is propagated")
	rows := graph.AllNodesLight(reader)
	require.Len(t, rows, 3)
	require.Equal(t, 1, spy.lightReads)
	require.Zero(t, spy.fullReads, "light scan must reach the selected light capability")
}

func TestContractCoreScanPreservesCancellation(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, nil)
	spy := &contractCoreScanSpy{Reader: base}
	reader := newContractCoreEdges(spy, t.Context(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	require.ErrorIs(t, graph.ScanNodeSearchKeys(ctx, reader, 1, func([]graph.NodeSearchKey) bool { called = true; return true }), context.Canceled)
	require.False(t, called)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	var pages int
	err := graph.ScanNodeSearchKeys(ctx, reader, 1, func([]graph.NodeSearchKey) bool { pages++; cancel(); return true })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, pages)
	require.Zero(t, spy.fullReads)
	require.Zero(t, spy.lightReads)
}
