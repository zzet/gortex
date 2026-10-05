package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type coreMembershipSpy struct {
	graph.Store
	membership, full int
	err              error
	cancel           context.CancelFunc
}

func (s *coreMembershipSpy) GetNodeIDsByKindsContext(ctx context.Context, ids []string, kinds []graph.NodeKind) (map[string]struct{}, error) {
	s.membership++
	if s.cancel != nil {
		s.cancel()
	}
	if s.err != nil {
		return map[string]struct{}{"partial": {}}, s.err
	}
	return graph.GetNodeIDsByKindsContext(ctx, s.Store, ids, kinds)
}
func (s *coreMembershipSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.full++
	return s.Store.GetNodesByIDs(ids)
}

func TestContractCoreMembershipStoreParityScoresAndWriter(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "core.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindFunction}, {ID: "c", Kind: graph.KindFunction},
		{ID: "contract", Kind: graph.KindContract}, {ID: "bridge", Kind: graph.KindContractBridge}, {ID: "config", Kind: graph.KindConfigKey}, {ID: "table", Kind: graph.KindTable}, {ID: "unknown", Kind: graph.NodeKind("future_kind")},
	}, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 1, Origin: graph.OriginLSPResolved, Confidence: 0.8, Meta: map[string]any{"evidence": "keep"}},
		{From: "b", To: "c", Kind: graph.EdgeReferences, Line: 2}, {From: "a", To: "b", Kind: graph.EdgeCalls, Line: 3, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a", To: "contract", Kind: graph.EdgeProvides}, {From: "bridge", To: "a", Kind: graph.EdgeCalls}, {From: "a", To: "config", Kind: graph.EdgeCalls},
		{From: "a", To: "table", Kind: graph.EdgeProvides}, {From: "a", To: "unknown", Kind: graph.EdgeReferences}, {From: "a", To: "missing", Kind: graph.EdgeCalls},
	})
	fast := &coreMembershipSpy{Store: s}
	fallback := &coreEdgeTimingReader{Store: s} // graph.Store embedding deliberately hides the optional accelerator.
	fastCtx := withContractCoreReadErrors(t.Context())
	fallbackCtx := withContractCoreReadErrors(t.Context())
	fastReader := newContractCoreEdges(fast, fastCtx, nil)
	fallbackReader := newContractCoreEdges(fallback, fallbackCtx, nil)
	ids := []string{"a", "b", "c", "bridge", "config", "contract", "missing"}
	require.Equal(t, fallbackReader.GetOutEdgesByNodeIDs(ids), fastReader.GetOutEdgesByNodeIDs(ids), "order and metadata must match the same selected physical reader")
	require.Equal(t, fallbackReader.GetInEdgesByNodeIDs(ids), fastReader.GetInEdgesByNodeIDs(ids))
	rows := fastReader.GetOutEdges("a")
	require.Len(t, rows, 4)
	for _, edge := range rows {
		if edge.To == "b" {
			require.Equal(t, graph.OriginLSPResolved, edge.Origin)
			require.Equal(t, 0.8, edge.Confidence)
			require.Equal(t, "keep", edge.Meta["evidence"])
		}
	}
	fastSrv := &Server{graph: fast}
	fallbackSrv := &Server{graph: fallback}
	installContractCoreKindTestRuntime(t, fastSrv)
	installContractCoreKindTestRuntime(t, fallbackSrv)
	require.Equal(t, fallbackSrv.boundedCentralityForRequestObserved(fallbackCtx, []string{"a"}, []string{"a", "b", "c"}, nil), fastSrv.boundedCentralityForRequestObserved(fastCtx, []string{"a"}, []string{"a", "b", "c"}, nil))
	require.Positive(t, fast.membership)
	require.Positive(t, fallback.reads.kinds)
	require.NoError(t, contractCoreReadError(fastCtx))
	before := fast.membership
	writer := newContractCoreEdges(fast, t.Context(), nil)
	require.Equal(t, rows, writer.GetOutEdges("a"))
	require.Equal(t, before, fast.membership, "legacy writer classification must not use the new optional projection")
	require.Positive(t, fast.full)
}

func TestContractCoreMembershipFailureCancellationAndSelectedFallback(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go"}, {ID: "same", Kind: graph.KindContract, RepoPrefix: "a", FilePath: "a/f.go"}, {ID: "deleted", Kind: graph.KindConfigKey, FilePath: "a/d.go"}, {ID: "foreign", Kind: graph.KindContract, RepoPrefix: "b", FilePath: "b/f.go"}}, []*graph.Edge{{From: "a", To: "same", Kind: graph.EdgeCalls}, {From: "a", To: "deleted", Kind: graph.EdgeCalls}, {From: "a", To: "foreign", Kind: graph.EdgeCalls}})
	sentinel := errors.New("positive membership unavailable")
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx = withContractCoreReadErrors(ctx)
		spy := &coreMembershipSpy{Store: base, err: sentinel}
		want := error(sentinel)
		if cancelled {
			spy.err = nil
			spy.cancel = cancel
			want = context.Canceled
		}
		require.Empty(t, newContractCoreEdges(spy, ctx, nil).GetOutEdges("a"))
		require.ErrorIs(t, contractCoreReadError(ctx), want)
		require.Equal(t, 1, spy.membership)
	}
	layer := graph.NewOverlayLayer()
	layer.AddNode("a/f.go", &graph.Node{ID: "a", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go"})
	layer.AddNode("a/f.go", &graph.Node{ID: "same", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go"})
	layer.MarkFile("a/d.go", true)
	selected := graph.NewOverlaidViewWithLayer(base, layer)
	rows, err := graph.GetNodeIDsByKindsContext(t.Context(), selected, []string{"same", "deleted", "foreign"}, []graph.NodeKind{graph.KindContract, graph.KindConfigKey})
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"foreign": {}}, rows)
	scoped := newBaseGraphReader(selected, "a")
	rows, err = graph.GetNodeIDsByKindsContext(t.Context(), scoped, []string{"same", "deleted", "foreign"}, []graph.NodeKind{graph.KindContract, graph.KindConfigKey})
	require.NoError(t, err)
	require.Empty(t, rows, "foreign ownership must not leak through an unwrapped accelerator")
}
