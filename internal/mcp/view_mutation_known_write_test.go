package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/indexer"
)

// The edit's ticket is admitted with the file it wrote and the bytes it wrote,
// so the capture sample can prove that file by content (one git status)
// instead of the fence.
func TestEditTicketCarriesTheCommittedWriteToItsCapture(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pkg", "edit.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("package pkg\n"), 0o644))
	state := &checkoutMutationState{root: resolveNearestExistingAncestor(root), committedHash: "abc123", committedPath: path}

	rel, sha, ok := gitstate.KnownWriteFrom(withCommittedWrite(context.Background(), state, path))
	require.True(t, ok, "the capture context names no known write")
	require.Equal(t, "pkg/edit.go", rel)
	require.Equal(t, "abc123", sha)

	// A different file than the one committed carries nothing.
	_, _, ok = gitstate.KnownWriteFrom(withCommittedWrite(context.Background(), state, filepath.Join(root, "other.go")))
	require.False(t, ok)
}

type knownWriteRecordingMutation struct {
	*receiptCheckoutMutation
	rel, sha string
	ok       bool
}

func (m *knownWriteRecordingMutation) EnqueueRefresh(ctx context.Context, path string) (*indexer.CheckoutRefreshTicket, error) {
	m.rel, m.sha, m.ok = gitstate.KnownWriteFrom(ctx)
	return m.receiptCheckoutMutation.EnqueueRefresh(ctx, path)
}

// Through the real commit and admission path: the refresh admission receives
// the committed file and its bytes' SHA-256.
func TestEditAdmissionHandsTheCommittedWriteToTheCapture(t *testing.T) {
	inner, done, path := newReceiptCheckoutMutation(t)
	mutation := &knownWriteRecordingMutation{receiptCheckoutMutation: inner}
	s := &Server{mutationReindexWait: time.Nanosecond}
	ctx := withToolReceivedAt(context.Background(), time.Now())
	ctx, _ = withMutationCommitNote(ctx)
	ctx = withCheckoutMutation(ctx, mutation, filepath.Dir(path))
	data := []byte("package committed\n")
	_, err := s.commitFileMutation(ctx, "write_file", "", "", "edit.go", path, data, 0o600)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	inner.ticket.ContentHash = hex.EncodeToString(sum[:])
	outcome := s.mutationReindexState(ctx, path)
	require.NoError(t, outcome.Err)
	require.True(t, mutation.ok, "the admission context named no known write")
	require.Equal(t, filepath.Base(path), mutation.rel)
	require.Equal(t, hex.EncodeToString(sum[:]), mutation.sha)
	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 31, Reindexed: true}
	waitCheckoutReceipt(t, s, outcome.Receipt)
}
