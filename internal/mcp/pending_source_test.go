package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestPendingSourceVersionsSurviveLedgerEviction(t *testing.T) {
	s := &Server{}
	old := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	old.markCommitted("old", 3)
	old.startedAt = time.Now().Add(-2 * mutationCommitRetention)
	newer := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	newer.markCommitted("new", 3)
	_, found := s.mutationCommits.byReceipt(old.id)
	require.False(t, found)
	require.Equal(t, []string{"/repo/a.go"}, s.pendingSourcePaths("", ""))
	old.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Equal(t, []string{"/repo/a.go"}, s.pendingSourcePaths("", ""), "older publication must not clear newer version")
	newer.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Empty(t, s.pendingSourcePaths("", ""))
}

func TestPendingSourceFailureIsNotPublication(t *testing.T) {
	s := &Server{}
	record := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	require.Equal(t, []string{"/repo/a.go"}, s.pendingSourcePaths("", ""), "registered before write")
	record.markCommitted("new", 3)
	record.recordGraph(mutationReindexOutcome{})
	require.NotEmpty(t, s.pendingSourcePaths("", ""))
	rejected := s.beginMutationCommit(context.Background(), "edit", "", "", "b.go", "/repo/b.go")
	rejected.markNotApplied(context.Canceled)
	require.Equal(t, []string{"/repo/a.go"}, s.pendingSourcePaths("", ""))
}

func TestPendingSourcePublicationOutlivesReceipt(t *testing.T) {
	s := &Server{}
	record := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	record.markCommitted("new", 3)
	receipt := &mutationReceipt{id: "ticket", completed: true, result: indexer.MutationResult{Reindexed: true, AppliedGeneration: 7}}
	s.mutationReceipts.Store(receipt.id, receipt)
	// Completion can precede the handler binding its pending outcome.
	record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: receipt.id, Generation: 7})
	s.mutationReceipts.Delete(receipt.id)
	require.Empty(t, s.pendingSourcePaths("", ""))
}
