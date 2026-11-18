package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractAnalysisRegistryIdentityTracksEveryProjectionInput(t *testing.T) {
	s := openTestStore(t)
	handle, source, shape := isolatedContractPayload(t, s, "/selected", true)
	require.NoError(t, s.AtGeneration(0).AddBatchChecked([]*graph.Node{source}, nil))
	m := newTestMaterializer(s)
	attachment := graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: handle.ViewGeneration(), InputVersion: "v", InputFingerprint: "fp"}
	m.contractAttachmentReader = contractAttachmentReadFunc(func(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
		copyAttachment := attachment
		return &copyAttachment, nil
	})
	input := ContractAnalysisInput{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp"}
	open := func(core graph.Reader) *ContractAnalysisView {
		t.Helper()
		v, err := m.OpenContractAnalysis(t.Context(), core, input)
		require.NoError(t, err)
		t.Cleanup(v.Close)
		return v
	}

	v := open(s.AtGeneration(0))
	id, ok := v.RegistryIdentity(t.Context())
	require.True(t, ok)
	registry, err := contracts.LoadRegistryFromGraphChecked(t.Context(), v.RegistryReader, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.Len(t, registry.ByID("http::GET::route"), 1)

	// Separately derived handles on one store and generation are one input.
	again, ok := open(s.AtGeneration(0)).RegistryIdentity(t.Context())
	require.True(t, ok)
	require.True(t, id.Equal(again))

	// Captured per-repository sources replace the core reader as the source
	// of owner identities.
	reader := v.RegistryReader.(*contractRegistryReader)
	reader.sources = map[string]graph.Reader{"repo": s.AtGeneration(0)}
	sourced, ok := v.RegistryIdentity(t.Context())
	require.True(t, ok)
	require.False(t, id.Equal(sourced))
	other := openTestStore(t)
	for name, source := range map[string]graph.Reader{
		"in-memory":     graph.New(),
		"wrapper":       &contractRegistryCheckedSource{Store: s.AtGeneration(0)},
		"another store": other.AtGeneration(0),
	} {
		reader.sources = map[string]graph.Reader{"repo": source}
		_, ok := v.RegistryIdentity(t.Context())
		require.False(t, ok, name)
	}
	reader.sources = nil
	_, ok = open(graph.New()).RegistryIdentity(t.Context())
	require.False(t, ok, "an in-memory core has no observable revision")

	attachment.CompletedTokens = []string{"token"}
	tokens, ok := open(s.AtGeneration(0)).RegistryIdentity(t.Context())
	require.True(t, ok)
	require.False(t, id.Equal(tokens))
	attachment.CompletedTokens = nil

	// Any committed core write moves the identity, including one that does
	// not touch an owner.
	require.NoError(t, s.AtGeneration(0).AddBatchChecked([]*graph.Node{{ID: "repo/other.go::Other", Kind: graph.KindFunction, FilePath: "repo/other.go", RepoPrefix: "repo"}}, nil))
	moved, ok := v.RegistryIdentity(t.Context())
	require.True(t, ok)
	require.False(t, id.Equal(moved))
	current, ok := open(s.AtGeneration(0)).RegistryIdentity(t.Context())
	require.True(t, ok)
	require.True(t, moved.Equal(current))

	// A payload correction after selection makes the layer's checked
	// projection refuse; it has no reusable identity.
	correction, err := s.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(t.Context(), nil, nil, []*graph.Node{{ID: shape.ID, Kind: shape.Kind, FilePath: shape.FilePath, RepoPrefix: "repo", Meta: map[string]any{"shape": &contracts.Shape{Kind: "string"}}}}))
	_, ok = v.RegistryIdentity(t.Context())
	require.False(t, ok)
	_, err = contracts.LoadRegistryFromGraphChecked(t.Context(), v.RegistryReader, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	_, err = correction.Finish(t.Context())
	require.NoError(t, err)
}
