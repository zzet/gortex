package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

// The real read-built projection path reuses the offer context when it asks
// the watcher for canonical catch-up. Observation must keep the first ticket
// without becoming backpressure on any later real queue admission.
func TestSharedReceiptObserverKeepsFirstTicketAndNeverBlocksCatchup(t *testing.T) {
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	f := newSharedReceiptFixture(t)
	defer f.Close()
	watcher, ok := f.server.currentWatcher().(*sharedReceiptWatcher)
	require.True(t, ok)
	offer := &sharedReceiptOffer{ticket: make(chan *indexer.MutationTicket, 1)}
	ctx, cancel := context.WithCancel(context.WithValue(f.contexts[0], sharedReceiptOfferKey{}, offer))
	defer cancel()
	path := filepath.Join(f.root, "main.go")
	first, err := watcher.EnqueueToolMutation(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Len(t, offer.ticket, 1, "the exact first native ticket is retained without a consumer")

	var later []*indexer.MutationTicket
	var laterErrors []error
	cancelledAdmissionReturned := false
	joined := make(chan struct{})
	beforeCancel := make(chan struct{})
	go func() {
		defer close(joined)
		for range 3 {
			ticket, err := watcher.EnqueueToolMutation(ctx, path)
			later = append(later, ticket)
			laterErrors = append(laterErrors, err)
		}
		close(beforeCancel)
		<-ctx.Done()
		// Forward the actual canceled-context admission too. Its native
		// result may be cancellation or a committed admission; neither can
		// strand the observer. No result or queue work is fabricated.
		ticket, err := watcher.EnqueueToolMutation(ctx, path)
		later = append(later, ticket)
		laterErrors = append(laterErrors, err)
		cancelledAdmissionReturned = true
	}()
	defer func() {
		cancel()
		// Failure-only cleanup also joins the original buggy observer:
		// drain its observation channel only AFTER the test has failed.
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-joined:
				return
			case <-offer.ticket:
			case <-deadline.C:
				t.Error("owned admission producer did not join during cleanup")
				return
			}
		}
	}()
	select {
	case <-beforeCancel:
	case <-time.After(time.Second):
		t.Fatal("same-context real catch-up admission blocked on a full observation channel")
	}
	cancel()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("canceled-context real admission did not join")
	}
	require.True(t, cancelledAdmissionReturned)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Len(t, later, 4)
	previous := first.Generation
	for i := range 3 {
		require.NoError(t, laterErrors[i])
		require.NotNil(t, later[i], "the observer cannot suppress a real queue admission")
		require.Greater(t, later[i].Generation, previous)
		previous = later[i].Generation
	}
	require.Len(t, offer.ticket, 1, "later admissions cannot replace the first observation")
	require.Same(t, first, <-offer.ticket)
	require.Empty(t, offer.ticket)
}
