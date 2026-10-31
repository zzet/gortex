package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// slotRelease is what the recorder saw when a wait gave its dispatch slot
// back: whether the receipt under test existed and had already completed.
type slotRelease struct {
	tracked, completed bool
}

type slotReleaseRecorder struct {
	mu       sync.Mutex
	releases []slotRelease
	called   chan struct{}
}

// recordSlotReleases stands in for the daemon's slot release on s.
func recordSlotReleases(s *Server, receipt func() *mutationReceipt) *slotReleaseRecorder {
	r := &slotReleaseRecorder{called: make(chan struct{}, 8)}
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
		var seen slotRelease
		if tracked := receipt(); tracked != nil {
			seen.tracked = true
			select {
			case <-tracked.done:
				seen.completed = true
			default:
			}
		}
		r.mu.Lock()
		r.releases = append(r.releases, seen)
		r.mu.Unlock()
		r.called <- struct{}{}
		return true
	}
	return r
}

func (r *slotReleaseRecorder) calls() []slotRelease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slotRelease(nil), r.releases...)
}

func (r *slotReleaseRecorder) await(t *testing.T) {
	t.Helper()
	select {
	case <-r.called:
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not give its dispatch slot back")
	}
}

// soleMutationReceipt resolves the one receipt a fresh server tracks.
func soleMutationReceipt(s *Server) func() *mutationReceipt {
	return func() *mutationReceipt {
		var found *mutationReceipt
		s.mutationReceipts.Range(func(_, value any) bool {
			found, _ = value.(*mutationReceipt)
			return false
		})
		return found
	}
}

func TestMutationReindexStateReturnsTheDispatchSlotOnceItOnlyWaits(t *testing.T) {
	for _, exit := range []string{"published", "timer", "canceled"} {
		t.Run(exit, func(t *testing.T) {
			// The marked run keeps its slot, as every rename file but the
			// last does; both runs must answer alike.
			var outcomes [2]mutationReindexOutcome
			for i, marked := range []bool{false, true} {
				path := filepath.Join(t.TempDir(), "edited.go")
				ctx, cancel, intent, w, released, complete := primaryPublicationIntentFixture(t, nil, path)
				wait := time.Minute
				if exit == "timer" {
					wait = 10 * time.Millisecond
				}
				s := &Server{watcher: w, mutationReindexWait: wait}
				recorder := recordSlotReleases(s, soleMutationReceipt(s))
				if marked {
					ctx = withoutDispatchSlotRelease(ctx)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					outcomes[i] = s.mutationReindexState(ctx, path)
				}()
				if marked {
					awaitPrimaryPublicationIntentRelease(t, released)
				} else {
					recorder.await(t)
				}
				switch exit {
				case "published":
					complete()
				case "canceled":
					cancel()
				}
				require.True(t, joinPrimaryPublicationWait(t, done))
				complete()
				intent.close()
				if marked {
					require.Empty(t, recorder.calls(), "a marked wait keeps its dispatch slot")
				} else {
					require.Equal(t, []slotRelease{{tracked: true}}, recorder.calls(),
						"one release, after the ticket was admitted and before it published")
				}
			}
			if exit == "published" {
				require.True(t, outcomes[0].Reindexed)
				require.False(t, outcomes[0].Pending)
			} else {
				require.True(t, outcomes[0].Pending)
				require.NotEmpty(t, outcomes[0].Receipt)
				require.NotEmpty(t, outcomes[1].Receipt)
				outcomes[0].Receipt, outcomes[1].Receipt = "", ""
			}
			require.Equal(t, outcomes[1], outcomes[0], "giving the slot back leaves the edit's answer unchanged")
		})
	}
}

func TestMutationReindexStateKeepsTheDispatchSlotWithoutAnAdmittedWait(t *testing.T) {
	for _, mode := range []string{"nil_ticket", "failed_ticket", "no_watcher", "checkout"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "edited.go")
			require.NoError(t, os.WriteFile(path, []byte("package fixture\n"), 0o600))
			ctx := context.Background()
			s := &Server{mutationReindexWait: time.Nanosecond}
			switch mode {
			case "nil_ticket":
				s.watcher = mutationTestWatcher{reject: true}
			case "failed_ticket":
				s.watcher = mutationTestWatcher{scheduleErr: errors.New("admission refused")}
			case "checkout":
				var mutation *receiptCheckoutMutation
				mutation, _, path = newReceiptCheckoutMutation(t)
				ctx = withCheckoutMutation(ctx, mutation, filepath.Dir(path))
			}
			recorder := recordSlotReleases(s, soleMutationReceipt(s))
			s.mutationReindexState(ctx, path)
			require.Empty(t, recorder.calls())
		})
	}
}

func TestWaitBatchGraphReceiptsReturnsTheDispatchSlotOnlyWhileAReceiptIsPending(t *testing.T) {
	track := func(t *testing.T, s *Server, path string, generation uint64) (*mutationReceipt, func()) {
		t.Helper()
		result := make(chan indexer.MutationResult, 1)
		receipt := s.trackMutationTicket(&indexer.MutationTicket{Path: path, Generation: generation, Done: result})
		publish := sync.OnceFunc(func() {
			result <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
			close(result)
		})
		t.Cleanup(publish)
		return receipt, publish
	}
	published := func(t *testing.T, receipt *mutationReceipt) {
		t.Helper()
		select {
		case <-receipt.done:
		case <-time.After(2 * time.Second):
			t.Fatal("published receipt did not complete")
		}
	}
	dir := t.TempDir()
	caller, target := filepath.Join(dir, "caller.go"), filepath.Join(dir, "target.go")

	t.Run("completed_then_pending", func(t *testing.T) {
		s := &Server{mutationReindexWait: time.Minute}
		first, publishFirst := track(t, s, caller, 1)
		second, publishSecond := track(t, s, target, 2)
		publishFirst()
		published(t, first)
		recorder := recordSlotReleases(s, func() *mutationReceipt { return second })
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.waitBatchGraphReceipts(context.Background(), []batchTransactionFile{
				{Path: caller, ReindexReceipt: first.id},
				{Path: target, ReindexReceipt: second.id},
			})
		}()
		recorder.await(t)
		publishSecond()
		require.True(t, joinPrimaryPublicationWait(t, done))
		require.Equal(t, []slotRelease{{tracked: true}}, recorder.calls(), "one release, while the pending receipt waits")
	})

	t.Run("nothing_pending", func(t *testing.T) {
		s := &Server{mutationReindexWait: time.Minute}
		first, publishFirst := track(t, s, caller, 1)
		second, publishSecond := track(t, s, target, 2)
		publishFirst()
		publishSecond()
		published(t, first)
		published(t, second)
		recorder := recordSlotReleases(s, func() *mutationReceipt { return nil })
		s.waitBatchGraphReceipts(context.Background(), []batchTransactionFile{{Path: caller}, {Path: target}})
		s.waitBatchGraphReceipts(context.Background(), []batchTransactionFile{
			{Path: caller, ReindexReceipt: first.id},
			{Path: target, ReindexReceipt: second.id},
		})
		require.Empty(t, recorder.calls())
	})

	t.Run("published_while_queued", func(t *testing.T) {
		s := &Server{mutationReindexWait: time.Minute}
		first, publishFirst := track(t, s, caller, 1)
		second, publishSecond := track(t, s, target, 2)
		queued := make(chan struct{})
		// Stands in for a slow-lane admission that stays queued for the whole
		// budget unless the wait it is for ends first.
		s.releaseDispatchSlot = func(_ context.Context, ended <-chan struct{}) bool {
			close(queued)
			select {
			case <-ended:
				return true
			case <-time.After(5 * time.Second):
				return false
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.waitBatchGraphReceipts(context.Background(), []batchTransactionFile{
				{Path: caller, ReindexReceipt: first.id},
				{Path: target, ReindexReceipt: second.id},
			})
		}()
		select {
		case <-queued:
		case <-time.After(2 * time.Second):
			t.Fatal("the batch wait never asked for slow-lane capacity")
		}
		publishFirst()
		published(t, first)
		select {
		case <-done:
			t.Fatal("the batch wait ended while a receipt was still pending")
		case <-time.After(20 * time.Millisecond):
		}
		publishSecond()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the batch wait stayed queued after every receipt published")
		}
	})
}

func TestMutationReindexStateStopsQueueingOncePublished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edited.go")
	ctx, cancel, intent, w, _, complete := primaryPublicationIntentFixture(t, nil, path)
	defer cancel()
	defer func() { complete(); intent.close() }()
	s := &Server{watcher: w, mutationReindexWait: time.Minute}
	queued := make(chan struct{})
	// Stands in for a slow-lane admission that stays queued for the whole
	// budget unless the wait it is for ends first.
	s.releaseDispatchSlot = func(_ context.Context, published <-chan struct{}) bool {
		close(queued)
		select {
		case <-published:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	outcomes := make(chan mutationReindexOutcome, 1)
	go func() { outcomes <- s.mutationReindexState(ctx, path) }()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("the edit wait never asked for slow-lane capacity")
	}
	complete()
	select {
	case outcome := <-outcomes:
		require.NoError(t, outcome.Err)
		require.True(t, outcome.Reindexed)
		require.False(t, outcome.Pending)
		require.Empty(t, outcome.Receipt)
	case <-time.After(time.Second):
		t.Fatal("a published edit stayed queued for slow-lane capacity")
	}
}

func TestRenameReturnsTheDispatchSlotOnlyAtItsLastFile(t *testing.T) {
	dir := t.TempDir()
	writes := make([]*renameFileWrite, 0, 3)
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		abs := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(abs, []byte("package p // old\n"), 0o644))
		writes = append(writes, &renameFileWrite{
			RelPath: name, AbsPath: abs, New: []byte("package p // new\n"), NewSHA: "sha-" + name,
		})
	}
	s := newMutationServer()
	s.watcher = mutationTestWatcher{}
	var lastWritten []bool
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
		content, err := os.ReadFile(writes[len(writes)-1].AbsPath)
		lastWritten = append(lastWritten, err == nil && string(content) == "package p // new\n")
		return true
	}

	results := s.commitRenameWrites(context.Background(), writes)
	require.Len(t, results, len(writes))
	for _, result := range results {
		require.Equal(t, "applied", result["status"])
	}
	require.Equal(t, []bool{true}, lastWritten, "one release, once the last file is written")
}

func TestMutationReindexStateKeepsReceiptWhenSlowWaitAdmissionUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edited.go")
	ctx, cancel, intent, w, _, complete := primaryPublicationIntentFixture(t, nil, path)
	defer cancel()
	defer func() { complete(); intent.close() }()
	committed := []byte("package fixture\nfunc Committed(){}\n")
	require.NoError(t, os.WriteFile(path, committed, 0600))
	s := &Server{watcher: w, mutationReindexWait: time.Minute}
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool { return false }
	outcome := s.mutationReindexState(ctx, path)
	require.True(t, outcome.Pending)
	require.NotEmpty(t, outcome.Receipt)
	require.NoError(t, outcome.Err, "slow lane admission cannot turn committed bytes into a retryable busy error")
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, committed, actual)
}
