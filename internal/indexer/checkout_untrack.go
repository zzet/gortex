package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// UntrackPlan names what an untrack of one checkout will actually do. Untrack
// is one verb over four quite different transactions, and which one it is
// depends on what else the family holds — so the plan is decided once, by the
// preview, and the confirm executes it rather than deciding again.
type UntrackPlan string

const (
	// UntrackPlanEvict has no catalog identity to reason about: a store with
	// no catalog, or a directory git does not administer. The repository
	// simply leaves the corpus.
	UntrackPlanEvict UntrackPlan = "evict"
	// UntrackPlanDemote hands a checkout to the family's automatic lane. Its
	// corpus goes; its identity, and the view a session gets for it, stay.
	UntrackPlanDemote UntrackPlan = "demote"
	// UntrackPlanForget removes the checkout and everything referencing it.
	// It is what an inaccessible checkout takes: there is no working tree to
	// build automatic layers from, so there is nothing to demote it to.
	UntrackPlanForget UntrackPlan = "forget"
	// UntrackPlanPrimaryClosure retires a primary graph and everything that
	// could only be served because of it.
	UntrackPlanPrimaryClosure UntrackPlan = "primary_closure"
	// UntrackPlanBlocked is an untrack that would leave the family with
	// nothing to serve the checkout from. It names the ways forward instead.
	UntrackPlanBlocked UntrackPlan = "blocked"
)

// ErrUntrackBlocked reports an untrack that cannot be carried out as asked.
// The error names the paths that can.
var ErrUntrackBlocked = errors.New("indexer: this checkout cannot be untracked on its own")

// UntrackPreview is what an untrack of one path or prefix would do.
//
// It is the payload the CLI and the tool surface render before asking, and the
// same value the confirm runs off, so what a user is shown and what happens
// cannot drift apart.
type UntrackPreview struct {
	Prefix      string
	CheckoutID  string
	Incarnation string
	FamilyID    string
	GraphID     string
	// Plan is the transaction the confirm will run.
	Plan UntrackPlan
	// Accessible reports whether the checkout's root answered when the
	// catalog last looked.
	Accessible bool
	// IsPrimary reports that the checkout owns its family's base corpus.
	IsPrimary bool
	// SolePrimary reports that no dedicated graph in the family survives the
	// retirement, so the closure carries on into a family teardown. It is the
	// difference between "this repository stops being served from here" and
	// "the daemon forgets this repository", which is the whole of what a
	// caller has to be shown before it confirms.
	SolePrimary bool
	// PrimaryEpoch is the family's compare-and-set token, carried into the
	// confirm so a primary that moved between the two is refused.
	PrimaryEpoch int64
	// Closure is everything the confirm removes, for a plan that removes
	// anything beyond the checkout itself.
	Closure []reconcile.Dependent
	// Preserved is what survives it.
	Preserved []reconcile.Dependent
	// Blockers explains a blocked plan: what is missing, and what to do
	// instead.
	Blockers []string
}

// resolveDestructivePrefix resolves only identities the caller named
// explicitly. Unlike ResolvePrefix, it never interprets a bare, unknown token
// relative to the daemon's working directory: doing that in a destructive
// flow can turn a typo into a path inside an unrelated tracked repository.
//
// Exact live prefixes and exact durable graph prefixes are accepted. Filesystem
// containment is accepted only for absolute paths, including paths whose
// checkout is currently absent from the in-memory index but still has a
// catalog identity.
func (l *CheckoutLifecycle) resolveDestructivePrefix(ctx context.Context, pathOrPrefix string) (string, error) {
	if l == nil || l.mi == nil || pathOrPrefix == "" {
		return "", nil
	}
	if meta := l.mi.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix, nil
	}
	if l.catalog != nil {
		graph, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(pathOrPrefix))
		if err != nil {
			return "", err
		}
		if ok && graph.RepoPrefix == pathOrPrefix {
			return pathOrPrefix, nil
		}
	}
	if !filepath.IsAbs(pathOrPrefix) {
		return "", nil
	}
	if prefix := l.ResolvePrefix(pathOrPrefix); prefix != "" {
		return prefix, nil
	}
	if l.catalog == nil {
		return "", nil
	}
	checkout, found, err := l.checkoutForPath(ctx, pathOrPrefix)
	if err != nil || !found {
		return "", err
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return "", err
	}
	for _, graph := range graphs {
		if graph.OwnerCheckoutID == checkout.CheckoutID && graph.RepoPrefix != "" {
			return graph.RepoPrefix, nil
		}
	}
	return "", nil
}

// PreviewUntrack decides what untracking one path or prefix would do, and
// enumerates what it would take with it.
//
// Nothing is written. Every branch below is a property of the catalog rows, so
// a preview and the confirm that follows it read the same evidence — the only
// thing that can come between them is another actor moving the rows, which is
// what the incarnation and epoch guards on the confirm are for.
type untrackDependentKey struct {
	kind string
	id   string
}

func uniqueUntrackDependents(dependents []reconcile.Dependent) []reconcile.Dependent {
	if len(dependents) < 2 {
		return dependents
	}
	seen := make(map[untrackDependentKey]struct{}, len(dependents))
	out := dependents[:0]
	for _, dependent := range dependents {
		key := untrackDependentKey{kind: string(dependent.Kind), id: dependent.ID}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, dependent)
	}
	return out
}

func (l *CheckoutLifecycle) PreviewUntrack(ctx context.Context, pathOrPrefix string) (UntrackPreview, error) {
	if l == nil || l.mi == nil {
		return UntrackPreview{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	prefix, err := l.resolveDestructivePrefix(ctx, pathOrPrefix)
	if err != nil {
		return UntrackPreview{}, err
	}
	if prefix == "" {
		return UntrackPreview{}, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, pathOrPrefix)
	}
	out := UntrackPreview{Prefix: prefix, Plan: UntrackPlanEvict}

	checkout, err := l.checkoutForPrefix(ctx, prefix)
	if err != nil || checkout == nil {
		return out, err
	}
	out.CheckoutID, out.Incarnation = checkout.CheckoutID, checkout.Incarnation
	out.FamilyID = checkout.FamilyID
	out.Accessible = checkout.State == store_sqlite.CheckoutStateReady

	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return out, err
	}
	var owned, primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkout.CheckoutID {
			owned = &graphs[i]
		}
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
		}
	}
	if owned != nil {
		out.GraphID = owned.GraphID
		out.IsPrimary = owned.IsPrimaryBase
	}

	if out.IsPrimary {
		closure, err := l.rec.PrimaryClosure(ctx, owned.GraphID)
		if err != nil {
			return out, err
		}
		out.Plan = UntrackPlanPrimaryClosure
		out.PrimaryEpoch = closure.PrimaryEpoch
		out.Closure, out.Preserved = uniqueUntrackDependents(closure.Dependents), closure.Preserved
		out.SolePrimary = closure.SoleGraph
		return out, nil
	}

	if !out.Accessible {
		// A root that cannot be read cannot be rebuilt into automatic layers
		// either, so the only thing left to do with it is to let it go.
		dependents, err := l.rec.Dependents(ctx, checkout.CheckoutID)
		if err != nil {
			return out, err
		}
		out.Plan, out.Closure = UntrackPlanForget, dependents
		return out, nil
	}

	servable := primary != nil &&
		primary.OwnerCheckoutID != checkout.CheckoutID &&
		primary.State == reconcile.GraphStateReady
	if !servable {
		out.Plan = UntrackPlanBlocked
		out.Blockers = []string{
			"family " + checkout.FamilyID + " has no other ready primary corpus to serve this checkout from",
			"set another checkout's graph as the family primary, then untrack this one",
			"or preview a forget, which removes the checkout and its corpus outright",
		}
		return out, nil
	}
	family, found, err := l.catalog.GetRepositoryFamily(ctx, checkout.FamilyID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, checkout.FamilyID)
	}
	out.Plan = UntrackPlanDemote
	out.PrimaryEpoch = family.PrimaryEpoch
	if owned != nil {
		out.Closure = append(out.Closure, reconcile.Dependent{
			Kind:   reconcile.DependentGraph,
			ID:     owned.GraphID,
			Detail: "corpus " + owned.RepoPrefix + " is retired; the checkout is served from the family primary",
		})
	}
	views, err := l.catalog.ListRefViews(ctx, out.GraphID)
	if err != nil {
		return out, err
	}
	for _, view := range views {
		out.Closure = append(out.Closure, reconcile.Dependent{
			Kind:   reconcile.DependentRefView,
			ID:     view.RefViewID,
			Detail: "view " + view.SelectorValue + " is rooted in this graph",
		})
	}
	out.Preserved = append(out.Preserved, reconcile.Dependent{
		Kind:   reconcile.DependentCheckout,
		ID:     checkout.CheckoutID,
		Detail: "checkout " + checkout.AdminName + " keeps its identity and is served from graph " + primary.GraphID,
	})
	out.Closure = uniqueUntrackDependents(out.Closure)
	return out, nil
}

// PreviewForget decides what forgetting one path or prefix would do.
//
// Forget is the deliberate removal untrack refuses to be: where untrack looks
// for a way to keep the checkout — demoting it into the family's automatic
// lane, or refusing when it cannot — forget says the identity and its corpus
// are to go. The primary branch is unchanged, because retiring a primary is
// already the removal it looks like.
func (l *CheckoutLifecycle) PreviewForget(ctx context.Context, pathOrPrefix string) (UntrackPreview, error) {
	preview, err := l.PreviewUntrack(ctx, pathOrPrefix)
	if err != nil {
		return preview, err
	}
	switch preview.Plan {
	case UntrackPlanEvict, UntrackPlanForget, UntrackPlanPrimaryClosure:
		return preview, nil
	}
	// A demote or a blocked untrack both describe a checkout that could have
	// kept its identity. The closure is re-read as the removal it is instead.
	dependents, err := l.rec.Dependents(ctx, preview.CheckoutID)
	if err != nil {
		return preview, err
	}
	preview.Plan = UntrackPlanForget
	preview.Preserved, preview.Blockers = nil, nil
	preview.Closure = dependents
	if preview.GraphID != "" {
		preview.Closure = append([]reconcile.Dependent{{
			Kind:   reconcile.DependentGraph,
			ID:     preview.GraphID,
			Detail: "corpus " + preview.Prefix + " is retired with the checkout",
		}}, preview.Closure...)
	}
	preview.Closure = append(preview.Closure, reconcile.Dependent{
		Kind:   reconcile.DependentCheckout,
		ID:     preview.CheckoutID,
		Detail: "the checkout identity is removed rather than demoted to the automatic lane",
	})
	return preview, nil
}

// demote hands one dedicated checkout to the family's automatic lane.
//
// The whole automatic stack is built while the checkout is still being served
// from its own corpus, and the route that installs it is one write. Nothing a
// reader can see moves until that write lands: before it the checkout is
// dedicated and reads its own corpus, after it the checkout is automatic and
// reads the primary's corpus under its own layers, and there is no state in
// between in which it is neither.
//
// The corpus it is leaving goes last, after the flip and through the guarded
// retirement path, so a view materialized just before the flip keeps its lease
// on what it is reading.
func (l *CheckoutLifecycle) demote(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	owned *store_sqlite.DedicatedGraph,
	authorization reconcile.DemotionAuthorization,
) error {
	expectedCoordinator := l.coordinatorFor(checkout.CheckoutID)
	coordinator, err := l.buildCoordinatorWithPoll(
		ctx, authorization.PrimaryGraphID, checkout, -time.Nanosecond,
	)
	if err != nil {
		return err
	}
	if coordinator == nil {
		return fmt.Errorf("indexer: the primary graph %s cannot serve checkout %s yet",
			authorization.PrimaryGraphID, checkout.CheckoutID)
	}
	finishPrepared := func() {
		if coordinator == nil {
			return
		}
		_ = coordinator.Close()
		l.oweRetirement(coordinator.DrainRetirements()...)
		coordinator = nil
	}
	defer finishPrepared()

	// Offer the private graph's payload before the catalog transaction journals
	// its retirement. The journal is the crash-recovery authority; this in-memory
	// backlog merely lets the current process reclaim generations promptly.
	if owned != nil {
		l.oweRetirement(l.graphGenerations(ctx, owned.GraphID)...)
	}
	var commit reconcile.DemotionCommitResult
	var commitErr error
	var topologyCtx context.Context
	var releaseTopology func()
	defer func() {
		if releaseTopology != nil {
			releaseTopology()
		}
	}()
	_, prepareErr := coordinator.prepareRehomeTo(
		ctx,
		authorization.PrimaryGraphID,
		func(
			ctx context.Context,
			route store_sqlite.CheckoutRoute,
			routed bool,
			_ primaryBase,
			commitGeneration, dirtyGeneration int64,
		) error {
			// The build above is off-route and needs no mutation fence. Acquire
			// only at the publication edge, while the old dedicated coordinator
			// is still alive to finish every previously admitted ticket, and keep
			// the checkout exclusive through the registry replacement below.
			if releaseTopology == nil {
				var topologyErr error
				topologyCtx, releaseTopology, topologyErr = l.reconcileFamilyCheckoutTopologyGuard(
					ctx,
					[]string{checkout.FamilyID},
					[]string{checkout.CheckoutID},
				)
				if topologyErr != nil {
					return topologyErr
				}
			}
			commit, commitErr = l.rec.CommitAuthorizedDemotion(
				topologyCtx,
				checkout,
				authorization,
				reconcile.DemotionPublication{
					ExpectedRoute:    route,
					RouteExists:      routed,
					CommitGeneration: commitGeneration,
					DirtyGeneration:  dirtyGeneration,
				},
			)
			if commit.Committed {
				// Graph cleanup is outside the publication transaction. Its error
				// keeps the durable transition standing, but the prepared stack is
				// now the route and must be adopted by the automatic coordinator.
				return nil
			}
			if commitErr == nil {
				return fmt.Errorf("%w: demotion publication did not commit",
					store_sqlite.ErrCatalogStaleGuard)
			}
			return commitErr
		},
	)
	if prepareErr != nil {
		if releaseTopology != nil {
			releaseTopology()
			releaseTopology = nil
		}
		finishPrepared()
		l.sweepRetirements(ctx)
		return prepareErr
	}

	replacement, err := l.buildCoordinator(ctx, authorization.PrimaryGraphID, checkout)
	if err != nil {
		if releaseTopology != nil {
			releaseTopology()
			releaseTopology = nil
		}
		finishPrepared()
		l.sweepRetirements(ctx)
		return fmt.Errorf("indexer: start demoted checkout coordinator: %w", err)
	}
	if replacement == nil {
		if releaseTopology != nil {
			releaseTopology()
			releaseTopology = nil
		}
		finishPrepared()
		l.sweepRetirements(ctx)
		return fmt.Errorf("indexer: the primary graph %s cannot keep serving checkout %s",
			authorization.PrimaryGraphID, checkout.CheckoutID)
	}
	installed := l.replaceCoordinatorFenced(
		checkout.CheckoutID,
		expectedCoordinator,
		replacement,
	)
	// The catalog flip above made this checkout automatic. Publish its source
	// watcher at the same boundary as the coordinator so filesystem events can
	// drive prompt dirty-layer rebuilds instead of waiting for the coordinator's
	// polling fallback. Watcher admission is deliberately best-effort: Ensure
	// owns retries and cleanup, while the already-published route remains
	// queryable if the filesystem backend is temporarily unavailable.
	demoted := checkout
	demoted.State = store_sqlite.CheckoutStateReady
	demoted.DesiredMode = store_sqlite.CheckoutModeAutomatic
	demoted.EffectiveMode = store_sqlite.CheckoutModeAutomatic
	if installed {
		if releaseTopology != nil {
			releaseTopology()
			releaseTopology = nil
		}
		l.ensureCheckoutSourceSignalWatcher(demoted, authorization.PrimaryGraphID)
	} else {
		if releaseTopology != nil {
			releaseTopology()
			releaseTopology = nil
		}
		finishPrepared()
		l.sweepRetirements(ctx)
		return fmt.Errorf("%w: checkout %s coordinator moved during demotion",
			store_sqlite.ErrCatalogStaleGuard, checkout.CheckoutID)
	}
	finishPrepared()

	if commitErr != nil {
		// The mode flip and graph-retirement journal committed together. Keep
		// the transition standing until the retryable graph saga has deleted
		// both the corpus and its durable binding under one admission tombstone.
		l.logger.Warn("checkout lifecycle: demoted graph cleanup remains journalled",
			zap.String("checkout", checkout.CheckoutID),
			zap.String("graph", authorization.OwnedGraphID), zap.Error(commitErr))
		l.sweepRetirements(ctx)
		return fmt.Errorf("indexer: retire demoted graph: %w", commitErr)
	}

	if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID,
		authorization.Transition.TransitionID); err != nil {
		l.sweepRetirements(ctx)
		return fmt.Errorf("indexer: complete demotion transition: %w", err)
	}
	l.sweepRetirements(ctx)
	return nil
}

// graphGenerations lists the payload generations built over one graph, so a
// retirement can offer them once nothing routes to them any more.
func (l *CheckoutLifecycle) graphGenerations(ctx context.Context, graphID string) []int64 {
	rows, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{GraphID: graphID})
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not list a graph's generations",
			zap.String("graph", graphID), zap.Error(err))
		return nil
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GenerationID)
	}
	return out
}

// blockedUntrack renders a blocked preview as the error the caller gets.
func blockedUntrack(preview UntrackPreview) error {
	return fmt.Errorf("%w: %s: %s", ErrUntrackBlocked, preview.Prefix,
		strings.Join(preview.Blockers, "; "))
}
