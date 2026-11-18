package mcp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// Only receipt retention is scheduled here. The clock never certifies bytes
// or changes a publication outcome, and it owns no real timers/goroutines.
type receiptTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []struct {
		at time.Time
		fn func()
	}
}

func (c *receiptTestClock) after(delay time.Duration, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, struct {
		at time.Time
		fn func()
	}{c.now.Add(delay), fn})
}

func (c *receiptTestClock) advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	var ready []func()
	var keep []struct {
		at time.Time
		fn func()
	}
	for _, timer := range c.timers {
		if !timer.at.After(c.now) {
			ready = append(ready, timer.fn)
		} else {
			keep = append(keep, timer)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, fn := range ready {
		fn()
	}
}

func TestPendingSourceRegistryDrainsCanonical(t *testing.T) {
	clock := &receiptTestClock{now: time.Now()}
	s := &Server{mutationReceiptAfterFunc: clock.after}
	edits := 0
	run := func(count int) {
		for range count {
			edits++
			record := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
			record.markCommitted("bytes", 5)
			done := make(chan indexer.MutationResult, 1)
			receipt := s.trackMutationTicket(&indexer.MutationTicket{Path: "/fixture/file.go", Generation: uint64(edits), Done: done})
			record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: receipt.id})
			done <- indexer.MutationResult{RequestedGeneration: uint64(edits), Err: context.DeadlineExceeded}
			close(done)
			<-receipt.done // Completion includes retention registration.
			require.Zero(t, pendingSourceVersionCount(s, ""))
			clock.advance(mutationReceiptRetention + time.Nanosecond)
			_, retained := s.mutationReceipts.Load(receipt.id)
			require.False(t, retained)
			require.Equal(t, mutationGraphFailed, record.snapshot().GraphStatus)
		}
	}
	// Warm the same paths/caches with ten mutation lifecycles, then compare
	// live heap after one thousand. Keep the bounded ledger alive through the
	// measurement: it is not an exclusive-registry allocation estimate.
	run(10)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	run(990)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(baseline.HeapAlloc)
	runtime.KeepAlive(s)
	require.Less(t, growth, int64(1<<20), "live heap grew by %d bytes above the ten-edit baseline", growth)
	require.Zero(t, pendingSourceVersionCount(s, ""))
	require.Empty(t, clock.timers)
	s.mutationCommits.mu.Lock()
	held, ordered := len(s.mutationCommits.byID), len(s.mutationCommits.ordered)
	s.mutationCommits.mu.Unlock()
	require.LessOrEqual(t, held, maxMutationCommits)
	require.LessOrEqual(t, ordered, maxMutationCommits)
	t.Logf("canonical edits=%d; baseline edits=10; heap growth=%d bytes; registry versions=0", edits, growth)
}

func TestOrphanedCommitRecordClockCannotPublish(t *testing.T) {
	for _, checkoutID := range []string{"", "checkout"} {
		for _, state := range []string{"seed", "pending"} {
			t.Run(fmt.Sprintf("%s/%s", checkoutID, state), func(t *testing.T) {
				clock := &receiptTestClock{now: time.Now()}
				s := &Server{mutationReceiptAfterFunc: clock.after}
				ctx := context.WithValue(context.Background(), checkoutMutationContextKey{}, &checkoutMutationState{
					checkoutID: checkoutID, incarnation: "incarnation",
				})
				record := s.beginMutationCommit(ctx, "edit", "", "", "file.go", "/fixture/file.go")
				record.markCommitted("bytes", 5)
				done := make(chan indexer.MutationResult, 1)
				receipt := s.trackScopedMutationTicket(&indexer.MutationTicket{Path: "/fixture/file.go", Generation: 7, Done: done}, "fixture", checkoutID, "incarnation", nil)
				// The handler has not recorded its ticket yet. Completion cannot
				// refresh that binding; its retained outcome will then expire.
				done <- indexer.MutationResult{RequestedGeneration: 7, Err: context.DeadlineExceeded}
				close(done)
				<-receipt.done
				expectedRetention := mutationReceiptRetention
				if checkoutID != "" {
					expectedRetention = mutationCommitRetention
				}
				require.Len(t, clock.timers, 1)
				require.Equal(t, expectedRetention, clock.timers[0].at.Sub(clock.now))
				clock.advance(2*mutationCommitRetention + time.Minute)
				require.Empty(t, clock.timers)
				if state == "pending" {
					record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: receipt.id})
				} else {
					record.mu.Lock()
					record.reindexReceipt = receipt.id
					record.mu.Unlock()
				}
				payload := s.mutationStatusPayload(record)
				require.Equal(t, mutationGraphFailed, payload["graph_status"])
				require.Equal(t, "receipt_lost", payload["reason"])
				require.Equal(t, true, payload["graph_status_terminal"])
				clock.advance(24 * time.Hour)
				require.Equal(t, mutationGraphFailed, record.snapshot().GraphStatus, "time cannot manufacture publication evidence")
				if checkoutID != "" {
					require.Equal(t, 1, pendingSourceVersionCount(s, checkoutID), "no evidence retired the failed checkout version")
				}
			})
		}
	}
}
