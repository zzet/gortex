package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// A publication record is handed to the refresh admission on the request
// context, so the coordinator binds the ticket to it before it wakes the cycle
// that serves it. A record bound only after the admission call returned would
// miss every mark that cycle made in between (cycle_started, admitted). The
// tests below make the "in between" deterministic: the fake admission plays
// the coordinator's demand-woken cycle before it returns.

func publicationOffsets(snapshot indexer.PublicationPhaseSnapshot) map[indexer.PublicationPhase]int64 {
	out := map[indexer.PublicationPhase]int64{}
	for _, phase := range snapshot.Phases {
		out[phase.Phase] = phase.OffsetNS
	}
	return out
}

// wakingCheckoutMutation admits like the coordinator: it binds the context's
// record to the ticket, then — before returning — runs the cycle the demand
// wake starts, which marks every record bound at or below its high water.
type wakingCheckoutMutation struct {
	*receiptCheckoutMutation
	sawRecord bool
}

func (m *wakingCheckoutMutation) EnqueueRefresh(ctx context.Context, path string) (*indexer.CheckoutRefreshTicket, error) {
	ticket, err := m.receiptCheckoutMutation.EnqueueRefresh(ctx, path)
	if err != nil {
		return ticket, err
	}
	record := indexer.PublicationRecordFromContext(ctx)
	m.sawRecord = record != nil
	if record != nil {
		record.BindTicket(ticket.Ticket.Generation)
		record.Mark(indexer.PublicationTicketEnqueued)
	}
	phases := indexer.DefaultPublicationPhases()
	phases.MarkCheckoutThrough(ticket.CheckoutID, ticket.Ticket.Generation, indexer.PublicationCycleStarted)
	phases.MarkCheckoutThrough(ticket.CheckoutID, ticket.Ticket.Generation, indexer.PublicationAdmitted)
	return ticket, nil
}

func TestMutationPublicationRecordIsBoundBeforeAdmissionReturns(t *testing.T) {
	inner, done, path := newReceiptCheckoutMutation(t)
	inner.ticket.CheckoutID = "checkout-bind-before-wake"
	mutation := &wakingCheckoutMutation{receiptCheckoutMutation: inner}
	s := &Server{mutationReindexWait: time.Nanosecond}
	ctx := withToolReceivedAt(context.Background(), time.Now())
	ctx, _ = withMutationCommitNote(ctx)
	ctx = withCheckoutMutation(ctx, mutation, filepath.Dir(path))
	data := []byte("package committed\n")
	record, err := s.commitFileMutation(ctx, "write_file", "", "", "edit.go", path, data, 0o600)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	inner.ticket.ContentHash = hex.EncodeToString(sum[:])

	outcome := s.mutationReindexState(ctx, path)
	require.True(t, outcome.Pending, "%+v", outcome)
	require.True(t, mutation.sawRecord, "the edit's publication record was not on the admission context")
	record.recordGraph(outcome)

	status := s.mutationStatusPayload(record)
	pending, ok := status["publication_phases"].(indexer.PublicationPhaseSnapshot)
	require.True(t, ok, "the mutation receipt renders no publication phases: %#v", status)
	require.Equal(t, outcome.Receipt, pending.Key, "the interim record was not filed under the receipt id")
	require.EqualValues(t, 7, pending.Ticket)
	offsets := publicationOffsets(pending)
	for _, want := range []indexer.PublicationPhase{
		indexer.PublicationReceived, indexer.PublicationReceiptCommitted, indexer.PublicationTicketEnqueued,
		indexer.PublicationCycleStarted, indexer.PublicationAdmitted,
	} {
		require.Contains(t, offsets, want, "the record lacks %s, which the demand-woken cycle marked before admission returned: %+v", want, pending)
	}
	require.LessOrEqual(t, offsets[indexer.PublicationTicketEnqueued], offsets[indexer.PublicationCycleStarted])
	require.LessOrEqual(t, offsets[indexer.PublicationCycleStarted], offsets[indexer.PublicationAdmitted])

	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 31, Reindexed: true}
	waitCheckoutReceipt(t, s, outcome.Receipt)
	require.Eventually(t, func() bool {
		final, _ := s.mutationStatusPayload(record)["publication_phases"].(indexer.PublicationPhaseSnapshot)
		return final.Terminal && final.DirtyGenerationID == 31
	}, 5*time.Second, time.Millisecond, "the ticket's completion never closed the record")
}

// A refused admission closes the record it opened, so no open record lingers
// under an interim key.
func TestMutationPublicationRecordFailsWhenAdmissionIsRefused(t *testing.T) {
	inner, _, path := newReceiptCheckoutMutation(t)
	inner.ticket.CheckoutID = "checkout-refused-admission"
	inner.enqueueErr = indexer.ErrCheckoutRefreshQueueFull
	s := &Server{mutationReindexWait: time.Nanosecond}
	ctx := withCheckoutMutation(context.Background(), inner, filepath.Dir(path))
	outcome := s.mutationReindexState(ctx, path)
	require.Error(t, outcome.Err)
	snapshots := indexer.DefaultPublicationPhases().Snapshot(inner.ticket.CheckoutID)
	require.Len(t, snapshots, 1)
	require.True(t, snapshots[0].Terminal, "a refused admission left its record open: %+v", snapshots[0])
	require.Equal(t, indexer.PublicationTicketFailed, snapshots[0].Phases[len(snapshots[0].Phases)-1].Phase)
}

// proofReusingWaiter is the lifecycle's proof, its ticket, and its
// ticket-after-proof admission.
type proofReusingWaiter struct {
	*provingFreshnessWaiter
	mu         sync.Mutex
	afterProof []indexer.CheckoutFreshProof
	boundAt    []bool
}

func (w *proofReusingWaiter) RequestCheckoutRefreshAfterProof(
	ctx context.Context, checkoutID, root string, proof indexer.CheckoutFreshProof,
) (*indexer.CheckoutRefreshTicket, error) {
	record := indexer.PublicationRecordFromContext(ctx)
	ticket, err := w.RequestCheckoutRefresh(ctx, checkoutID, root)
	if err == nil && record != nil {
		record.BindTicket(ticket.Ticket.Generation)
		indexer.DefaultPublicationPhases().MarkCheckoutThrough(checkoutID, ticket.Ticket.Generation, indexer.PublicationCycleStarted)
		indexer.DefaultPublicationPhases().MarkCheckoutThrough(checkoutID, ticket.Ticket.Generation, indexer.PublicationAdmitted)
	}
	w.mu.Lock()
	w.afterProof = append(w.afterProof, proof)
	w.boundAt = append(w.boundAt, record != nil)
	w.mu.Unlock()
	return ticket, err
}

// A refused proof hands its own sample to the one ticket it falls back to, and
// that ticket's publication record is on the admission context, so the
// demand-woken cycle's marks reach it.
func TestRequireFreshAdmitsTheFallbackTicketAfterTheProofWithABoundRecord(t *testing.T) {
	stack := newViewStack(t)
	sampledAt := time.Now()
	var sequence uint64 = 900_000
	waiter := &proofReusingWaiter{provingFreshnessWaiter: &provingFreshnessWaiter{
		fakeFreshnessWaiter: &fakeFreshnessWaiter{
			answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				sequence++
				ticket := settledTicket(checkoutID, root, uint64(stack.dirty))
				ticket.Ticket.Generation = sequence
				return ticket, nil
			},
		},
		prove: func(checkoutID, root string) (indexer.CheckoutFreshProof, error) {
			return indexer.CheckoutFreshProof{
				Reason: indexer.FreshProofSnapshotDiffers, CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty,
				SampledAt: sampledAt, Sample: 4 * time.Millisecond, Fingerprint: "sampled-by-the-proof",
			}, nil
		},
	}}
	stack.srv.freshnessWaiter = waiter

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.EqualValues(t, 1, waiter.proofs.Load())
	require.Len(t, waiter.observed(), 1, "the refused proof did not fall back to exactly one ticket")
	waiter.mu.Lock()
	afterProof, bound := append([]indexer.CheckoutFreshProof(nil), waiter.afterProof...), append([]bool(nil), waiter.boundAt...)
	waiter.mu.Unlock()
	require.Len(t, afterProof, 1, "the fallback ticket was not admitted through the proof's sample")
	require.Equal(t, "sampled-by-the-proof", afterProof[0].Fingerprint, "the admission was handed another proof")
	require.Equal(t, []bool{true}, bound, "the ticket was admitted without its publication record on the context")

	rider := resultFreshness(t, res)
	key, _ := rider["publication_record"].(string)
	require.NotEmpty(t, key, "rider = %v", rider)
	recorded, ok := indexer.DefaultPublicationPhases().Lookup(key)
	require.True(t, ok)
	snapshot := recorded.Snapshot()
	offsets := publicationOffsets(snapshot)
	for _, want := range []indexer.PublicationPhase{
		indexer.PublicationReceived, indexer.PublicationChangeObserved, indexer.PublicationTicketEnqueued,
		indexer.PublicationCycleStarted, indexer.PublicationAdmitted, indexer.PublicationTicketCompleted,
	} {
		require.Contains(t, offsets, want, "the fresh record lacks %s: %+v", want, snapshot)
	}
	require.True(t, snapshot.Terminal)
}

// Against a real lifecycle whose coordinator loop is running, an MCP edit's
// ticket wakes its cycle by demand; the record the edit opened before
// admission carries that cycle's marks, in order, under the receipt id.
func TestWorktreeEditPublicationRecordCarriesTheDemandWokenCycle(t *testing.T) {
	fixture := newRealCheckoutMutationFixture(t)
	cwd := fixture.worktree
	written := fixture.edit(t, cwd, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func PhasedInWorktree() {}",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	fixture.awaitMutation(t, cwd, written)

	// The fixture's checkout is private to this test, so its one MCP-edit
	// record is this edit's.
	var snapshot indexer.PublicationPhaseSnapshot
	require.Eventually(t, func() bool {
		for _, record := range indexer.DefaultPublicationPhases().Snapshot(fixture.checkoutID) {
			if record.Source == indexer.PublicationSourceMCPEdit && record.Terminal {
				snapshot = record
				return true
			}
		}
		return false
	}, 20*time.Second, 10*time.Millisecond, "the edit's record never closed")
	t.Logf("edit record: %+v", snapshot)
	require.True(t, strings.HasPrefix(snapshot.Key, "mutation-"), "the record was not filed under its graph-refresh receipt: %q", snapshot.Key)
	require.Equal(t, fixture.checkoutID, snapshot.CheckoutID)
	require.Equal(t, indexer.PublicationSourceMCPEdit, snapshot.Source)
	require.NotZero(t, snapshot.Ticket)
	require.Positive(t, snapshot.DirtyGenerationID)
	offsets := publicationOffsets(snapshot)
	order := []indexer.PublicationPhase{
		indexer.PublicationReceived, indexer.PublicationReceiptCommitted, indexer.PublicationTicketEnqueued,
		indexer.PublicationCycleStarted, indexer.PublicationAdmitted, indexer.PublicationPublished,
		indexer.PublicationTicketCompleted,
	}
	previous := int64(-1)
	for _, phase := range order {
		offset, ok := offsets[phase]
		require.True(t, ok, "the edit's record lacks %s: %+v", phase, snapshot.Phases)
		require.GreaterOrEqual(t, offset, previous, "%s precedes the phase before it: %+v", phase, snapshot.Phases)
		previous = offset
	}
}

// An MCP edit's record times every step between the call arriving and its
// disk commit, and the ticket capture after it, in the order they run: a
// slow commit is then attributable to one named step.
func TestWorktreeEditPublicationRecordTimesTheCommitSubPhases(t *testing.T) {
	fixture := newRealCheckoutMutationFixture(t)
	cwd := fixture.worktree
	written := fixture.edit(t, cwd, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func SubPhasedInWorktree() {}",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	fixture.awaitMutation(t, cwd, written)

	var snapshot indexer.PublicationPhaseSnapshot
	require.Eventually(t, func() bool {
		for _, record := range indexer.DefaultPublicationPhases().Snapshot(fixture.checkoutID) {
			if record.Source == indexer.PublicationSourceMCPEdit && record.Terminal {
				snapshot = record
				return true
			}
		}
		return false
	}, 20*time.Second, 10*time.Millisecond, "the edit's record never closed")
	offsets := publicationOffsets(snapshot)
	order := []indexer.PublicationPhase{
		indexer.PublicationReceived, indexer.PublicationViewSelected, indexer.PublicationViewResolved, indexer.PublicationMutationLocked,
		indexer.PublicationMutationAdmitted, indexer.PublicationHandlerStarted, indexer.PublicationParseGated,
		indexer.PublicationWriteSampled, indexer.PublicationWriteValidated, indexer.PublicationRouteWithdrawn,
		indexer.PublicationDiskWriteStarted, indexer.PublicationReceiptCommitted, indexer.PublicationTicketCaptured,
		indexer.PublicationTicketEnqueued,
	}
	previous := int64(-1)
	for _, phase := range order {
		offset, ok := offsets[phase]
		require.True(t, ok, "the edit's record lacks %s: %+v", phase, snapshot.Phases)
		require.GreaterOrEqual(t, offset, previous, "%s precedes the phase before it: %+v", phase, snapshot.Phases)
		previous = offset
	}
}
