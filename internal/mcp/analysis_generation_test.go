package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
)

// The analysis cache's request-surface half of the payload-view-generation
// axis: the cache is keyed by the payload view generation it was computed
// over, because the store's coarse mutation revision cannot stand in for one.
//
// backendStore() hands back the INDEXER's handle, which reads the base corpus.
// The analysis passes walk s.graph (analysis_persistence.go:
// `analysisGraph := s.graph`). When the server serves a routed view those are
// two different payload generations, and persisting through the base handle
// stamps the cache with a generation the analysis was never computed over.
// The store's mutation revision cannot catch that: it is one coarse
// process-local counter shared by every generation handle.

const analysisWiringViewGeneration = int64(5)

// seedAnalysisWiringStore writes one small connected corpus into the base
// generation and a different one into analysisWiringViewGeneration, so a
// cache stamped with the wrong generation describes visibly wrong content.
func seedAnalysisWiringStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "analysis_view_gen.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "pkg/base.go::BaseAlpha", Kind: graph.KindFunction, Name: "BaseAlpha", FilePath: "pkg/base.go", Language: "go"},
		{ID: "pkg/base.go::BaseBeta", Kind: graph.KindFunction, Name: "BaseBeta", FilePath: "pkg/base.go", Language: "go"},
	}, []*graph.Edge{
		{From: "pkg/base.go::BaseAlpha", To: "pkg/base.go::BaseBeta", Kind: graph.EdgeCalls, FilePath: "pkg/base.go", Line: 3},
	})

	overlay := store.AtGeneration(analysisWiringViewGeneration)
	require.NotNil(t, overlay)
	overlay.AddBatch([]*graph.Node{
		{ID: "pkg/overlay.go::OverlayAlpha", Kind: graph.KindFunction, Name: "OverlayAlpha", FilePath: "pkg/overlay.go", Language: "go"},
		{ID: "pkg/overlay.go::OverlayBeta", Kind: graph.KindFunction, Name: "OverlayBeta", FilePath: "pkg/overlay.go", Language: "go"},
	}, []*graph.Edge{
		{From: "pkg/overlay.go::OverlayAlpha", To: "pkg/overlay.go::OverlayBeta", Kind: graph.EdgeCalls, FilePath: "pkg/overlay.go", Line: 4},
	})
	return store
}

// newAnalysisWiringServer builds the real divergence: an indexer holding the
// base handle (what backendStore returns) and a server graph pinned to the
// routed generation (what the analysis passes walk).
func newAnalysisWiringServer(t *testing.T, store *store_sqlite.Store) (*Server, *store_sqlite.Store) {
	t.Helper()
	overlay := store.AtGeneration(analysisWiringViewGeneration)
	idx := indexer.New(store, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	require.Equal(t, int64(0), idx.Graph().(*store_sqlite.Store).ViewGeneration(),
		"the indexer must hold the base handle for this case to mean anything")

	server := NewServer(query.NewEngine(overlay), overlay, idx, nil, zap.NewNop(), nil)
	// Registered before the store's own Close cleanup runs (cleanups run LIFO,
	// and the store's was registered first), so the prune RunAnalysis spawns
	// finishes while the database is still open.
	t.Cleanup(server.DrainBackground)
	return server, overlay
}

// TestAnalysisGenerationBackendsFollowTheSelectedViewGeneration pins the
// selector: the analysis writer/query pair is the backend re-pinned to the
// generation s.graph reads, not the indexer's base handle.
//
// Revert-red: point analysisGenerationBackends back at s.backendStore() and
// the asserted generation drops to 0.
func TestAnalysisGenerationBackendsFollowTheSelectedViewGeneration(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)

	require.Equal(t, analysisWiringViewGeneration, server.analysisViewGeneration())

	writer, queryStore := server.analysisGenerationBackends()
	require.NotNil(t, writer)
	require.NotNil(t, queryStore)

	writerHandle, ok := writer.(*store_sqlite.Store)
	require.True(t, ok, "analysis writer must be the sqlite store")
	require.Equal(t, analysisWiringViewGeneration, writerHandle.ViewGeneration(),
		"the analysis writer is pinned to the base corpus, not to the selected view")

	queryHandle, ok := queryStore.(*store_sqlite.Store)
	require.True(t, ok, "analysis query store must be the sqlite store")
	require.Equal(t, analysisWiringViewGeneration, queryHandle.ViewGeneration())
}

// TestRunAnalysisPersistsUnderTheSelectedViewGeneration is the
// production-entrypoint trace: the exported RunAnalysis path reaches
// BeginAnalysisGeneration through a generation-pinned handle, so the resulting
// cache is addressable from the selected view and invisible from the base one.
func TestRunAnalysisPersistsUnderTheSelectedViewGeneration(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, overlay := newAnalysisWiringServer(t, store)

	server.RunAnalysis()

	header, found, err := overlay.LoadActiveAnalysisHeader(analysisGenerationFormatVersion)
	require.NoError(t, err)
	require.True(t, found, "the selected view has no active analysis after RunAnalysis")
	require.NotZero(t, header.GenerationID)

	// The base corpus never asked for an analysis and must not have gained one.
	_, baseFound, err := store.LoadActiveAnalysisHeader(analysisGenerationFormatVersion)
	require.NoError(t, err)
	require.False(t, baseFound, "the base corpus was handed the routed view's analysis")

	// The cached rows are the selected view's content, not the base's.
	metrics, err := overlay.AnalysisNodeMetrics(header.GenerationID, []string{
		"pkg/overlay.go::OverlayAlpha", "pkg/base.go::BaseAlpha",
	})
	require.NoError(t, err)
	seen := make(map[string]bool, len(metrics))
	for _, metric := range metrics {
		seen[metric.NodeID] = true
	}
	require.True(t, seen["pkg/overlay.go::OverlayAlpha"], "the cached analysis is missing the selected view's node")
	require.False(t, seen["pkg/base.go::BaseAlpha"], "the cached analysis carries a base-corpus node")

	// And the base handle cannot read the routed view's analysis at all.
	_, err = store.AnalysisNodeMetrics(header.GenerationID, []string{"pkg/overlay.go::OverlayAlpha"})
	require.ErrorIs(t, err, graph.ErrAnalysisGenerationInactive)
}
