package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestEditAcknowledgementIncludesSlowWaitAdmission(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			s := newMutationServer()
			done := make(chan indexer.MutationResult)
			s.watcher = mutationTestWatcher{done: done}
			ctx, ack, _ := s.withEditAcknowledgement(canonicalTimingContext())
			path := filepath.Join(t.TempDir(), "pending.go")
			t.Cleanup(func() {
				close(done)
				s.mutationReceipts.Range(func(_, value any) bool {
					if receipt, ok := value.(*mutationReceipt); ok {
						select {
						case <-receipt.done:
						case <-time.After(5 * time.Second):
							t.Error("owned ticket consumer did not finish")
						}
					}
					return true
				})
			})
			called := false
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
				called = true
				lap := ack.snapshot().ReindexWait
				require.EqualValues(t, 1, lap.Started, "slow-lane admission is part of the actual passive wait")
				require.Zero(t, lap.Completed)
				return false // capacity refusal must leave the committed receipt pending
			}
			if batch {
				receipt := s.trackMutationTicket(&indexer.MutationTicket{Path: path, Generation: 1, Done: done})
				s.waitBatchGraphReceipts(ctx, []batchTransactionFile{{Path: path, ReindexReceipt: receipt.id}})
				require.True(t, receipt.outcome(true).Pending)
			} else {
				commitTimingFixture(t, s, ctx, path, "package p\n")
				outcome := s.mutationReindexState(ctx, path)
				require.NoError(t, outcome.Err)
				require.True(t, outcome.Pending)
				require.NotEmpty(t, outcome.Receipt)
			}
			require.True(t, called)
			lap := ack.snapshot().ReindexWait
			require.EqualValues(t, 1, lap.Started)
			require.EqualValues(t, 1, lap.Completed, "refused passive-wait admission must close the lap")
		})
	}
}
