package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	deferredRetirementDiscoveryTimeout = 5 * time.Second
	deferredRetirementSliceBudget      = 2 * time.Second
	deferredRetirementLockPoll         = 25 * time.Millisecond
)

// SweepDeferredRetirements runs one resumable background cleanup slice. It
// fairly selects one generation across live coordinator backlogs and durable
// orphan/owed work, then advances at most one physical retirement slice.
func (l *CheckoutLifecycle) SweepDeferredRetirements(ctx context.Context) (retired int, pending bool, err error) {
	if l == nil || l.store == nil {
		return 0, false, nil
	}
	if ctx == nil {
		return 0, true, fmt.Errorf("deferred retirement: nil context")
	}
	// Idle checkouts give their layers to this sweep (at most hourly).
	_, _ = l.ReleaseIdleCheckouts(ctx)
	if err := l.lockRetirementSweep(ctx); err != nil {
		return 0, true, err
	}
	sweepLocked := true
	defer func() {
		if sweepLocked {
			l.retirementSweepMu.Unlock()
		}
	}()

	l.coordMu.Lock()
	served := make(map[string]struct{}, len(l.coordinators))
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for checkoutID, coordinator := range l.coordinators {
		served[checkoutID] = struct{}{}
		coordinators = append(coordinators, coordinator)
	}
	owed := make([]int64, 0, len(l.owed))
	for generationID := range l.owed {
		owed = append(owed, generationID)
	}
	l.coordMu.Unlock()

	// Interactive work that wants the writer goes first — before the
	// catalog scan for candidates as well as before a slice: nothing of the
	// sweep runs while an edit cycle holds the lane or a writer waits
	// (checkout_deferred_retirement_preempt.go), unless the sweep has been
	// starved for too long to keep yielding.
	started := time.Now()
	gate := l.buildGate()
	armed := true
	if gate == nil {
		armed = l.retirementPreemptionArmed(started)
	}
	if gate == nil && l.retirementShouldStandDown(armed, coordinators) {
		deferredRetirementPreemptions.Add(1)
		return 0, true, nil
	}

	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, deferredRetirementDiscoveryTimeout)
	discovered, discoveryErr := l.discoverDeferredRetirements(discoveryCtx, served, owed)
	cancelDiscovery()

	// Record every partial discovery result before observing cancellation. A
	// later pass can drain it even when this pass's inventory read was cut short.
	l.coordMu.Lock()
	for _, generationID := range discovered {
		l.owed[generationID] = struct{}{}
	}
	owed = owed[:0]
	for generationID := range l.owed {
		owed = append(owed, generationID)
	}
	l.coordMu.Unlock()

	// Build one generation-ordered work list across coordinator backlogs and
	// durable orphan/owed work. The lifecycle cursor provides fairness across
	// both classes while each worker pass performs at most one physical slice.
	owners := make(map[int64]*CheckoutCoordinator)
	ordered := make([]int64, 0, len(owed))
	seen := make(map[int64]struct{}, len(owed))
	for _, coordinator := range coordinators {
		for _, generationID := range coordinator.pendingRetirementGenerations() {
			if _, duplicate := seen[generationID]; duplicate {
				continue
			}
			seen[generationID] = struct{}{}
			owners[generationID] = coordinator
			ordered = append(ordered, generationID)
		}
	}
	for _, generationID := range owed {
		if _, duplicate := seen[generationID]; duplicate {
			continue
		}
		seen[generationID] = struct{}{}
		ordered = append(ordered, generationID)
	}
	retireNewestFirst(ordered)
	// A generation the catalog still references waits, parked, for a
	// reference to go (checkout_deferred_retirement_parked.go).
	offered := ordered[:0:0]
	for _, generationID := range ordered {
		if !l.retirementParkedNow(generationID, started) {
			offered = append(offered, generationID)
		}
	}
	ordered = offered
	if gate != nil {
		l.noteEligibleRetirementDebt(started, len(ordered) > 0, discoveryErr == nil)
		if len(ordered) == 0 {
			return 0, discoveryErr != nil, discoveryErr
		}
		if !l.retirementDebtAged(started) && l.retirementShouldStandDown(true, coordinators) {
			deferredRetirementPreemptions.Add(1)
			return 0, true, discoveryErr
		}
		// An explicit cleanup may own the build lane and need this mutex.
		// Never hold it while waiting for background lane admission.
		sweepLocked = false
		l.retirementSweepMu.Unlock()
		return l.serveDeferredRetirementBurst(ctx, gate, ordered, owners, discoveryErr)
	}
	// While a checkout is being edited: smallest first, large ones held for a
	// longer idle, shorter slices (checkout_deferred_retirement_pacing.go).
	pace := l.retirementPaceNow(started, !armed)
	ordered = orderForPace(pace, ordered, l.generationStorageBytes(ctx))
	sliceBudget := pace.sliceBudget()

	if len(ordered) == 0 {
		if discoveryErr != nil {
			return 0, true, discoveryErr
		}
		return 0, false, nil
	}
	if l.retirementShouldStandDown(armed, coordinators) {
		deferredRetirementPreemptions.Add(1)
		return 0, true, discoveryErr
	}
	parentCtx := ctx
	{
		// A starved sweep stops yielding to waiting writers, never to an
		// edit cycle: the edit cycle always cancels a chunk in flight.
		var stop func()
		ctx, stop = l.preemptOnInteractiveWriteWhen(ctx, func() bool {
			return l.retirementShouldStandDown(armed, coordinators)
		})
		defer stop()
	}
	defer func() {
		if err != nil && retirementPreempted(parentCtx, ctx) {
			deferredRetirementPreemptions.Add(1)
			pending, err = true, discoveryErr
			return
		}
		// The legacy API does not report partial committed rows. A refused
		// or budget-limited attempt cannot reset the clock as if it completed.
		if retired > 0 {
			l.noteRetirementProgress(time.Now())
		}
	}()

	generationID := ordered[0]
	if !pace.active() {
		generationID = l.nextDeferredRetirement(ordered)
	}
	if coordinator := owners[generationID]; coordinator != nil {
		var stillPending bool
		retiredOne, stillPending, retireErr := coordinator.retirePayloadGenerationSlice(
			ctx, generationID, sliceBudget,
		)
		if retiredOne {
			retired = 1
			l.removeOwedRetirement(generationID)
		}
		if retireErr != nil && retirementStillReferenced(retireErr) {
			l.parkReferencedRetirement(generationID, time.Now())
			retireErr, stillPending = nil, false
		}
		pending = stillPending || len(ordered) > 1
		err = retireErr
	} else {
		var retireErr error
		if l.retireOwedSlice != nil {
			retireErr = l.retireOwedSlice(ctx, generationID)
		} else {
			retireErr = l.store.RetirePayloadGenerationSlice(
				ctx, generationID, l.deferredRetirementInUse, sliceBudget,
			)
		}
		stillPending := false
		switch {
		case retireErr == nil:
			retired = 1
			l.removeOwedRetirement(generationID)
		case errors.Is(retireErr, store_sqlite.ErrCatalogNotFound):
			l.removeOwedRetirement(generationID)
		case errors.Is(retireErr, store_sqlite.ErrPayloadSweepBudgetExhausted),
			errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse):
			stillPending = true
			retireErr = nil
		case retirementStillReferenced(retireErr):
			l.parkReferencedRetirement(generationID, time.Now())
			retireErr = nil
		default:
			stillPending = true
		}
		pending = len(ordered) > 1 || stillPending
		err = retireErr
	}
	if retired > 0 {
		// A retired generation may have been the last one built on another.
		l.unparkRetirement(generationID)
		noteRetirementReferenceReleased()
	}
	// Recheck live state after the physical slice. New backlog/owed work may
	// arrive while the slice commits and must keep the worker on its base pause.
	pending = pending || l.hasDeferredRetirementWork()
	if discoveryErr != nil {
		pending = true
	}
	return retired, pending, errors.Join(discoveryErr, err)
}

func (l *CheckoutLifecycle) lockRetirementSweep(ctx context.Context) error {
	for {
		if l.retirementSweepMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(deferredRetirementLockPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *CheckoutLifecycle) nextDeferredRetirement(ordered []int64) int64 {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	if len(ordered) == 0 {
		return 0
	}
	// ordered is newest-first. Continue below the previous generation even
	// when that completed generation disappeared from the next snapshot; this
	// prevents a stream of newly-added generations from starving older work.
	next := 0
	if l.deferredRetirementCursor > 0 {
		for i, generationID := range ordered {
			if generationID < l.deferredRetirementCursor {
				next = i
				break
			}
		}
	}
	selected := ordered[next]
	l.deferredRetirementCursor = selected
	return selected
}

func (l *CheckoutLifecycle) removeOwedRetirement(generationID int64) {
	l.coordMu.Lock()
	delete(l.owed, generationID)
	l.coordMu.Unlock()
}

func (l *CheckoutLifecycle) deferredRetirementInUse(generationID int64) bool {
	if (l.leases != nil && l.leases.InUse(generationID)) ||
		(l.store != nil && l.store.PayloadBuildFlightActive(generationID)) {
		return true
	}
	l.coordMu.Lock()
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, coordinator := range l.coordinators {
		coordinators = append(coordinators, coordinator)
	}
	l.coordMu.Unlock()
	for _, coordinator := range coordinators {
		if coordinator.retirementInUse(generationID) {
			return true
		}
	}
	return false
}

func (l *CheckoutLifecycle) hasDeferredRetirementWork() bool {
	now := time.Now()
	l.coordMu.Lock()
	owed := make([]int64, 0, len(l.owed))
	for generationID := range l.owed {
		owed = append(owed, generationID)
	}
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, coordinator := range l.coordinators {
		coordinators = append(coordinators, coordinator)
	}
	l.coordMu.Unlock()
	// A parked generation is not work: it waits for a reference to go.
	for _, generationID := range owed {
		if !l.retirementParkedNow(generationID, now) {
			return true
		}
	}
	for _, coordinator := range coordinators {
		for _, generationID := range coordinator.pendingRetirementGenerations() {
			if !l.retirementParkedNow(generationID, now) {
				return true
			}
		}
	}
	return false
}

// discoverDeferredRetirements mirrors the lifecycle inventory rules while
// returning partial candidates and every catalog error. An incomplete scan is
// retryable work, never evidence that startup cleanup is empty.
// deferredRetirementScans counts the catalog scans for retirement
// candidates (tests and measurement).
var deferredRetirementScans atomic.Int64

type deferredGenerationList func(context.Context, store_sqlite.ViewGenerationFilter) ([]store_sqlite.ViewGeneration, error)

func (l *CheckoutLifecycle) discoverDeferredRetirements(
	ctx context.Context,
	served map[string]struct{},
	known []int64,
) ([]int64, error) {
	if l.catalog == nil {
		return nil, nil
	}
	deferredRetirementScans.Add(1)
	return l.discoverDeferredRetirementsWith(
		ctx, served, known, l.catalog.ListViewGenerations, l.dedicatedChainRetirementCandidates,
	)
}

func (l *CheckoutLifecycle) discoverDeferredRetirementsWith(
	ctx context.Context,
	served map[string]struct{},
	known []int64,
	list deferredGenerationList,
	infer deferredDedicatedInference,
) ([]int64, error) {
	if list == nil {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(known))
	for _, generationID := range known {
		seen[generationID] = struct{}{}
	}
	var out []int64
	collect := func(row store_sqlite.ViewGeneration) {
		if row.GenerationID <= 0 {
			return
		}
		if _, duplicate := seen[row.GenerationID]; duplicate {
			return
		}
		seen[row.GenerationID] = struct{}{}
		out = append(out, row.GenerationID)
	}
	var dedicated []store_sqlite.ViewGeneration
	const pageSize = 512

	var before int64
	for {
		rows, scanErr := list(ctx, store_sqlite.ViewGenerationFilter{
			States: []store_sqlite.ViewGenerationState{
				store_sqlite.ViewGenerationSuperseded,
				store_sqlite.ViewGenerationRetiring,
			},
			BeforeGenerationID: before,
			Limit:              pageSize,
		})
		for _, row := range rows {
			if dedicatedBaseGenerationRow(row) {
				dedicated = append(dedicated, row)
			} else {
				// Superseded/retiring rows may represent coordinator backlog
				// that vanished at restart, before or during its first physical
				// slice. Re-offer them even for served checkouts; the worker uses
				// every live coordinator's in-use predicate before retiring
				// discovered work, conservatively preserving ancestry and pins.
				collect(row)
			}
		}
		if scanErr != nil {
			return out, fmt.Errorf("scan discarded generations: %w", scanErr)
		}
		if len(rows) < pageSize {
			break
		}
		before = rows[len(rows)-1].GenerationID
	}

	const abandonedBuildingGrace = time.Minute
	abandonedBefore := l.clock().Add(-abandonedBuildingGrace).Unix()
	scanState := func(state store_sqlite.ViewGenerationState, label string) error {
		var cursor int64
		for {
			rows, scanErr := list(ctx, store_sqlite.ViewGenerationFilter{
				States:             []store_sqlite.ViewGenerationState{state},
				BeforeGenerationID: cursor,
				Limit:              pageSize,
			})
			for _, row := range rows {
				if state == store_sqlite.ViewGenerationBuilding &&
					row.CreatedAt >= l.buildingRecoveryCutoff &&
					row.CreatedAt > abandonedBefore {
					continue
				}
				if state == store_sqlite.ViewGenerationBuilding && l.store.PayloadBuildFlightActive(row.GenerationID) {
					continue
				}
				collect(row)
			}
			if scanErr != nil {
				return fmt.Errorf("scan %s generations: %w", label, scanErr)
			}
			if len(rows) < pageSize {
				return nil
			}
			cursor = rows[len(rows)-1].GenerationID
		}
	}
	if scanErr := scanState(store_sqlite.ViewGenerationFailed, "failed"); scanErr != nil {
		return out, scanErr
	}
	if scanErr := scanState(store_sqlite.ViewGenerationBuilding, "building"); scanErr != nil {
		return out, scanErr
	}

	before = 0
	for {
		rows, scanErr := list(ctx, store_sqlite.ViewGenerationFilter{
			States:             []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady},
			MissingGraph:       true,
			BeforeGenerationID: before,
			Limit:              pageSize,
		})
		for _, row := range rows {
			collect(row)
		}
		if scanErr != nil {
			return out, fmt.Errorf("scan deleted-graph generations: %w", scanErr)
		}
		if len(rows) < pageSize {
			break
		}
		before = rows[len(rows)-1].GenerationID
	}

	routes := map[string]store_sqlite.CheckoutRoute{}
	before = 0
	for {
		layers, scanErr := list(ctx, store_sqlite.ViewGenerationFilter{
			States:             []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady},
			OwnerKind:          checkoutLayerOwnerKind,
			BeforeGenerationID: before,
			Limit:              pageSize,
		})
		for _, row := range layers {
			if dedicatedBaseGenerationRow(row) {
				dedicated = append(dedicated, row)
			}
		}
		candidates, routeErr := readyLayerRetirementCandidates(
			ctx, layers, served, routes, l.catalog.GetCheckoutRoutes,
		)
		if routeErr != nil {
			return out, fmt.Errorf("read checkout routes: %w", routeErr)
		}
		for _, row := range candidates {
			collect(row)
		}
		if scanErr != nil {
			return out, fmt.Errorf("scan checkout layers: %w", scanErr)
		}
		if len(layers) < pageSize {
			break
		}
		before = layers[len(layers)-1].GenerationID
	}

	for _, row := range inferDeferredDedicatedCandidates(ctx, dedicated, nil, infer) {
		collect(row)
	}
	return out, nil
}

type deferredDedicatedInference func(context.Context, []store_sqlite.ViewGeneration) []store_sqlite.ViewGeneration

func inferDeferredDedicatedCandidates(
	ctx context.Context,
	dedicated []store_sqlite.ViewGeneration,
	discoveryErr error,
	infer deferredDedicatedInference,
) []store_sqlite.ViewGeneration {
	if discoveryErr != nil || infer == nil {
		return nil
	}
	return infer(ctx, dedicated)
}
