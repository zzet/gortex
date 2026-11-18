package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestPendingRelationBindingRejectsForeignPhysicalRoot(t *testing.T) {
	s, ctx, _ := projectedCarryFixture(t, false)
	_, err := s.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err)
	original := liveEntry(pendingFileProjections, filepath.Join(s.indexer.RootPath(), "caller.go"))
	require.NotNil(t, original)
	raw, err := os.ReadFile(original.absPath)
	require.NoError(t, err)
	foreignPath := filepath.Join(t.TempDir(), "caller.go")
	require.NoError(t, os.WriteFile(foreignPath, raw, 0600))
	stamp, err := statDiskStamp(foreignPath)
	require.NoError(t, err)
	foreign := original.committedCopy(19, stamp)
	foreign.absPath, foreign.key = foreignPath, pathkey.MutationKey(foreignPath)
	pendingFileProjections.drop(original)
	require.NoError(t, pendingFileProjections.publish(foreign))
	foreign.suspect.Store(true)
	selected, err := s.bindPendingRelationReads(ctx)
	require.NoError(t, err)
	require.Empty(t, requestProjectionLayers(selected))
	require.False(t, s.hasRequestProjectionFact(selected, foreign.graphPath))
	require.True(t, foreign.suspect.Load(), "an equal hash/prefix in another physical root cannot consume its invalidation")
	require.Same(t, foreign, liveEntry(pendingFileProjections, foreignPath))
}

func TestPendingRelationBindingKeepsOriginalAllowanceAndCancellation(t *testing.T) {
	s, ctx, _ := projectedCarryFixture(t, false)
	path := filepath.Join(s.indexer.RootPath(), "caller.go")
	entry, err := s.projectCurrentTrackedFile(t.Context(), ctx, path, "caller.go", "caller.go")
	require.NoError(t, err)
	require.NotNil(t, entry, "the expired-budget case starts with a real published pending entry")
	originalDeadline := time.Now().Add(-time.Millisecond)
	allowance := &projectedSourceAllowance{tool: "get_call_chain"}
	allowance.attempts.Store(2)
	exhausted := context.WithValue(ctx, projectedSourceDeadlineKey{}, originalDeadline)
	exhausted = context.WithValue(exhausted, projectedSourceAllowanceKey{}, allowance)
	selected, err := s.bindPendingRelationReads(exhausted)
	require.NoError(t, err)
	require.Equal(t, originalDeadline, selected.Value(projectedSourceDeadlineKey{}))
	require.EqualValues(t, 2, allowance.attempts.Load())
	require.False(t, allowance.claim(), "relation binding cannot revive an exhausted whole-call allowance")
	require.Empty(t, requestProjectionLayers(selected))
	require.Equal(t, "external_write", requestDeferredProjectionFiles(selected)["caller.go"])
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.bindPendingRelationReads(cancelled)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPendingRelationBindingCannotReviveOldRequestFacts(t *testing.T) {
	s, ctx, _ := projectedCarryFixture(t, false)
	pinned, err := s.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err)
	path := filepath.Join(s.indexer.RootPath(), "caller.go")
	old := liveEntry(pendingFileProjections, path)
	require.NotNil(t, old)
	pendingFileProjections.markSuspect(path)
	_, err = s.bindPendingRelationReads(pinned)
	require.ErrorIs(t, err, projectionSuperseded)
	require.True(t, old.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(pinned), projectionSuperseded)
}
