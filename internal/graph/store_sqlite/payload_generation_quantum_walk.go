package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Only the quantum API batches empty proofs. Hints are transaction-local until
// commit; a canceled statement cannot skip an unproved source on resumption.
type payloadQuantumHints struct {
	step                int
	analysisPointerDone bool
	analysisID          int64
	analysisPhase       int
}

type payloadQuantumGuardKey struct{}

func payloadQuantumStatementGuard(ctx context.Context) error {
	if s, ok := ctx.Value(payloadQuantumGuardKey{}).(*Store); ok {
		return s.retirementQuantumAdmission(ctx)
	}
	return ctx.Err()
}

func (s *Store) sweepPayloadQuantumTransaction(ctx context.Context, generationID int64, pass *payloadSweepPass) (err error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if err := s.retirementQuantumAdmission(ctx); err != nil {
		return err
	}
	committed := false
	ctx, stop := s.preemptRetirementQuantum(ctx)
	defer stop()
	defer func() {
		if !committed && err != nil && errors.Is(context.Cause(ctx), ErrPayloadRetirementWriteWanted) {
			err = ErrPayloadRetirementWriteWanted
		}
	}()
	ctx = context.WithValue(ctx, payloadQuantumGuardKey{}, s)
	ctx = context.WithValue(ctx, sweepBatchKey{}, payloadSweepMinBatch)
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	transactionStarted := payloadQuantumNow(ctx)
	if err := payloadQuantumStatementGuard(ctx); err != nil {
		return err
	}
	retiring, err := payloadGenerationRetiringTx(ctx, tx, generationID)
	if err != nil {
		return err
	}
	if !retiring {
		return fmt.Errorf("payload generation gc: generation %d left the retiring state", generationID)
	}
	state := pass.state
	state.mu.Lock()
	hints := payloadQuantumHints{state.step, state.analysisPointerDone, state.analysisID, state.analysisPhase}
	state.mu.Unlock()
	tables := payloadSweepTables()
	totalSteps := 1 + len(generationFTSDocidMaps) + len(tables) + 2
	if hints.step < 0 || hints.step > totalSteps {
		hints.step = 0
	}
	advanced := 0
	var removed int64
walk:
	for hints.step < totalSteps {
		if err := payloadQuantumStatementGuard(ctx); err != nil {
			return err
		}
		var chunk payloadSweepChunk
		switch {
		case hints.step == 0:
			var done bool
			chunk, done, err = nextAnalysisQuantumChunk(ctx, tx, generationID, &hints)
			if err != nil {
				return err
			}
			if done {
				hints.step++
				advanced++
				if payloadQuantumCommitMargin(ctx, transactionStarted) {
					break walk
				}
				continue
			}
		case hints.step <= len(generationFTSDocidMaps):
			chunk = deleteFTSDocidChunk(generationFTSDocidMaps[hints.step-1], generationID)
		case hints.step <= len(generationFTSDocidMaps)+len(tables):
			table := tables[hints.step-1-len(generationFTSDocidMaps)]
			query, e := payloadQuantumDeleteSQL(ctx, tx, table)
			if e != nil {
				return e
			}
			chunk = deleteGenerationRowsChunk(query, generationID)
		case hints.step == totalSteps-2:
			chunk = deleteGenerationRowsChunk(deleteGenerationEdgesSQL, generationID)
		default:
			chunk = deleteGenerationRowsChunk(deleteGenerationNodesSQL, generationID)
		}
		if err := payloadQuantumStatementGuard(ctx); err != nil {
			return err
		}
		removed, err = chunk(ctx, tx)
		if err != nil {
			return err
		}
		if removed > 0 {
			break
		}
		if hints.step == 0 {
			if !hints.analysisPointerDone {
				hints.analysisPointerDone = true
			} else if hints.analysisPhase < len(analysisGenerationGCTables) {
				hints.analysisPhase++
			} else {
				hints.analysisID, hints.analysisPhase = 0, 0
			}
		} else {
			hints.step++
		}
		advanced++
		if payloadQuantumCommitMargin(ctx, transactionStarted) {
			break
		}
	}
	if err := payloadQuantumStatementGuard(ctx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	state.mu.Lock()
	state.step, state.analysisPointerDone, state.analysisID, state.analysisPhase = hints.step, hints.analysisPointerDone, hints.analysisID, hints.analysisPhase
	state.mu.Unlock()
	pass.spend(removed)
	pass.budget.quantum.StepsAdvanced += advanced
	if removed > 0 {
		s.constantInputCounter(generationID).Add(1)
	}
	if hints.step < totalSteps {
		return fmt.Errorf("%w: generation %d", ErrPayloadSweepBudgetExhausted, generationID)
	}
	return nil
}

func nextAnalysisQuantumChunk(ctx context.Context, tx *sql.Tx, generationID int64, hints *payloadQuantumHints) (payloadSweepChunk, bool, error) {
	if !hints.analysisPointerDone {
		return deleteAnalysisPointerChunk(generationID), false, nil
	}
	if err := payloadQuantumStatementGuard(ctx); err != nil {
		return nil, false, err
	}
	// A stable manifest ID, rather than a shrinking list ordinal, is the cursor.
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT generation_id FROM analysis_generations WHERE view_gen = ? ORDER BY generation_id DESC LIMIT 1`, generationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if hints.analysisID != id {
		hints.analysisID, hints.analysisPhase = id, 0
	}
	if hints.analysisPhase < len(analysisGenerationGCTables) {
		return deleteAnalysisChildChunkWithLimit(analysisGenerationGCTables[hints.analysisPhase], id, payloadSweepMinBatch), false, nil
	}
	return deleteAnalysisManifestChunk(id), false, nil
}

func payloadQuantumDeleteSQL(ctx context.Context, tx *sql.Tx, table string) (string, error) {
	if err := payloadQuantumStatementGuard(ctx); err != nil {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return "", err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return "", err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("table has no primary key")
	}
	list := strings.Join(keys, ", ")
	target := list
	if len(keys) > 1 {
		target = "(" + list + ")"
	}
	return `DELETE FROM ` + table + ` WHERE view_gen = ? AND ` + target + ` IN (SELECT ` + list + ` FROM ` + table + ` WHERE view_gen = ? LIMIT ?)`, nil
}

// Reserve best-effort commit time after a completed empty proof. This does not
// extend the original deadline or turn an expired transaction into progress.
func payloadQuantumCommitMargin(ctx context.Context, started time.Time) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	now := payloadQuantumNow(ctx)
	return now.Sub(started) >= deadline.Sub(now)
}

// A private per-call clock lets tests model a slow completed proof without
// sleeping or replacing the real cancellation deadline.
type payloadQuantumClockKey struct{}

func payloadQuantumNow(ctx context.Context) time.Time {
	if clock, ok := ctx.Value(payloadQuantumClockKey{}).(func() time.Time); ok {
		return clock()
	}
	return time.Now()
}
