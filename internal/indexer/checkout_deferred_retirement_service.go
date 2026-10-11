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
	// A burst nobody waits on runs toward the idle slice.
	deferredRetirementIdleBurstDuration = deferredRetirementSliceBudget
	deferredRetirementIdleBurstChunks   = 4096
)

// noteEligibleRetirementDebt starts the debt clock on eligible work and
// clears it when a complete discovery finds none (retirementDebtDrained).
// Debt that drains while work stays parked keeps, per parked generation, the
// age it had reached when that generation was parked, and a generation a
// release brings back runs on its own clock started that age ago: a parent
// released later is the same debt, not a new idle window. Only that
// generation carries the age — debt beside it, and debt that arrives after an
// idle drain, keeps the shared clock and its stand-down. Only time spent
// eligible counts, so a generation parked for an hour does not come back aged
// by that hour.
func (l *CheckoutLifecycle) noteEligibleRetirementDebt(now time.Time, eligible, complete bool) {
	if eligible {
		l.deferredRetirementEligibleSince.CompareAndSwap(0, now.UnixNano())
	} else if complete {
		l.retirementDebtDrained(now)
	}
}

// retirementDebtDrained clears the debt clock: no eligible debt is left, so
// no released generation is waiting on its own clock either. Work still
// parked keeps the age the debt had reached aside (keepParkedDebtAge).
func (l *CheckoutLifecycle) retirementDebtDrained(time.Time) {
	since := l.deferredRetirementEligibleSince.Swap(0)
	l.coordMu.Lock()
	l.retirementReleasedAged = nil
	l.coordMu.Unlock()
	if l.retirementParkedCount() == 0 || since == 0 {
		return
	}
	l.keepParkedDebtAge(time.Unix(0, since))
}

func (l *CheckoutLifecycle) retirementDebtAged(now time.Time) bool {
	return l.retirementClockAged(l.deferredRetirementEligibleSince.Load(), now)
}

// retirementClockAged reports a debt clock (unix nanos, 0 unset) past the
// starvation limit.
func (l *CheckoutLifecycle) retirementClockAged(since int64, now time.Time) bool {
	limit := l.retirementStarvationLimit
	if limit == 0 {
		limit = deferredRetirementStarvationLimit
	}
	return since != 0 && (limit < 0 || now.Sub(time.Unix(0, since)) >= limit)
}

// serveDeferredRetirementBurst is the permit-backed daemon path. Waiting for
// the fair background lane consumes no cycle lock or writer; the one shared
// active deadline bounds every quantum, catalog fence and cursor advance.
// Queued edits get the lane back within the existing bounded burst. Queue
// presence alone does not discard its budget; an actual writer can still
// interrupt a transaction through the Store.
func (l *CheckoutLifecycle) serveDeferredRetirementBurst(
	ctx context.Context, gate *ViewBuildGate, ordered []int64,
	owners map[int64]*CheckoutCoordinator, discoveryErr error,
) (retired int, pending bool, err error) {
	// Read the candidates' shapes before taking the lane: the burst's time
	// is for deleting.
	ordered = append([]int64(nil), ordered...)
	shapes := l.retirementShapes(ctx, ordered)
	queuedAt := time.Now()
	debtAge := time.Duration(0)
	if since := l.retirementDebtSince(ordered); since != 0 {
		debtAge = queuedAt.Sub(time.Unix(0, since))
	}
	release, err := gate.acquireRetirement(ctx, debtAge)
	if err != nil {
		return 0, true, errors.Join(discoveryErr, err)
	}
	defer release()
	admittedAt := time.Now()
	debtSince := l.retirementDebtSince(ordered)
	clearHolder := gate.NoteHolder(ViewBuildLaneHolder{Kind: "payload_retirement", Priority: "background"})
	defer clearHolder()
	// With the checkout quiet and nobody waiting — no build of either
	// priority queued for the lane
	// (a watcher-driven edit cycle and a chain fold queue as background), no
	// foreground work or chain fold in flight, no writer on the gate, no
	// refresh or mutation pending, no synchronous sweep waiting for the sweep
	// mutex — the burst runs toward the idle slice with wide quanta, and gives
	// the lane back at the first quantum boundary after its bounded budget
	// once anybody does.
	coordinators := l.registeredCoordinators()
	nobodyWaits := func() bool {
		stats := gate.Stats()
		if stats.InteractiveQueued > 0 || stats.BackgroundQueued > 0 || l.retirementSweepWaiting.Load() > 0 {
			return false
		}
		if busy, _ := l.retirementForegroundWork(); busy != "" {
			return false
		}
		return !l.chainFoldInFlight() && !l.interactiveWriteWanted(coordinators)
	}
	// The long slice is for a quiet checkout: foreground work seen within
	// deferredRetirementStarvedIdle (between the saves of a steady stream,
	// say) keeps the burst to its bounded budget and narrow quanta, however
	// old the debt.
	_, lastForeground := l.retirementForegroundWork()
	quiet := lastForeground.IsZero() || admittedAt.Sub(lastForeground) >= deferredRetirementStarvedIdle
	extended := quiet && nobodyWaits()
	budget, maxAttempts := deferredRetirementBurstDuration, deferredRetirementBurstChunks
	if extended {
		budget, maxAttempts = deferredRetirementIdleBurstDuration, deferredRetirementIdleBurstChunks
		if l.retirementIdleBurst > 0 {
			budget = l.retirementIdleBurst
		}
	}
	burstCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// A synchronous cleanup may still own the sweep mutex. Waiting for it
	// under the build lane would invert ownership, so retry without blocking.
	if !l.retirementSweepMu.TryLock() {
		return 0, true, discoveryErr
	}
	defer l.retirementSweepMu.Unlock()
	// The permit excludes active checkout work. Off-lane folds retain exact
	// sources and continuously own their destination through landing; the
	// per-generation Store fences below protect those without blocking siblings.
	if l.editCycleHoldsBuildLane() {
		deferredRetirementPreemptions.Add(1)
		noteRetirementStandDown("edit_cycle")
		return 0, true, discoveryErr
	}
	if wal := l.walSinceReset(); wal > deferredRetirementWALPause {
		l.noteRetirementWALPause(wal)
		l.store.RequestWALReclaim()
		return 0, true, discoveryErr
	}
	mark := l.store.WALWriteMark()
	var committed store_sqlite.PayloadRetirementProgress
	var touched []int64
	defer func() {
		if committed.ChunksCommitted == 0 && !committed.CatalogRemoved {
			return
		}
		deferredRetirementCommitted.Add(committed.ChunksCommitted + int64(retired))
		l.noteRetirementProgress(time.Now())
		l.retirementWALPaused.Store(false)
		if l.logger != nil {
			wal := store_sqlite.WALWrittenBetween(mark, l.store.WALWriteMark())
			l.logger.Info("indexer: deferred retirement committed a bounded burst",
				zap.Int64s("generations", touched),
				zap.Int("generations_touched", len(touched)),
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
	// Finish what was started (checkout_deferred_retirement_order.go): the
	// burst stays on its generation while quanta commit, and the next burst
	// resumes it.
	generationID := l.pickDeferredRetirement(ordered, shapes, admittedAt)
	var servedChunks int64
	fenced := map[int64]bool{}
	defer func() { l.noteDeferredRetirementServed(generationID, servedChunks > 0, false, time.Now()) }()
	for attempts := 0; attempts < maxAttempts && len(ordered) > 0; attempts++ {
		if burstCtx.Err() != nil {
			break
		}
		wide := extended && nobodyWaits()
		if extended && !wide && time.Since(admittedAt) >= deferredRetirementBurstDuration {
			break
		}
		if l.editCycleHoldsBuildLane() {
			noteRetirementStandDown("edit_cycle")
			break
		}
		if wal := l.walSinceReset(); wal > deferredRetirementWALPause {
			l.noteRetirementWALPause(wal)
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
			// Fence once per burst: the first quantum on a generation runs
			// the full preamble, the rest continue the fence it took.
			runQuantum = l.store.RetirePayloadGenerationQuantum
			if fenced[generationID] {
				runQuantum = l.store.RetirePayloadGenerationQuantumFenced
			}
		}
		if len(touched) == 0 || touched[len(touched)-1] != generationID {
			touched = append(touched, generationID)
		}
		releaseSeq := retirementReleaseSeq()
		quantumCtx := burstCtx
		if wide {
			quantumCtx = store_sqlite.WithWideRetirementQuantum(burstCtx)
		}
		progress, retireErr := runQuantum(quantumCtx, generationID, inUse)
		parked := false
		// database/sql may finish an automatic context rollback before the
		// caller reaches Commit. Only the cancelled burst makes ErrTxDone a
		// budget/cancellation outcome; a live transaction error stays fatal.
		if errors.Is(retireErr, sql.ErrTxDone) && burstCtx.Err() != nil {
			retireErr = burstCtx.Err()
		}
		committed.RowsDeleted += progress.RowsDeleted
		committed.ChunksCommitted += progress.ChunksCommitted
		committed.StepsAdvanced += progress.StepsAdvanced
		servedChunks += progress.ChunksCommitted
		if progress.ChunksCommitted > 0 {
			fenced[generationID] = true
		}
		if progress.CatalogRemoved {
			committed.CatalogRemoved = true
			retired++
			l.deferredRetirementLastRemoved.Store(time.Now().Unix())
			l.forgetDeferredRetirement(generationID, owners[generationID], true)
			l.unparkRetirement(generationID)
			// The Store reported the base the removal released.
		} else if errors.Is(retireErr, store_sqlite.ErrCatalogNotFound) {
			l.forgetDeferredRetirement(generationID, owners[generationID], false)
		} else if retirementStillReferenced(retireErr) {
			l.parkReferencedRetirement(generationID, time.Now(), releaseSeq, retireErr)
			parked = true
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
			l.noteRetirementWALPause(l.walSinceReset())
			break
		}
		// A parked generation rotates whatever it committed: a fenced
		// continuation on a swept generation commits an empty quantum before
		// the catalog refuses it, and staying on it would repeat that refusal
		// for the rest of the burst.
		if progress.CatalogRemoved || progress.ChunksCommitted == 0 || parked {
			l.noteDeferredRetirementServed(generationID, false, progress.CatalogRemoved, time.Now())
			ordered = withoutGeneration(ordered, generationID)
			servedChunks = 0
			if len(ordered) > 0 {
				generationID = l.pickDeferredRetirement(ordered, shapes, time.Now())
			}
		}
	}
	pending = l.hasDeferredRetirementWork() || discoveryErr != nil
	if !pending {
		l.retirementDebtDrained(time.Now())
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
