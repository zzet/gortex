package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

const checkoutMoveRetryDelay = 5 * time.Second

// applyReconcileReport is the single post-catalog convergence boundary shared
// by forced reconciliation, topology events, startup repair and the janitor.
// Root moves converge before ordinary coordinator admission so a stale
// process-local root is never mistaken for a compatible live coordinator.
func (l *CheckoutLifecycle) applyReconcileReport(
	ctx context.Context,
	report reconcile.FamilyReport,
) error {
	if l == nil {
		return nil
	}
	// Arm ordinary availability/removal deadlines first. A move failure then
	// replaces that timer with the nearer repair deadline instead of having a
	// deadline-free report accidentally cancel the repair.
	l.scheduleFamilyRetry(report)
	// Keep coordinator admission inside the same component cut as root
	// convergence. A report from another family must not reinstall a watcher
	// while a collision-connected swap is quiesced.
	l.lockCheckoutTopologyPublication()
	defer l.topologyPublishMu.Unlock()
	l.moveMu.Lock()
	topologyEvents, moveErr := l.convergeCheckoutRootsLocked(ctx, report)
	blocked, blockedErr := l.unresolvedCheckoutRootMoveIDs(ctx)
	if blockedErr != nil {
		moveErr = errors.Join(moveErr, fmt.Errorf(
			"resolve checkout move admission fence: %w", blockedErr))
	} else {
		l.applyCoordinators(ctx, checkoutMoveAdmissionReport(report, blocked))
	}
	l.moveMu.Unlock()
	for _, event := range topologyEvents {
		l.notifyCheckoutTopologyChanged(event)
	}
	if moveErr != nil {
		l.scheduleFamilyRetryAt(report.FamilyID, l.now().Add(checkoutMoveRetryDelay).Unix())
	}
	return moveErr
}

func (l *CheckoutLifecycle) unresolvedCheckoutRootMoveIDs(
	ctx context.Context,
) (map[string]struct{}, error) {
	moves, err := l.catalog.ListCheckoutRootMoves(ctx)
	if err != nil {
		return nil, err
	}
	blocked := make(map[string]struct{}, len(moves))
	for _, move := range moves {
		blocked[move.CheckoutID] = struct{}{}
	}
	return blocked, nil
}

func checkoutMoveAdmissionReport(
	report reconcile.FamilyReport,
	blocked map[string]struct{},
) reconcile.FamilyReport {
	if len(blocked) == 0 {
		return report
	}
	filtered := report
	filtered.Checkouts = make([]reconcile.CheckoutReport, 0, len(report.Checkouts))
	for _, checkout := range report.Checkouts {
		if _, unresolved := blocked[checkout.CheckoutID]; unresolved {
			continue
		}
		filtered.Checkouts = append(filtered.Checkouts, checkout)
	}
	return filtered
}

func (l *CheckoutLifecycle) convergeCheckoutRoots(
	ctx context.Context,
	report reconcile.FamilyReport,
) error {
	if l == nil || l.catalog == nil {
		return nil
	}
	l.lockCheckoutTopologyPublication()
	defer l.topologyPublishMu.Unlock()
	l.moveMu.Lock()
	events, err := l.convergeCheckoutRootsLocked(ctx, report)
	l.moveMu.Unlock()
	for _, event := range events {
		l.notifyCheckoutTopologyChanged(event)
	}
	return err
}

// convergeCheckoutRootsLocked widens one report-local edge to every durable
// pending root move, partitions them into collision-connected components, and
// converges each component independently. The caller holds moveMu through
// coordinator admission so another family cannot observe a half-published
// swap.
func (l *CheckoutLifecycle) convergeCheckoutRootsLocked(
	ctx context.Context,
	_ reconcile.FamilyReport,
) ([]CheckoutTopologyEvent, error) {
	if l == nil || l.catalog == nil {
		return nil, nil
	}
	moves, blocked, resolveErr := l.resolvePendingPreparedMoveConfigs(ctx)
	if moves == nil && resolveErr != nil {
		return nil, resolveErr
	}
	participants := make([]checkoutRootMoveParticipant, 0, len(moves))
	for _, move := range moves {
		participant := checkoutRootMoveParticipant{move: move}
		if _, failed := blocked[move.CheckoutID]; failed {
			participant.discoveryErr = fmt.Errorf(
				"checkout %s prepared config state is unresolved", move.CheckoutID)
		}
		checkout, found, err := l.catalog.GetCheckout(ctx, move.CheckoutID)
		if err != nil {
			participant.discoveryErr = errors.Join(participant.discoveryErr, err)
			participants = append(participants, participant)
			continue
		}
		if !found {
			participant.discoveryErr = errors.Join(participant.discoveryErr,
				fmt.Errorf("checkout %s move owner is missing", move.CheckoutID))
			participants = append(participants, participant)
			continue
		}
		participant.checkout = checkout
		if checkout.State != store_sqlite.CheckoutStateReady ||
			checkout.Incarnation != move.Incarnation ||
			!coordinatorRootEqual(checkout.RootPath, move.CurrentRootPath) {
			participant.discoveryErr = errors.Join(participant.discoveryErr, fmt.Errorf(
				"%w: checkout %s move journal no longer names its ready current root",
				store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID))
		}
		if checkout.ActiveIntentTransitionID != "" {
			participant.discoveryErr = errors.Join(participant.discoveryErr, fmt.Errorf(
				"checkout %s move deferred behind intent transition %s",
				checkout.CheckoutID, checkout.ActiveIntentTransitionID))
		}
		switch checkout.EffectiveMode {
		case store_sqlite.CheckoutModeAutomatic:
			participant.graphID, err = l.primaryGraphIDForFamily(ctx, checkout.FamilyID)
			if err == nil && participant.graphID == "" {
				err = fmt.Errorf("automatic checkout %s has no serving primary",
					checkout.CheckoutID)
			}
		case store_sqlite.CheckoutModeDedicated:
			participant.prefix = l.prefixForCheckout(ctx, checkout.CheckoutID)
			if participant.prefix == "" {
				err = fmt.Errorf("dedicated checkout %s has no bound prefix",
					checkout.CheckoutID)
				break
			}
			participant.graphID = GraphIDFor(participant.prefix)
			previous, sources, _, stateErr := l.dedicatedRootRepairState(
				ctx, checkoutMoveReport(checkout, move), checkout, move, participant.prefix,
			)
			participant.previousRoots = previous
			participant.sources = sources
			err = stateErr
			if err == nil && !sources.TopLevel && len(sources.Projects) == 0 {
				err = fmt.Errorf("%w: checkout %s has no durable config source",
					config.ErrRepoRelocationSourceMissing, checkout.CheckoutID)
			}
		default:
			err = fmt.Errorf("checkout %s has unsupported move mode %q",
				checkout.CheckoutID, checkout.EffectiveMode)
		}
		participant.discoveryErr = errors.Join(participant.discoveryErr, err)
		participants = append(participants, participant)
	}

	var failures []error
	var topologyEvents []CheckoutTopologyEvent
	if resolveErr != nil {
		failures = append(failures, resolveErr)
	}
	changed := false
	for _, component := range partitionCheckoutRootMoveComponents(participants) {
		componentChanged, componentEvents, err := l.convergeCheckoutRootMoveComponent(ctx, component)
		changed = changed || componentChanged
		topologyEvents = append(topologyEvents, componentEvents...)
		if err == nil {
			continue
		}
		failures = append(failures, fmt.Errorf(
			"checkout move component %s: %w",
			component[0].move.CheckoutID, err))
		l.scheduleCheckoutMoveComponentRetries(component)
	}
	if changed {
		l.notifyTrackedSetChanged()
	}
	return topologyEvents, errors.Join(failures...)
}

type checkoutRootMoveParticipant struct {
	checkout      store_sqlite.Checkout
	move          store_sqlite.CheckoutRootMove
	graphID       string
	prefix        string
	previousRoots []string
	sources       config.RepoRelocationSources
	discoveryErr  error
}

func partitionCheckoutRootMoveComponents(
	participants []checkoutRootMoveParticipant,
) [][]checkoutRootMoveParticipant {
	if len(participants) == 0 {
		return nil
	}
	parent := make([]int, len(participants))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	union := func(left, right int) {
		left, right = find(left), find(right)
		if left != right {
			parent[right] = left
		}
	}
	// Each journal contributes a bounded set of identities. Canonicalize each
	// one once and union through its first owner; pairwise comparison made the
	// disjoint case quadratic and repeatedly resolved the same filesystem
	// aliases thousands of times during large startup recovery.
	preparedOwners := make(map[string]int, len(participants))
	rootOwners := make(map[string]int, len(participants)*2)
	for index, participant := range participants {
		move := participant.move
		if move.ConfigPreparedBeforeHash != "" && move.ConfigPreparedAfterHash != "" {
			key := move.ConfigPreparedBeforeHash + "\x00" + move.ConfigPreparedAfterHash
			if first, exists := preparedOwners[key]; exists {
				union(index, first)
			} else {
				preparedOwners[key] = index
			}
		}
		for _, root := range checkoutRootMoveCollisionRoots(move) {
			if root == "" {
				continue
			}
			key := checkoutRootMoveCanonicalKey(root)
			if first, exists := rootOwners[key]; exists {
				union(index, first)
			} else {
				rootOwners[key] = index
			}
		}
	}
	indices := make(map[int]int)
	components := make([][]checkoutRootMoveParticipant, 0, len(participants))
	for i, participant := range participants {
		root := find(i)
		index, exists := indices[root]
		if !exists {
			index = len(components)
			indices[root] = index
			components = append(components, nil)
		}
		components[index] = append(components[index], participant)
	}
	return components
}

func checkoutRootMoveCanonicalKey(root string) string {
	key := pathkey.Normalize(pathkey.CanonicalExistingRoot(root))
	if pathkey.CaseInsensitivePaths {
		key = strings.ToLower(key)
	}
	return key
}

func checkoutRootMoveCollisionRoots(move store_sqlite.CheckoutRootMove) []string {
	return []string{
		move.PreviousRootPath,
		move.LatestPreviousRootPath,
		move.ConfigRootPath,
		move.CurrentRootPath,
		move.ConfigPreparedFromPath,
		move.ConfigPreparedToPath,
	}
}

func (l *CheckoutLifecycle) convergeCheckoutRootMoveComponent(
	ctx context.Context,
	component []checkoutRootMoveParticipant,
) (bool, []CheckoutTopologyEvent, error) {
	for _, participant := range component {
		if participant.discoveryErr != nil {
			return false, nil, participant.discoveryErr
		}
	}
	if err := l.validateCheckoutRootMoveComponentOccupants(ctx, component); err != nil {
		return false, nil, err
	}
	l.observeCheckoutMoveComponentPhase("discovered", component)

	// Retire every tracked watcher before stopping any coordinator. If one
	// watcher refuses teardown, restore only the watchers already removed and
	// abort with every shell/coordinator still at the last coherent cut.
	removedWatchers := make([]checkoutRootMoveParticipant, 0, len(component))
	for _, participant := range component {
		if participant.prefix == "" {
			continue
		}
		if err := l.removeTrackedWatcherForMove(ctx, participant.prefix); err != nil {
			var restoreFailures []error
			for _, removed := range removedWatchers {
				if restoreErr := l.ensureTrackedWatcherOnce(ctx, removed.prefix); restoreErr != nil {
					restoreFailures = append(restoreFailures, restoreErr)
				}
			}
			return false, nil, errors.Join(err, errors.Join(restoreFailures...))
		}
		removedWatchers = append(removedWatchers, participant)
	}
	for _, participant := range component {
		l.dropCheckoutSourceSignalWatcher(participant.checkout.CheckoutID)
		l.dropCoordinator(participant.checkout.CheckoutID)
	}
	l.observeCheckoutMoveComponentPhase("quiesced", component)
	if err := l.validateCheckoutRootMoveComponentOccupants(ctx, component); err != nil {
		return false, nil, err
	}

	for i := range component {
		participant := &component[i]
		checkout, found, err := l.catalog.GetCheckout(ctx, participant.checkout.CheckoutID)
		if err != nil || !found || checkout.State != store_sqlite.CheckoutStateReady ||
			checkout.Incarnation != participant.checkout.Incarnation ||
			!coordinatorRootEqual(checkout.RootPath, participant.checkout.RootPath) ||
			checkout.EffectiveMode != participant.checkout.EffectiveMode ||
			checkout.ActiveIntentTransitionID != "" {
			if err == nil {
				err = store_sqlite.ErrCatalogStaleGuard
			}
			_, _ = l.reinstallCheckoutRootMoveComponent(ctx, component)
			return false, nil, fmt.Errorf("checkout %s changed while quiesced: %w",
				participant.checkout.CheckoutID, err)
		}
		move, found, err := l.catalog.GetCheckoutRootMove(ctx, checkout.CheckoutID)
		if err != nil || !found || move.Incarnation != participant.move.Incarnation ||
			!coordinatorRootEqual(move.CurrentRootPath, participant.move.CurrentRootPath) ||
			move.ConfigPreparedBeforeHash != participant.move.ConfigPreparedBeforeHash ||
			move.ConfigPreparedAfterHash != participant.move.ConfigPreparedAfterHash {
			if err == nil {
				err = store_sqlite.ErrCatalogStaleGuard
			}
			_, _ = l.reinstallCheckoutRootMoveComponent(ctx, component)
			return false, nil, fmt.Errorf("checkout %s move changed while quiesced: %w",
				checkout.CheckoutID, err)
		}
		participant.checkout = checkout
		participant.move = move
		if checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
			graphID, graphErr := l.primaryGraphIDForFamily(ctx, checkout.FamilyID)
			if graphErr != nil || graphID == "" || graphID != participant.graphID {
				_, _ = l.reinstallCheckoutRootMoveComponent(ctx, component)
				if graphErr == nil {
					graphErr = store_sqlite.ErrCatalogStaleGuard
				}
				return false, nil, fmt.Errorf("checkout %s primary changed while quiesced: %w",
					checkout.CheckoutID, graphErr)
			}
		}
	}
	l.observeCheckoutMoveComponentPhase("revalidated", component)

	changed := false
	for _, participant := range component {
		rebound, err := l.rebindCheckoutRootMoveShell(ctx, participant)
		changed = changed || rebound
		if err != nil {
			reinstalled, reinstallErr := l.reinstallCheckoutRootMoveComponent(ctx, component)
			changed = changed || reinstalled
			return changed, nil, errors.Join(
				fmt.Errorf("publish checkout %s shell: %w",
					participant.checkout.CheckoutID, err),
				reinstallErr,
			)
		}
	}
	l.observeCheckoutMoveComponentPhase("published", component)

	reinstalled, reinstallErr := l.reinstallCheckoutRootMoveComponent(ctx, component)
	changed = changed || reinstalled
	if reinstallErr != nil {
		return changed, nil, reinstallErr
	}
	l.observeCheckoutMoveComponentPhase("reinstalled", component)

	dedicated := make([]dedicatedCheckoutMove, 0, len(component))
	for _, participant := range component {
		if participant.prefix == "" {
			continue
		}
		dedicated = append(dedicated, dedicatedCheckoutMove{
			observed:      checkoutMoveReport(participant.checkout, participant.move),
			checkout:      participant.checkout,
			move:          participant.move,
			prefix:        participant.prefix,
			previousRoots: participant.previousRoots,
			sources:       participant.sources,
		})
	}
	if len(dedicated) != 0 {
		configChanged, err := l.convergeDedicatedMoveConfigBatch(ctx, dedicated)
		changed = changed || configChanged
		if err != nil {
			return changed, nil, err
		}
	}

	topologyEvents := make([]CheckoutTopologyEvent, 0, len(component))
	for _, participant := range component {
		locatorsMoved, err := l.catalog.RelocateActiveTrackingIntentLocators(
			ctx, participant.checkout.CheckoutID,
			participant.checkout.Incarnation, participant.checkout.RootPath,
		)
		changed = changed || locatorsMoved != 0
		if err != nil {
			return changed, topologyEvents, fmt.Errorf("checkout %s intent locators: %w",
				participant.checkout.CheckoutID, err)
		}
		move, found, err := l.catalog.GetCheckoutRootMove(
			ctx, participant.checkout.CheckoutID,
		)
		if err != nil || !found {
			if err == nil {
				err = store_sqlite.ErrCatalogStaleGuard
			}
			return changed, topologyEvents, fmt.Errorf("checkout %s move disappeared before completion: %w",
				participant.checkout.CheckoutID, err)
		}
		event, err := l.completeCheckoutRootMove(ctx, move)
		if err != nil {
			return changed, topologyEvents, fmt.Errorf("checkout %s complete move: %w",
				participant.checkout.CheckoutID, err)
		}
		topologyEvents = append(topologyEvents, event)
	}
	l.observeCheckoutMoveComponentPhase("completed", component)
	return changed, topologyEvents, nil
}

func (l *CheckoutLifecycle) completeCheckoutRootMove(
	ctx context.Context, move store_sqlite.CheckoutRootMove,
) (CheckoutTopologyEvent, error) {
	if err := l.catalog.CompleteCheckoutRootMove(ctx, move); err != nil {
		return CheckoutTopologyEvent{}, err
	}
	return CheckoutTopologyEvent{
		Kind:         CheckoutTopologyRootMoveCompleted,
		CheckoutID:   move.CheckoutID,
		Incarnation:  move.Incarnation,
		PreviousRoot: move.PreviousRootPath,
		CurrentRoot:  move.CurrentRootPath,
	}, nil
}

func (l *CheckoutLifecycle) validateCheckoutRootMoveComponentOccupants(
	ctx context.Context,
	component []checkoutRootMoveParticipant,
) error {
	checkoutIDs := make(map[string]struct{}, len(component))
	prefixes := make(map[string]struct{}, len(component))
	targets := make([]string, 0, len(component))
	for _, participant := range component {
		checkoutIDs[participant.checkout.CheckoutID] = struct{}{}
		if participant.prefix != "" {
			prefixes[participant.prefix] = struct{}{}
		}
		targets = append(targets, participant.checkout.RootPath)
	}
	occupiesTarget := func(root string) bool {
		for _, target := range targets {
			if coordinatorRootEqual(root, target) {
				return true
			}
		}
		return false
	}

	// Runtime registries are caches and can be empty immediately after a
	// restart. Durable checkout ownership is therefore the first collision
	// boundary: a live row outside this component cannot be overwritten merely
	// because its shell/coordinator has not been restored yet.
	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return err
	}
	for _, family := range families {
		checkouts, err := l.catalog.ListCheckouts(ctx, family.FamilyID)
		if err != nil {
			return err
		}
		for _, checkout := range checkouts {
			if _, participant := checkoutIDs[checkout.CheckoutID]; participant {
				continue
			}
			if occupiesTarget(checkout.RootPath) {
				return fmt.Errorf(
					"%w: nonparticipant durable checkout %s occupies a move target",
					store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID,
				)
			}
		}
	}

	l.coordMu.Lock()
	for checkoutID, coordinator := range l.coordinators {
		if coordinator == nil || !occupiesTarget(coordinator.root) {
			continue
		}
		if _, participant := checkoutIDs[checkoutID]; !participant {
			l.coordMu.Unlock()
			return fmt.Errorf("%w: nonparticipant checkout %s occupies a move target",
				store_sqlite.ErrCatalogStaleGuard, checkoutID)
		}
	}
	l.coordMu.Unlock()

	l.mi.mu.RLock()
	conflictingPrefixes := make([]string, 0)
	for prefix, meta := range l.mi.repos {
		if meta == nil || !occupiesTarget(meta.RootPath) {
			continue
		}
		if _, participant := prefixes[prefix]; !participant {
			conflictingPrefixes = append(conflictingPrefixes, prefix)
		}
	}
	l.mi.mu.RUnlock()
	for _, prefix := range conflictingPrefixes {
		owned, err := l.mi.routeOwnsDedicatedCorpus(ctx, prefix)
		if err != nil {
			return err
		}
		if owned {
			return fmt.Errorf("%w: nonparticipant dedicated shell %s occupies a move target",
				store_sqlite.ErrCatalogStaleGuard, prefix)
		}
	}
	return nil
}

func (l *CheckoutLifecycle) rebindCheckoutRootMoveShell(
	ctx context.Context,
	participant checkoutRootMoveParticipant,
) (bool, error) {
	if participant.prefix == "" {
		return false, nil
	}
	if barrier := l.moveShellPublishBarrier; barrier != nil {
		if err := barrier(participant.checkout.CheckoutID); err != nil {
			return false, err
		}
	}
	return l.mi.RebindRouteOwnedRepoRoot(
		ctx,
		participant.checkout.CheckoutID,
		participant.prefix,
		participant.checkout.RootPath,
	)
}

func (l *CheckoutLifecycle) reinstallCheckoutRootMoveComponent(
	ctx context.Context,
	component []checkoutRootMoveParticipant,
) (bool, error) {
	// A failed publication can leave one dedicated shell at its new root while
	// a peer is still at its old root. Re-read every durable checkout first,
	// then converge every shell to that authoritative catalog cut before any
	// coordinator or watcher is allowed to build against process-local state.
	authoritative := append([]checkoutRootMoveParticipant(nil), component...)
	for i := range authoritative {
		participant := &authoritative[i]
		checkout, found, err := l.catalog.GetCheckout(ctx, participant.checkout.CheckoutID)
		if err != nil {
			return false, err
		}
		if !found || checkout.State != store_sqlite.CheckoutStateReady ||
			checkout.Incarnation != participant.checkout.Incarnation ||
			checkout.EffectiveMode != participant.checkout.EffectiveMode ||
			checkout.ActiveIntentTransitionID != "" {
			return false, fmt.Errorf(
				"%w: checkout %s cannot be reinstalled from its authoritative catalog state",
				store_sqlite.ErrCatalogStaleGuard, participant.checkout.CheckoutID,
			)
		}
		participant.checkout = checkout
		switch checkout.EffectiveMode {
		case store_sqlite.CheckoutModeAutomatic:
			graphID, err := l.primaryGraphIDForFamily(ctx, checkout.FamilyID)
			if err != nil {
				return false, err
			}
			if graphID == "" {
				return false, fmt.Errorf("automatic checkout %s has no serving primary",
					checkout.CheckoutID)
			}
			participant.graphID = graphID
		case store_sqlite.CheckoutModeDedicated:
			prefix := l.prefixForCheckout(ctx, checkout.CheckoutID)
			if prefix == "" || prefix != participant.prefix {
				return false, fmt.Errorf(
					"%w: dedicated checkout %s route changed during component recovery",
					store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID,
				)
			}
			participant.graphID = GraphIDFor(prefix)
		default:
			return false, fmt.Errorf("checkout %s has unsupported recovery mode %q",
				checkout.CheckoutID, checkout.EffectiveMode)
		}
	}

	changed := false
	for _, participant := range authoritative {
		rebound, err := l.rebindCheckoutRootMoveShell(ctx, participant)
		changed = changed || rebound
		if err != nil {
			return changed, fmt.Errorf("checkout %s shell: %w",
				participant.checkout.CheckoutID, err)
		}
	}

	var failures []error
	for _, participant := range authoritative {
		rebound, err := l.rebindCheckoutCoordinatorRoot(
			ctx, participant.graphID, participant.checkout, true,
		)
		changed = changed || rebound
		if err != nil {
			failures = append(failures, fmt.Errorf("checkout %s coordinator: %w",
				participant.checkout.CheckoutID, err))
			continue
		}
		if participant.prefix != "" {
			if err := l.ensureTrackedWatcherOnce(ctx, participant.prefix); err != nil {
				failures = append(failures, fmt.Errorf("checkout %s watcher: %w",
					participant.checkout.CheckoutID, err))
			}
		}
	}
	return changed, errors.Join(failures...)
}

func (l *CheckoutLifecycle) observeCheckoutMoveComponentPhase(
	phase string,
	component []checkoutRootMoveParticipant,
) {
	if l.moveComponentBarrier == nil {
		return
	}
	ids := make([]string, 0, len(component))
	for _, participant := range component {
		ids = append(ids, participant.move.CheckoutID)
	}
	l.moveComponentBarrier(phase, ids)
}

func (l *CheckoutLifecycle) scheduleCheckoutMoveComponentRetries(
	component []checkoutRootMoveParticipant,
) {
	deadline := l.now().Add(checkoutMoveRetryDelay).Unix()
	families := make(map[string]struct{}, len(component))
	for _, participant := range component {
		if participant.checkout.FamilyID == "" {
			continue
		}
		families[participant.checkout.FamilyID] = struct{}{}
	}
	for familyID := range families {
		l.scheduleFamilyRetryAt(familyID, deadline)
	}
}

func (l *CheckoutLifecycle) convergeCheckoutRootsLegacy(
	ctx context.Context,
	report reconcile.FamilyReport,
) error {
	if l == nil || l.catalog == nil {
		return nil
	}
	l.lockCheckoutTopologyPublication()
	defer l.topologyPublishMu.Unlock()
	var (
		failures       []error
		changed        bool
		dedicatedMoves []dedicatedCheckoutMove
		topologyEvents []CheckoutTopologyEvent
	)
	for _, observed := range report.Checkouts {
		if !observed.Durable || observed.CheckoutID == "" ||
			observed.State != store_sqlite.CheckoutStateReady {
			continue
		}
		checkout, found, err := l.catalog.GetCheckout(ctx, observed.CheckoutID)
		if err != nil {
			failures = append(failures, fmt.Errorf("checkout %s: %w", observed.CheckoutID, err))
			continue
		}
		if !found || checkout.State != store_sqlite.CheckoutStateReady {
			continue
		}
		move, pending, err := l.catalog.GetCheckoutRootMove(ctx, checkout.CheckoutID)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"checkout %s move journal: %w", checkout.CheckoutID, err))
			continue
		}
		if !pending {
			// Every physical move publishes a journal in the same transaction as
			// its root CAS. A report alone is not durable recovery authority.
			continue
		}
		if move.Incarnation != checkout.Incarnation ||
			!coordinatorRootEqual(move.CurrentRootPath, checkout.RootPath) {
			failures = append(failures, fmt.Errorf(
				"%w: checkout %s move journal no longer names current root",
				store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID))
			continue
		}
		if checkout.ActiveIntentTransitionID != "" {
			// A demotion/forget transition owns both graph retirement and durable
			// config removal. Active intents may already be revoked at this cut, so
			// move repair must not infer "no sources" and clear the journal ahead of
			// that transaction.
			failures = append(failures, fmt.Errorf(
				"checkout %s move deferred behind intent transition %s",
				checkout.CheckoutID, checkout.ActiveIntentTransitionID))
			continue
		}

		switch checkout.EffectiveMode {
		case store_sqlite.CheckoutModeAutomatic:
			rebound, err := l.rebindCheckoutCoordinatorRoot(
				ctx, report.PrimaryGraphID, checkout, true,
			)
			changed = changed || rebound
			if err != nil {
				failures = append(failures, fmt.Errorf("automatic checkout %s: %w", checkout.CheckoutID, err))
				continue
			}
			locatorsMoved, err := l.catalog.RelocateActiveTrackingIntentLocators(
				ctx, checkout.CheckoutID, checkout.Incarnation, checkout.RootPath,
			)
			changed = changed || locatorsMoved != 0
			if err != nil {
				failures = append(failures, fmt.Errorf(
					"automatic checkout %s intent locators: %w", checkout.CheckoutID, err))
				continue
			}
		case store_sqlite.CheckoutModeDedicated:
			dedicated, rebound, err := l.convergeDedicatedCheckoutRuntime(
				ctx, observed, checkout, move,
			)
			changed = changed || rebound
			if err != nil {
				failures = append(failures, fmt.Errorf("dedicated checkout %s: %w", checkout.CheckoutID, err))
				continue
			}
			dedicatedMoves = append(dedicatedMoves, dedicated)
			continue
		default:
			failures = append(failures, fmt.Errorf(
				"checkout %s has unsupported move mode %q",
				checkout.CheckoutID, checkout.EffectiveMode))
			continue
		}
		event, completeErr := l.completeCheckoutRootMove(ctx, move)
		if completeErr != nil {
			failures = append(failures, fmt.Errorf(
				"checkout %s complete move: %w", checkout.CheckoutID, completeErr))
		} else {
			topologyEvents = append(topologyEvents, event)
		}
	}
	if len(dedicatedMoves) != 0 {
		var expandChanged bool
		var expandErr error
		dedicatedMoves, expandChanged, expandErr =
			l.convergeAllPendingDedicatedMoveRuntime(ctx, dedicatedMoves)
		changed = changed || expandChanged
		if expandErr != nil {
			// Expansion failures are component-local. Healthy, disjoint move
			// components still converge in this pass.
			failures = append(failures, expandErr)
		}
	}
	if len(dedicatedMoves) != 0 {
		successful, configChanged, configErr :=
			l.convergeDedicatedMoveConfigComponents(ctx, dedicatedMoves)
		changed = changed || configChanged
		if configErr != nil {
			failures = append(failures, configErr)
		}
		for i := range successful {
			dedicated := &successful[i]
			locatorsMoved, locatorErr := l.catalog.RelocateActiveTrackingIntentLocators(
				ctx, dedicated.checkout.CheckoutID,
				dedicated.checkout.Incarnation, dedicated.checkout.RootPath,
			)
			changed = changed || locatorsMoved != 0
			if locatorErr != nil {
				failures = append(failures, fmt.Errorf(
					"dedicated checkout %s intent locators: %w",
					dedicated.checkout.CheckoutID, locatorErr))
				continue
			}
			event, completeErr := l.completeCheckoutRootMove(ctx, dedicated.move)
			if completeErr != nil {
				failures = append(failures, fmt.Errorf(
					"checkout %s complete move: %w",
					dedicated.checkout.CheckoutID, completeErr))
			} else {
				topologyEvents = append(topologyEvents, event)
			}
		}
	}
	for _, event := range topologyEvents {
		l.notifyCheckoutTopologyChanged(event)
	}
	if changed {
		l.notifyTrackedSetChanged()
	}
	return errors.Join(failures...)
}

// convergeAllPendingDedicatedMoveRuntime widens a report-local trigger to all
// pending dedicated moves, but it does not make them one transaction. Runtime
// failures stay attached to their own collision component; healthy disjoint
// components can still converge in the same pass.
func (l *CheckoutLifecycle) convergeAllPendingDedicatedMoveRuntime(
	ctx context.Context,
	_ []dedicatedCheckoutMove,
) ([]dedicatedCheckoutMove, bool, error) {
	pending, blocked, resolveErr := l.resolvePendingPreparedMoveConfigs(ctx)
	if pending == nil && resolveErr != nil {
		return nil, false, resolveErr
	}
	changed := false
	failures := []error{resolveErr}
	ready := make([]dedicatedCheckoutMove, 0, len(pending))
	for _, move := range pending {
		if _, failed := blocked[move.CheckoutID]; failed {
			continue
		}
		checkout, found, readErr := l.catalog.GetCheckout(ctx, move.CheckoutID)
		if readErr != nil {
			failures = append(failures, readErr)
			continue
		}
		if !found || checkout.State != store_sqlite.CheckoutStateReady ||
			checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated {
			continue
		}
		if checkout.ActiveIntentTransitionID != "" {
			failures = append(failures, fmt.Errorf(
				"checkout %s move deferred behind intent transition %s",
				checkout.CheckoutID, checkout.ActiveIntentTransitionID))
			continue
		}
		if checkout.Incarnation != move.Incarnation ||
			!coordinatorRootEqual(checkout.RootPath, move.CurrentRootPath) {
			failures = append(failures, fmt.Errorf(
				"%w: checkout %s pending move identity changed",
				store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID))
			continue
		}
		state, rebound, convergeErr := l.convergeDedicatedCheckoutRuntime(
			ctx, checkoutMoveReport(checkout, move), checkout, move,
		)
		changed = changed || rebound
		if convergeErr != nil {
			failures = append(failures, fmt.Errorf(
				"dedicated checkout %s: %w", checkout.CheckoutID, convergeErr))
			continue
		}
		ready = append(ready, state)
	}
	return ready, changed, errors.Join(failures...)
}

// resolvePendingPreparedMoveConfigs collapses every durable cross-store cut
// before a new component is allowed to replace YAML. Rows carrying the same
// before/after hash came from one atomic file replacement and are therefore
// acknowledged or cleared together, even when their runtimes are unavailable.
// A bad hash group is retained and excluded without blocking unrelated groups.
func (l *CheckoutLifecycle) resolvePendingPreparedMoveConfigs(
	ctx context.Context,
) ([]store_sqlite.CheckoutRootMove, map[string]struct{}, error) {
	pending, err := l.catalog.ListCheckoutRootMoves(ctx)
	if err != nil {
		return nil, nil, err
	}
	groups := make(map[string][]store_sqlite.CheckoutRootMove)
	order := make([]string, 0)
	for _, move := range pending {
		if !checkoutMoveConfigPrepared(move) {
			continue
		}
		key := checkoutMovePreparedHashKey(move)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], move)
	}
	blocked := make(map[string]struct{})
	var failures []error
	for _, key := range order {
		members := groups[key]
		if _, err := l.resolvePreparedMoveConfigForCleanup(ctx, members[0]); err != nil {
			for _, move := range members {
				blocked[move.CheckoutID] = struct{}{}
			}
			failures = append(failures, fmt.Errorf(
				"resolve prepared config component %s: %w", members[0].CheckoutID, err))
		}
	}
	if len(groups) != 0 {
		pending, err = l.catalog.ListCheckoutRootMoves(ctx)
		if err != nil {
			return nil, blocked, errors.Join(errors.Join(failures...), err)
		}
	}
	return pending, blocked, errors.Join(failures...)
}

func checkoutMoveConfigPrepared(move store_sqlite.CheckoutRootMove) bool {
	return move.ConfigPreparedFromPath != "" || move.ConfigPreparedToPath != "" ||
		move.ConfigPreparedBeforeHash != "" || move.ConfigPreparedAfterHash != ""
}

func checkoutMovePreparedHashKey(move store_sqlite.CheckoutRootMove) string {
	if move.ConfigPreparedBeforeHash == "" || move.ConfigPreparedAfterHash == "" {
		return "incomplete\x00" + move.CheckoutID
	}
	return move.ConfigPreparedBeforeHash + "\x00" + move.ConfigPreparedAfterHash
}

// preparePendingRootMovesForSeed repairs durable configuration before Seed
// interprets any configured path as a new checkout. It returns every stale
// source spelling still covered by a journal; Seed treats those registrations
// as unresolved when an atomic save fails, rather than creating a phantom
// family at the vanished address.
func (l *CheckoutLifecycle) preparePendingRootMovesForSeed(
	ctx context.Context,
) ([]string, error) {
	if l == nil || l.catalog == nil {
		return nil, nil
	}
	moves, err := l.catalog.ListCheckoutRootMoves(ctx)
	if err != nil {
		return nil, err
	}
	staleRoots := make([]string, 0, len(moves)*2)
	var (
		failures       []error
		dedicatedMoves []dedicatedCheckoutMove
	)
	for _, move := range moves {
		checkout, found, err := l.catalog.GetCheckout(ctx, move.CheckoutID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !found || checkout.Incarnation != move.Incarnation ||
			!coordinatorRootEqual(checkout.RootPath, move.CurrentRootPath) {
			failures = append(failures, fmt.Errorf(
				"%w: checkout %s move journal is stale",
				store_sqlite.ErrCatalogStaleGuard, move.CheckoutID))
			continue
		}
		if checkout.ActiveIntentTransitionID != "" {
			failures = append(failures, fmt.Errorf(
				"checkout %s move deferred behind intent transition %s",
				checkout.CheckoutID, checkout.ActiveIntentTransitionID))
			continue
		}
		if checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated || l.cfgMgr == nil {
			// Automatic history is process/runtime evidence only. Suppressing its
			// old roots here would make an unrelated explicit repository added at
			// the vacated address disappear from cold-start registration.
			continue
		}
		// Only exact addresses at which this dedicated checkout's YAML can still
		// reside are startup suppression authority. Previous physical roots are
		// not config provenance; prepared from/to are, across the crash cut.
		staleRoots = appendUniqueCheckoutRoot(staleRoots, move.ConfigRootPath)
		staleRoots = appendUniqueCheckoutRoot(staleRoots, move.ConfigPreparedFromPath)
		staleRoots = appendUniqueCheckoutRoot(staleRoots, move.ConfigPreparedToPath)
		prefix := l.prefixForCheckout(ctx, checkout.CheckoutID)
		if prefix == "" {
			failures = append(failures, fmt.Errorf(
				"checkout %s pending move has no dedicated prefix", checkout.CheckoutID))
			continue
		}
		observed := checkoutMoveReport(checkout, move)
		previous, sources, _, stateErr := l.dedicatedRootRepairState(
			ctx, observed, checkout, move, prefix,
		)
		if stateErr != nil {
			failures = append(failures, fmt.Errorf(
				"checkout %s move sources: %w", checkout.CheckoutID, stateErr))
			continue
		}
		dedicatedMoves = append(dedicatedMoves, dedicatedCheckoutMove{
			observed:      checkoutMoveReport(checkout, move),
			checkout:      checkout,
			move:          move,
			prefix:        prefix,
			previousRoots: previous,
			sources:       sources,
		})
	}
	if len(dedicatedMoves) != 0 {
		if _, err := l.convergeDedicatedMoveConfigBatch(ctx, dedicatedMoves); err != nil {
			failures = append(failures, fmt.Errorf("prepare pending moved configs: %w", err))
		}
	}
	return staleRoots, errors.Join(failures...)
}

// recoverPendingRootMoves finishes process runtime and intent convergence
// after Seed has restored route-owned shells from the now-current config.
// It performs no inventory walk and no generation build; one catalog row per
// moved checkout bounds startup work.
func (l *CheckoutLifecycle) recoverPendingRootMoves(
	ctx context.Context,
) (map[string]string, error) {
	reports, err := l.pendingRootMoveReports(ctx)
	if err != nil {
		return nil, err
	}
	seeded := make(map[string]string, len(reports))
	var failures []error
	for _, report := range reports {
		for _, checkout := range report.Checkouts {
			seeded[report.FamilyID] = checkout.RootPath
		}
		if err := l.convergeCheckoutRoots(ctx, report); err != nil {
			failures = append(failures, err)
			l.scheduleFamilyRetryAt(
				report.FamilyID, l.now().Add(checkoutMoveRetryDelay).Unix(),
			)
		}
	}
	return seeded, errors.Join(failures...)
}

func (l *CheckoutLifecycle) pendingRootMoveReports(
	ctx context.Context,
) ([]reconcile.FamilyReport, error) {
	if l == nil || l.catalog == nil {
		return nil, nil
	}
	moves, err := l.catalog.ListCheckoutRootMoves(ctx)
	if err != nil {
		return nil, err
	}
	byFamily := make(map[string]int, len(moves))
	reports := make([]reconcile.FamilyReport, 0, len(moves))
	for _, move := range moves {
		checkout, found, err := l.catalog.GetCheckout(ctx, move.CheckoutID)
		if err != nil {
			return nil, err
		}
		if !found || checkout.State != store_sqlite.CheckoutStateReady {
			continue
		}
		index, exists := byFamily[checkout.FamilyID]
		if !exists {
			family, found, err := l.catalog.GetRepositoryFamily(ctx, checkout.FamilyID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("checkout %s move family is missing", checkout.CheckoutID)
			}
			primaryGraphID, err := l.primaryGraphIDForFamily(ctx, checkout.FamilyID)
			if err != nil {
				return nil, err
			}
			index = len(reports)
			byFamily[checkout.FamilyID] = index
			reports = append(reports, reconcile.FamilyReport{
				FamilyID:        checkout.FamilyID,
				CommonDir:       family.CommonDirIdentity,
				InventoryUsable: true,
				PrimaryGraphID:  primaryGraphID,
			})
		}
		reports[index].Checkouts = append(
			reports[index].Checkouts, checkoutMoveReport(checkout, move),
		)
	}
	return reports, nil
}

func (l *CheckoutLifecycle) primaryGraphIDForFamily(
	ctx context.Context,
	familyID string,
) (string, error) {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return "", err
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			return graph.GraphID, nil
		}
	}
	return "", nil
}

func checkoutMoveReport(
	checkout store_sqlite.Checkout,
	move store_sqlite.CheckoutRootMove,
) reconcile.CheckoutReport {
	return reconcile.CheckoutReport{
		AdminName:        checkout.AdminName,
		RootPath:         checkout.RootPath,
		PreviousRootPath: move.PreviousRootPath,
		RootMoved:        true,
		Main:             checkout.AdminName == gitstate.MainAdminName,
		CheckoutID:       checkout.CheckoutID,
		Incarnation:      checkout.Incarnation,
		Durable:          true,
		State:            checkout.State,
	}
}

func appendUniqueCheckoutRoot(roots []string, candidate string) []string {
	if candidate == "" {
		return roots
	}
	for _, root := range roots {
		if coordinatorRootEqual(root, candidate) {
			return roots
		}
	}
	return append(roots, candidate)
}

// rebindCheckoutCoordinatorRoot swaps only the process-local coordinator and
// source watcher. The replacement starts with polling disabled and receives
// no registration signal, so the already-routed generations stay byte-for-byte
// unchanged until a later real source/HEAD event asks for a build.
func (l *CheckoutLifecycle) rebindCheckoutCoordinatorRoot(
	ctx context.Context,
	primaryGraphID string,
	checkout store_sqlite.Checkout,
	installMissing bool,
) (bool, error) {
	if primaryGraphID == "" {
		if installMissing {
			return false, fmt.Errorf("moved checkout has no serving graph")
		}
		return false, nil
	}
	l.coordMu.Lock()
	previous := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if previous != nil && previous.Running() &&
		coordinatorRootEqual(previous.root, checkout.RootPath) &&
		previous.graphID == primaryGraphID {
		l.ensureCheckoutSourceSignalWatcher(checkout, primaryGraphID)
		return false, nil
	}
	if previous == nil && !installMissing {
		return false, nil
	}
	if previous != nil && previous.graphID != primaryGraphID {
		// Ordinary coordinator admission owns graph changes; this helper is only
		// the same-graph root relocation fast path.
		if installMissing {
			return false, fmt.Errorf(
				"%w: moved checkout coordinator still names graph %s, want %s",
				store_sqlite.ErrCatalogStaleGuard, previous.graphID, primaryGraphID,
			)
		}
		return false, nil
	}

	replacement, err := l.buildCoordinatorWithPoll(ctx, primaryGraphID, checkout, -time.Nanosecond)
	if err != nil {
		return false, err
	}
	if replacement == nil {
		return false, fmt.Errorf("graph %s cannot back the moved coordinator yet", primaryGraphID)
	}
	if barrier := l.moveRebindBarrier; barrier != nil {
		barrier()
	}
	// Construction may touch Git and is intentionally outside coordMu. Re-read
	// the durable identity at the publication edge so a B coordinator built
	// while inventory advances B -> C can never replace the still-valid A/C
	// binding. The newer journal will drive a fresh C replacement.
	current, found, guardErr := l.catalog.GetCheckout(ctx, checkout.CheckoutID)
	if guardErr != nil || !found || current.Incarnation != checkout.Incarnation ||
		!coordinatorRootEqual(current.RootPath, checkout.RootPath) ||
		current.State != store_sqlite.CheckoutStateReady ||
		current.EffectiveMode != checkout.EffectiveMode ||
		current.ActiveIntentTransitionID != "" {
		_ = replacement.Close()
		l.oweRetirement(replacement.DrainRetirements()...)
		if guardErr != nil {
			return false, guardErr
		}
		return false, fmt.Errorf(
			"%w: checkout %s root advanced during coordinator rebind",
			store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID,
		)
	}

	l.coordMu.Lock()
	if l.coordinatorClosing || !replacement.Running() {
		l.coordMu.Unlock()
		_ = replacement.Close()
		l.oweRetirement(replacement.DrainRetirements()...)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, ErrIndexerClosed
	}
	if l.coordinators[checkout.CheckoutID] != previous {
		l.coordMu.Unlock()
		_ = replacement.Close()
		l.oweRetirement(replacement.DrainRetirements()...)
		return false, fmt.Errorf("%w: checkout coordinator moved during root rebind",
			store_sqlite.ErrCatalogStaleGuard)
	}
	l.coordinators[checkout.CheckoutID] = replacement
	if l.coordinatorHeads == nil {
		l.coordinatorHeads = map[string]checkoutHeadIdentity{}
	}
	l.coordinatorHeads[checkout.CheckoutID] = checkoutHeadIdentity{
		ref: checkout.HeadRef, commit: checkout.HeadCommit,
	}
	viewmetrics.SetGauge(viewmetrics.Coordinators, int64(len(l.coordinators)))
	l.coordMu.Unlock()

	// Watcher identity includes both the exact coordinator pointer and root,
	// so retire it before publishing the replacement source binding.
	l.dropCheckoutSourceSignalWatcher(checkout.CheckoutID)
	if previous != nil {
		_ = previous.Close()
		l.oweRetirement(previous.DrainRetirements()...)
		l.stopCheckoutWorkspaces(previous.root)
	}
	l.ensureCheckoutSourceSignalWatcher(checkout, primaryGraphID)
	return true, nil
}

func coordinatorRootEqual(a, b string) bool {
	return pathkey.EqualPaths(
		pathkey.CanonicalExistingRoot(a),
		pathkey.CanonicalExistingRoot(b),
	)
}

type dedicatedCheckoutMove struct {
	observed      reconcile.CheckoutReport
	checkout      store_sqlite.Checkout
	move          store_sqlite.CheckoutRootMove
	prefix        string
	previousRoots []string
	sources       config.RepoRelocationSources
}

func (l *CheckoutLifecycle) convergeDedicatedCheckoutRuntime(
	ctx context.Context,
	observed reconcile.CheckoutReport,
	checkout store_sqlite.Checkout,
	move store_sqlite.CheckoutRootMove,
) (dedicatedCheckoutMove, bool, error) {
	state := dedicatedCheckoutMove{observed: observed, checkout: checkout, move: move}
	prefix := l.prefixForCheckout(ctx, checkout.CheckoutID)
	if prefix == "" {
		return state, false, fmt.Errorf("no dedicated prefix is bound")
	}
	state.prefix = prefix

	previousRoots, sources, repairNeeded, err := l.dedicatedRootRepairState(
		ctx, observed, checkout, move, prefix,
	)
	if err != nil {
		return state, false, err
	}
	state.previousRoots = previousRoots
	state.sources = sources
	if !sources.TopLevel && len(sources.Projects) == 0 {
		return state, false, fmt.Errorf(
			"%w: checkout %s has no durable config source",
			config.ErrRepoRelocationSourceMissing, checkout.CheckoutID,
		)
	}
	changed := false
	if repairNeeded {
		meta := l.mi.GetMetadata(prefix)
		if meta != nil && !coordinatorRootEqual(meta.RootPath, checkout.RootPath) {
			if err := l.removeTrackedWatcherForMove(ctx, prefix); err != nil {
				return state, changed, err
			}
		}
		rebound, rebindErr := l.mi.RebindRouteOwnedRepoRoot(
			ctx, checkout.CheckoutID, prefix, checkout.RootPath,
		)
		changed = changed || rebound
		if rebindErr != nil {
			return state, changed, rebindErr
		}
		coordinatorRebound, coordinatorErr := l.rebindCheckoutCoordinatorRoot(
			ctx, GraphIDFor(prefix), checkout, true,
		)
		changed = changed || coordinatorRebound
		if coordinatorErr != nil {
			return state, changed, coordinatorErr
		}
		// Ensure runs after metadata changed, so both the file watcher and Git
		// topology watcher are constructed against the new root.
		if err := l.ensureTrackedWatcherOnce(ctx, prefix); err != nil {
			return state, changed, err
		}
	}
	return state, changed, nil
}

func (l *CheckoutLifecycle) convergeDedicatedMoveConfigComponents(
	ctx context.Context,
	moves []dedicatedCheckoutMove,
) ([]dedicatedCheckoutMove, bool, error) {
	components := partitionDedicatedMoveComponents(moves)
	successful := make([]dedicatedCheckoutMove, 0, len(moves))
	changed := false
	var failures []error
	for _, component := range components {
		componentChanged, err := l.convergeDedicatedMoveConfigBatch(ctx, component)
		changed = changed || componentChanged
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"checkout move component %s: %w",
				component[0].checkout.CheckoutID, err))
			continue
		}
		successful = append(successful, component...)
	}
	return successful, changed, errors.Join(failures...)
}

// partitionDedicatedMoveComponents returns the smallest atomic config groups.
// Moves connect only when they can address the same authorized YAML collection
// and a canonical source/target path collides. Prepared rows sharing one exact
// before/after hash are always unioned because they came from one file replace.
func partitionDedicatedMoveComponents(
	moves []dedicatedCheckoutMove,
) [][]dedicatedCheckoutMove {
	if len(moves) == 0 {
		return nil
	}
	parent := make([]int, len(moves))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	union := func(left, right int) {
		left = find(left)
		right = find(right)
		if left != right {
			parent[right] = left
		}
	}
	for left := range moves {
		for right := left + 1; right < len(moves); right++ {
			if dedicatedMovesSharePreparedHash(moves[left], moves[right]) ||
				(dedicatedMoveCollectionsOverlap(moves[left], moves[right]) &&
					dedicatedMoveRootsCollide(moves[left], moves[right])) {
				union(left, right)
			}
		}
	}
	componentIndex := make(map[int]int)
	components := make([][]dedicatedCheckoutMove, 0, len(moves))
	for i, move := range moves {
		root := find(i)
		index, exists := componentIndex[root]
		if !exists {
			index = len(components)
			componentIndex[root] = index
			components = append(components, nil)
		}
		components[index] = append(components[index], move)
	}
	return components
}

func dedicatedMovesSharePreparedHash(left, right dedicatedCheckoutMove) bool {
	return left.move.ConfigPreparedBeforeHash != "" &&
		left.move.ConfigPreparedAfterHash != "" &&
		left.move.ConfigPreparedBeforeHash == right.move.ConfigPreparedBeforeHash &&
		left.move.ConfigPreparedAfterHash == right.move.ConfigPreparedAfterHash
}

func dedicatedMoveCollectionsOverlap(left, right dedicatedCheckoutMove) bool {
	if left.sources.TopLevel && right.sources.TopLevel {
		return true
	}
	for project := range left.sources.Projects {
		if _, exists := right.sources.Projects[project]; exists {
			return true
		}
	}
	return false
}

func dedicatedMoveRootsCollide(left, right dedicatedCheckoutMove) bool {
	leftRoots := [...]string{
		left.move.ConfigRootPath,
		left.checkout.RootPath,
		left.move.ConfigPreparedFromPath,
		left.move.ConfigPreparedToPath,
	}
	rightRoots := [...]string{
		right.move.ConfigRootPath,
		right.checkout.RootPath,
		right.move.ConfigPreparedFromPath,
		right.move.ConfigPreparedToPath,
	}
	for _, leftRoot := range leftRoots {
		if leftRoot == "" {
			continue
		}
		for _, rightRoot := range rightRoots {
			if rightRoot != "" && coordinatorRootEqual(leftRoot, rightRoot) {
				return true
			}
		}
	}
	return false
}

func (l *CheckoutLifecycle) convergeDedicatedMoveConfigBatch(
	ctx context.Context,
	moves []dedicatedCheckoutMove,
) (bool, error) {
	if len(moves) == 0 {
		return false, nil
	}
	if l.cfgMgr == nil {
		return false, fmt.Errorf("persist moved repository config: config manager is unavailable")
	}

	// Resolve any earlier prepared replacement before planning a newer root.
	// The exact raw file hash distinguishes pre-save from post-save without
	// guessing ownership from a repo name or path now occupied by a swap peer.
	preparedStates := make(map[string]config.PreparedRepoRelocationState)
	for i := range moves {
		move := &moves[i].move
		prepared := move.ConfigPreparedFromPath != "" ||
			move.ConfigPreparedToPath != "" ||
			move.ConfigPreparedBeforeHash != "" ||
			move.ConfigPreparedAfterHash != ""
		if !prepared {
			continue
		}
		if move.ConfigPreparedFromPath == "" || move.ConfigPreparedToPath == "" ||
			move.ConfigPreparedBeforeHash == "" || move.ConfigPreparedAfterHash == "" ||
			!coordinatorRootEqual(move.ConfigPreparedFromPath, move.ConfigRootPath) {
			return false, fmt.Errorf(
				"%w: checkout %s has incomplete prepared config state",
				store_sqlite.ErrCatalogStaleGuard, move.CheckoutID,
			)
		}
		key := move.ConfigPreparedBeforeHash + "\x00" + move.ConfigPreparedAfterHash
		state, known := preparedStates[key]
		if !known {
			var err error
			state, err = l.cfgMgr.PreparedRepoRelocationState(
				move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			)
			if err != nil {
				return false, fmt.Errorf("resolve prepared moved config: %w", err)
			}
			preparedStates[key] = state
		}
		switch state {
		case config.PreparedRepoRelocationBefore:
			if err := l.catalog.ClearCheckoutRootMoveConfigPreparation(
				ctx, move.CheckoutID, move.Incarnation,
				move.ConfigRootPath, move.ConfigPreparedToPath,
				move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			); err != nil {
				return false, fmt.Errorf("clear unapplied moved config: %w", err)
			}
		case config.PreparedRepoRelocationAfter:
			if err := l.catalog.AcknowledgeCheckoutRootMoveConfig(
				ctx, move.CheckoutID, move.Incarnation,
				move.ConfigRootPath, move.ConfigPreparedToPath,
				move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			); err != nil {
				return false, fmt.Errorf("acknowledge prepared moved config: %w", err)
			}
			move.ConfigRootPath = move.ConfigPreparedToPath
		default:
			return false, fmt.Errorf("unknown prepared config state %q", state)
		}
		move.ConfigPreparedFromPath = ""
		move.ConfigPreparedToPath = ""
		move.ConfigPreparedBeforeHash = ""
		move.ConfigPreparedAfterHash = ""
	}

	relocations := make([]config.RepoRelocation, 0, len(moves))
	moveByID := make(map[string]*dedicatedCheckoutMove, len(moves))
	for i := range moves {
		move := &moves[i]
		if coordinatorRootEqual(move.move.ConfigRootPath, move.checkout.RootPath) {
			continue
		}
		relocations = append(relocations, config.RepoRelocation{
			ID:          move.checkout.CheckoutID,
			ConfigRoot:  move.move.ConfigRootPath,
			CurrentRoot: move.checkout.RootPath,
			Prefix:      move.prefix,
			Sources:     move.sources,
		})
		moveByID[move.checkout.CheckoutID] = move
	}
	if len(relocations) == 0 {
		return false, nil
	}
	batch, err := l.cfgMgr.PrepareRepoRelocationBatch(relocations)
	if err != nil {
		return false, fmt.Errorf("prepare moved repository config: %w", err)
	}
	if batch == nil || batch.BeforeHash() == "" || batch.AfterHash() == "" {
		return false, fmt.Errorf("prepare moved repository config: empty batch")
	}
	for _, relocation := range relocations {
		move := moveByID[relocation.ID]
		if err := l.catalog.PrepareCheckoutRootMoveConfig(
			ctx, move.checkout.CheckoutID, move.checkout.Incarnation,
			move.move.ConfigRootPath, move.checkout.RootPath,
			batch.BeforeHash(), batch.AfterHash(),
		); err != nil {
			return false, fmt.Errorf(
				"prepare checkout %s moved config: %w",
				move.checkout.CheckoutID, err,
			)
		}
		move.move.ConfigPreparedFromPath = move.move.ConfigRootPath
		move.move.ConfigPreparedToPath = move.checkout.RootPath
		move.move.ConfigPreparedBeforeHash = batch.BeforeHash()
		move.move.ConfigPreparedAfterHash = batch.AfterHash()
	}
	changed, err := l.cfgMgr.CommitRepoRelocationBatch(batch)
	if err != nil {
		return false, fmt.Errorf("persist moved repository config: %w", err)
	}
	for _, relocation := range relocations {
		move := moveByID[relocation.ID]
		if err := l.catalog.AcknowledgeCheckoutRootMoveConfig(
			ctx, move.checkout.CheckoutID, move.checkout.Incarnation,
			move.move.ConfigRootPath, move.checkout.RootPath,
			batch.BeforeHash(), batch.AfterHash(),
		); err != nil {
			return changed, fmt.Errorf(
				"acknowledge checkout %s moved config: %w",
				move.checkout.CheckoutID, err,
			)
		}
		move.move.ConfigRootPath = move.checkout.RootPath
		move.move.ConfigPreparedFromPath = ""
		move.move.ConfigPreparedToPath = ""
		move.move.ConfigPreparedBeforeHash = ""
		move.move.ConfigPreparedAfterHash = ""
	}
	return changed, nil
}

// resolvePreparedMoveConfigForCleanup collapses a prepared cross-store cut to
// the exact side currently on disk before any cleanup mutates YAML. Every row
// from the same atomic batch is acknowledged/cleared together; otherwise the
// first disappearing swap participant would change the file hash and strand
// its peer's journal forever.
func (l *CheckoutLifecycle) resolvePreparedMoveConfigForCleanup(
	ctx context.Context,
	target store_sqlite.CheckoutRootMove,
) (store_sqlite.CheckoutRootMove, error) {
	prepared := target.ConfigPreparedFromPath != "" ||
		target.ConfigPreparedToPath != "" ||
		target.ConfigPreparedBeforeHash != "" || target.ConfigPreparedAfterHash != ""
	if !prepared {
		return target, nil
	}
	if l.cfgMgr == nil || target.ConfigPreparedFromPath == "" ||
		target.ConfigPreparedToPath == "" || target.ConfigPreparedBeforeHash == "" ||
		target.ConfigPreparedAfterHash == "" ||
		!coordinatorRootEqual(target.ConfigPreparedFromPath, target.ConfigRootPath) {
		return target, fmt.Errorf("%w: checkout %s has incomplete prepared config state",
			store_sqlite.ErrCatalogStaleGuard, target.CheckoutID)
	}
	state, err := l.cfgMgr.PreparedRepoRelocationState(
		target.ConfigPreparedBeforeHash, target.ConfigPreparedAfterHash,
	)
	if err != nil {
		return target, err
	}
	moves, err := l.catalog.ListCheckoutRootMoves(ctx)
	if err != nil {
		return target, err
	}
	for _, move := range moves {
		if move.ConfigPreparedBeforeHash != target.ConfigPreparedBeforeHash ||
			move.ConfigPreparedAfterHash != target.ConfigPreparedAfterHash {
			continue
		}
		switch state {
		case config.PreparedRepoRelocationBefore:
			err = l.catalog.ClearCheckoutRootMoveConfigPreparation(
				ctx, move.CheckoutID, move.Incarnation,
				move.ConfigRootPath, move.ConfigPreparedToPath,
				move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			)
		case config.PreparedRepoRelocationAfter:
			err = l.catalog.AcknowledgeCheckoutRootMoveConfig(
				ctx, move.CheckoutID, move.Incarnation,
				move.ConfigRootPath, move.ConfigPreparedToPath,
				move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			)
		}
		if err != nil {
			return target, err
		}
	}
	updated, found, err := l.catalog.GetCheckoutRootMove(ctx, target.CheckoutID)
	if err != nil {
		return target, err
	}
	if !found {
		return target, fmt.Errorf("%w: checkout %s move disappeared during cleanup",
			store_sqlite.ErrCatalogStaleGuard, target.CheckoutID)
	}
	return updated, nil
}

func (l *CheckoutLifecycle) dedicatedRootRepairState(
	ctx context.Context,
	observed reconcile.CheckoutReport,
	checkout store_sqlite.Checkout,
	move store_sqlite.CheckoutRootMove,
	prefix string,
) ([]string, config.RepoRelocationSources, bool, error) {
	previous := make([]string, 0, 4)
	sources := config.RepoRelocationSources{Projects: map[string]struct{}{}}
	addPrevious := func(root string) {
		if root == "" || coordinatorRootEqual(root, checkout.RootPath) {
			return
		}
		for _, existing := range previous {
			if coordinatorRootEqual(existing, root) {
				return
			}
		}
		previous = append(previous, root)
	}
	addPrevious(move.PreviousRootPath)
	addPrevious(move.LatestPreviousRootPath)
	addPrevious(move.ConfigRootPath)
	if observed.RootMoved {
		addPrevious(observed.PreviousRootPath)
	}
	repair := true
	if meta := l.mi.GetMetadata(prefix); meta == nil {
		repair = true
	} else if !coordinatorRootEqual(meta.RootPath, checkout.RootPath) {
		addPrevious(meta.RootPath)
		repair = true
	}
	l.coordMu.Lock()
	coordinator := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if coordinator != nil && !coordinatorRootEqual(coordinator.root, checkout.RootPath) {
		addPrevious(coordinator.root)
		repair = true
	}

	intents, err := l.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	if err != nil {
		return nil, sources, false, err
	}
	for _, intent := range intents {
		if !intent.Active {
			continue
		}
		if pathBearingIntent(intent.SourceKind) {
			sources.TopLevel = true
			if !coordinatorRootEqual(intent.SourceLocator, checkout.RootPath) {
				addPrevious(intent.SourceLocator)
				repair = true
			}
			continue
		}
		if intent.SourceKind == store_sqlite.IntentSourceProjectMembership {
			if name := strings.TrimPrefix(intent.SourceLocator, "project:"); name != "" && name != intent.SourceLocator {
				sources.Projects[name] = struct{}{}
			}
		}
	}
	return previous, sources, repair, nil
}

func pathBearingIntent(kind store_sqlite.IntentSourceKind) bool {
	switch kind {
	case store_sqlite.IntentSourceCLITrack,
		store_sqlite.IntentSourceMCPTrack,
		store_sqlite.IntentSourceManualConfig:
		return true
	default:
		return false
	}
}

func (l *CheckoutLifecycle) removeTrackedWatcherForMove(ctx context.Context, prefix string) error {
	watcher := l.watcher()
	if watcher == nil {
		return nil
	}
	var err error
	if contextual, ok := watcher.(contextRepoWatcher); ok {
		err = contextual.RemoveRepoContext(ctx, prefix)
	} else {
		err = watcher.RemoveRepo(prefix)
	}
	if err != nil {
		return fmt.Errorf("retire moved watcher %s: %w", prefix, err)
	}
	return nil
}
