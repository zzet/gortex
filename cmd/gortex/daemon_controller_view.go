package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// probeView is the graph one path-scoped control probe reads through, plus
// what the answer says about it.
//
// Every field is derived from the catalog, never guessed: a probe that cannot
// establish which graph serves a path says so instead of answering from a
// graph that happens to be at hand.
type probeView struct {
	// answer rides on the wire. nil when the request carried no path, which
	// is what keeps an older client's response byte for byte what it was.
	answer *daemon.ProbeView
	// reader is the composed routed stack. nil means the base corpus.
	reader graph.Reader
	// repoPrefix is the corpus the path's content lives under. It keys the
	// file lookup and filters the nodes a coverage answer counts.
	repoPrefix string
	// searchScope narrows a symbol probe to one repo prefix. It is empty for
	// a dedicated checkout and for an untracked path, because narrowing them
	// would change the answer those callers already get.
	searchScope string
	// root is the working copy the path is relative to, empty when no
	// checkout owns it.
	root string
	// servable reports that something can answer for this path at all. It is
	// false for a registered checkout whose exact route or generation-backed
	// base is not ready: answering from generation zero would describe a
	// different state while looking authoritative.
	servable bool
	// release drops the lease a materialized view holds. Never nil.
	release func()
}

// noopRelease is the release of a view that leased nothing.
func noopRelease() {}

// probeReconcileDebounce bounds how often one family is reconciled on a
// probe's behalf. A PreToolUse hook probes once per tool call, so an agent
// working inside an unrouted worktree would otherwise raise the same
// reconciliation dozens of times a minute while the first one still runs.
const probeReconcileDebounce = 30 * time.Second

type topologyNudgeLease struct {
	once    sync.Once
	release func()
}

type topologyNudgeRequest struct {
	ctx   context.Context
	lease *topologyNudgeLease
}

type topologyReconcileSourceContextKey struct{}

func withTopologyReconcileSource(ctx context.Context, source string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == "" {
		return ctx
	}
	return context.WithValue(ctx, topologyReconcileSourceContextKey{}, source)
}

func topologyReconcileSource(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	source, _ := ctx.Value(topologyReconcileSourceContextKey{}).(string)
	return source
}

type topologyReconcileFamilyContextKey struct{}

func withTopologyReconcileFamily(ctx context.Context, familyID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, topologyReconcileFamilyContextKey{}, familyID)
}

func topologyReconcileFamily(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	familyID, _ := ctx.Value(topologyReconcileFamilyContextKey{}).(string)
	return familyID
}

func newTopologyNudgeRequest(ctx context.Context, release func()) topologyNudgeRequest {
	if ctx == nil {
		ctx = context.Background()
	}
	request := topologyNudgeRequest{ctx: context.WithoutCancel(ctx)}
	if release != nil {
		request.lease = &topologyNudgeLease{release: release}
	}
	return request
}

func (request *topologyNudgeRequest) finish() {
	if request == nil || request.lease == nil {
		return
	}
	request.lease.once.Do(request.lease.release)
}

type topologyNudgeState struct {
	currentSource string
	pending       *topologyNudgeRequest
}

// resolveProbeView decides which graph answers a probe about path.
//
// The order is the catalog's: a path no checkout owns reads generation zero;
// a generation-backed dedicated checkout opens its sealed active base; and a
// ready route for either mode opens the composed checkout view. Anything short
// of the catalog-selected view is reported rather than approximated.
func (c *realController) resolveProbeView(ctx context.Context, path string) probeView {
	view := c.selectProbeView(ctx, path)
	recordProbeAnswer(view.answer)
	return view
}

// recordProbeAnswer counts one path-scoped probe by the kind of view that
// answered and whether that view was the path's own.
//
// A probe that names no view at all — a daemon with no view catalog — is not
// counted: it has no view model to have an opinion with, and folding it into
// the base bucket would make a daemon without routed views look like one whose
// worktrees keep falling back.
func recordProbeAnswer(answer *daemon.ProbeView) {
	if answer == nil {
		return
	}
	exact := viewmetrics.AnswerFallback
	if answer.Exact {
		exact = viewmetrics.AnswerExact
	}
	viewmetrics.Count(viewmetrics.ProbeAnswerTotal, answer.Kind, exact)
}

// selectProbeView is resolveProbeView's decision, split out so every path
// through it is counted exactly once.
func (c *realController) selectProbeView(ctx context.Context, path string) probeView {
	base := probeView{servable: true, release: noopRelease}
	if c == nil || path == "" || c.lifecycle == nil || c.viewMaterializer == nil {
		// No view catalog is wired, so there are no composed views to route to
		// and the indexed corpus is the only graph there is. The answer names
		// no view at all rather than claiming the base is the path's own: this
		// daemon has no view model to have an opinion with.
		return base
	}

	binding, err := c.lifecycle.ExplainView(ctx, path)
	if err != nil {
		// The binding is an optimization over the base corpus, not a
		// precondition for answering. The path may well sit inside a routed
		// checkout, so the degradation rides on the answer rather than
		// passing for the path's own view.
		base.answer = fallbackProbeView(daemon.ProbeViewBase, "", "", daemon.FallbackCheckoutInaccessible)
		return base
	}

	unrouted := func() probeView {
		c.nudgeFamily(binding.FamilyID)
		return probeView{
			answer:   fallbackProbeView(daemon.ProbeViewUnrouted, binding.CheckoutID, binding.RepoPrefix, daemon.FallbackViewBuilding),
			root:     binding.RootPath,
			servable: false,
			release:  noopRelease,
		}
	}

	if binding.Matched && binding.CheckoutState != string(store_sqlite.CheckoutStateReady) {
		// Availability and removal grace are read-only fallbacks even for a
		// checkout that was dedicated. The path is not live, so presenting its
		// retained corpus as an exact working-copy view would make stale data
		// indistinguishable from the checkout itself.
		fallback := probeView{
			answer:      fallbackProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix, binding.CheckoutState),
			repoPrefix:  binding.RepoPrefix,
			searchScope: binding.RepoPrefix,
			root:        binding.RootPath,
			servable:    true,
			release:     noopRelease,
		}
		if binding.ActiveGenerationID <= 0 {
			if !c.probeBindingStillCurrent(ctx, path, binding) {
				fallback.servable = false
				c.nudgeFamily(binding.FamilyID)
			}
			return fallback
		}
		if binding.GraphID == "" ||
			(binding.GraphState != store_sqlite.DedicatedGraphStateReady &&
				binding.GraphState != store_sqlite.DedicatedGraphStateRefreshing) {
			fallback.servable = false
			c.nudgeFamily(binding.FamilyID)
			return fallback
		}
		view, viewErr := c.viewMaterializer.MaterializeBase(
			ctx, binding.GraphID, binding.ActiveGenerationID,
		)
		if viewErr != nil {
			if c.logger != nil {
				c.logger.Debug("probe view: could not materialize the grace base",
					zap.String("checkout", binding.CheckoutID), zap.Error(viewErr))
			}
			fallback.servable = false
			c.nudgeFamily(binding.FamilyID)
			return fallback
		}
		if !materializedBaseMatchesBinding(view, binding) ||
			!c.probeBindingStillCurrent(ctx, path, binding) {
			view.Close()
			fallback.servable = false
			c.nudgeFamily(binding.FamilyID)
			return fallback
		}
		fallback.reader = view.Reader
		fallback.release = view.Close
		return fallback
	}

	if !binding.Matched {
		// An ordinary tracked path with no checkout identity remains on the
		// legacy corpus.
		base.answer = exactProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix)
		base.repoPrefix = binding.RepoPrefix
		base.root = binding.RootPath
		return base
	}

	if binding.Composed {
		view, viewErr := c.viewMaterializer.MaterializeCheckout(ctx, binding.CheckoutID)
		if viewErr == nil {
			if binding.Route == nil ||
				!materializedCheckoutMatchesRoute(
					view, binding,
					binding.Route.CommitGenerationID,
					binding.Route.DirtyGenerationID,
					binding.ActiveGenerationID,
				) || !c.probeBindingStillCurrent(ctx, path, binding) {
				view.Close()
				return unrouted()
			}
			kind := daemon.ProbeViewBase
			if binding.EffectiveMode == string(store_sqlite.CheckoutModeAutomatic) {
				kind = daemon.ProbeViewWorktree
			}
			return probeView{
				answer:      exactProbeView(kind, binding.CheckoutID, binding.RepoPrefix),
				reader:      view.Reader,
				repoPrefix:  binding.RepoPrefix,
				searchScope: binding.RepoPrefix,
				root:        binding.RootPath,
				servable:    true,
				release:     view.Close,
			}
		}
		if c.logger != nil {
			c.logger.Debug("probe view: could not materialize the checkout's view",
				zap.String("checkout", binding.CheckoutID), zap.Error(viewErr))
		}
		return unrouted()
	}

	if binding.EffectiveMode == string(store_sqlite.CheckoutModeAutomatic) || binding.Route != nil {
		// Automatic checkouts always require both routed layers. A dedicated
		// checkout with a standing incomplete route also cannot fall back to its
		// base: that would hide the state the route is publishing.
		return unrouted()
	}

	if binding.GraphID == "" || binding.GraphState != store_sqlite.DedicatedGraphStateReady {
		return unrouted()
	}
	direct := probeView{
		answer:     exactProbeView(daemon.ProbeViewBase, binding.CheckoutID, binding.RepoPrefix),
		repoPrefix: binding.RepoPrefix,
		root:       binding.RootPath,
		servable:   true,
		release:    noopRelease,
	}
	if binding.ActiveGenerationID <= 0 {
		// Compatibility for graphs created before generation-backed bases.
		if !c.probeBindingStillCurrent(ctx, path, binding) {
			return unrouted()
		}
		return direct
	}
	view, viewErr := c.viewMaterializer.MaterializeBase(
		ctx, binding.GraphID, binding.ActiveGenerationID,
	)
	if viewErr != nil {
		if c.logger != nil {
			c.logger.Debug("probe view: could not materialize the dedicated base",
				zap.String("checkout", binding.CheckoutID), zap.Error(viewErr))
		}
		return unrouted()
	}
	if !materializedBaseMatchesBinding(view, binding) ||
		!c.probeBindingStillCurrent(ctx, path, binding) {
		view.Close()
		return unrouted()
	}
	direct.reader = view.Reader
	direct.release = view.Close
	return direct
}

// exactProbeView names a graph that is the path's own.
func exactProbeView(kind, checkoutID, repoPrefix string) *daemon.ProbeView {
	return &daemon.ProbeView{
		Kind:       kind,
		CheckoutID: checkoutID,
		RepoPrefix: repoPrefix,
		Exact:      true,
	}
}

// fallbackProbeView names a graph that is not the path's own, with the reason
// it stood in. Exact is false by construction: there is no way to build one of
// these that claims to be exact.
func fallbackProbeView(kind, checkoutID, repoPrefix, reason string) *daemon.ProbeView {
	return &daemon.ProbeView{
		Kind:           kind,
		CheckoutID:     checkoutID,
		RepoPrefix:     repoPrefix,
		Exact:          false,
		FallbackReason: reason,
	}
}

func sameProbeBinding(left, right indexer.ViewBinding) bool {
	if left.Matched != right.Matched || left.FamilyID != right.FamilyID ||
		left.CheckoutID != right.CheckoutID || left.Incarnation != right.Incarnation ||
		left.RootPath != right.RootPath || left.CheckoutState != right.CheckoutState ||
		left.EffectiveMode != right.EffectiveMode || left.GraphID != right.GraphID ||
		left.GraphState != right.GraphState ||
		left.ActiveGenerationID != right.ActiveGenerationID ||
		left.RepoPrefix != right.RepoPrefix || left.PrimaryGraphID != right.PrimaryGraphID ||
		left.Composed != right.Composed {
		return false
	}
	if left.Route == nil || right.Route == nil {
		return left.Route == nil && right.Route == nil
	}
	return *left.Route == *right.Route
}

func (c *realController) probeBindingStillCurrent(
	ctx context.Context, path string, expected indexer.ViewBinding,
) bool {
	if c == nil || c.lifecycle == nil {
		return false
	}
	if c.probeViewRevalidateBarrier != nil {
		c.probeViewRevalidateBarrier()
	}
	current, err := c.lifecycle.ExplainView(ctx, path)
	return err == nil && sameProbeBinding(expected, current)
}

func materializedBaseMatchesBinding(view *graphview.RepoView, binding indexer.ViewBinding) bool {
	if view == nil || binding.ActiveGenerationID <= 0 ||
		view.ID.BaseGraphID != binding.GraphID ||
		view.ID.RepoPrefix != binding.RepoPrefix ||
		view.ID.BaseGeneration != binding.ActiveGenerationID {
		return false
	}
	generations := view.Generations()
	return len(generations) == 1 && generations[0] == binding.ActiveGenerationID
}

func materializedCheckoutMatchesRoute(
	view *graphview.RepoView,
	binding indexer.ViewBinding,
	commitGenerationID, dirtyGenerationID int64,
	activeGenerationID int64,
) bool {
	if view == nil || activeGenerationID <= 0 ||
		view.ID.BaseGraphID != binding.GraphID ||
		view.ID.RepoPrefix != binding.RepoPrefix ||
		view.ID.BaseGeneration != commitGenerationID {
		return false
	}
	generations := view.Generations()
	return len(generations) == 3 && generations[0] == activeGenerationID &&
		generations[len(generations)-2] == commitGenerationID &&
		generations[len(generations)-1] == dirtyGenerationID
}

func checkoutRouteMatchesBinding(route store_sqlite.CheckoutRoute, binding indexer.ViewBinding) bool {
	return binding.Route != nil && binding.Route.GraphID == route.GraphID &&
		binding.Route.CommitGenerationID == route.CommitGenerationID &&
		binding.Route.DirtyGenerationID == route.DirtyGenerationID &&
		binding.Route.RouteEpoch == route.RouteEpoch &&
		binding.Route.State == string(route.State) &&
		binding.Route.Ready == graphview.RouteReady(route)
}

// TrackReadiness proves that the exact routed view for path can be opened now.
// It deliberately reads only the checkout catalog and generation metadata;
// unlike the legacy status heuristic, no node or edge scan is part of a poll.
func (c *realController) TrackReadiness(ctx context.Context, path string) (daemon.TrackReadiness, error) {
	legacy := daemon.TrackReadiness{State: daemon.TrackReadinessLegacy}
	if c == nil || c.lifecycle == nil || c.viewMaterializer == nil || c.viewMaterializer.Catalog == nil {
		return legacy, nil
	}

	binding, err := c.lifecycle.ExplainView(ctx, path)
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("resolve track readiness for %q: %w", path, err)
	}
	if !binding.Matched {
		// A configured non-Git root has no checkout identity or route. It is
		// the one case that still needs the historical stable node-count poll.
		if binding.RepoPrefix != "" {
			return legacy, nil
		}
		return trackViewBuilding("", "", "repository is not registered in the checkout catalog yet"), nil
	}

	failed := func(reason string) daemon.TrackReadiness {
		if reason == "" {
			reason = "the checkout promotion failed"
		}
		result := daemon.TrackReadiness{
			State: daemon.TrackReadinessFailed,
			View:  fallbackProbeView(daemon.ProbeViewUnrouted, binding.CheckoutID, binding.RepoPrefix, daemon.FallbackViewBuilding),
			Error: reason,
		}
		result.View.Incarnation = binding.Incarnation
		return result
	}

	catalog := c.viewMaterializer.Catalog
	latest, err := catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		CheckoutID: binding.CheckoutID,
		Limit:      1,
	})
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read latest generation for checkout %q: %w", binding.CheckoutID, err)
	}
	building := func(reason string) daemon.TrackReadiness {
		// A failed unpublished generation has no route to inspect below. Prefer
		// the newest attempt only: an older failure must not mask a newer retry.
		if len(latest) == 1 && latest[0].State == store_sqlite.ViewGenerationFailed {
			return failed(latest[0].Error)
		}
		if len(latest) == 1 && latest[0].State == store_sqlite.ViewGenerationBuilding {
			if processReason, failedInProcess := c.lifecycle.CheckoutBuildFailure(
				binding.CheckoutID, latest[0].GenerationID,
			); failedInProcess {
				return failed(processReason)
			}
		}
		result := trackViewBuilding(binding.CheckoutID, binding.RepoPrefix, reason)
		result.View.Incarnation = binding.Incarnation
		return result
	}
	ready := func(kind string) daemon.TrackReadiness {
		result := daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  exactProbeView(kind, binding.CheckoutID, binding.RepoPrefix),
		}
		result.View.Incarnation = binding.Incarnation
		return result
	}

	if binding.CheckoutState != string(store_sqlite.CheckoutStateReady) {
		return building("checkout state is " + binding.CheckoutState), nil
	}

	transition, transitioning, err := catalog.GetIntentTransition(ctx, binding.CheckoutID)
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read checkout transition %q: %w", binding.CheckoutID, err)
	}
	if transitioning {
		if transition.State == store_sqlite.IntentTransitionFailed {
			return failed(transition.LastError), nil
		}
		return building("checkout promotion is " + string(transition.State)), nil
	}

	checkout, found, err := catalog.GetCheckout(ctx, binding.CheckoutID)
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read checkout %q: %w", binding.CheckoutID, err)
	}
	if !found {
		return building("checkout catalog row is not published yet"), nil
	}
	if _, moving, moveErr := catalog.GetCheckoutRootMove(ctx, binding.CheckoutID); moveErr != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read checkout root move %q: %w", binding.CheckoutID, moveErr)
	} else if moving {
		return building("checkout root move recovery is pending"), nil
	}
	var (
		graphRow   store_sqlite.DedicatedGraph
		graphFound bool
	)
	if binding.GraphID != "" {
		graphRow, graphFound, err = catalog.GetDedicatedGraph(ctx, binding.GraphID)
		if err != nil {
			return daemon.TrackReadiness{}, fmt.Errorf("read dedicated graph %q: %w", binding.GraphID, err)
		}
		if graphFound && graphRow.GraphID == binding.GraphID &&
			graphRow.RepoPrefix == binding.RepoPrefix &&
			graphRow.ActiveGenerationID == binding.ActiveGenerationID &&
			graphRow.State == store_sqlite.DedicatedGraphStateRefreshing {
			if refreshFailure, failedInProcess := c.lifecycle.DedicatedBaseRefreshFailure(
				graphRow.GraphID, graphRow.ActiveGenerationID,
			); failedInProcess {
				return failed(refreshFailure), nil
			}
		}
	}
	// A base refresh failure is checked first because it owns every dependent
	// route in the graph. A still-pending automatic coordinator must not mask
	// the owner's terminal required-publication verdict.
	startupStatus := c.lifecycle.CheckoutStartupBuildStatus
	if c.checkoutStartupBuildStatus != nil {
		startupStatus = c.checkoutStartupBuildStatus
	}
	startupPending, startupFailure := startupStatus(binding.CheckoutID)
	if startupFailure != "" {
		return failed(startupFailure), nil
	}
	if startupPending {
		return building("startup checkout reconciliation is pending"), nil
	}

	route, routed, err := catalog.GetCheckoutRoute(ctx, binding.CheckoutID)
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read checkout route %q: %w", binding.CheckoutID, err)
	}
	if routed && !graphview.RouteReady(route) {
		return building("checkout route is not active and complete"), nil
	}
	if routed && !checkoutRouteMatchesBinding(route, binding) {
		return building("checkout route changed while readiness was being checked"), nil
	}
	if !routed && binding.Route != nil {
		return building("checkout route changed while readiness was being checked"), nil
	}
	if routed && (binding.GraphID == "" || route.GraphID != binding.GraphID) {
		return building("checkout route does not target the graph selected for this path"), nil
	}
	if !routed && binding.EffectiveMode == string(store_sqlite.CheckoutModeAutomatic) {
		return building("checkout route is not active and complete"), nil
	}
	if binding.GraphID == "" {
		return building("checkout has no selected dedicated graph"), nil
	}

	if !graphFound || graphRow.State != store_sqlite.DedicatedGraphStateReady {
		return building("dedicated graph has no active ready generation"), nil
	}
	if graphRow.GraphID != binding.GraphID || graphRow.RepoPrefix != binding.RepoPrefix ||
		graphRow.State != binding.GraphState ||
		graphRow.ActiveGenerationID != binding.ActiveGenerationID {
		return building("dedicated graph changed while readiness was being checked"), nil
	}
	if graphRow.ActiveGenerationID > 0 {
		active, activeFound, activeErr := catalog.GetViewGeneration(ctx, graphRow.ActiveGenerationID)
		if activeErr != nil {
			return daemon.TrackReadiness{}, fmt.Errorf("read active generation %d: %w", graphRow.ActiveGenerationID, activeErr)
		}
		if !activeFound || active.State != store_sqlite.ViewGenerationReady {
			if activeFound && active.State == store_sqlite.ViewGenerationFailed {
				return failed(active.Error), nil
			}
			return building("dedicated graph generation is not ready"), nil
		}
	} else if routed {
		return building("dedicated graph has no active ready generation"), nil
	}

	if !routed {
		if graphRow.ActiveGenerationID > 0 {
			view, materializeErr := c.viewMaterializer.MaterializeBase(
				ctx, graphRow.GraphID, graphRow.ActiveGenerationID,
			)
			if materializeErr != nil {
				var coded interface{ ErrorCode() string }
				if errors.As(materializeErr, &coded) &&
					(coded.ErrorCode() == graphview.CodeViewBuilding ||
						coded.ErrorCode() == graphview.CodePrimaryNotReady) {
					return building(materializeErr.Error()), nil
				}
				return failed(materializeErr.Error()), nil
			}
			if !materializedBaseMatchesBinding(view, binding) ||
				!c.probeBindingStillCurrent(ctx, path, binding) {
				view.Close()
				return building("dedicated graph changed while readiness was being checked"), nil
			}
			view.Close()
		} else if !c.probeBindingStillCurrent(ctx, path, binding) {
			return building("dedicated graph changed while readiness was being checked"), nil
		}
		return ready(daemon.ProbeViewBase), nil
	}

	// This is the exact HEAD fence request routing applies before exposing a
	// ready-looking route. A stale commit generation remains view_building.
	routedCommit, found, err := catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read routed commit generation %d: %w", route.CommitGenerationID, err)
	}
	if !found {
		return building("routed commit generation is not published"), nil
	}
	if routedCommit.State == store_sqlite.ViewGenerationFailed {
		return failed(routedCommit.Error), nil
	}
	if routedCommit.State != store_sqlite.ViewGenerationReady && routedCommit.State != store_sqlite.ViewGenerationSuperseded {
		return building("routed commit generation is not servable"), nil
	}
	if routedCommit.TreeOID != checkout.HeadTree {
		return building("routed commit generation is stale for checkout HEAD"), nil
	}
	dirty, dirtyFound, dirtyErr := catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if dirtyErr != nil {
		return daemon.TrackReadiness{}, fmt.Errorf("read routed dirty generation %d: %w", route.DirtyGenerationID, dirtyErr)
	}
	if !dirtyFound {
		return building("routed dirty generation is not published"), nil
	}
	if dirty.State == store_sqlite.ViewGenerationFailed {
		return failed(dirty.Error), nil
	}
	if dirty.State != store_sqlite.ViewGenerationReady && dirty.State != store_sqlite.ViewGenerationSuperseded {
		return building("routed dirty generation is not servable"), nil
	}

	// MaterializeCheckout is the final query gate: it revalidates the route
	// around a generation lease, opens every routed/base generation, and
	// refuses a moving, incomplete, or unservable stack.
	view, err := c.viewMaterializer.MaterializeCheckout(ctx, binding.CheckoutID)
	if err != nil {
		var coded interface{ ErrorCode() string }
		if errors.As(err, &coded) && coded.ErrorCode() == graphview.CodeViewBuilding {
			return building(err.Error()), nil
		}
		return failed(err.Error()), nil
	}
	if !materializedCheckoutMatchesRoute(
		view, binding, route.CommitGenerationID, route.DirtyGenerationID,
		graphRow.ActiveGenerationID,
	) {
		view.Close()
		return building("checkout route does not compose over the selected active base"), nil
	}
	if !c.probeBindingStillCurrent(ctx, path, binding) {
		view.Close()
		return building("checkout route changed while readiness was being checked"), nil
	}
	view.Close()

	kind := daemon.ProbeViewBase
	if binding.EffectiveMode == string(store_sqlite.CheckoutModeAutomatic) {
		kind = daemon.ProbeViewWorktree
	}
	return ready(kind), nil
}

func trackViewBuilding(checkoutID, repoPrefix, reason string) daemon.TrackReadiness {
	return daemon.TrackReadiness{
		State: daemon.TrackReadinessBuilding,
		View:  fallbackProbeView(daemon.ProbeViewUnrouted, checkoutID, repoPrefix, daemon.FallbackViewBuilding),
		Error: reason,
	}
}

// FileCoverage answers whether the graph serving Path holds definition
// symbols for it, and how many.
//
// It is the coverage verdict a PreToolUse hook turns into a deny, so the
// answer is scoped to the view the probing path actually reads: a worktree
// served by its family's automatic lane is answered from its composed view,
// and one whose view is not built yet is answered as uncovered so the hook
// allows the native tool through.
func (c *realController) FileCoverage(ctx context.Context, p daemon.FileCoverageParams) (daemon.FileCoverageResult, error) {
	if c == nil || p.Path == "" {
		return daemon.FileCoverageResult{}, nil
	}
	abs := p.Path
	if resolved, err := filepath.Abs(abs); err == nil {
		abs = resolved
	}

	view := c.resolveProbeView(ctx, abs)
	defer view.release()

	out := daemon.FileCoverageResult{View: view.answer}
	if !view.servable {
		return out, nil
	}
	reader := view.reader
	if reader == nil {
		reader = c.graph
	}
	if reader == nil {
		return out, nil
	}
	prefix, key, ok := c.fileGraphKey(abs, view)
	if !ok {
		return out, nil
	}
	for _, n := range reader.GetFileNodes(key) {
		// The file and import nodes ride on the by-file index for other
		// walkers; the coverage question is "what does this file define".
		if !probeSymbolCandidate(n, prefix) {
			continue
		}
		out.Symbols++
	}
	out.Covered = out.Symbols > 0
	return out, nil
}

// fileGraphKey renders an absolute path the way the graph spells a file key:
// the repo prefix, then the path relative to the working copy's root.
//
// A path the daemon has no root for cannot be keyed, and a path that escapes
// the root it was measured against is not in that repository at all. Both
// report ok=false, which the caller reads as "not covered" rather than
// guessing a key.
func (c *realController) fileGraphKey(abs string, view probeView) (prefix, key string, ok bool) {
	prefix, root := view.repoPrefix, view.root
	if root == "" {
		prefix, root, ok = c.trackedRoot(abs)
		if !ok {
			return "", "", false
		}
	}
	if prefix == "" {
		// A checkout the catalog knows but whose graph row names no prefix
		// still has a corpus its root belongs to.
		prefix = c.lifecycle.ResolvePrefix(root)
	}
	rel, ok := pathRelativeTo(root, abs)
	if !ok {
		return "", "", false
	}
	if prefix == "" {
		return "", rel, true
	}
	return prefix, prefix + "/" + rel, true
}

// pathRelativeTo measures abs against root, retrying with symlinks resolved
// when the two are spelled differently.
//
// Git spells a worktree root with its symlinks resolved and a caller spells
// its path the way the shell handed it over — which on macOS is a path through
// /var into /private/var. Comparing one spelling only reports a file inside the
// checkout as outside it, which reads as "not covered" for a file that plainly
// is. The file itself need not exist: only its directory is resolved, so a
// path naming a file about to be created still measures correctly.
func pathRelativeTo(root, abs string) (string, bool) {
	if rel, ok := relativeUnder(root, abs); ok {
		return rel, true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	realAbs := abs
	if dir, dirErr := filepath.EvalSymlinks(filepath.Dir(abs)); dirErr == nil {
		realAbs = filepath.Join(dir, filepath.Base(abs))
	}
	return relativeUnder(realRoot, realAbs)
}

// relativeUnder returns abs relative to root, or ok=false when abs is not
// inside it.
func relativeUnder(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return rel, true
}

// trackedRoot finds the tracked repository root that contains abs, longest
// match first so a repository nested inside another resolves to the inner one.
func (c *realController) trackedRoot(abs string) (prefix, root string, ok bool) {
	if c.multiIndexer == nil {
		return "", "", false
	}
	prefix = c.multiIndexer.RepoForFile(abs)
	if prefix == "" {
		return "", "", false
	}
	root, ok = c.multiIndexer.RepoRoot(prefix)
	return prefix, root, ok && root != ""
}

// nudgeFamily asks for one family's reconciliation on a probe's behalf,
// debounced per family and never on the probe's own goroutine.
//
// The probe is the first thing that notices a working copy nobody has routed
// yet — the janitor's tick is an hour away — but it is also the call an agent
// waits on before every tool use. Asking and returning is the only shape that
// serves both: the reconciliation runs through the lifecycle's own per-family
// path, and this probe answers from what exists right now.
func (c *realController) nudgeFamily(familyID string) {
	if c == nil || familyID == "" {
		return
	}
	run := c.probeReconcile
	if run == nil {
		if c.lifecycle == nil {
			return
		}
		run = c.reconcileFamilyForProbe
	}
	if !c.claimFamilyNudge(familyID) {
		return
	}
	go run(familyID)
}

func (c *realController) nudgeFamilyTopologyRequest(ctx context.Context, familyID string, release func()) {
	request := newTopologyNudgeRequest(ctx, release)
	if c == nil || familyID == "" {
		request.finish()
		return
	}
	// A watcher removal can synchronously promote another member and emit a
	// callback using this reconcile's context. That callback is an effect of
	// the current authoritative pass, not new external evidence; queueing it
	// behind itself retains a watcher lease the same pass may need to drain.
	if topologyReconcileFamily(ctx) == familyID {
		request.finish()
		return
	}
	run := func(runCtx context.Context, id string) {
		if c.topologyReconcile != nil {
			c.topologyReconcile(runCtx, id)
			return
		}
		if c.probeReconcile != nil {
			c.probeReconcile(id)
			return
		}
		if c.lifecycle != nil {
			c.reconcileFamilyForProbeContext(runCtx, id)
		}
	}
	if c.topologyReconcile == nil && c.probeReconcile == nil && c.lifecycle == nil {
		request.finish()
		return
	}

	c.topologyNudgeMu.Lock()
	if c.topologyNudges == nil {
		c.topologyNudges = make(map[string]*topologyNudgeState)
	}
	if state := c.topologyNudges[familyID]; state != nil {
		// The attachment catalog pass is a durable backstop, not newer
		// filesystem evidence. If a watcher request already owns this family,
		// that pass covers the same inventory and must not become trailing.
		if topologyReconcileSource(request.ctx) == "catalog" {
			c.topologyNudgeMu.Unlock()
			request.finish()
			return
		}
		// A watcher event arriving during any active pass is real trailing
		// evidence and must still run, but its dispatch lease cannot wait behind
		// that pass: the active reconcile may synchronously retire the same
		// watcher and wait for admitted dispatches to drain. Detach the queued
		// request from that lease while preserving its context and execution.
		request.finish()
		request.lease = nil
		superseded := state.pending
		state.pending = &request
		c.topologyNudgeMu.Unlock()
		superseded.finish()
		return
	}
	c.topologyNudges[familyID] = &topologyNudgeState{
		currentSource: topologyReconcileSource(request.ctx),
	}
	c.topologyNudgeMu.Unlock()

	go c.runTopologyNudgeLoop(run, familyID, request)
}

func (c *realController) runTopologyNudgeLoop(
	run func(context.Context, string), familyID string, current topologyNudgeRequest,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.topologyNudgeMu.Lock()
			state := c.topologyNudges[familyID]
			var pending *topologyNudgeRequest
			if state != nil {
				pending = state.pending
				delete(c.topologyNudges, familyID)
			}
			c.topologyNudgeMu.Unlock()
			pending.finish()
			panic(recovered)
		}
	}()

	for {
		func() {
			defer current.finish()
			run(withTopologyReconcileFamily(current.ctx, familyID), familyID)
		}()

		c.topologyNudgeMu.Lock()
		state := c.topologyNudges[familyID]
		if state != nil && state.pending != nil {
			current = *state.pending
			state.pending = nil
			state.currentSource = topologyReconcileSource(current.ctx)
			c.topologyNudgeMu.Unlock()
			continue
		}
		delete(c.topologyNudges, familyID)
		c.topologyNudgeMu.Unlock()
		return
	}
}

// claimFamilyNudge reports whether this caller won the right to reconcile the
// family now, stamping the window when it did.
func (c *realController) claimFamilyNudge(familyID string) bool {
	c.probeNudgeMu.Lock()
	defer c.probeNudgeMu.Unlock()
	if last, seen := c.probeNudgedAt[familyID]; seen && time.Since(last) < probeReconcileDebounce {
		return false
	}
	if c.probeNudgedAt == nil {
		c.probeNudgedAt = make(map[string]time.Time)
	}
	c.probeNudgedAt[familyID] = time.Now()
	return true
}

// reconcileFamilyForProbe runs the lifecycle's own family reconciliation. A
// failure is logged and dropped: the janitor asks the same question on its own
// schedule, and a probe must not turn a reconciliation failure into an answer.
func (c *realController) reconcileFamilyForProbe(familyID string) {
	c.reconcileFamilyForProbeContext(context.Background(), familyID)
}

func (c *realController) reconcileFamilyForProbeContext(ctx context.Context, familyID string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := c.lifecycle.ReconcileFamily(ctx, familyID); err != nil && c.logger != nil {
		c.logger.Debug("probe view: reconciling the family failed",
			zap.String("family", familyID),
			zap.String("source", topologyReconcileSource(ctx)),
			zap.Error(err))
	}
}
