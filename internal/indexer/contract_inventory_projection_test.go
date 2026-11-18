package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func inventoryProjectionFor(core graph.Reader, handles ...*store_sqlite.Store) *contractInventoryProjection {
	return newContractInventoryProjection(map[string]graph.Reader{"r": core}, map[string][]*store_sqlite.Store{"r": handles})
}

func TestContractInventoryProjectionReusesOnlyCurrentOwnedRows(t *testing.T) {
	s := sparseShapeStore(t)
	h := s.AtGeneration(10)
	ctx := t.Context()
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "accepted", Size: 12, NodeCount: 1}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	p := inventoryProjectionFor(h, h)
	require.True(t, p.entries["r"].trusted)
	got, err := p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	got[row.FilePath] = graph.FileMetaRow{ContentHash: "caller mutation"}
	delete(got, row.FilePath)
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 1, p.entries["r"].physicalReads, "unchanged full scans share one actual checked projection")
	row.ContentHash = "new-source"
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 2, p.entries["r"].physicalReads)
	require.NoError(t, h.DeleteFileMetasByFiles("r", []string{row.FilePath}))
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Empty(t, got)
	require.Equal(t, 3, p.entries["r"].physicalReads)
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 4, p.entries["r"].physicalReads)
	require.NoError(t, s.AtGeneration(11).SetFileMetas("r", []graph.FileMetaRow{{FilePath: "r/foreign.go", ContentHash: "unselected"}}))
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, 1, len(got))
	require.Equal(t, 4, p.entries["r"].physicalReads, "unselected generations do not invalidate the captured inventory")
}

func TestContractInventoryProjectionMaskChangeCannotReuseOldVisibility(t *testing.T) {
	s := sparseShapeStore(t)
	base, overlay := s.AtGeneration(10), s.AtGeneration(11)
	ctx := t.Context()
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "base", Size: 1}
	require.NoError(t, base.SetFileMetas("r", []graph.FileMetaRow{row}))
	layer, err := graphview.NewGenerationLayerContext(ctx, overlay)
	require.NoError(t, err)
	reader := graph.NewOverlaidViewWithLayer(base, layer)
	p := inventoryProjectionFor(reader, base, overlay)
	got, err := p.read(ctx, "r")
	require.NoError(t, err)
	require.Contains(t, got, row.FilePath)
	require.NoError(t, overlay.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "r", FilePath: row.FilePath, Mode: store_sqlite.OwnershipDelete}}))
	got, err = p.read(ctx, "r")
	require.Error(t, err, "the immutable layer's changed masks must reach the original checked refusal")
	require.Nil(t, got)
	layer, err = graphview.NewGenerationLayerContext(ctx, overlay)
	require.NoError(t, err)
	current := inventoryProjectionFor(graph.NewOverlaidViewWithLayer(base, layer), base, overlay)
	got, err = current.read(ctx, "r")
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, overlay.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "r", FilePath: row.FilePath, Mode: store_sqlite.OwnershipContext}}))
	got, err = current.read(ctx, "r")
	require.Error(t, err)
	require.Nil(t, got)
	layer, err = graphview.NewGenerationLayerContext(ctx, overlay)
	require.NoError(t, err)
	revealed := inventoryProjectionFor(graph.NewOverlaidViewWithLayer(base, layer), base, overlay)
	got, err = revealed.read(ctx, "r")
	require.NoError(t, err)
	require.Contains(t, got, row.FilePath, "removing the mask reveals current inherited membership")
}

type inventoryOpaqueReader struct {
	graph.Reader
	hash    string
	failure error
	calls   int
}

func (r *inventoryOpaqueReader) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	r.calls++
	value, err := r.Reader.(graph.ConstantValueProjectionReader).ReadConstantValueProjectionContext(ctx, ids, files)
	if err != nil {
		return value, err
	}
	for key, row := range value.Files {
		row.ContentHash = r.hash
		value.Files[key] = row
	}
	return value, r.failure
}

func TestContractInventoryProjectionOpaqueErrorsAndAdmissionFallBack(t *testing.T) {
	s := sparseShapeStore(t)
	h := s.AtGeneration(10)
	ctx := t.Context()
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "accepted", Size: 1}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	opaque := &inventoryOpaqueReader{Reader: h, hash: "first"}
	p := inventoryProjectionFor(opaque, h)
	require.False(t, p.entries["r"].trusted)
	got, err := p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "first", got[row.FilePath].ContentHash)
	opaque.hash = "second"
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "second", got[row.FilePath].ContentHash)
	require.Equal(t, 2, opaque.calls)
	cause := errors.New("physical answer failed")
	opaque.failure = cause
	got, err = p.read(ctx, "r")
	require.ErrorIs(t, err, cause)
	require.Nil(t, got)
	// Admission is bounded before cloning another owned backing allocation.
	row.Errors = strings.Repeat("x", contractFollowupCompactLimit)
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	bounded := inventoryProjectionFor(h, h)
	got, err = bounded.read(ctx, "r")
	require.NoError(t, err)
	require.Len(t, got[row.FilePath].Errors, contractFollowupCompactLimit)
	require.Nil(t, bounded.entries["r"].rows)
	require.Zero(t, bounded.bytes)
}

func TestContractInventoryProjectionTornCaptureAndCancellationAreNotAdmitted(t *testing.T) {
	s := sparseShapeStore(t)
	h := s.AtGeneration(10)
	ctx := t.Context()
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "first", Size: 1}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	p := inventoryProjectionFor(h, h)
	got, err := p.readSnapshot(ctx, "r", func() { row.ContentHash = "after"; require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row})) })
	require.NoError(t, err)
	require.Equal(t, "first", got[row.FilePath].ContentHash)
	require.Nil(t, p.entries["r"].rows, "a mixed capture must not become an unchanged-input proof")
	got, err = p.read(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "after", got[row.FilePath].ContentHash)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = p.read(cancelled, "r")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got, "cached rows cannot hide cancellation")
	p = inventoryProjectionFor(h, h)
	during, cancel := context.WithCancel(ctx)
	got, err = p.readSnapshot(during, "r", cancel)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	require.Nil(t, p.entries["r"].rows)
}
