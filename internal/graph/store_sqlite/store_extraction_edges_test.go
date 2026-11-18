package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractExtractionEdgesPreserveFullPayloadAndPhysicalGeneration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "extraction.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	const id = "repo/source.go::Caller"
	edges := []*graph.Edge{
		{From: id, To: "repo/target.go::Bean", Kind: graph.EdgeCalls, FilePath: "foreign.go", Line: 17, Confidence: .9, CrossRepo: true, Meta: map[string]any{"via": "spring.Bean", "nested": map[string]any{"kept": true}}},
		{From: id, To: "unresolved::TOKEN", Kind: graph.EdgeConsumes, FilePath: "source.go", Line: 18, Meta: map[string]any{graph.MetaDIToken: "TOKEN", "via": "@Inject"}},
		{From: id, To: "unresolved::OTHER", Kind: graph.EdgeProvides, FilePath: "source.go", Line: 19, Meta: map[string]any{graph.MetaDIToken: "OTHER", graph.MetaDIBinding: graph.DIBindingUseValue}},
		{From: id, To: "repo/target.go::Ignored", Kind: graph.EdgeReferences, FilePath: "source.go", Line: 20},
	}
	require.NoError(t, s.AddBatchChecked(nil, edges))
	other := s.AtGeneration(7)
	changed := *edges[0]
	changed.Meta = map[string]any{"via": "changed-generation"}
	require.NoError(t, other.AddBatchChecked(nil, []*graph.Edge{&changed}))
	for _, store := range []*Store{s, other} {
		full, truncated, err := store.GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), []string{id}, 10)
		require.NoError(t, err)
		require.False(t, truncated)
		var want []*graph.Edge
		for _, edge := range full[id] {
			if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
				want = append(want, edge)
			}
		}
		selected, truncated, err := store.ContractExtractionEdgesContext(t.Context(), []string{id, id}, 10)
		require.NoError(t, err)
		require.False(t, truncated)
		require.ElementsMatch(t, want, selected[id], "metadata, promoted fields, recording provenance and generation must equal the full decoder")
	}
	rows, truncated, err := s.ContractExtractionEdgesContext(t.Context(), []string{id}, 2)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, rows[id], 2, "the physical limit sentinel is retained, never a complete answer")
	rows, truncated, err = s.ContractExtractionEdgesContext(t.Context(), []string{id}, 0)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, _, err = s.ContractExtractionEdgesContext(ctx, []string{id}, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}

func TestContractExtractionEdgesSkipOnlyUnconsumedKindsBeforeDecode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "extraction-kind.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	const id = "repo/source.go::Caller"
	require.NoError(t, s.AddBatchChecked(nil, []*graph.Edge{
		{From: id, To: "ignored", Kind: graph.EdgeReferences},
		{From: id, To: "needed", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
	}))
	// An invalid unused payload proves that the projection filters in SQL,
	// rather than materializing all full metadata before discarding it.
	_, err = s.writerDB.Exec(`UPDATE edges SET meta = ? WHERE view_gen = 0 AND kind = 'references'`, []byte{0xff, 0x01})
	require.NoError(t, err)
	rows, truncated, err := s.ContractExtractionEdgesContext(t.Context(), []string{id}, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, rows[id], 1)
	require.Equal(t, "spring.Bean", rows[id][0].Meta["via"])
	_, _, err = s.GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), []string{id}, 10)
	require.Error(t, err, "the unchanged full read still validates its own complete payload")
	_, err = s.writerDB.Exec(`UPDATE edges SET meta = ? WHERE view_gen = 0 AND kind = 'calls'`, []byte{0xff, 0x01})
	require.NoError(t, err)
	rows, _, err = s.ContractExtractionEdgesContext(t.Context(), []string{id}, 10)
	require.Error(t, err, "a consumed candidate's decode error cannot become absence")
	require.Nil(t, rows)
}
