package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

const (
	payloadRetirementQuantumDuration   = 250 * time.Millisecond
	payloadRetirementQuantumWALPause   = 256 << 20
	payloadRetirementQuantumWriterPoll = time.Millisecond
)

// ErrPayloadRetirementWriteWanted means a real writer preempted a retirement
// quantum. Its interrupted transaction rolled back and can be resumed.
var ErrPayloadRetirementWriteWanted = errors.New("store_sqlite: retirement quantum yielded to a waiting writer")

// PayloadRetirementProgress reports committed work, including empty-source
// proofs that advanced the sealed generation's resume cursor. A refusal never
// reports deletion or catalog removal. Partial progress remains valid with err.
type PayloadRetirementProgress struct {
	RowsDeleted     int64
	ChunksCommitted int64
	StepsAdvanced   int
	CatalogRemoved  bool
}

// RetirePayloadGenerationQuantum performs at most one 16-row payload
// transaction under a 250 ms context, retaining all normal retirement fences.
// The caller must own the shared background build permit; this operation does
// not wait for announced mutations or for the WAL to shrink. Above 256 MiB it
// requests reclaim and refuses service. Existing full/slice APIs are unchanged.
func (s *Store) RetirePayloadGenerationQuantum(ctx context.Context, generationID int64, inUse func(int64) bool) (PayloadRetirementProgress, error) {
	var progress PayloadRetirementProgress
	if ctx == nil {
		return progress, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return progress, err
	}
	if s.coreless() {
		return progress, fmt.Errorf("%w: a retirement quantum needs an open store", ErrCatalogInvalidValue)
	}
	if generationID <= baseViewGeneration {
		return progress, fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	ctx, cancel := context.WithTimeout(ctx, s.retirementQuantum())
	defer cancel()
	if err := s.retirementQuantumAdmission(ctx); err != nil {
		return progress, err
	}
	budget := payloadSweepBudget{maxRows: payloadSweepMinBatch, maxChunks: 1, quantum: &progress}
	err := s.retirePayloadGeneration(ctx, generationID, inUse, budget)
	return progress, err
}

// RetirePayloadGenerationQuantumFenced is RetirePayloadGenerationQuantum for a
// generation the caller's burst already fenced through a full quantum.
//
// The full quantum re-ran its whole preamble every time — two catalog reads,
// the reference check, a fence transaction that re-ran the reference query
// and committed nothing once the row was retiring, and a writer drain — so a
// 16-row quantum took the writer three times. Once this process has fenced,
// sealed and drained a generation, the seal refuses payload writes and the
// drain has run, and the catalog writes that add a route, ref view, base or
// dedicated reference refuse a retiring generation. Not every reference can be
// refused, though: the contract-input term of the reference predicate
// (contractAttachmentReferenceSQL) can turn true through a row written in
// another generation, without any write to this one. The per-quantum check
// never protected the payload from that — the first quantum has already
// deleted rows — so a continuation checks only the in-memory ownership (build
// flight, fold hold, inUse) and runs the quantum transaction, which itself
// rechecks that the row is still retiring; the final catalog delete rechecks
// every reference and refuses a generation referenced again, which the caller
// parks like any other refusal. A generation this process has not fenced takes
// the full quantum.
func (s *Store) RetirePayloadGenerationQuantumFenced(ctx context.Context, generationID int64, inUse func(int64) bool) (PayloadRetirementProgress, error) {
	if ctx == nil || s == nil {
		return s.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
	}
	seal := s.payloadSealIfPresent(generationID)
	if seal == nil || seal.state.Load() != payloadSealRetired {
		return s.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
	}
	row, fenced := seal.sweep.fencedRow()
	if !fenced {
		return s.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
	}
	var progress PayloadRetirementProgress
	if err := ctx.Err(); err != nil {
		return progress, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.retirementQuantum())
	defer cancel()
	if err := s.retirementQuantumAdmission(ctx); err != nil {
		return progress, err
	}
	if s.PayloadBuildFlightActive(generationID) || s.chainFoldHolds(generationID) || (inUse != nil && inUse(generationID)) {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedLeased)
		return progress, fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
	}
	if err := seal.sweep.retirementGate.LockContext(ctx); err != nil {
		return progress, err
	}
	defer seal.sweep.retirementGate.Unlock()
	// Another retiree may have removed the generation while this one waited.
	if current := s.payloadSealIfPresent(generationID); current != seal {
		return s.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
	}
	s.ClearStorageFailure(generationID)
	budget := payloadSweepBudget{maxRows: payloadSweepMinBatch, maxChunks: 1, quantum: &progress}
	err := s.sweepFencedPayloadGeneration(ctx, s.Catalog(), row, seal, budget)
	return progress, err
}

// RetirementPreambles reports how many full retirement preambles this store
// has run (measurement).
func (s *Store) RetirementPreambles() int64 { return s.retirementPreambles.Load() }

// retirementQuantum is the real-time budget of one retirement quantum:
// payloadRetirementQuantumDuration unless a test has raised it.
func (s *Store) retirementQuantum() time.Duration {
	if s.retirementQuantumOverride > 0 {
		return s.retirementQuantumOverride
	}
	return payloadRetirementQuantumDuration
}

func (s *Store) retirementQuantumAdmission(ctx context.Context) error {
	if logBytes, ok := ctx.Value(payloadQuantumLogBytesKey{}).(func() int64); ok {
		return s.retirementQuantumAdmissionAt(ctx, logBytes())
	}
	return s.retirementQuantumAdmissionAt(ctx, s.retirementLogBytes())
}

// payloadQuantumLogBytesKey carries a test's WAL size for a quantum's
// admission checks: a per-call seam like payloadQuantumClockKey.
type payloadQuantumLogBytesKey struct{}

func (s *Store) retirementQuantumAdmissionAt(ctx context.Context, walBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if walBytes > payloadRetirementQuantumWALPause {
		s.RequestWALReclaim()
		return fmt.Errorf("%w: retirement WAL pause", ErrPayloadSweepBudgetExhausted)
	}
	if s.writeMu.waiting() > 0 {
		return ErrPayloadRetirementWriteWanted
	}
	return nil
}

// The quantum already holds writeMu, so its own waiter cannot cancel it.
// Unlike ordinary mutation announcements, a real queued writer interrupts SQL.
func (s *Store) preemptRetirementQuantum(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(payloadRetirementQuantumWriterPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.writeMu.waiting() > 0 {
					cancel(ErrPayloadRetirementWriteWanted)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled); <-done }
}

// The analysis drain has nested child tables. A one-transaction quantum must
// resume within that drain, rather than repeatedly consuming its budget on an
// already-empty pointer. Hints bind stable IDs, never a shrinking list ordinal.
// The retirement gate serializes these hints, and the retired seal forbids new analyses.
func (s *Store) sweepAnalysisGenerationQuantum(ctx context.Context, generationID int64, pass *payloadSweepPass) error {
	state := pass.state
	if !state.analysisPointerDone {
		if err := s.deletePayloadChunks(ctx, generationID, deleteAnalysisPointerChunk(generationID), pass); err != nil {
			return err
		}
		state.analysisPointerDone = true
		pass.budget.quantum.StepsAdvanced++
	}
	for {
		ids, err := s.analysisGenerationIDsForView(ctx, generationID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		present := false
		for _, id := range ids {
			present = present || id == state.analysisID
		}
		if !present {
			state.analysisID, state.analysisPhase = ids[0], 0
		}
		for state.analysisPhase < len(analysisGenerationGCTables) {
			table := analysisGenerationGCTables[state.analysisPhase]
			chunk := deleteAnalysisChildChunkWithLimit(table, state.analysisID, payloadSweepMinBatch)
			if err := s.deletePayloadChunks(ctx, generationID, chunk, pass); err != nil {
				return err
			}
			state.analysisPhase++
			pass.budget.quantum.StepsAdvanced++
		}
		if err := s.deletePayloadChunks(ctx, generationID, deleteAnalysisManifestChunk(state.analysisID), pass); err != nil {
			return err
		}
		state.analysisID, state.analysisPhase = 0, 0
		pass.budget.quantum.StepsAdvanced++
	}
}
