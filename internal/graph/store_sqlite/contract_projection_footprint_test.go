package store_sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractProjectionFootprintKeepsPhysicalNegativeFrontier(t *testing.T) {
	s := openPayloadStore(t)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	h := s.AtGeneration(17)
	h.AddBatch([]*graph.Node{
		{ID: "r/f.go::Old", Name: "Old", Kind: graph.KindFunction, FilePath: "r/f.go", RepoPrefix: "r"},
		{ID: "r/f.go::C", Name: "C", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"},
	}, []*graph.Edge{{From: "r/f.go::Old", To: "foreign/t.go::Target", Kind: graph.EdgeCalls, FilePath: "recorded/elsewhere.go", Line: 9}})
	require.NoError(t, h.BulkSetConstantValues("r", []graph.ConstantValueRow{{NodeID: "r/f.go::C", FilePath: "r/f.go", Value: "value"}}))
	require.NoError(t, h.ReplaceSemanticBindingTypes("r", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{FilePath: "r/f.go", Line: 4, Name: ""}, TypeName: "Reply"}}))
	require.NoError(t, h.SetFileMasks([]FileMask{{RepoPrefix: "r", FilePath: "r/hidden.go", Mode: OwnershipDelete}, {RepoPrefix: "r", FilePath: "r/f.go", Mode: OwnershipReplace}}))
	require.NoError(t, h.SetNodeIdentityReplacements([]string{"r/f.go::Old"}))
	require.NoError(t, h.SetEdgeSourceMasks([]EdgeSourceMask{{SourceID: "foreign/source.go::Caller", Mode: OwnershipReplace}}))
	s.AtGeneration(18).AddNode(&graph.Node{ID: "r/not-selected.go::Other", Name: "Other", Kind: graph.KindFunction, FilePath: "r/not-selected.go"})
	got, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	require.Len(t, got.Declarations, 2, "all declaration kinds, not only constants, contribute to negative/cap proof")
	require.Equal(t, "C", got.Declarations[0].Name)
	require.Equal(t, "Old", got.Declarations[1].Name)
	require.Equal(t, []ContractProjectionEdge{{From: "r/f.go::Old", To: "foreign/t.go::Target", Kind: "calls", FilePath: "recorded/elsewhere.go", Line: 9}}, got.Edges)
	require.Equal(t, []ContractProjectionConstant{{ID: "r/f.go::C", RepoPrefix: "r", FilePath: "r/f.go"}}, got.Constants)
	require.Equal(t, []graph.SemanticBindingSite{{RepoPrefix: "r", FilePath: "r/f.go", Line: 4}}, got.Bindings)
	require.Len(t, got.Files, 2, "empty masked paths survive even with no physical node")
	require.Equal(t, []NodeIdentityMask{{NodeID: "r/f.go::Old", Kind: NodeIdentityMaskReplace}}, got.Identities)
	require.Equal(t, []EdgeSourceMask{{SourceID: "foreign/source.go::Caller", Mode: OwnershipReplace}}, got.EdgeSources)
	require.Greater(t, got.RetainedBytes, int64(0))
	require.LessOrEqual(t, got.RetainedBytes, int64(65536))
	got.Declarations[0].Name = "caller changed owned result"
	again, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	require.Equal(t, "C", again.Declarations[0].Name)
}

func TestContractProjectionFootprintBoundsBeforeStringScan(t *testing.T) {
	s := openPayloadStore(t)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	h := s.AtGeneration(17)
	h.AddNode(&graph.Node{ID: "r/f.go::A", Name: strings.Repeat("x", 8192), Kind: graph.KindFunction, FilePath: "r/f.go"})
	got, err := h.ContractProjectionFootprintContext(t.Context(), 4096)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Empty(t, got.Declarations)
	require.Zero(t, got.RetainedBytes)
	got, err = h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	require.Len(t, got.Declarations[0].Name, 8192)
	require.LessOrEqual(t, got.RetainedBytes, int64(65536))
}

func TestContractProjectionFootprintRefusesChangedSnapshotWithoutWriterHold(t *testing.T) {
	s := openPayloadStore(t)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	h := s.AtGeneration(17)
	h.AddNode(&graph.Node{ID: "r/f.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "r/f.go"})
	got, err := h.contractProjectionFootprintContext(t.Context(), 65536, func() {
		// The aggregate reads have pinned the read snapshot. A real writer
		// must commit without this reader retaining the global writer gate.
		h.AddNode(&graph.Node{ID: "r/f.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "r/f.go"})
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Empty(t, got.Declarations)
	current, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.NoError(t, err)
	require.Len(t, current.Declarations, 2)
}

func TestContractProjectionFootprintCancellationInvalidMasksAndPhysicalBase(t *testing.T) {
	s := openPayloadStore(t)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	_, err := s.ContractProjectionFootprintContext(t.Context(), 65536)
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	h := s.AtGeneration(17)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.ContractProjectionFootprintContext(ctx, 65536)
	require.ErrorIs(t, err, context.Canceled)
	_, err = h.writerDB.Exec("INSERT INTO generation_file_masks(view_gen,repo_prefix,file_path,ownership_mode) VALUES(17,'r','r/f.go','unknown')")
	require.NoError(t, err)
	got, err := h.ContractProjectionFootprintContext(t.Context(), 65536)
	require.ErrorIs(t, err, ErrGenerationMaskInvalidValue)
	require.Empty(t, got.Files)
}

func TestContractProjectionClockOwnsEqualityWithoutAllocatingHistory(t *testing.T) {
	s := openPayloadStore(t)
	h := s.AtGeneration(22)
	before, err := h.CaptureContractProjectionClock(t.Context())
	require.NoError(t, err)
	require.Nil(t, s.contractProjectionHistory)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	withHistory, err := h.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, withHistory, "enabling bounded history does not change scalar store identity")
	h.AddNode(&graph.Node{ID: "r/f.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "r/f.go"})
	after, err := h.CaptureContractProjectionClock(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, before, after)
}
