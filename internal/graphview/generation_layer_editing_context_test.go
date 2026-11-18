package graphview

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestEditingContextSelectedGenerationFullParityAndStale(t *testing.T) {
	store := openTestStore(t)
	file, neighbor := "repo/a.go", "repo/b.go"
	base := []*graph.Node{
		{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo"},
		{ID: file + "::f", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", Meta: map[string]any{"signature": "old"}},
		{ID: neighbor + "::g", Kind: graph.KindFunction, FilePath: neighbor, RepoPrefix: "repo", Meta: map[string]any{"signature": "full neighbor"}},
	}
	require.NoError(t, store.AddBatchChecked(base, []*graph.Edge{{From: base[2].ID, To: base[1].ID, Kind: graph.EdgeCalls, FilePath: neighbor, Meta: map[string]any{"full": "caller"}}}))
	_, handle := beginTestGeneration(t, store, "editing-context")
	current := &graph.Node{ID: base[1].ID, Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", Meta: map[string]any{"signature": "new"}}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{base[0], current}, []*graph.Edge{{From: current.ID, To: base[2].ID, Kind: graph.EdgeCalls, FilePath: file, Meta: map[string]any{"full": "selected callee"}}}))
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: file, Mode: store_sqlite.OwnershipReplace}}))
	publishTestGeneration(t, store, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(store, layer)
	// The scoped read runs first, before the legacy read can populate the
	// layer's NodeByID cache, so parity cannot depend on an earlier legacy
	// hydration. The separate SQLite admission control fixes its pool at one.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var got *graph.FileEditingContextResult
	require.NoError(t, graph.WithFileEditingContextReader(ctx, view, func(_ context.Context, rows graph.FileEditingContextRows) error {
		got = graph.FileEditingContextOf(rows, file, []graph.NodeKind{graph.KindFunction})
		return nil
	}))
	require.Equal(t, graph.FileEditingContextOf(view, file, []graph.NodeKind{graph.KindFunction}), got)
	require.Equal(t, "new", got.Defines[0].Meta["signature"])
	require.Equal(t, base[2].Meta, got.CalledBy[0].Meta)
	require.Len(t, got.Calls, 1)
	require.Equal(t, base[2].Meta, got.Calls[0].Meta)
	correction, err := store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(t.Context(), []string{current.ID}, []*graph.Edge{{From: current.ID, To: base[2].ID, Kind: graph.EdgeAccessesField, FilePath: file}}, nil))
	err = graph.WithFileEditingContextReader(t.Context(), view, func(context.Context, graph.FileEditingContextRows) error {
		t.Fatal("stale layer invoked bundle callback")
		return nil
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	_, err = correction.Finish(t.Context())
	require.NoError(t, err)
}

func TestEditingContextStagedContradictionsDoNotOverrideOwnership(t *testing.T) {
	store := openTestStore(t)
	file, other := "repo/a.go", "repo/other.go"
	function := &graph.Node{ID: file + "::f", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", Meta: map[string]any{"signature": "old"}}
	sibling := &graph.Node{ID: file + "::sibling", Kind: graph.KindType, FilePath: file, RepoPrefix: "repo", Meta: map[string]any{"signature": "lower sibling"}}
	neighbor := &graph.Node{ID: other + "::caller", Kind: graph.KindFunction, FilePath: other, RepoPrefix: "repo", Meta: map[string]any{"signature": "lower caller"}}
	require.NoError(t, store.AddBatchChecked([]*graph.Node{function, sibling, neighbor}, []*graph.Edge{{From: neighbor.ID, To: function.ID, Kind: graph.EdgeCalls, FilePath: other, Meta: map[string]any{"full": "lower adjacency"}}, {From: function.ID, To: neighbor.ID, Kind: graph.EdgeCalls, FilePath: file, Meta: map[string]any{"full": "identity replacement keeps lower outgoing"}}}))
	_, handle := beginTestGeneration(t, store, "editing-context-detached")
	current := *function
	current.Meta = map[string]any{"signature": "selected full"}
	poisonSibling := *sibling
	poisonSibling.Meta = map[string]any{"signature": "unclaimed poison"}
	poisonNeighbor := *neighbor
	poisonNeighbor.Meta = map[string]any{"signature": "context poison"}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{&current, &poisonSibling, &poisonNeighbor}, nil))
	require.NoError(t, handle.SetNodeIdentityReplacements([]string{function.ID}))
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: other, Mode: store_sqlite.OwnershipContext}}))
	// Production publication must reject this deliberately staged contradiction.
	// Match the existing layer-context defence fixtures: leave it unpublished
	// while checking that the layer itself never serves carried context poison.
	require.ErrorIs(t, store.PublishPayloadGeneration(t.Context(), handle.ViewGeneration(), 2000), store_sqlite.ErrGenerationMaskIntegrity)
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(store, layer)
	var got *graph.FileEditingContextResult
	require.NoError(t, graph.WithFileEditingContextReader(t.Context(), view, func(_ context.Context, rows graph.FileEditingContextRows) error {
		got = graph.FileEditingContextOf(rows, file, []graph.NodeKind{graph.KindFunction})
		return nil
	}))
	require.Equal(t, graph.FileEditingContextOf(view, file, []graph.NodeKind{graph.KindFunction}), got)
	require.Len(t, got.Defines, 2)
	byID := map[string]*graph.Node{}
	for _, node := range got.Defines {
		byID[node.ID] = node
	}
	require.Equal(t, current.Meta, byID[function.ID].Meta)
	require.Equal(t, sibling.Meta, byID[sibling.ID].Meta)
	require.Equal(t, neighbor.Meta, got.CalledBy[0].Meta)
	require.Len(t, got.Calls, 1)
	require.Equal(t, neighbor.Meta, got.Calls[0].Meta)
}
