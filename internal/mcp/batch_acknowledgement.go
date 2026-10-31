package mcp

import (
	"context"
	"crypto/sha256"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
)

// bindBatchAcknowledgement belongs exclusively to runBatchTransaction's first
// execute arm. Duplicate/status/recovery calls must not bind their own clocks.
// No timing pointer or payload is retained by the unbounded transaction map.
func (s *Server) bindBatchAcknowledgement(ctx context.Context, state *batchTransactionState) {
	ack := editAcknowledgementFrom(ctx, s)
	if ack == nil {
		return
	}
	state.mu.Lock()
	if state.acknowledgementReceipt != "" {
		state.mu.Unlock()
		return
	}
	id := "batch-ack-" + strconv.FormatUint(ack.id, 10)
	transactionID, checkoutID, incarnation := state.receipt.TransactionID, state.receipt.CheckoutID, state.receipt.Incarnation
	state.acknowledgementReceipt = id
	state.mu.Unlock()
	// No state lock crosses the existing ledger's eviction or record locks.
	// A racing reader between scalar reservation and this put reports missing
	// evidence, never an invented wall. No work is awakened by this helper.
	s.mutationCommits.put(&mutationCommitRecord{
		kind: batchAcknowledgementCommit, id: id, startedAt: ack.entered,
		acknowledgement: ack, batchOwner: sha256.Sum256([]byte(transactionID)),
		checkoutID: checkoutID, checkoutIncarnation: incarnation,
	})
}

// A batch timing receipt is a separate explicit status variant. It carries no
// physical disk/graph verdict and never routes through byReceipt/replay/source
// evidence. The durable transaction remains the authority for those outcomes.
func (s *Server) batchAcknowledgementReceiptPayload(ctx context.Context, id string) (map[string]any, bool) {
	if !strings.HasPrefix(id, "batch-ack-") {
		return nil, false
	}
	checkoutID, incarnation := mutationCheckoutScope(ctx)
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	record := ledger.byID[id]
	var ack *editAcknowledgement
	if record != nil && record.kind == batchAcknowledgementCommit &&
		mutationCheckoutScopeMatches(checkoutID, incarnation, record.checkoutID, record.checkoutIncarnation) {
		record.mu.RLock()
		ack = record.acknowledgement
		record.mu.RUnlock()
	}
	ledger.mu.Unlock()
	payload := map[string]any{
		"found": ack != nil, "receipt": id, "receipt_kind": "batch_acknowledgement",
		"guidance": "timing only; query the batch transaction_id for its durable source and graph outcomes",
	}
	timings := (indexer.PublicationPhaseSnapshot{}).EditTimings()
	payload["phases_ms"], payload["phase_states"], payload["phase_timing"] = timings.Milliseconds, timings.States, timings.Window
	for name := range payload["phase_states"].(map[string]string) {
		payload["phase_states"].(map[string]string)[name] = "not_applicable_shared_rpc"
	}
	var snapshot *editAcknowledgementSnapshot
	if ack != nil {
		copy := ack.snapshot()
		snapshot = &copy
	}
	attachAcknowledgementPayload(payload, snapshot, "unavailable_ledger_entry_or_scope")
	return payload, true
}

// The response-only envelope never enters cloneBatchReceipt or the manifest.
// A reference can outlive its bounded payload; mutation_status reports that
// absence explicitly rather than borrowing a retry/status call's own clock.
type batchAcknowledgementResponse struct {
	batchTransactionReceipt
	AcknowledgementReceipt string                          `json:"acknowledgement_receipt,omitempty"`
	AcknowledgementState   string                          `json:"acknowledgement_state"`
	FileObservations       []batchFileObservationReference `json:"file_observations,omitempty"`
}

func (s *Server) batchAcknowledgementResponse(receipt batchTransactionReceipt) batchAcknowledgementResponse {
	result := batchAcknowledgementResponse{batchTransactionReceipt: receipt, AcknowledgementState: "unavailable_reference"}
	if value, ok := s.batchTransactions.Load(receipt.TransactionID); ok {
		if state, ok := value.(*batchTransactionState); ok {
			state.mu.RLock()
			result.AcknowledgementReceipt = state.acknowledgementReceipt
			state.mu.RUnlock()
			if result.AcknowledgementReceipt != "" {
				result.AcknowledgementState = "reference_only_query_mutation_status"
			}
		}
	}
	result.FileObservations = s.batchFileObservationReferences(receipt)
	return result
}

func (r batchAcknowledgementResponse) referenceLine() string {
	if r.AcknowledgementReceipt != "" {
		return "acknowledgement_receipt=" + r.AcknowledgementReceipt + "\n"
	}
	return "acknowledgement_state=" + r.AcknowledgementState + "\n"
}

type batchAcknowledgementSnapshot struct {
	Reference string
	State     string
	Snapshot  *editAcknowledgementSnapshot
}

func (s *Server) batchAcknowledgement(state *batchTransactionState) batchAcknowledgementSnapshot {
	state.mu.RLock()
	id, transactionID := state.acknowledgementReceipt, state.receipt.TransactionID
	state.mu.RUnlock()
	result := batchAcknowledgementSnapshot{Reference: id, State: "unavailable_reference"}
	if id == "" {
		return result // restarted/raw/legacy transactions do not acquire a new origin
	}
	owner := sha256.Sum256([]byte(transactionID))
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	record := ledger.byID[id]
	var ack *editAcknowledgement
	if record != nil && record.kind == batchAcknowledgementCommit && record.batchOwner == owner {
		record.mu.RLock()
		ack = record.acknowledgement
		record.mu.RUnlock()
	}
	ledger.mu.Unlock()
	result.State = "unavailable_ledger_entry"
	if ack == nil {
		return result
	}
	// Borrow only long enough to produce this owned snapshot. No callback,
	// formatter or encoder executes while the ledger/record locks are held.
	snapshot := ack.snapshot()
	result.State, result.Snapshot = "observed", &snapshot
	return result
}
