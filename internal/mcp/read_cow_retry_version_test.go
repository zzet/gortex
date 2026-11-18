package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadCOWRetryRequiresVerifiedPhysicalSourceVersion(t *testing.T) {
	for _, transition := range []string{"verified_read", "new_physical_commit", "later_event", "event_legacy_lookup", "changed_content", "unchecked_removal", "cancelled", "expired", "full_metadata"} {
		t.Run(transition, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, path, original, raw := acknowledgedCanonicalFile(t)
			require.Positive(t, original.sourceCommitSeq)
			require.Equal(t, original.commitSeq, original.sourceCommitSeq)
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			deadline := time.Now().Add(projectedReadBudget)
			ctx := context.WithValue(caller, projectedSourceDeadlineKey{}, deadline)
			tool := "get_symbol_source"
			if transition == "full_metadata" {
				tool = "get_symbol"
			}
			allowance := &projectedSourceAllowance{tool: tool}
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			ctx, err := retainRequestProjection(ctx, original)
			require.NoError(t, err)

			if transition == "new_physical_commit" {
				held, release, err := acquireMutationPath(t.Context(), path)
				require.NoError(t, err)
				commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0600)
				release()
				require.NoError(t, writeErr)
				require.Greater(t, commit.commitSeq, original.commitSeq)
				physical := liveEntry(pendingFileProjections, path)
				require.Equal(t, commit.commitSeq, physical.sourceCommitSeq)
				// The physical bytes really were committed above. Sharing their
				// identical immutable parse must not erase that new execution.
				shared := original.committedCopy(commit.commitSeq, physical.disk)
				require.Same(t, original.layer, shared.layer)
				require.Equal(t, original.rawHash, shared.rawHash)
				require.NoError(t, pendingFileProjections.publish(shared))
			}
			if transition == "changed_content" {
				require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 55 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
			}
			pendingFileProjections.markSuspect(path)
			build, stop := context.WithDeadline(caller, deadline)
			defer stop()
			current, err := s.projectCurrentTrackedFile(build, t.Context(), path, "main.go", "main.go")
			require.NoError(t, err)
			require.NotNil(t, current)
			require.Zero(t, current.commitSeq)
			require.NotSame(t, original, current)
			require.Same(t, current, liveEntry(pendingFileProjections, path))
			if transition == "new_physical_commit" {
				require.Same(t, original.layer, current.layer)
				require.Equal(t, original.rawHash, current.rawHash)
				require.Greater(t, current.sourceCommitSeq, original.sourceCommitSeq)
			} else if transition != "changed_content" {
				require.Same(t, original.layer, current.layer)
				require.Equal(t, original.sourceCommitSeq, current.sourceCommitSeq)
			}
			switch transition {
			case "later_event", "event_legacy_lookup":
				pendingFileProjections.markSuspect(path)
				if transition == "event_legacy_lookup" {
					found, err := pendingFileProjections.lookup(path)
					require.NoError(t, err)
					require.Same(t, current, found)
					require.False(t, current.suspect.Load())
				}
			case "unchecked_removal":
				pendingFileProjections.drop(current)
				require.False(t, original.checkedRetired.Load())
			case "cancelled":
				cancel()
			case "expired":
				ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, time.Now().Add(-time.Nanosecond))
			}
			require.Equal(t, transition == "verified_read", retryAfterCheckedProjectionChange(ctx))
			require.Zero(t, allowance.attempts.Load(), "permission itself neither runs nor claims another attempt")
			require.Error(t, validateRequestProjections(ctx), "no new proof can revive the original facts")
		})
	}
}

func TestReadCOWSourceVersionCopyAndByteAccounting(t *testing.T) {
	selectedProjectionRegistry(t)
	s, path, physical, _ := acknowledgedCanonicalFile(t)
	charged := physical.size
	require.Equal(t, charged, pendingFileProjections.snap.Load().bytes)
	pendingFileProjections.markSuspect(path)
	read, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.Zero(t, read.commitSeq)
	require.Equal(t, physical.commitSeq, read.sourceCommitSeq)
	require.Equal(t, charged, read.size)
	require.Equal(t, charged, pendingFileProjections.snap.Load().bytes, "COW replacement must not double-charge a payload")
	peer := read.committedCopy(physical.commitSeq+1, read.disk)
	require.Equal(t, physical.commitSeq+1, peer.sourceCommitSeq)
	require.Equal(t, charged, peer.size)
	nextRead := peer.committedCopy(0, peer.disk)
	require.Equal(t, peer.sourceCommitSeq, nextRead.sourceCommitSeq)
	require.Zero(t, nextRead.sourceEvents)
	require.False(t, nextRead.checkedRetired.Load())
	deleted := newProjectionTombstone(path, "main.go", "", 77)
	require.Equal(t, int64(32), deleted.size, "charge source version, source-event revision, deferred ACK retirement revision, and shared state flags")
	require.Equal(t, uint64(77), deleted.sourceCommitSeq)
	require.Equal(t, uint64(77), deleted.committedCopy(0, deleted.disk).sourceCommitSeq)
	require.Equal(t, uint64(78), deleted.committedCopy(78, deleted.disk).sourceCommitSeq)
}

func TestReadCOWWithoutPhysicalSourceVersionCannotGrantRetry(t *testing.T) {
	selectedProjectionRegistry(t)
	s, _, path := canonicalPromotionReadFixture(t)
	raw := []byte("package probe\n\nfunc Target() int { return 7 }\n\nfunc Neighbor() int { return 9 }\n")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	// This is an external read projection, with no original tool execution.
	original, err := projectFile(t.Context(), s.indexer, path, raw, 0, stamp)
	require.NoError(t, err)
	require.NoError(t, pendingFileProjections.publish(original))
	ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, time.Now().Add(projectedReadBudget))
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: "get_symbol_source"})
	ctx, err = retainRequestProjection(ctx, original)
	require.NoError(t, err)
	pendingFileProjections.markSuspect(path)
	current, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotSame(t, original, current)
	require.Same(t, original.layer, current.layer)
	require.Zero(t, current.sourceCommitSeq, "unavailable physical provenance must stay unavailable")
	require.False(t, retryAfterCheckedProjectionChange(ctx))
	require.ErrorIs(t, validateRequestProjections(ctx), projectionSuperseded)
}
