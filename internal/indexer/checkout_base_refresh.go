package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
	"go.uber.org/zap"
)

type dedicatedBaseRefreshRequest struct {
	graphID          string
	checkoutID       string
	familyID         string
	baseGenerationID int64
}

func dedicatedBaseGenerationStructurallyValid(
	row store_sqlite.ViewGeneration, graph store_sqlite.DedicatedGraph,
) bool {
	return row.GenerationID == graph.ActiveGenerationID &&
		row.GraphID == graph.GraphID &&
		row.OwnerKind == dedicatedBaseGenerationKind &&
		row.GenerationKind == dedicatedBaseGenerationKind &&
		row.LayerID == graph.GraphID+":base" &&
		row.CheckoutID == graph.OwnerCheckoutID &&
		row.BaseGenerationID == 0 && servableGeneration(row.State) && row.TreeOID != ""
}

// dedicatedBaseGenerationRefreshable recognizes full-corpus identities from
// the pre-epoch worktree implementation as well as the canonical vNext form.
// A legacy full corpus is never admitted by graphBase: it is only an immutable
// source snapshot from which the lifecycle may build a canonical replacement.
// Sparse commit/dirty generations deliberately do not match this predicate.
func dedicatedBaseGenerationRefreshable(
	row store_sqlite.ViewGeneration, graph store_sqlite.DedicatedGraph,
) bool {
	if dedicatedBaseGenerationStructurallyValid(row, graph) {
		return true
	}
	return row.GenerationID == graph.ActiveGenerationID &&
		row.GraphID == graph.GraphID &&
		row.CheckoutID == graph.OwnerCheckoutID &&
		row.OwnerKind == checkoutLayerOwnerKind &&
		row.GenerationKind == "dedicated" &&
		row.BaseGenerationID == 0 &&
		row.TreeOID != "" && servableGeneration(row.State)
}

func dedicatedBaseGenerationPipelineCurrent(
	row store_sqlite.ViewGeneration, desired dedicatedBaseIdentity,
) bool {
	return row.ConfigHash == desired.configHash &&
		row.ExtractorVersions == desired.extractorVersions &&
		row.ResolverVersion == desired.resolverVersion
}

func dedicatedBaseGenerationCurrent(
	row store_sqlite.ViewGeneration,
	graph store_sqlite.DedicatedGraph,
	desired dedicatedBaseIdentity,
) bool {
	return dedicatedBaseGenerationStructurallyValid(row, graph) &&
		dedicatedBaseGenerationPipelineCurrent(row, desired)
}

func (l *CheckoutLifecycle) desiredDedicatedBaseIdentity(repoPrefix string) dedicatedBaseIdentity {
	index := config.Default().Index
	if l != nil && l.cfgMgr != nil {
		index = l.cfgMgr.GetRepoConfig(repoPrefix).Index
	}
	pipeline := DedicatedBasePipelineFor(index)
	return dedicatedBaseIdentity{
		configHash: pipeline.ConfigHash, extractorVersions: pipeline.ExtractorVersions,
		resolverVersion: pipeline.ResolverVersion,
	}
}

// beginDedicatedBaseRefresh drains mutations composed over the old base before
// making every route on that graph visibly pending. The exclusive graph token
// is released immediately after the catalog transaction; new mutation
// admission then rejects the pending routes while the off-route build runs.
func (l *CheckoutLifecycle) beginDedicatedBaseRefresh(
	ctx context.Context, graphID string, expectedBaseGenerationID int64,
) error {
	topology, err := l.AcquireCheckoutGraphTopology(ctx, graphID)
	if err != nil {
		return err
	}
	defer topology.Release()
	return l.catalog.BeginDedicatedBaseRefresh(ctx, graphID, expectedBaseGenerationID)
}

// scheduleDedicatedBaseRefreshIfNeeded reports whether graph cannot currently
// admit sparse builds. Pipeline-stale bases are queued once per graph; broken
// or half-published catalog identities remain fail-closed for the normal
// promotion/recovery path rather than guessing an immutable source tree.
func (l *CheckoutLifecycle) scheduleDedicatedBaseRefreshIfNeeded(
	ctx context.Context,
	graph store_sqlite.DedicatedGraph,
	checkout store_sqlite.Checkout,
) bool {
	if l == nil || l.catalog == nil {
		return false
	}
	if graph.ActiveGenerationID <= 0 {
		return true
	}
	row, found, err := l.catalog.GetViewGeneration(ctx, graph.ActiveGenerationID)
	if err != nil || !found {
		return true
	}
	desired := l.desiredDedicatedBaseIdentity(graph.RepoPrefix)
	refreshable := dedicatedBaseGenerationRefreshable(row, graph)
	if refreshable && !dedicatedBaseGenerationCurrent(row, graph, desired) {
		if err := l.beginDedicatedBaseRefresh(ctx, graph.GraphID, row.GenerationID); err != nil {
			l.logger.Warn("checkout lifecycle: could not mark dedicated base refresh pending",
				zap.String("graph", graph.GraphID), zap.Error(err))
			return true
		}
		l.scheduleDedicatedBaseRefresh(dedicatedBaseRefreshRequest{
			graphID: graph.GraphID, checkoutID: checkout.CheckoutID, familyID: checkout.FamilyID,
			baseGenerationID: row.GenerationID,
		})
		return true
	}
	return !refreshable
}

func (l *CheckoutLifecycle) scheduleDedicatedBaseRefresh(req dedicatedBaseRefreshRequest) {
	if l == nil || req.graphID == "" || req.checkoutID == "" || req.familyID == "" {
		return
	}
	l.transitionMu.Lock()
	if l.transitionClosed {
		l.transitionMu.Unlock()
		return
	}
	if l.baseRefreshPending == nil {
		l.baseRefreshPending = map[string]dedicatedBaseRefreshRequest{}
	}
	if l.baseRefreshInFlight == nil {
		l.baseRefreshInFlight = map[string]struct{}{}
	}
	if l.baseRefreshWake == nil {
		l.baseRefreshWake = make(chan struct{}, 1)
	}
	if _, running := l.baseRefreshInFlight[req.graphID]; running {
		l.transitionMu.Unlock()
		return
	}
	if _, pending := l.baseRefreshPending[req.graphID]; pending {
		l.transitionMu.Unlock()
		return
	}
	l.baseRefreshPending[req.graphID] = req
	startWorker := !l.baseRefreshWorkerStarted
	if startWorker {
		l.baseRefreshWorkerStarted = true
		l.transitionWG.Add(1)
	}
	l.transitionMu.Unlock()
	select {
	case l.baseRefreshWake <- struct{}{}:
	default:
	}
	if startWorker {
		go l.runDedicatedBaseRefreshWorker()
	}
}

func (l *CheckoutLifecycle) runDedicatedBaseRefreshWorker() {
	defer l.transitionWG.Done()
	for {
		req, ok := l.takeDedicatedBaseRefresh()
		if !ok {
			select {
			case <-l.transitionCtx.Done():
				return
			case <-l.baseRefreshWake:
				continue
			}
		}
		execute := l.refreshDedicatedBase
		if l.baseRefreshExecute != nil {
			execute = l.baseRefreshExecute
		}
		if req.baseGenerationID > 0 {
			// A newly admitted retry supersedes the prior process-local verdict
			// even though it rebuilds from the same active base generation.
			l.buildFailures.startBaseRefresh(req.graphID, req.baseGenerationID)
		}
		err := execute(l.transitionCtx, req)
		if req.baseGenerationID > 0 {
			if err == nil {
				l.buildFailures.clearBaseRefresh(req.graphID, req.baseGenerationID)
			} else if !errors.Is(err, context.Canceled) {
				l.buildFailures.recordBaseRefresh(
					req.graphID,
					req.baseGenerationID,
					dedicatedBaseRefreshFailureReason(err),
				)
			}
		}
		l.transitionMu.Lock()
		delete(l.baseRefreshInFlight, req.graphID)
		l.transitionMu.Unlock()
		if l.baseRefreshDone != nil {
			l.baseRefreshDone(req, err)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			l.logger.Warn("checkout lifecycle: dedicated base refresh failed",
				zap.String("graph", req.graphID),
				zap.String("checkout", req.checkoutID), zap.Error(err))
		}
	}
}

func dedicatedBaseRefreshFailureReason(err error) string {
	var storageErr *store_sqlite.StorageError
	if errors.As(err, &storageErr) {
		return store_sqlite.SafeStorageFailureReason(err)
	}
	return "dedicated base refresh failed; see daemon log"
}

func (l *CheckoutLifecycle) takeDedicatedBaseRefresh() (dedicatedBaseRefreshRequest, bool) {
	l.transitionMu.Lock()
	defer l.transitionMu.Unlock()
	if l.transitionClosed {
		return dedicatedBaseRefreshRequest{}, false
	}
	for graphID, req := range l.baseRefreshPending {
		delete(l.baseRefreshPending, graphID)
		l.baseRefreshInFlight[graphID] = struct{}{}
		return req, true
	}
	return dedicatedBaseRefreshRequest{}, false
}

func (l *CheckoutLifecycle) refreshDedicatedBase(
	ctx context.Context, req dedicatedBaseRefreshRequest,
) error {
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, req.graphID)
	if err != nil {
		return err
	}
	if !found || graph.OwnerCheckoutID != req.checkoutID || graph.FamilyID != req.familyID ||
		graph.ActiveGenerationID <= 0 ||
		(graph.State != reconcile.GraphStateReady && graph.State != store_sqlite.DedicatedGraphStateRefreshing) {
		return fmt.Errorf("%w: dedicated graph %s moved before refresh",
			store_sqlite.ErrCatalogStaleGuard, req.graphID)
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, req.checkoutID)
	if err != nil {
		return err
	}
	if !found || checkout.FamilyID != req.familyID ||
		checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated ||
		checkout.State != store_sqlite.CheckoutStateReady {
		return fmt.Errorf("%w: dedicated checkout %s moved before refresh",
			store_sqlite.ErrCatalogStaleGuard, req.checkoutID)
	}
	oldBase, found, err := l.catalog.GetViewGeneration(ctx, graph.ActiveGenerationID)
	if err != nil {
		return err
	}
	if !found || !dedicatedBaseGenerationRefreshable(oldBase, graph) {
		return fmt.Errorf("%w: dedicated graph %s has no refreshable immutable base",
			store_sqlite.ErrCatalogStaleGuard, req.graphID)
	}
	desired := l.desiredDedicatedBaseIdentity(graph.RepoPrefix)
	if dedicatedBaseGenerationCurrent(oldBase, graph, desired) {
		return nil
	}
	if err := l.beginDedicatedBaseRefresh(ctx, graph.GraphID, oldBase.GenerationID); err != nil {
		return err
	}

	if err := l.ensurePromotedRepoShell(ctx, checkout, graph.RepoPrefix); err != nil {
		return err
	}
	coordinator, err := l.buildCoordinatorWithPoll(ctx, graph.GraphID, checkout, -1)
	if err != nil {
		return err
	}
	if coordinator == nil {
		return fmt.Errorf("indexer: build dedicated refresh coordinator for checkout %s", checkout.CheckoutID)
	}
	defer coordinator.Close()

	release := func() {}
	if gate := l.buildGate(); gate != nil {
		// A configured dedicated graph cannot become exact until this refresh
		// publishes. Its reserved lifecycle admission cannot be displaced by ref
		// views or automatic overlay maintenance during daemon startup.
		release, err = gate.Acquire(ctx, ViewBuildRequired)
		if err != nil {
			return fmt.Errorf("indexer: wait for dedicated base refresh admission: %w", err)
		}
	}
	_, newBaseGenerationID, buildErr := l.buildDedicatedCorpusSnapshot(
		ctx, graph.GraphID, checkout, graph.RepoPrefix, coordinator,
		checkoutSample{tree: oldBase.TreeOID, commit: oldBase.ProvenanceCommitOID},
	)
	release()
	if buildErr != nil {
		return buildErr
	}

	head, err := sampleCheckout(ctx, checkout.RootPath)
	if err != nil {
		coordinator.abandonBuild(ctx, newBaseGenerationID, true)
		return err
	}
	if head.tree == "" {
		coordinator.abandonBuild(ctx, newBaseGenerationID, true)
		return fmt.Errorf("indexer: checkout %s has no HEAD tree during base refresh", checkout.RootPath)
	}

	var committed store_sqlite.CommitDedicatedBaseRefreshResult
	newBase := primaryBase{
		graphID: graph.GraphID, generationID: newBaseGenerationID, treeOID: oldBase.TreeOID,
	}
	_, err = coordinator.preparePromotion(ctx, newBase, head.tree,
		func(ctx context.Context, route store_sqlite.CheckoutRoute, routed bool, graphID string,
			commitGeneration, dirtyGeneration int64) error {
			topology, topologyErr := l.AcquireCheckoutGraphTopology(ctx, graphID)
			if topologyErr != nil {
				return topologyErr
			}
			defer topology.Release()
			var commitErr error
			committed, commitErr = l.catalog.CommitDedicatedBaseRefresh(ctx,
				store_sqlite.CommitDedicatedBaseRefreshRequest{
					CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation,
					FamilyID: checkout.FamilyID, GraphID: graphID,
					RequiredGraphState:       store_sqlite.DedicatedGraphStateRefreshing,
					ExpectedBaseGenerationID: oldBase.GenerationID,
					NewBaseGenerationID:      newBaseGenerationID,
					BaseTreeOID:              oldBase.TreeOID,
					ConfigHash:               desired.configHash,
					ExtractorVersions:        desired.extractorVersions,
					ResolverVersion:          desired.resolverVersion,
					CommitGenerationID:       commitGeneration,
					DirtyGenerationID:        dirtyGeneration,
					CommitTreeOID:            head.tree,
					RouteExists:              routed, ExpectedRouteEpoch: route.RouteEpoch,
					LastSeen: l.now().Unix(),
				})
			return commitErr
		})
	if err != nil {
		current, currentFound, readErr := l.catalog.GetDedicatedGraph(ctx, graph.GraphID)
		if readErr == nil && currentFound && current.ActiveGenerationID != newBaseGenerationID {
			coordinator.abandonBuild(ctx, newBaseGenerationID, true)
		}
		return err
	}

	for _, generationID := range committed.RetiredGenerationIDs {
		l.oweRetirement(generationID)
	}
	for _, checkoutID := range committed.InvalidatedCheckoutIDs {
		l.SignalCheckout(checkoutID, "dedicated base pipeline refreshed")
	}
	l.reconcileFamilyNow(ctx, graph.FamilyID, checkout.RootPath)
	l.sweepRetirements(ctx)
	return nil
}
