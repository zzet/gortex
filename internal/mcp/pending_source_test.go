package mcp

import (
	"context"
	"path/filepath"
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
	require.Equal(t, []string{filepath.Clean("/repo/a.go")}, s.pendingSourcePaths("", ""))
	old.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Equal(t, []string{filepath.Clean("/repo/a.go")}, s.pendingSourcePaths("", ""), "older publication must not clear newer version")
	newer.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Empty(t, s.pendingSourcePaths("", ""))
}

func TestPendingSourceFailureIsNotPublication(t *testing.T) {
	s := &Server{}
	record := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	require.Equal(t, []string{filepath.Clean("/repo/a.go")}, s.pendingSourcePaths("", ""), "registered before write")
	record.markCommitted("new", 3)
	record.recordGraph(mutationReindexOutcome{})
	require.NotEmpty(t, s.pendingSourcePaths("", ""))
	rejected := s.beginMutationCommit(context.Background(), "edit", "", "", "b.go", "/repo/b.go")
	rejected.markNotApplied(context.Canceled)
	require.Equal(t, []string{filepath.Clean("/repo/a.go")}, s.pendingSourcePaths("", ""))
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

func TestPendingSourceRecoveryPreservesHistoricalFailure(t *testing.T) {
	s := &Server{}
	record := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	record.markCommitted("new", 3)
	record.recordGraph(mutationReindexOutcome{Receipt: "failed"})
	receipt := &mutationReceipt{id: "failed", completed: true, barrierRecoveredGeneration: 8}
	s.mutationReceipts.Store(receipt.id, receipt)
	require.Empty(t, s.pendingSourcePaths("", ""))
	require.Equal(t, mutationGraphStale, record.snapshot().GraphStatus, "recovery must not rewrite historical receipt")
}

func TestPendingSourcePrimaryRepairPreservesHistoricalFailure(t *testing.T) {
	s := &Server{}
	record := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	record.markCommitted("new", 3)
	record.recordGraph(mutationReindexOutcome{Receipt: "failed"})
	receipt := &mutationReceipt{id: "failed", completed: true, result: indexer.MutationResult{Reindexed: true}}
	s.mutationReceipts.Store(receipt.id, receipt)
	require.Empty(t, s.pendingSourcePaths("", ""))
	s.mutationReceipts.Delete(receipt.id)
	require.Empty(t, s.pendingSourcePaths("", ""))
	require.Equal(t, mutationGraphStale, record.snapshot().GraphStatus)
}

func TestPendingSourceRecoveryAfterReceiptExpiryOnlyClearsCapturedVersion(t *testing.T) {
	s := &Server{}
	old := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	old.markCommitted("old", 3)
	old.recordGraph(mutationReindexOutcome{Receipt: "expired"})
	eligible := s.failedReceiptsBefore([]string{"a.go"}, "/repo")
	require.Contains(t, eligible, old.id)
	newer := s.beginMutationCommit(context.Background(), "edit", "", "", "a.go", "/repo/a.go")
	newer.markCommitted("new", 3)
	s.resolveReindexedPathReceipts("/repo/a.go", eligible)
	require.Equal(t, []string{filepath.Clean("/repo/a.go")}, s.pendingSourcePaths("", ""))
	require.NotContains(t, s.pendingSourceFiles.versions, old.id)
	require.Contains(t, s.pendingSourceFiles.versions, newer.id)
}

func checkoutSourceVersion(s *Server, checkoutID, incarnation, absPath string) *mutationCommitRecord {
	record := s.beginMutationCommit(context.Background(), "edit", "", "", filepath.Base(absPath), absPath)
	record.mu.Lock()
	record.checkoutID, record.checkoutIncarnation = checkoutID, incarnation
	record.mu.Unlock()
	return record
}

func checkoutOutcome(record *mutationCommitRecord, outcome mutationReindexOutcome) mutationReindexOutcome {
	snap := record.snapshot()
	outcome.checkoutScoped = true
	outcome.checkoutID, outcome.checkoutIncarnation = snap.CheckoutID, snap.CheckoutIncarnation
	return outcome
}

func supersededOutcome(record *mutationCommitRecord) mutationReindexOutcome {
	return checkoutOutcome(record, mutationReindexOutcome{Receipt: record.id + "-ticket", Err: indexer.ErrCheckoutRefreshSuperseded})
}

func freshOutcome(record *mutationCommitRecord) mutationReindexOutcome {
	return checkoutOutcome(record, mutationReindexOutcome{Reindexed: true})
}

func TestPendingSourceSupersededVersionRetiredByNewerFreshCommit(t *testing.T) {
	s := &Server{}
	path := filepath.Clean("/wt/pending.go")
	old := checkoutSourceVersion(s, "co", "inc1", path)
	old.markCommitted("old", 3)
	newer := checkoutSourceVersion(s, "co", "inc1", path)
	newer.markCommitted("new", 3)
	old.recordGraph(supersededOutcome(old))
	newer.recordGraph(checkoutOutcome(newer, mutationReindexOutcome{Pending: true, Receipt: "newer-ticket"}))
	require.Equal(t, []string{path}, s.pendingSourcePaths("co", "inc1"), "pending newer version keeps the path pending")
	newer.recordGraph(freshOutcome(newer))
	require.Empty(t, s.pendingSourcePaths("co", "inc1"))
	require.Empty(t, s.pendingSourceFiles.versions)
	require.Empty(t, s.pendingSourceFiles.freshCommits, "watermark pruned once no older version remains")
	require.Equal(t, mutationGraphFailed, old.snapshot().GraphStatus, "retirement must not rewrite historical receipt")
}

func TestPendingSourceSupersededFailureAfterNewerFreshCommit(t *testing.T) {
	s := &Server{}
	path := filepath.Clean("/wt/pending.go")
	old := checkoutSourceVersion(s, "co", "inc1", path)
	old.markCommitted("old", 3)
	newer := checkoutSourceVersion(s, "co", "inc1", path)
	newer.markCommitted("new", 3)
	newer.recordGraph(freshOutcome(newer))
	old.recordGraph(supersededOutcome(old))
	require.Empty(t, s.pendingSourcePaths("co", "inc1"))
	require.Empty(t, s.pendingSourceFiles.versions)

	// A read between the newer publication and the older failure.
	s = &Server{}
	old = checkoutSourceVersion(s, "co", "inc1", path)
	old.markCommitted("old", 3)
	newer = checkoutSourceVersion(s, "co", "inc1", path)
	newer.markCommitted("new", 3)
	old.recordGraph(checkoutOutcome(old, mutationReindexOutcome{Pending: true, Receipt: "old-ticket"}))
	newer.recordGraph(freshOutcome(newer))
	require.Empty(t, s.pendingSourcePaths("co", "inc1"))
	old.recordGraph(supersededOutcome(old))
	require.Empty(t, s.pendingSourcePaths("co", "inc1"))
	require.Empty(t, s.pendingSourceFiles.versions)
}

func TestPendingSourceCommitOrderDecidesNewerVersion(t *testing.T) {
	s := &Server{}
	path := filepath.Clean("/wt/pending.go")
	first := checkoutSourceVersion(s, "co", "inc1", path)
	second := checkoutSourceVersion(s, "co", "inc1", path)
	second.markCommitted("second", 3)
	first.markCommitted("first", 3)
	second.recordGraph(supersededOutcome(second))
	first.recordGraph(freshOutcome(first))
	require.Empty(t, s.pendingSourcePaths("co", "inc1"), "later disk commit retires the earlier one")

	s = &Server{}
	first = checkoutSourceVersion(s, "co", "inc1", path)
	second = checkoutSourceVersion(s, "co", "inc1", path)
	second.markCommitted("second", 3)
	first.markCommitted("first", 3)
	first.recordGraph(supersededOutcome(first))
	second.recordGraph(freshOutcome(second))
	require.Equal(t, []string{path}, s.pendingSourcePaths("co", "inc1"), "earlier disk commit cannot retire the later one")
}

func TestPendingSourceSupersededVersionNotRetiredWithoutMatchingFreshCommit(t *testing.T) {
	path := filepath.Clean("/wt/pending.go")
	cases := map[string]func(s *Server){
		"newer not applied": func(s *Server) {
			checkoutSourceVersion(s, "co", "inc1", path).markNotApplied(context.Canceled)
		},
		"newer disk failed": func(s *Server) {
			checkoutSourceVersion(s, "co", "inc1", path).markFailed(context.DeadlineExceeded)
		},
		"newer failed publication": func(s *Server) {
			newer := checkoutSourceVersion(s, "co", "inc1", path)
			newer.markCommitted("new", 3)
			newer.recordGraph(checkoutOutcome(newer, mutationReindexOutcome{Receipt: "newer-failed"}))
		},
		"other incarnation": func(s *Server) {
			newer := checkoutSourceVersion(s, "co", "inc2", path)
			newer.markCommitted("new", 3)
			newer.recordGraph(freshOutcome(newer))
		},
		"other path": func(s *Server) {
			newer := checkoutSourceVersion(s, "co", "inc1", filepath.Clean("/wt/other.go"))
			newer.markCommitted("new", 3)
			newer.recordGraph(freshOutcome(newer))
		},
		"other checkout": func(s *Server) {
			newer := checkoutSourceVersion(s, "co2", "inc1", path)
			newer.markCommitted("new", 3)
			newer.recordGraph(freshOutcome(newer))
		},
	}
	for name, newer := range cases {
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			old := checkoutSourceVersion(s, "co", "inc1", path)
			old.markCommitted("old", 3)
			old.recordGraph(supersededOutcome(old))
			newer(s)
			require.Equal(t, []string{path}, s.pendingSourcePaths("co", "inc1"))
			require.Contains(t, s.pendingSourceFiles.versions, old.id)
		})
	}
}

func TestPendingSourceGenericFailureWithoutNewerVersionStaysPending(t *testing.T) {
	s := &Server{}
	path := filepath.Clean("/wt/pending.go")
	old := checkoutSourceVersion(s, "co", "inc1", path)
	old.markCommitted("old", 3)
	old.recordGraph(checkoutOutcome(old, mutationReindexOutcome{Receipt: "failed", Err: context.DeadlineExceeded}))
	require.Equal(t, []string{path}, s.pendingSourcePaths("co", "inc1"))
	require.Equal(t, []string{path}, s.pendingSourcePaths("co", ""))
	require.Contains(t, s.pendingSourceFiles.versions, old.id)
}
