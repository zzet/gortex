package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPendingSearchCOWSuccessorKeepsSourceAndCallerFences(t *testing.T) {
	for _, transition := range []string{"checked_same_version", "source_event", "physical_execution", "caller_cancellation", "expired_allowance"} {
		t.Run(transition, func(t *testing.T) {
			s, _, base, path, original := snapshotFacadeFixture(t)
			oldFacts, err := s.bindPendingSearch(base)
			require.NoError(t, err)
			pendingFileProjections.markSuspect(path)
			read := snapshotRegisteredCall(t, s, base, 30, "read", map[string]any{
				"operation": "source", "target": map[string]any{"symbol": "main.go::Target"},
				"context": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"},
			})
			require.False(t, read.IsError, resultText(read))
			current := liveEntry(pendingFileProjections, path)
			require.NotNil(t, current)
			require.NotSame(t, original, current)
			require.Same(t, original.layer, current.layer)
			require.Zero(t, current.commitSeq)
			require.True(t, current.readVersionChecked)
			before := snapshotPhysicalTarget(t, path)
			ctx, cancel := context.WithTimeout(base, projectedReadBudget)
			defer cancel()
			switch transition {
			case "source_event":
				pendingFileProjections.markSuspect(path)
			case "physical_execution":
				ack := snapshotRegisteredCall(t, s, base, 40, "edit", snapshotEditArgs(path, "NeighborBefore", "Neighbor"))
				require.False(t, ack.IsError, resultText(ack))
				require.Equal(t, "local", extractTextResult(t, ack)["ack_mode"])
				require.NotSame(t, current, liveEntry(pendingFileProjections, path))
			case "caller_cancellation":
				cancel()
			case "expired_allowance":
				var expiredCancel context.CancelFunc
				ctx, expiredCancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer expiredCancel()
			}
			selected := checkedPendingSearchCOWSuccessor(ctx, original)
			if transition == "checked_same_version" {
				require.Same(t, current, selected)
			} else {
				require.Nil(t, selected, "a checked historical owner cannot authorize this transition")
			}
			require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
			after := snapshotPhysicalTarget(t, path)
			if transition == "physical_execution" {
				require.NotEqual(t, before, after)
			} else {
				require.Equal(t, before, after)
			}
		})
	}
}

func TestPendingSearchCOWSuccessorRequiresCheckedZeroOrigin(t *testing.T) {
	selectedProjectionRegistry(t)
	s, _, path := canonicalPromotionReadFixture(t)
	raw := []byte("package probe\n\nfunc Target() int { return 7 }\n\nfunc Neighbor() int { return 9 }\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	unchecked, err := projectFile(t.Context(), s.indexer, path, raw, 0, stamp)
	require.NoError(t, err)
	require.NoError(t, pendingFileProjections.publish(unchecked))
	pendingFileProjections.markSuspect(path)
	first, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, first)
	require.True(t, first.readVersionChecked)
	require.False(t, unchecked.readVersionChecked)
	require.Same(t, unchecked.layer, first.layer)
	require.Zero(t, first.sourceCommitSeq)
	ctx, cancel := context.WithTimeout(t.Context(), projectedReadBudget)
	defer cancel()
	require.Nil(t, checkedPendingSearchCOWSuccessor(ctx, unchecked), "unverified zero provenance cannot authorize a handoff")
	pendingFileProjections.markSuspect(path)
	second, err := s.projectCurrentTrackedFile(ctx, t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NotSame(t, first, second)
	require.Same(t, first.layer, second.layer)
	require.True(t, second.readVersionChecked)
	require.Zero(t, second.sourceCommitSeq, "a read witness never fabricates physical provenance")
	require.Same(t, second, checkedPendingSearchCOWSuccessor(ctx, first), "both zero-origin owners earned actual checked read publication")
}
