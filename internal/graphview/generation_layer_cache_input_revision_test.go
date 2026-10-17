package graphview

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"testing"
)

func TestLayerCacheRefreshesCommittedCorrectionBeforeFinish(t *testing.T) {
	ctx := t.Context()
	store := openStackStore(t, "layer-cache-chunk-revision")
	_, dirty := seedRoutedStack(t, store)
	cache := newGenerationLayerCache(8, 1000000)
	row, found, err := store.Catalog().GetViewGeneration(ctx, dirty)
	require.NoError(t, err)
	require.True(t, found)
	open := func() *GenerationLayer {
		layer, err := cache.open(ctx, layerCacheKeyFor(store, dirty, row), store.AtGeneration(dirty), NewGenerationLayerContext)
		require.NoError(t, err)
		return layer
	}
	first := open()
	require.Equal(t, first.inputRevision, first.masksOnly().withHandle(store.AtGeneration(dirty)).inputRevision)
	epoch := store.GenerationCorrectionEpoch(dirty)
	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{GenerationID: dirty, Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(ctx, []string{stackKeeperID}, []*graph.Edge{{From: stackKeeperID, To: stackNewID, Kind: graph.EdgeAccessesField, FilePath: stackKeepFile, Line: 6}}, nil))
	require.Equal(t, epoch, store.GenerationCorrectionEpoch(dirty), "test must reopen before Finish")
	second := open()
	require.NotEqual(t, first.inputRevision, second.inputRevision)
	require.Equal(t, store.AtGeneration(dirty).PayloadInputRevision(), second.inputRevision)
	_, misses, entries := cache.stats()
	require.Equal(t, int64(2), misses)
	require.Equal(t, 1, entries)
	edges := second.OutEdges(stackKeeperID)
	foundEdge := false
	for _, edge := range edges {
		if edge.Kind == graph.EdgeAccessesField {
			foundEdge = true
		}
	}
	require.True(t, foundEdge)
	_, err = correction.Finish(ctx)
	require.NoError(t, err)
}

func TestLayerCacheOwnRevisionIgnoresUnrelatedWrites(t *testing.T) {
	ctx := t.Context()
	store := openStackStore(t, "layer-cache-unrelated-revision")
	commit, dirty := seedRoutedStack(t, store)
	row, found, err := store.Catalog().GetViewGeneration(ctx, dirty)
	require.NoError(t, err)
	require.True(t, found)
	key := layerCacheKeyFor(store, dirty, row)
	cache := newGenerationLayerCache(8, 1000000)
	_, err = cache.open(ctx, key, store.AtGeneration(dirty), NewGenerationLayerContext)
	require.NoError(t, err)
	require.NoError(t, store.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "base.go", ContentHash: "base"}}))
	// A correction in another actual positive generation is irrelevant too.
	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{GenerationID: commit, Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(ctx, []string{stackKeeperID}, nil, nil))
	require.Equal(t, key, layerCacheKeyFor(store, dirty, row))
	_, err = cache.open(ctx, key, store.AtGeneration(dirty), NewGenerationLayerContext)
	require.NoError(t, err)
	hits, misses, _ := cache.stats()
	require.Equal(t, int64(1), hits)
	require.Equal(t, int64(1), misses)
	_, err = correction.Finish(ctx)
	require.NoError(t, err)
}

// A committed correction chunk can race construction before its epoch moves.
// Only that observed stale input is retried, under the caller's original context.
func TestLayerCacheRetryOneCorrectionDuringLoad(t *testing.T) {
	ctx := t.Context()
	store := openStackStore(t, "layer-cache-racing-chunk")
	_, dirty := seedRoutedStack(t, store)
	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{GenerationID: dirty, Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	row, found, err := store.Catalog().GetViewGeneration(ctx, dirty)
	require.NoError(t, err)
	require.True(t, found)
	calls := 0
	loader := func(ctx context.Context, h *store_sqlite.Store) (*GenerationLayer, error) {
		calls++
		layer, err := NewGenerationLayerContext(ctx, h)
		if calls == 1 {
			require.NoError(t, correction.ReplaceSourceEdges(ctx, []string{stackKeeperID}, []*graph.Edge{{From: stackKeeperID, To: stackNewID, Kind: graph.EdgeAccessesField, FilePath: stackKeepFile, Line: 6}}, nil))
		}
		return layer, err
	}
	cache := newGenerationLayerCache(8, 1000000)
	layer, err := cache.open(ctx, layerCacheKeyFor(store, dirty, row), store.AtGeneration(dirty), loader)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, store.AtGeneration(dirty).PayloadInputRevision(), layer.inputRevision)
	_, err = correction.Finish(ctx)
	require.NoError(t, err)
}

func TestLayerCacheStaleRetryBoundCancellationAndOtherError(t *testing.T) {
	for _, mode := range []string{"continuous_stale", "canceled", "other_error"} {
		t.Run(mode, func(t *testing.T) {
			store := openStackStore(t, "layer-cache-retry-"+mode)
			_, dirty := seedRoutedStack(t, store)
			row, found, err := store.Catalog().GetViewGeneration(t.Context(), dirty)
			require.NoError(t, err)
			require.True(t, found)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			unrelated := errors.New("read failed")
			calls := 0
			loader := func(context.Context, *store_sqlite.Store) (*GenerationLayer, error) {
				calls++
				if mode == "canceled" {
					cancel()
				}
				if mode == "other_error" {
					return nil, unrelated
				}
				return nil, graph.ErrContractProjectionStale
			}
			_, err = newGenerationLayerCache(8, 1000000).open(ctx, layerCacheKeyFor(store, dirty, row), store.AtGeneration(dirty), loader)
			switch mode {
			case "continuous_stale":
				require.ErrorIs(t, err, graph.ErrContractProjectionStale)
				require.Equal(t, 2, calls)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
			case "other_error":
				require.ErrorIs(t, err, unrelated)
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestLayerCacheHitPreservesNonPathCanonicalIdentityClaim(t *testing.T) {
	ctx := t.Context()
	store := openStackStore(t, "layer-cache-canonical-claim")
	const id = "contract::http::GET::/same"
	const path = "repo/route.go"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/lower.go", Name: "lower"})
	generation, err := store.Catalog().CreateViewGeneration(ctx, store_sqlite.ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "claim-cache-fixture", GenerationKind: "dedicated", TreeOID: "tree", ConfigHash: "policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1})
	require.NoError(t, err)
	handle := store.AtGeneration(generation)
	handle.AddNode(&graph.Node{ID: id, Kind: graph.KindContract, RepoPrefix: "repo", FilePath: path, Name: "upper"})
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: path, Mode: store_sqlite.OwnershipReplace}}))
	require.NoError(t, handle.SetNodeIdentityReplacements([]string{id}))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 2))
	row, found, err := store.Catalog().GetViewGeneration(ctx, generation)
	require.NoError(t, err)
	require.True(t, found)
	cache := newGenerationLayerCache(8, 1000000)
	key := layerCacheKeyFor(store, generation, row)
	for i := 0; i < 2; i++ {
		layer, err := cache.open(ctx, key, handle, NewGenerationLayerContext)
		require.NoError(t, err)
		require.True(t, layer.OwnsNodeIdentity(id))
		require.False(t, layer.CoversNodeID(id), "canonical identity deliberately is not a path identity")
		node := graph.NewOverlaidViewWithLayer(store, layer).GetNode(id)
		require.NotNil(t, node)
		require.Equal(t, "upper", node.Name)
		require.Equal(t, 1, len(layer.claimedIDs))
		require.Equal(t, layerMaskWeight(layer), layerMaskWeight(layer.masksOnly()))
	}
	hits, misses, _ := cache.stats()
	require.Equal(t, int64(1), hits)
	require.Equal(t, int64(1), misses)
}
