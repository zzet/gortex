package mcp

import (
	"context"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckedExternalReadVersionKeepsMutationAndEventFences(t *testing.T) {
	for _, transition := range []string{"verified_read", "new_physical_commit", "ordinary_copy", "later_event", "event_legacy_lookup", "changed_content", "unchecked_removal", "cancelled", "expired", "full_metadata"} {
		t.Run(transition, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, path, physical, raw := acknowledgedCanonicalFile(t)
			prior, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), physical)
			require.NoError(t, err)
			external := []byte(strings.Replace(string(raw), "func Target()", "// external checked version\n\nfunc Target()", 1))
			require.NoError(t, os.WriteFile(path, external, 0600))
			original, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
			require.NoError(t, err)
			require.NotNil(t, original)
			require.Zero(t, original.sourceCommitSeq)
			require.True(t, original.readVersionChecked)
			require.ErrorIs(t, validateRequestProjections(prior), projectionSuperseded, "external version cannot authorize prior physical facts")
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
			ctx, err = retainRequestProjection(ctx, original)
			require.NoError(t, err)
			if transition == "new_physical_commit" {
				held, release, lockErr := acquireMutationPath(t.Context(), path)
				require.NoError(t, lockErr)
				commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, external, 0600)
				release()
				require.NoError(t, writeErr)
				require.Positive(t, commit.commitSeq)
				current := liveEntry(pendingFileProjections, path)
				require.NotNil(t, current)
				require.Equal(t, commit.commitSeq, current.sourceCommitSeq)
				require.False(t, current.readVersionChecked, "physical execution cannot inherit checked-read admission")
			}
			if transition == "changed_content" {
				require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(external), "value := 7", "value := 8", 1)), 0600))
			}
			pendingFileProjections.markSuspect(path)
			build, stop := context.WithDeadline(caller, deadline)
			defer stop()
			current, err := s.projectCurrentTrackedFile(build, t.Context(), path, "main.go", "main.go")
			require.NoError(t, err)
			require.NotNil(t, current)
			require.NotSame(t, original, current)
			require.True(t, current.readVersionChecked)
			if transition == "changed_content" {
				require.NotEqual(t, original.rawHash, current.rawHash)
				require.NotSame(t, original.layer, current.layer)
			}
			switch transition {
			case "ordinary_copy":
				unverified := current.committedCopy(0, current.disk)
				require.False(t, unverified.readVersionChecked)
				require.NoError(t, pendingFileProjections.publish(unverified))
				current = unverified
			case "later_event", "event_legacy_lookup":
				pendingFileProjections.markSuspect(path)
				if transition == "event_legacy_lookup" {
					found, lookupErr := pendingFileProjections.lookup(path)
					require.NoError(t, lookupErr)
					require.Same(t, current, found)
					require.False(t, current.suspect.Load())
				}
			case "unchecked_removal":
				pendingFileProjections.drop(current)
			case "cancelled":
				cancel()
			case "expired":
				ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, time.Now().Add(-time.Nanosecond))
			}
			if transition != "expired" {
				require.True(t, time.Now().Before(deadline), "negative must not accidentally pass by allowance expiry")
			}
			require.Equal(t, transition == "verified_read", retryAfterCheckedProjectionChange(ctx))
			require.Zero(t, allowance.attempts.Load(), "permission cannot itself consume or reset the whole-call allowance")
			if transition == "cancelled" {
				require.ErrorIs(t, validateRequestProjections(ctx), context.Canceled)
			} else {
				require.ErrorIs(t, validateRequestProjections(ctx), projectionSuperseded)
			}
		})
	}
}

func TestCheckedReadWitnessCannotBeCopiedOrRepublished(t *testing.T) {
	selectedProjectionRegistry(t)
	s, path, physical, raw := acknowledgedCanonicalFile(t)
	external := []byte(strings.Replace(string(raw), "func Target()", "// external checked version\n\nfunc Target()", 1))
	require.NoError(t, os.WriteFile(path, external, 0600))
	read, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, read)
	require.True(t, read.readVersionChecked)
	edges := 0
	for range read.layer.Edges() {
		edges++
	}
	require.Equal(t, int64(len(read.layer.FileNodes("main.go")))*projectionNodeBytes+int64(edges)*projectionEdgeBytes+read.names.chargedBytes+24, read.size, "admission witness is charged before publication")
	require.Equal(t, read.size, pendingFileProjections.snap.Load().bytes)
	for _, sequence := range []uint64{0, physical.commitSeq + 1} {
		copied := read.committedCopy(sequence, read.disk)
		require.False(t, copied.readVersionChecked, "every copy must earn a new checked-read publication")
		require.Equal(t, read.size, copied.size)
	}
	pendingFileProjections.drop(read)
	require.ErrorIs(t, pendingFileProjections.publish(read), projectionSuperseded, "ordinary publication cannot recycle a checked-read owner")
	require.Nil(t, liveEntry(pendingFileProjections, path))
}
