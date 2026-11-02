package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreparedProjectionVerificationRequiresHeldExactJournalAfterImage(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	prepared, _ := s.buildFileProjection(ctx, path, raw)
	require.NotNil(t, prepared.entry)
	before := pendingFileProjections.snap.Load()
	_, err = s.verifyPreparedProjection(t.Context(), prepared, 1, prepared.contentSHA256)
	require.Error(t, err, "an otherwise exact after-image is not held-path proof")
	_, err = s.verifyPreparedProjection(ctx, prepared, 1, "different-journal-sha")
	require.Error(t, err)
	entry, err := s.verifyPreparedProjection(ctx, prepared, 1, prepared.contentSHA256)
	require.NoError(t, err)
	require.EqualValues(t, 1, entry.commitSeq)
	require.Equal(t, prepared.contentSHA256, entry.contentSHA256)
	require.NotSame(t, prepared.entry, entry)
	require.Same(t, before, pendingFileProjections.snap.Load(), "verification alone must never publish a member")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.verifyPreparedProjection(canceled, prepared, 1, prepared.contentSHA256)
	require.ErrorIs(t, err, context.Canceled)
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] = ' '
	require.NoError(t, os.WriteFile(path, changed, 0o644))
	_, err = s.verifyPreparedProjection(ctx, prepared, 1, prepared.contentSHA256)
	require.Error(t, err, "journal SHA cannot substitute for exact current after-image verification")
	release()
	_, err = s.verifyPreparedProjection(ctx, prepared, 1, prepared.contentSHA256)
	require.Error(t, err, "a released path proof cannot authorize publication")
}
