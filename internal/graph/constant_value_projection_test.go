package graph

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func constantProjectionFixture(t *testing.T) (*Graph, []*Node) {
	t.Helper()
	g := New()
	nodes := []*Node{{ID: "repo/const.go", Kind: KindFile, FilePath: "repo/const.go", RepoPrefix: "repo"}, {ID: "repo/const.go::A", Kind: KindConstant, Name: "A", FilePath: "repo/const.go", RepoPrefix: "repo"}, {ID: "repo/const.go::B", Kind: KindConstant, Name: "B", FilePath: "repo/const.go", RepoPrefix: "repo"}}
	g.AddBatch(nodes, nil)
	require.NoError(t, g.BulkSetConstantValues("repo", []ConstantValueRow{{NodeID: nodes[1].ID, FilePath: nodes[1].FilePath, Value: "old-A"}, {NodeID: nodes[2].ID, FilePath: nodes[2].FilePath, Value: "old-B"}}))
	require.NoError(t, g.SetFileMetas("repo", []FileMetaRow{{FilePath: nodes[0].FilePath, ContentHash: "accepted-hash", NodeCount: 3}}))
	return g, nodes
}
func TestComposedConstantValuesPartialOverwriteAndAuthoritativeEmpty(t *testing.T) {
	base, nodes := constantProjectionFixture(t)
	sidecar := New()
	dw := NewDeltaWriter(base, sidecar)
	require.NoError(t, dw.BulkSetConstantValues("repo", []ConstantValueRow{{NodeID: nodes[1].ID, FilePath: nodes[1].FilePath, Value: "new-A"}}))
	values, err := dw.ConstantValuesByNodeIDs([]string{nodes[1].ID, nodes[2].ID})
	require.NoError(t, err)
	require.Equal(t, map[string]string{nodes[1].ID: "new-A", nodes[2].ID: "old-B"}, values)
	throughView, err := ConstantValuesByNodeIDsContext(context.Background(), dw.View(), []string{nodes[1].ID, nodes[2].ID})
	require.NoError(t, err)
	require.Equal(t, values, throughView)
	require.NoError(t, dw.DeleteConstantValuesByFiles("repo", []string{nodes[0].FilePath}))
	values, err = dw.ConstantValuesByNodeIDs([]string{nodes[1].ID, nodes[2].ID})
	require.NoError(t, err)
	require.Empty(t, values)
	// The graph stayed identical, but deletion-to-empty must survive pruning.
	payload := dw.Payload(nil)
	require.Equal(t, []string{nodes[0].FilePath}, payload.ReplacePaths)
	require.Equal(t, 0, payload.DroppedPaths)
	inventory, err := sidecar.FileMetasByPaths("repo", []string{nodes[0].FilePath})
	require.NoError(t, err)
	require.Equal(t, "accepted-hash", inventory[nodes[0].FilePath].ContentHash)
	inherited, err := base.ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Equal(t, "old-A", inherited[nodes[1].ID])
}
func TestComposedConstantValuesIdentityEnrichmentAndTombstones(t *testing.T) {
	base, nodes := constantProjectionFixture(t)
	enrichment := NewOverlayLayer()
	// A detached identity re-emission updates metadata without parsing the file.
	replacement := *nodes[1]
	replacement.Meta = map[string]any{"semantic_type": "string"}
	enrichment.nodeByID[replacement.ID] = &replacement
	view := NewOverlaidView(base, enrichment)
	values, err := view.ConstantValuesByNodeIDs([]string{replacement.ID})
	require.NoError(t, err)
	require.Equal(t, "old-A", values[replacement.ID])
	removed := NewOverlayLayer()
	removed.MarkRemoved(nodes[1].Name, nodes[1].ID)
	values, err = NewOverlaidView(base, removed).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Empty(t, values)
	deleted := NewOverlayLayer()
	deleted.MarkFile(nodes[0].FilePath, true)
	values, err = NewOverlaidView(base, deleted).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.NoError(t, err)
	require.Empty(t, values)
}
func TestComposedConstantValuesRefuseAmbiguousReplaceAndMissingAcceptedDelete(t *testing.T) {
	base, nodes := constantProjectionFixture(t)
	unknown := NewOverlayLayer()
	unknown.MarkFile(nodes[0].FilePath, false)
	for _, n := range nodes {
		unknown.AddNode(n.FilePath, n)
	}
	values, err := NewOverlaidView(base, unknown).ConstantValuesByNodeIDs([]string{nodes[1].ID})
	require.ErrorIs(t, err, ErrConstantProjectionIncomplete)
	require.Nil(t, values)
	missing := New()
	missing.AddBatch(nodes, nil)
	dw := NewDeltaWriter(missing, New())
	require.ErrorIs(t, dw.DeleteConstantValuesByFiles("repo", []string{nodes[0].FilePath}), ErrConstantProjectionIncomplete)
	require.ErrorIs(t, dw.ConstantValueReadError(), ErrConstantProjectionIncomplete)
	require.Empty(t, dw.Payload(nil).ReplacePaths)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	values, err = dw.ConstantValuesByNodeIDsContext(ctx, []string{nodes[1].ID})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, values)
}

type constantThinWrapper struct{ Reader }

func (w constantThinWrapper) Unwrap() Reader { return w.Reader }
func TestComposedConstantValuesTraverseThinWrappers(t *testing.T) {
	base, nodes := constantProjectionFixture(t)
	dw := NewDeltaWriter(constantThinWrapper{Reader: base}, New())
	values, err := dw.ConstantValuesByNodeIDsContext(context.Background(), []string{nodes[1].ID})
	require.NoError(t, err)
	require.Equal(t, "old-A", values[nodes[1].ID])
}

func TestComposedConstantValuesDeletedFileDoesNotRestoreInventory(t *testing.T) {
	base, nodes := constantProjectionFixture(t)
	sidecar := New()
	dw := NewDeltaWriter(base, sidecar)
	// Match deleteIncrementalSidecars: graph deletion, metadata, constants.
	dw.EvictFiles([]string{nodes[0].FilePath})
	require.NoError(t, dw.DeleteFileMetasByFiles("repo", []string{nodes[0].FilePath}))
	require.NoError(t, dw.DeleteConstantValuesByFiles("repo", []string{nodes[0].FilePath}))
	key := ConstantFileKey{RepoPrefix: "repo", FilePath: nodes[0].FilePath}
	projection, err := readConstantProjection(context.Background(), dw, []string{nodes[1].ID}, []ConstantFileKey{key})
	require.NoError(t, err)
	require.Empty(t, projection.Files)
	require.Empty(t, projection.Rows)
	metadata, err := sidecar.FileMetasByPaths("repo", []string{nodes[0].FilePath})
	require.NoError(t, err)
	require.Empty(t, metadata)
	require.Equal(t, []string{nodes[0].FilePath}, dw.Payload(nil).DeletePaths)
}
