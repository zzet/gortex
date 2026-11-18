package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBasePinAcceptedCurrentRejectsStableApplyAndRequiresAcceptedSource(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "repo", "one")
	unwitnessed := m.AcquireBaseCorpus("repo")
	require.ErrorIs(t, unwitnessed.ValidateAcceptedCurrent(), ErrBaseCorpusUnwitnessed)
	unwitnessed.Release()
	_, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a")
	require.NoError(t, err)
	accepted := m.AcquireBaseCorpus("repo")
	defer accepted.Release()
	require.NoError(t, accepted.ValidateAcceptedCurrent())
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	require.NoError(t, err)
	defer write.Release()
	applying := m.AcquireBaseCorpus("repo")
	defer applying.Release()
	require.NoError(t, applying.ValidateCurrent(), "unchanged observation alone does not certify acceptance")
	require.ErrorIs(t, applying.ValidateAcceptedCurrent(), ErrBaseCorpusUnwitnessed)
	require.ErrorIs(t, accepted.ValidateAcceptedCurrent(), ErrBaseCorpusChanged)
	require.NoError(t, write.Complete("source-b"))
	require.ErrorIs(t, applying.ValidateAcceptedCurrent(), ErrBaseCorpusChanged)
	current := m.AcquireBaseCorpus("repo")
	require.NoError(t, current.ValidateAcceptedCurrent())
	current.Release()
	require.ErrorIs(t, current.ValidateAcceptedCurrent(), ErrBaseCorpusUnwitnessed)
}
