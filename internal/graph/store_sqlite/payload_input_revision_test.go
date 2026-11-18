package store_sqlite

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestPayloadInputRevisionOwnClocksAndCoreIdentity(t *testing.T) {
	s := openPayloadStore(t)
	selected := s.AtGeneration(11)
	before := selected.PayloadInputRevision()
	s.AddNode(&graph.Node{ID: "base", Kind: graph.KindFunction})
	require.Equal(t, before, selected.PayloadInputRevision(), "unselected base analysis is irrelevant")
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "base.go"}}))
	require.Equal(t, before, selected.PayloadInputRevision(), "unselected base input is irrelevant")
	selected.AddNode(&graph.Node{ID: "selected", Kind: graph.KindFunction})
	afterAnalysis := selected.PayloadInputRevision()
	require.NotEqual(t, before, afterAnalysis)
	require.NoError(t, selected.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "selected.go"}}))
	afterInput := selected.PayloadInputRevision()
	require.NotEqual(t, afterAnalysis, afterInput)
	s.payloadInputAdminRevision.Add(1)
	require.NotEqual(t, afterInput, selected.PayloadInputRevision())
	other := openPayloadStore(t)
	require.NotEqual(t, s.PayloadInputRevision(), other.PayloadInputRevision())
	require.NotEqual(t, s.PayloadInputRevision(), s.AtGeneration(12).PayloadInputRevision())
	var nilStore *Store
	require.Equal(t, PayloadInputRevision{}, nilStore.PayloadInputRevision())
}

func TestPayloadInputRevisionContextCanceledGate(t *testing.T) {
	s := openPayloadStore(t)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.PayloadInputRevisionContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
