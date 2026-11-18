package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func sparseShapeStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "sparse-proof.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	return s
}

func sparseShapeLayer(t *testing.T, s *store_sqlite.Store, generation int64, nodes []*graph.Node, masks []store_sqlite.FileMask) (store_sqlite.ContractProjectionFootprint, *graphview.GenerationLayer) {
	t.Helper()
	h := s.AtGeneration(generation)
	h.AddBatch(nodes, nil)
	require.NoError(t, h.SetFileMasks(masks))
	footprint, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	layer, err := graphview.NewGenerationLayerContext(t.Context(), h)
	require.NoError(t, err)
	return footprint, layer
}

func sparseShapeRead(t *testing.T, reader graph.Reader, name string) []store_sqlite.ContractProjectionDeclaration {
	t.Helper()
	var out []store_sqlite.ContractProjectionDeclaration
	require.NoError(t, graph.VisitNodeDeclarationsByNamesContext(t.Context(), reader, []string{name}, func(n *graph.Node) bool {
		out = append(out, store_sqlite.ContractProjectionDeclaration{ID: n.ID, Name: n.Name, Kind: string(n.Kind), FilePath: n.FilePath, RepoPrefix: n.RepoPrefix})
		return true
	}))
	return out
}

func TestSparseDeclarationShapeRetainsRealFoldedProjection(t *testing.T) {
	s := sparseShapeStore(t)
	base := s.AtGeneration(10)
	base.AddNode(&graph.Node{ID: "r/f.go::Old", Name: "Old", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"})
	nodeF := &graph.Node{ID: "r/f.go::Route", Name: "Route", Kind: graph.KindFunction, FilePath: "r/f.go", RepoPrefix: "r"}
	nodeG := &graph.Node{ID: "r/g.go::Boundary", Name: "Boundary", Kind: graph.KindConstant, FilePath: "r/g.go", RepoPrefix: "r"}
	maskF := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipReplace}
	maskG := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/g.go", Mode: store_sqlite.OwnershipReplace}
	first, layerF := sparseShapeLayer(t, s, 11, []*graph.Node{nodeF}, []store_sqlite.FileMask{maskF})
	second, layerG := sparseShapeLayer(t, s, 12, []*graph.Node{nodeG}, []store_sqlite.FileMask{maskG})
	folded, layerFolded := sparseShapeLayer(t, s, 13, []*graph.Node{nodeF, nodeG}, []store_sqlite.FileMask{maskF, maskG})
	before := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, layerF), layerG)
	after := graph.NewOverlaidViewWithLayer(base, layerFolded)
	oldShape, newShape := sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{first, second}), sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{folded})
	for _, name := range []string{"Route", "Boundary", "Old", "Missing"} {
		require.True(t, oldShape.sameNameProjection(newShape, name), name)
		require.Equal(t, sparseShapeRead(t, before, name), sparseShapeRead(t, after, name), "same sparse fold is checked against the actual production visitor")
	}
}

func TestSparseDeclarationShapeRefusesNegativeRevealWithoutPrefixProof(t *testing.T) {
	s := sparseShapeStore(t)
	base := s.AtGeneration(10)
	base.AddNode(&graph.Node{ID: "r/f.go::Old", Name: "Old", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"})
	masked, layerMasked := sparseShapeLayer(t, s, 11, nil, []store_sqlite.FileMask{{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipDelete}})
	unmasked, layerUnmasked := sparseShapeLayer(t, s, 12, nil, nil)
	before, after := graph.NewOverlaidViewWithLayer(base, layerMasked), graph.NewOverlaidViewWithLayer(base, layerUnmasked)
	require.Empty(t, sparseShapeRead(t, before, "Old"))
	require.Len(t, sparseShapeRead(t, after, "Old"), 1)
	require.False(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{masked}).sameNameProjection(sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{unmasked}), "Old"), "empty suffixes do not prove a negative when masks reveal the prefix")
	// A declaration added while masked remains invisible, even when the
	// negative read is refreshed. Removing the mask must still refuse.
	base.AddNode(&graph.Node{ID: "r/f.go::New", Name: "New", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"})
	require.Empty(t, sparseShapeRead(t, before, "New"))
	require.Len(t, sparseShapeRead(t, after, "New"), 1)
	require.False(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{masked}).sameNameProjection(sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{unmasked}), "New"))
}

func TestSparseDeclarationShapePreservesOrderAndAllKinds(t *testing.T) {
	s := sparseShapeStore(t)
	base := s.AtGeneration(10)
	nodeF := &graph.Node{ID: "r/f.go::Value", Name: "Value", Kind: graph.KindParam, FilePath: "r/f.go", RepoPrefix: "r"}
	nodeG := &graph.Node{ID: "r/g.go::Value", Name: "Value", Kind: graph.KindConstant, FilePath: "r/g.go", RepoPrefix: "r"}
	maskF := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipReplace}
	maskG := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/g.go", Mode: store_sqlite.OwnershipReplace}
	first, layerF := sparseShapeLayer(t, s, 11, []*graph.Node{nodeF}, []store_sqlite.FileMask{maskF})
	second, layerG := sparseShapeLayer(t, s, 12, []*graph.Node{nodeG}, []store_sqlite.FileMask{maskG})
	folded, layerFolded := sparseShapeLayer(t, s, 13, []*graph.Node{nodeF, nodeG}, []store_sqlite.FileMask{maskF, maskG})
	before := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, layerF), layerG)
	after := graph.NewOverlaidViewWithLayer(base, layerFolded)
	oldRows, newRows := sparseShapeRead(t, before, "Value"), sparseShapeRead(t, after, "Value")
	require.Len(t, oldRows, 2, "nonconstants contribute to the unchanged name cap")
	require.ElementsMatch(t, oldRows, newRows)
	require.NotEqual(t, oldRows, newRows, "physical fold reorders overlay-first rows")
	require.False(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{first, second}).sameNameProjection(sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{folded}), "Value"), "unordered membership is insufficient")
}

func TestSparseDeclarationShapeTracksNewProducerAndContext(t *testing.T) {
	s := sparseShapeStore(t)
	base := s.AtGeneration(10)
	mask := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipReplace}
	empty, layerEmpty := sparseShapeLayer(t, s, 11, nil, []store_sqlite.FileMask{mask})
	node := &graph.Node{ID: "r/f.go::Missing", Name: "Missing", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"}
	added, layerAdded := sparseShapeLayer(t, s, 12, []*graph.Node{node}, []store_sqlite.FileMask{mask})
	require.Empty(t, sparseShapeRead(t, graph.NewOverlaidViewWithLayer(base, layerEmpty), "Missing"))
	require.Len(t, sparseShapeRead(t, graph.NewOverlaidViewWithLayer(base, layerAdded), "Missing"), 1)
	require.False(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{empty}).sameNameProjection(sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{added}), "Missing"))
	context, layerContext := sparseShapeLayer(t, s, 13, []*graph.Node{node}, []store_sqlite.FileMask{{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipContext}})
	require.Empty(t, sparseShapeRead(t, graph.NewOverlaidViewWithLayer(base, layerContext), "Missing"))
	require.Empty(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{context}).names["Missing"], "context payload is never a declaration answer")
}

func TestSparseDeclarationShapeContextAndExplicitClaimMatchVisitor(t *testing.T) {
	s := sparseShapeStore(t)
	base := s.AtGeneration(10)
	node := &graph.Node{ID: "detached-identity", Name: "Value", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"}
	first, layerFirst := sparseShapeLayer(t, s, 11, []*graph.Node{node}, nil)
	h := s.AtGeneration(12)
	h.AddNode(node)
	require.NoError(t, h.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "one", FilePath: "r/f.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "two", FilePath: "r/f.go", Mode: store_sqlite.OwnershipContext},
	}))
	require.NoError(t, h.SetNodeIdentityReplacements([]string{node.ID}))
	second, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	layerSecond, err := graphview.NewGenerationLayerContext(t.Context(), h)
	require.NoError(t, err)
	selected := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, layerFirst), layerSecond)
	require.Empty(t, sparseShapeRead(t, selected, "Value"), "own context row is omitted but independent covered-file explicit identity still hides lower row")
	require.Empty(t, sparseDeclarationShape([]store_sqlite.ContractProjectionFootprint{first, second}).names["Value"])
}
