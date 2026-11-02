package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/agents"
)

func TestBatchRollbackRequiresLiveHeldRecoveryPaths(t *testing.T) {
	for _, mode := range []string{"held", "cancelled-held", "unheld", "released", "wrong-path"} {
		t.Run(mode, func(t *testing.T) {
			s := newAtomicBatchTestServer(t, mutationTestWatcher{})
			path := writeAtomicBatchFixture(t, t.TempDir(), "source.txt", "before\n")
			receipt := prepareAtomicRecoveryFixture(t, s, "held-restore", []string{path}, []string{"before\n"}, []string{"after\n"})
			require.NoError(t, agents.AtomicWriteFile(path, []byte("after\n"), 0o644))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode != "unheld" {
				lockedPath := path
				if mode == "wrong-path" {
					lockedPath = filepath.Join(filepath.Dir(path), "other.txt")
				}
				var release func()
				var err error
				ctx, release, err = acquireMutationPath(ctx, lockedPath)
				require.NoError(t, err)
				defer release()
				if mode == "released" {
					release()
				}
			}
			if mode == "cancelled-held" {
				cancel() // restore is terminalization of already committed bytes
			}
			status, err := s.rollbackBatchReceipt(receipt, ctx)
			current, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			if mode == "held" || mode == "cancelled-held" {
				require.NoError(t, err)
				require.Equal(t, "aborted", status)
				require.Equal(t, "before\n", string(current))
			} else {
				assert.Error(t, err, "restore must prove that its original recovery path set is still held")
				assert.Equal(t, "recovery_conflict", status)
				assert.Equal(t, "after\n", string(current), "a refused restore must not touch source bytes")
			}
			_, physical := s.mutationCommits.recentForPath(path)
			require.False(t, physical, "the durable batch remains the only source authority")
		})
	}
}

func TestBatchForwardWritesKeepTransactionAsOnlySourceAuthority(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	path := writeAtomicBatchFixture(t, t.TempDir(), "source.txt", "before\n")
	ctx, note := withMutationCommitNote(t.Context())
	ctx, ack, _ := s.withEditAcknowledgement(ctx)
	defer ack.finishActual()
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		_, physical := s.mutationCommits.recentForPath(path)
		require.False(t, physical, "a forward file write must not pre-empt the transaction verdict")
		require.Empty(t, note.verdict().Snapshots, "an abandoned batch must use its durable transaction status")
		return agents.AtomicWriteFile(path, content, mode)
	}
	receipt, err := s.runBatchTransaction(ctx, []batchEditItem{atomicFileEdit(path, "before", "after")}, "only-batch-authority")
	require.NoError(t, err)
	require.Equal(t, "committed", receipt.Status)
	require.Equal(t, "committed", receipt.DiskStatus)
	require.Equal(t, "after\n", readAtomicBatchFixture(t, path))
	_, physical := s.mutationCommits.recentForPath(path)
	require.False(t, physical)
	require.Empty(t, note.verdict().Snapshots)
}

func TestBatchSourceOperationsKeepHeldAuthorityThroughTerminalization(t *testing.T) {
	for _, kind := range []sourceMutationOperationKind{batchForwardWrite, batchForwardRemove, batchRestoreWrite, batchRestoreRemove} {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprintf("operation_%d/held_%t", kind, held), func(t *testing.T) {
				s := newAtomicBatchTestServer(t, mutationTestWatcher{})
				path := writeAtomicBatchFixture(t, t.TempDir(), "source.txt", "before\n")
				ctx, note := withMutationCommitNote(t.Context())
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if held {
					var release func()
					var err error
					ctx, release, err = acquireMutationPath(ctx, path)
					require.NoError(t, err)
					defer release()
				}
				cancel() // a prepared transaction must finish after this boundary
				record, err := s.commitFileMutation(ctx, "batch_edit", "", "", "source.txt", path, []byte("after\n"), 0o644, sourceMutationOperation{kind: kind})
				require.Nil(t, record)
				if !held {
					require.ErrorIs(t, err, errMutationNotApplied)
					require.Equal(t, "before\n", readAtomicBatchFixture(t, path))
				} else {
					require.NoError(t, err)
					if kind == batchForwardRemove || kind == batchRestoreRemove {
						_, statErr := os.Stat(path)
						require.ErrorIs(t, statErr, os.ErrNotExist)
					} else {
						require.Equal(t, "after\n", readAtomicBatchFixture(t, path))
					}
				}
				_, physical := s.mutationCommits.recentForPath(path)
				require.False(t, physical)
				require.Empty(t, note.verdict().Snapshots)
			})
		}
	}
}
