package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestPendingSourceLoadedReceiptCannotOverwriteRebinding(t *testing.T) {
	for _, event := range []string{"fresh completion", "failed completion", "recovery marker", "historical repair"} {
		t.Run(event, func(t *testing.T) {
			s := &Server{}
			record := checkoutSourceVersion(s, "checkout", "incarnation", "/fixture/file.go")
			record.markCommitted("bytes", 5)
			old := pendingFreshnessReceipt(s, "old", "fixture", "/fixture/file.go", 1)
			newer := pendingFreshnessReceipt(s, "new", "fixture", "/fixture/file.go", 2)
			for _, receipt := range []*mutationReceipt{old, newer} {
				receipt.checkoutID, receipt.checkoutIncarnation, receipt.checkoutScoped = "checkout", "incarnation", true
			}
			record.recordGraph(checkoutOutcome(record, mutationReindexOutcome{Pending: true, Receipt: old.id}))
			if event == "historical repair" {
				record.recordGraph(checkoutOutcome(record, mutationReindexOutcome{Receipt: old.id, Err: context.DeadlineExceeded}))
			}
			// The refresher has sampled/loaded old, then pauses before applying.
			sampled := record.snapshot()
			record.recordGraph(checkoutOutcome(record, mutationReindexOutcome{Pending: true, Receipt: newer.id}))
			result := indexer.MutationResult{Reindexed: true, AppliedGeneration: 1}
			if event == "failed completion" || event == "recovery marker" {
				result = indexer.MutationResult{Err: context.DeadlineExceeded}
			}
			completeFreshnessReceipt(old, result)
			if event == "recovery marker" {
				old.mu.Lock()
				old.barrierRecoveredGeneration = 2
				old.mu.Unlock()
			}
			// Resume the actual loaded-receipt consumer, with no scheduling or
			// production pause hook needed to make this interleaving exact.
			s.applyPendingSourceReceipt(record, sampled, old)
			snap := record.snapshot()
			require.Equal(t, newer.id, snap.ReindexReceipt)
			require.Equal(t, mutationGraphPending, snap.GraphStatus)
			record.mu.RLock()
			recovered := record.pendingSourceRecovered
			record.mu.RUnlock()
			require.False(t, recovered)
			require.NotEmpty(t, s.pendingSourcePaths("checkout", "incarnation"), "old evidence must not retire the newer pending binding")
			require.Equal(t, 1, pendingSourceVersionCount(s, "checkout"))
		})
	}
}

func TestPendingSourceRebindingClearsPriorReceiptRecovery(t *testing.T) {
	s := &Server{}
	record, old := unobservedFailedSource(s, "checkout", "incarnation", "/fixture/file.go", 1)
	old.mu.Lock()
	old.barrierRecoveredGeneration = 2
	old.mu.Unlock()
	s.refreshPendingSourceRecord(record)
	record.mu.RLock()
	recovered := record.pendingSourceRecovered
	record.mu.RUnlock()
	require.True(t, recovered, "the old binding has actual recovery evidence")

	newer := pendingFreshnessReceipt(s, "new", "fixture", "/fixture/file.go", 3)
	newer.checkoutID, newer.checkoutIncarnation, newer.checkoutScoped = "checkout", "incarnation", true
	record.recordGraph(checkoutOutcome(record, mutationReindexOutcome{Pending: true, Receipt: newer.id}))
	record.mu.RLock()
	recovered = record.pendingSourceRecovered
	record.mu.RUnlock()
	require.False(t, recovered, "recovery belongs to the old receipt binding")
	require.NotEmpty(t, s.pendingSourcePaths("checkout", "incarnation"))
	require.Equal(t, 1, pendingSourceVersionCount(s, "checkout"))
}
