package graphview

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"testing"
)

func constantLayerFixture(t *testing.T) (*store_sqlite.Store, []*graph.Node) {
	t.Helper()
	s := openTestStore(t)
	nodes := []*graph.Node{{ID: "/repo/c.go", Kind: graph.KindFile, FilePath: "/repo/c.go", RepoPrefix: "repo"}, {ID: "/repo/c.go::C", Kind: graph.KindConstant, Name: "C", FilePath: "/repo/c.go", RepoPrefix: "repo"}}
	s.AddBatch(nodes, nil)
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: nodes[0].FilePath, ContentHash: "accepted", NodeCount: 2}}))
	require.NoError(t, s.BulkSetConstantValues("repo", []graph.ConstantValueRow{{NodeID: nodes[1].ID, FilePath: nodes[1].FilePath, Value: "old"}}))
	return s, nodes
}
func TestComposedConstantValuesPublishedDeleteToEmptyRetainsAcceptedFile(t *testing.T) {
	s, nodes := constantLayerFixture(t)
	_, handle := beginTestGeneration(t, s, "constant-empty")
	dw := graph.NewDeltaWriter(s, handle)
	require.NoError(t, dw.DeleteConstantValuesByFiles("repo", []string{nodes[0].FilePath}))
	payload := dw.Payload(nil)
	require.Equal(t, []string{nodes[0].FilePath}, payload.ReplacePaths)
	handle.AddBatch(payload.Nodes, payload.Edges)
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: nodes[0].FilePath, RepoPrefix: "repo", Mode: store_sqlite.OwnershipReplace}}))
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	values, err := graph.NewOverlaidViewWithLayer(s, layer).ConstantValuesByNodeIDsContext(context.Background(), []string{nodes[1].ID})
	require.NoError(t, err)
	require.Empty(t, values)
	old, err := s.ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Equal(t, "old", old[nodes[1].ID])
}

func TestComposedConstantValuesPublishedFileDeletionKeepsInventoryAbsent(t *testing.T) {
	s, nodes := constantLayerFixture(t)
	_, handle := beginTestGeneration(t, s, "constant-file-delete")
	dw := graph.NewDeltaWriter(s, handle)
	dw.EvictFiles([]string{nodes[0].FilePath})
	require.NoError(t, dw.DeleteFileMetasByFiles("repo", []string{nodes[0].FilePath}))
	require.NoError(t, dw.DeleteConstantValuesByFiles("repo", []string{nodes[0].FilePath}))
	require.Equal(t, []string{nodes[0].FilePath}, dw.Payload(nil).DeletePaths)
	metadata, err := handle.FileMetasByPaths("repo", []string{nodes[0].FilePath})
	require.NoError(t, err)
	require.Empty(t, metadata)
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: nodes[0].FilePath, RepoPrefix: "repo", Mode: store_sqlite.OwnershipDelete}}))
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(s, layer)
	values, err := view.ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Empty(t, values)
	projection, err := view.ReadConstantValueProjectionContext(context.Background(), nil, []graph.ConstantFileKey{{RepoPrefix: "repo", FilePath: nodes[0].FilePath}})
	require.NoError(t, err)
	require.Empty(t, projection.Files)
}
func TestComposedConstantValuesPublishedIdentityEnrichmentInherits(t *testing.T) {
	s, nodes := constantLayerFixture(t)
	_, handle := beginTestGeneration(t, s, "constant-enrichment")
	changed := *nodes[1]
	changed.Meta = map[string]any{"semantic_type": "string"}
	handle.AddBatch([]*graph.Node{&changed}, nil)
	require.NoError(t, handle.SetNodeIdentityReplacements([]string{changed.ID}))
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(s, layer)
	values, err := view.ConstantValuesByNodeIDs([]string{changed.ID})
	require.NoError(t, err)
	require.Equal(t, "old", values[changed.ID])
	// Unrelated background generation writes and base corpus writes must not
	// make this positive layer's cached ownership permanently ineligible.
	_, other := beginTestGeneration(t, s, "constant-unrelated")
	other.AddBatch([]*graph.Node{{ID: "/repo/other.go", Kind: graph.KindFile, FilePath: "/repo/other.go"}}, nil)
	s.AddBatch([]*graph.Node{{ID: "/repo/base-extra.go", Kind: graph.KindFile, FilePath: "/repo/base-extra.go"}}, nil)
	values, err = view.ConstantValuesByNodeIDs([]string{changed.ID})
	require.NoError(t, err)
	require.Equal(t, "old", values[changed.ID])
}
func TestComposedConstantValuesPublishedContextAndAmbiguousReplacement(t *testing.T) {
	for _, mode := range []store_sqlite.OwnershipMode{store_sqlite.OwnershipContext, store_sqlite.OwnershipReplace} {
		t.Run(string(mode), func(t *testing.T) {
			s, nodes := constantLayerFixture(t)
			_, handle := beginTestGeneration(t, s, "constant-mode")
			if mode == store_sqlite.OwnershipReplace {
				handle.AddBatch(nodes, nil)
			}
			require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: nodes[0].FilePath, RepoPrefix: "repo", Mode: mode}}))
			publishTestGeneration(t, s, handle.ViewGeneration())
			layer, err := NewGenerationLayer(handle)
			require.NoError(t, err)
			values, err := graph.NewOverlaidViewWithLayer(s, layer).ConstantValuesByNodeIDs([]string{nodes[1].ID})
			if mode == store_sqlite.OwnershipReplace {
				require.ErrorIs(t, err, graph.ErrConstantProjectionIncomplete)
				require.Nil(t, values)
			} else {
				require.NoError(t, err)
				require.Equal(t, "old", values[nodes[1].ID])
			}
		})
	}
}
func TestComposedConstantValuesDeclineOldMaskSnapshotAndClosedLayer(t *testing.T) {
	s, nodes := constantLayerFixture(t)
	_, handle := beginTestGeneration(t, s, "constant-mask-snapshot")
	handle.AddBatch(nodes, nil)
	require.NoError(t, handle.SetNodeIdentityReplacements([]string{nodes[0].ID, nodes[1].ID}))
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	correction, err := s.BeginDerivedCorrection(context.Background(), store_sqlite.DerivedCorrectionRequest{GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	updated := *nodes[0]
	updated.Meta = map[string]any{"source_derived_decl_fingerprint": "changed"}
	require.NoError(t, correction.ReplaceSourceEdges(context.Background(), []string{nodes[1].ID}, nil, []*graph.Node{&updated}))
	// A committed chunk precedes CorrectionEpoch advancement at Finish.
	require.Zero(t, s.GenerationCorrectionEpoch(handle.ViewGeneration()))
	oldView := graph.NewOverlaidViewWithLayer(s, layer)
	projection, err := oldView.LoadContractFileProjectionContext(context.Background(), "repo", []string{nodes[0].FilePath})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Equal(t, graph.ContractFileProjection{}, projection)
	projection, err = oldView.LoadContractIDProjectionContext(context.Background(), []string{nodes[1].ID})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Equal(t, graph.ContractFileProjection{}, projection)
	values, err := graph.NewOverlaidViewWithLayer(s, layer).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.ErrorIs(t, err, graph.ErrConstantProjectionStale)
	require.Nil(t, values)
	_, err = correction.Finish(context.Background())
	require.NoError(t, err)
	fresh, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	values, err = graph.NewOverlaidViewWithLayer(s, fresh).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Equal(t, "old", values[nodes[1].ID])
	require.NoError(t, s.Close())
	values, err = graph.NewOverlaidViewWithLayer(s, layer).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.Error(t, err)
	require.Nil(t, values)
}
