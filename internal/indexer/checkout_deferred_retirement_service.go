package indexer

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

const (
	deferredRetirementBurstDuration = 50 * time.Millisecond
	deferredRetirementBurstChunks   = 128
)

func (l *CheckoutLifecycle) noteEligibleRetirementDebt(now time.Time, eligible, complete bool) {
	if eligible {
		l.deferredRetirementEligibleSince.CompareAndSwap(0, now.UnixNano())
	} else if complete {
		l.deferredRetirementEligibleSince.Store(0)
	}
}

func (l *CheckoutLifecycle) retirementDebtAged(now time.Time) bool {
	limit := l.retirementStarvationLimit
	if limit == 0 {
		limit = deferredRetirementStarvationLimit
	}
	since := l.deferredRetirementEligibleSince.Load()
	return since != 0 && (limit < 0 || now.Sub(time.Unix(0, since)) >= limit)
}

// serveDeferredRetirementBurst is the permit-backed daemon path. Waiting for
// the fair background lane consumes no cycle lock or writer; the one shared
// active deadline bounds every quantum, catalog fence and cursor advance.
// Queued edits get the lane back after one committed chunk, not the whole
// burst. An actual writer can also interrupt a transaction through the Store.
func (l *CheckoutLifecycle) serveDeferredRetirementBurst(
	ctx context.Context, gate *ViewBuildGate, ordered []int64,
	owners map[int64]*CheckoutCoordinator, discoveryErr error,
) (retired int, pending bool, err error) {
	queuedAt := time.Now()
	release, err := gate.Acquire(ctx, ViewBuildBackground)
	if err != nil {
		return 0, true, errors.Join(discoveryErr, err)
	}
	defer release()
	admittedAt := time.Now()
	debtSince := l.deferredRetirementEligibleSince.Load()
	clearHolder := gate.NoteHolder(ViewBuildLaneHolder{Kind: "payload_retirement", Priority: "background"})
	defer clearHolder()
	burstCtx, cancel := context.WithTimeout(ctx, deferredRetirementBurstDuration)
	defer cancel()
	// A synchronous cleanup may still own the sweep mutex. Waiting for it
	// under the build lane would invert ownership, so retry without blocking.
	if !l.retirementSweepMu.TryLock() {
		return 0, true, discoveryErr
	}
	defer l.retirementSweepMu.Unlock()
	// A planned fold can retain sources and do work outside the lane. Do not
	// overlap it, or an actual cycle/transition reported by the lifecycle.
	if l.editCycleHoldsBuildLane() || l.chainFoldInFlight() {
		deferredRetirementPreemptions.Add(1)
		return 0, true, discoveryErr
	}
	if l.walSinceReset() > deferredRetirementWALPause {
		l.store.RequestWALReclaim()
		return 0, true, discoveryErr
	}
	mark := l.store.WALWriteMark()
	var committed store_sqlite.PayloadRetirementProgress
	defer func() {
		if committed.ChunksCommitted == 0 && !committed.CatalogRemoved {
			return
		}
		l.noteRetirementProgress(time.Now())
		if l.logger != nil {
			wal := store_sqlite.WALWrittenBetween(mark, l.store.WALWriteMark())
			l.logger.Info("indexer: deferred retirement committed a bounded burst",
				zap.Int64("rows_deleted", committed.RowsDeleted),
				zap.Int64("chunks_committed", committed.ChunksCommitted),
				zap.Int("steps_advanced", committed.StepsAdvanced),
				zap.Int("retired_generations", retired),
				zap.Duration("lane_wait", admittedAt.Sub(queuedAt)),
				zap.Duration("lane_hold", time.Since(admittedAt)),
				zap.Duration("debt_age", admittedAt.Sub(time.Unix(0, debtSince))),
				zap.Int("interactive_queued", gate.Stats().InteractiveQueued),
				zap.Bool("wal_delta_valid", wal.Valid), zap.Int64("wal_bytes", wal.Bytes))
		}
	}()
	pending = true
	// Keep a selected generation's nested cursor moving during this burst;
	// the existing lifecycle cursor rotates generations between bursts.
	generationID := l.nextDeferredRetirement(ordered)
	for attempts := 0; attempts < deferredRetirementBurstChunks && len(ordered) > 0; attempts++ {
		if burstCtx.Err() != nil {
			break
		}
		if l.editCycleHoldsBuildLane() || l.chainFoldInFlight() || l.walSinceReset() > deferredRetirementWALPause {
			break
		}
		inUse := l.deferredRetirementInUse
		if owner := owners[generationID]; owner != nil {
			// Preserve both lifecycle leases/flight checks and the exact
			// coordinator's retained ancestry, even if it was unregistered.
			inUse = func(id int64) bool { return l.deferredRetirementInUse(id) || owner.retirementInUse(id) }
		}
		runQuantum := l.retireQuantum
		if runQuantum == nil {
			runQuantum = l.store.RetirePayloadGenerationQuantum
		}
		progress, retireErr := runQuantum(burstCtx, generationID, inUse)
		// database/sql may finish an automatic context rollback before the
		// caller reaches Commit. Only the cancelled burst makes ErrTxDone a
		// budget/cancellation outcome; a live transaction error stays fatal.
		if errors.Is(retireErr, sql.ErrTxDone) && burstCtx.Err() != nil {
			retireErr = burstCtx.Err()
		}
		committed.RowsDeleted += progress.RowsDeleted
		committed.ChunksCommitted += progress.ChunksCommitted
		committed.StepsAdvanced += progress.StepsAdvanced
		if progress.CatalogRemoved {
			committed.CatalogRemoved = true
			retired++
			l.forgetDeferredRetirement(generationID, owners[generationID], true)
			l.unparkRetirement(generationID)
			noteRetirementReferenceReleased()
		} else if errors.Is(retireErr, store_sqlite.ErrCatalogNotFound) {
			l.forgetDeferredRetirement(generationID, owners[generationID], false)
		} else if retirementStillReferenced(retireErr) {
			l.parkReferencedRetirement(generationID, time.Now())
		} else if errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse) {
			// Lease refusals are not progress; rotate to a sibling this burst.
		} else if errors.Is(retireErr, store_sqlite.ErrPayloadRetirementWriteWanted) {
			deferredRetirementPreemptions.Add(1)
			break
		} else if errors.Is(retireErr, context.DeadlineExceeded) && ctx.Err() == nil {
			break
		} else if retireErr != nil && !errors.Is(retireErr, store_sqlite.ErrPayloadSweepBudgetExhausted) {
			return retired, true, errors.Join(discoveryErr, retireErr)
		}
		if errors.Is(retireErr, store_sqlite.ErrPayloadSweepBudgetExhausted) && progress.ChunksCommitted == 0 {
			// The Store's own WAL observation refused admission. This is
			// neither an empty-source proof nor a reason to churn siblings.
			break
		}
		if committed.ChunksCommitted > 0 && gate.Stats().InteractiveQueued > 0 {
			break
		}
		if progress.CatalogRemoved || progress.ChunksCommitted == 0 {
			for i, id := range ordered {
				if id == generationID {
					ordered = append(ordered[:i], ordered[i+1:]...)
					break
				}
			}
			if len(ordered) > 0 {
				generationID = l.nextDeferredRetirement(ordered)
			}
		}
	}
	pending = l.hasDeferredRetirementWork() || discoveryErr != nil
	if !pending {
		l.deferredRetirementEligibleSince.Store(0)
	}
	if ctx.Err() != nil {
		return retired, pending, errors.Join(discoveryErr, ctx.Err())
	}
	return retired, pending, discoveryErr
}

func (l *CheckoutLifecycle) forgetDeferredRetirement(id int64, owner *CheckoutCoordinator, collected bool) {
	l.removeOwedRetirement(id)
	if owner != nil {
		owner.mu.Lock()
		delete(owner.backlog, id)
		owner.mu.Unlock()
		if collected {
			viewmetrics.Count(viewmetrics.GenerationSweepCollectedTotal, viewmetrics.SweepCheckout)
		}
	}
}
