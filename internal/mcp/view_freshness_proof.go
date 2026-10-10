package mcp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// checkoutFreshnessProver is the optional half of the freshness waiter: prove,
// with one working-copy sample taken now, that the route already describes the
// working copy. The checkout lifecycle implements it; a test waiter that does
// not keeps every wait on the ticket path.
type checkoutFreshnessProver interface {
	ProveCheckoutFresh(ctx context.Context, checkoutID, expectedRoot string) (indexer.CheckoutFreshProof, error)
}

// Values of the fresh_via rider field.
const (
	freshViaProof  = "proof"
	freshViaTicket = "ticket"
)

// proveCheckoutFresh runs the proof when the waiter offers one. proved is true
// only when the proof itself settled the wait; fresh is then true. A proof
// that could not decide — any reason, any error — leaves the wait to the
// ticket path, which is the only path that can make a stale route current and
// which classifies identity errors itself.
func (s *Server) proveCheckoutFresh(
	ctx context.Context,
	waiter checkoutFreshnessWaiter,
	checkout store_sqlite.Checkout,
	deadline time.Time,
	trace *freshnessTrace,
) (proved, fresh bool) {
	prover, ok := waiter.(checkoutFreshnessProver)
	if !ok {
		return false, false
	}
	proofCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Any sample begun after this request arrived proves it; concurrent
	// requests of one checkout share one (indexer.WithFreshRequestArrival).
	proofCtx = indexer.WithFreshRequestArrival(proofCtx, trace.arrival())
	proof, err := prover.ProveCheckoutFresh(proofCtx, checkout.CheckoutID, checkout.RootPath)
	trace.noteProof(proof, err)
	if err != nil || !proof.Fresh {
		return false, false
	}
	return true, true
}

// freshnessTrace is what one require_fresh wait did, beyond fresh/reason: which
// path settled it, what the proof saw, and — when a publication was owed — the
// phase record opened for it. It rides on the request context from
// settleRequestFreshness into awaitCheckoutFreshness so the wait's signature
// stays the one its tests drive.
type freshnessTrace struct {
	mu          sync.Mutex
	received    time.Time
	via         string
	proofReason string
	proofSample time.Duration
	proofError  bool
	observedAt  time.Time
	record      *indexer.PublicationPhaseRecord
	// proof is the refused proof the first refresh ticket is admitted after;
	// the lifecycle reuses its sample when it can
	// (indexer.CheckoutFreshProof.ReusableSample). It is handed out once: a
	// re-admission after the tree moved again must sample again.
	proof *indexer.CheckoutFreshProof
}

type freshnessTraceCtxKey struct{}

func withFreshnessTrace(ctx context.Context, trace *freshnessTrace) context.Context {
	if ctx == nil || trace == nil {
		return ctx
	}
	return context.WithValue(ctx, freshnessTraceCtxKey{}, trace)
}

func freshnessTraceFromContext(ctx context.Context) *freshnessTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(freshnessTraceCtxKey{}).(*freshnessTrace)
	return trace
}

func newFreshnessTrace(ctx context.Context, started time.Time) *freshnessTrace {
	received := toolReceivedAt(ctx)
	if received.IsZero() || received.After(started) {
		received = started
	}
	return &freshnessTrace{received: received}
}

// arrival is the instant the request entered the server, zero without a
// trace.
func (t *freshnessTrace) arrival() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.received
}

func (t *freshnessTrace) noteProof(proof indexer.CheckoutFreshProof, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.proofSample = proof.Sample
	switch {
	case err != nil:
		t.proofError = true
	case proof.Fresh:
		t.via = freshViaProof
	default:
		t.proofReason = proof.Reason
		if proof.Reason == indexer.FreshProofSnapshotDiffers && !proof.SampledAt.IsZero() {
			// The first instant the daemon saw the tree differ from the
			// published route. For a filesystem edit nothing earlier exists.
			t.observedAt = proof.SampledAt.Add(proof.Sample)
		}
		// Kept for the first ticket whatever the reason: the lifecycle
		// decides whether its sample can be reused
		// (RequestCheckoutRefreshAfterProof), and samples afresh otherwise.
		kept := proof
		t.proof = &kept
	}
}

// takeReusableProof hands out the refused proof whose sample the next ticket
// may be admitted against, once.
func (t *freshnessTrace) takeReusableProof() (indexer.CheckoutFreshProof, bool) {
	if t == nil {
		return indexer.CheckoutFreshProof{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proof == nil {
		return indexer.CheckoutFreshProof{}, false
	}
	proof := *t.proof
	t.proof = nil
	return proof, true
}

// checkoutFreshnessProofRefresher is the optional half of the waiter that
// admits a ticket against a refused proof's sample instead of taking another
// (indexer.CheckoutLifecycle.RequestCheckoutRefreshAfterProof).
type checkoutFreshnessProofRefresher interface {
	RequestCheckoutRefreshAfterProof(ctx context.Context, checkoutID, expectedRoot string, proof indexer.CheckoutFreshProof) (*indexer.CheckoutRefreshTicket, error)
}

// requestFreshnessTicket admits one refresh ticket for the wait. The wait's
// publication record rides on the context (indexer.WithPublicationRecord), so
// the coordinator binds the ticket to it before it wakes the cycle that will
// serve it. The first admission after a refused proof reuses the proof's
// sample when the waiter can; every other one binds at completion when the
// waiter can, and samples afresh otherwise.
func requestFreshnessTicket(
	ctx context.Context,
	waiter checkoutFreshnessWaiter,
	checkout store_sqlite.Checkout,
	trace *freshnessTrace,
) (*indexer.CheckoutRefreshTicket, error) {
	ctx = indexer.WithPublicationRecord(ctx, trace.beginRecord(checkout.CheckoutID))
	if refresher, ok := waiter.(checkoutFreshnessProofRefresher); ok {
		if proof, reusable := trace.takeReusableProof(); reusable {
			// The first admission right after the proof may be admitted
			// against any sample begun after the request arrived; a
			// re-admission (the tree moved again) binds at completion.
			ctx = indexer.WithFreshRequestArrival(ctx, trace.arrival())
			return refresher.RequestCheckoutRefreshAfterProof(ctx, checkout.CheckoutID, checkout.RootPath, proof)
		}
	}
	if bound, ok := waiter.(checkoutFreshnessBoundRefresher); ok {
		// Any other admission binds at completion: the first publication
		// whose own sample began after it answers the wait, however the
		// tree moves meanwhile. A ticket pinned to a sample of its own would
		// be superseded by every publication of another state, which a
		// working copy saved faster than it builds produces each cycle. Any
		// sample begun after the request arrived decides it.
		ctx = indexer.WithFreshRequestArrival(ctx, trace.arrival())
		return bound.RequestBoundCheckoutRefresh(ctx, checkout.CheckoutID, checkout.RootPath)
	}
	return waiter.RequestCheckoutRefresh(ctx, checkout.CheckoutID, checkout.RootPath)
}

// checkoutFreshnessBoundRefresher is the optional half of the waiter that
// admits a ticket bound at completion
// (indexer.CheckoutLifecycle.RequestBoundCheckoutRefresh).
type checkoutFreshnessBoundRefresher interface {
	RequestBoundCheckoutRefresh(ctx context.Context, checkoutID, expectedRoot string) (*indexer.CheckoutRefreshTicket, error)
}

var freshRecordSequence atomic.Uint64

// beginRecord opens the wait's publication record, once, before its first
// ticket is admitted: the coordinator binds the ticket to it at admission
// (indexer.WithPublicationRecord), ahead of the demand wake, so the cycle's
// first marks cannot miss it.
func (t *freshnessTrace) beginRecord(checkoutID string) *indexer.PublicationPhaseRecord {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.record == nil {
		key := fmt.Sprintf("fresh-%d", freshRecordSequence.Add(1))
		t.record = indexer.DefaultPublicationPhases().Begin(checkoutID, key, indexer.PublicationSourceFreshRequest, t.received)
		t.record.MarkAt(indexer.PublicationChangeObserved, t.observedAt)
	}
	return t.record
}

// noteTicket records that a ticket settled the wait's path. The coordinator
// already bound the ticket to the record at admission; binding again is a
// first-wins no-op kept for waiters that admit tickets without the
// coordinator (test seams), whose records would otherwise stay unbound.
func (t *freshnessTrace) noteTicket(checkoutID string, sequence uint64) {
	if t == nil {
		return
	}
	record := t.beginRecord(checkoutID)
	t.mu.Lock()
	t.via = freshViaTicket
	t.mu.Unlock()
	record.BindTicket(sequence)
	record.Mark(indexer.PublicationTicketEnqueued)
}

func (t *freshnessTrace) noteTicketEnd(fresh bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	record := t.record
	t.mu.Unlock()
	if fresh {
		record.Mark(indexer.PublicationTicketCompleted)
	} else {
		record.Mark(indexer.PublicationTicketFailed)
	}
}

// notePublished records the dirty generation the successful wait's route
// names on the phase record.
func (t *freshnessTrace) notePublished(route store_sqlite.CheckoutRoute) {
	if t == nil {
		return
	}
	t.mu.Lock()
	record := t.record
	t.mu.Unlock()
	record.SetGeneration(route.DirtyGenerationID)
}

// riderFields renders the trace onto the freshness rider. Only what the wait
// actually did is said: no proof ran, nothing about it is rendered.
func (t *freshnessTrace) riderFields(fields map[string]any) {
	if t == nil || fields == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.via != "" {
		fields["fresh_via"] = t.via
	}
	if t.proofSample > 0 {
		fields["fresh_sample_ms"] = float64(t.proofSample.Microseconds()) / 1000
	}
	if t.proofReason != "" {
		fields["fresh_proof_reason"] = t.proofReason
	}
	if t.record != nil {
		fields["publication_record"] = t.record.Snapshot().Key
	}
}

// toolReceivedAt is the instant the tool call entered the MCP middleware,
// zero for a call that did not come through it.
type toolReceivedAtCtxKey struct{}

func withToolReceivedAt(ctx context.Context, at time.Time) context.Context {
	if ctx == nil {
		return ctx
	}
	if _, stamped := ctx.Value(toolReceivedAtCtxKey{}).(time.Time); stamped {
		return ctx
	}
	return context.WithValue(ctx, toolReceivedAtCtxKey{}, at)
}

func toolReceivedAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	at, _ := ctx.Value(toolReceivedAtCtxKey{}).(time.Time)
	return at
}

var pendingEditRecordSequence atomic.Uint64

// openMutationPhases opens the publication record of one MCP edit before its
// refresh ticket is admitted, under an interim key: the receipt id it will be
// read by is minted only after admission. The caller hands the record to the
// admission through indexer.WithPublicationRecord, so the coordinator binds
// the ticket to it (and marks ticket_enqueued) before it wakes the cycle that
// serves it. Its origin is the instant the tool call entered the middleware.
func openMutationPhases(ctx context.Context, checkoutID, path string) (*indexer.PublicationPhaseRecord, string) {
	key := fmt.Sprintf("edit-pending-%d", pendingEditRecordSequence.Add(1))
	record := indexer.DefaultPublicationPhases().Begin(checkoutID, key, indexer.PublicationSourceMCPEdit, toolReceivedAt(ctx))
	// The disk commit happened before admission, on this request: its
	// instant is on the request's commit note (same process, same monotonic
	// clock). Marking it here rather than when a receipt is rendered keeps
	// the record complete for a status reader that never asks for it.
	record.MarkAt(indexer.PublicationReceiptCommitted, committedAtFor(ctx, path))
	record.Absorb(indexer.PublicationStampsFrom(ctx))
	return record, key
}

// failMutationPhases closes a record whose admission was refused.
func failMutationPhases(record *indexer.PublicationPhaseRecord) {
	record.Mark(indexer.PublicationTicketFailed)
}

// beginMutationPhases files the edit's record under its graph-refresh receipt
// once the receipt exists, so the mutation receipt can render it, and closes
// the record when the ticket reports. The ticket was bound at admission; the
// bind here is a first-wins no-op for schedulers that admit without the
// coordinator.
func (s *Server) beginMutationPhases(record *indexer.PublicationPhaseRecord, interimKey string, receipt *mutationReceipt, ticket *indexer.CheckoutRefreshTicket) {
	if record == nil || receipt == nil || ticket == nil || ticket.Ticket == nil {
		return
	}
	indexer.DefaultPublicationPhases().Rekey(interimKey, receipt.id)
	record.BindTicket(ticket.Ticket.Generation)
	record.Mark(indexer.PublicationTicketEnqueued)
	go func() {
		<-receipt.done
		receipt.mu.RLock()
		result := receipt.result
		receipt.mu.RUnlock()
		if result.Err == nil && result.Reindexed {
			record.SetGeneration(int64(result.AppliedGeneration))
			record.Mark(indexer.PublicationTicketCompleted)
			return
		}
		record.Mark(indexer.PublicationTicketFailed)
	}()
}

// committedAtFor returns when this request committed path to disk, zero when
// its commit note holds no committed record for it.
func committedAtFor(ctx context.Context, path string) time.Time {
	note := mutationCommitNoteFrom(ctx)
	if note == nil {
		return time.Time{}
	}
	note.mu.Lock()
	records := append([]*mutationCommitRecord(nil), note.records...)
	note.mu.Unlock()
	var at time.Time
	for _, record := range records {
		record.mu.RLock()
		committed, abs := record.committedAt, record.absPath
		record.mu.RUnlock()
		if committed.IsZero() || !pathkey.EqualPaths(abs, path) {
			continue
		}
		if at.IsZero() || committed.After(at) {
			at = committed
		}
	}
	return at
}

// publicationPhasesPayload renders the publication record kept under a
// graph-refresh receipt, folding in the disk commit instant the commit ledger
// holds (same process, same monotonic clock). Nil when no record exists.
func publicationPhasesPayload(receiptID string, committedAt time.Time) any {
	record, ok := indexer.DefaultPublicationPhases().Lookup(receiptID)
	if !ok {
		return nil
	}
	record.MarkAt(indexer.PublicationReceiptCommitted, committedAt)
	return record.Snapshot()
}
