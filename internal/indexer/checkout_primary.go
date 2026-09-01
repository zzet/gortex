package indexer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// rebuildBudget bounds how long one automatic checkout may take to rebuild its
// stack onto a new primary before the promotion gives up on it and moves to
// the next.
//
// It is a whole index of one checkout's difference from the new base, so the
// budget is generous — a checkout that misses it keeps the route it has, which
// is a stale view of a real state rather than a failure.
const rebuildBudget = 2 * time.Minute

// ErrPrimaryNotReady reports a graph that cannot become a family's primary
// base: it does not exist, belongs to another family, or is not ready to
// serve.
var ErrPrimaryNotReady = errors.New("indexer: this graph cannot become the family primary")

// SetPrimaryPreview is what moving a family's primary base would do.
type SetPrimaryPreview struct {
	FamilyID string
	// GraphID / RepoPrefix name the graph that would become the primary.
	GraphID    string
	RepoPrefix string
	// CurrentGraphID / CurrentRepoPrefix name the incumbent, empty when the
	// family has no primary at all.
	CurrentGraphID    string
	CurrentRepoPrefix string
	// PrimaryEpoch is the family's compare-and-set token at preview time.
	PrimaryEpoch int64
	// Dependents are the automatic checkouts that will rebuild their layers
	// over the new base. Each one carries whether a coordinator is live for it
	// and where its route points today.
	Dependents []reconcile.Dependent
	// Ready reports that the confirm would be accepted.
	Ready bool
	// Blockers explains a preview that is not ready.
	Blockers []string
}

// SetPrimaryResult is what one primary move did.
type SetPrimaryResult struct {
	FamilyID string
	GraphID  string
	// Rebuilt are the automatic checkouts now serving over the new base.
	Rebuilt []string
	// Stale are the automatic checkouts that could not rebuild. Each one keeps
	// the route it had, and the next sweep tries again.
	Stale []string
	// Errors are the per-checkout rebuild failures, in the order they were hit.
	Errors []error
}

// PreviewSetPrimary validates a primary move and enumerates what will rebuild.
//
// It writes nothing. The epoch it reads is carried into the confirm, so a
// primary that moved between the preview and the confirm is refused rather
// than overwritten.
func (l *CheckoutLifecycle) PreviewSetPrimary(ctx context.Context, graphID string) (SetPrimaryPreview, error) {
	if l == nil || l.catalog == nil {
		return SetPrimaryPreview{}, errNoCatalog
	}
	out := SetPrimaryPreview{GraphID: graphID}
	dedicated, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: no dedicated graph %s", ErrPrimaryNotReady, graphID)
	}
	out.FamilyID, out.RepoPrefix = dedicated.FamilyID, dedicated.RepoPrefix

	family, found, err := l.catalog.GetRepositoryFamily(ctx, dedicated.FamilyID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, dedicated.FamilyID)
	}
	out.PrimaryEpoch = family.PrimaryEpoch

	graphs, err := l.catalog.ListDedicatedGraphs(ctx, dedicated.FamilyID)
	if err != nil {
		return out, err
	}
	for _, candidate := range graphs {
		if candidate.IsPrimaryBase {
			out.CurrentGraphID, out.CurrentRepoPrefix = candidate.GraphID, candidate.RepoPrefix
			break
		}
	}

	if dedicated.State != reconcile.GraphStateReady {
		out.Blockers = append(out.Blockers,
			"graph "+graphID+" is "+dedicated.State+", not ready to serve")
	}
	if l.mi.GetIndexer(dedicated.RepoPrefix) == nil {
		out.Blockers = append(out.Blockers,
			"corpus "+dedicated.RepoPrefix+" is bound but not served: nothing can be composed over it yet")
	}
	owner, ownerFound, err := l.catalog.GetCheckout(ctx, dedicated.OwnerCheckoutID)
	if err != nil {
		return out, err
	}
	switch {
	case !ownerFound:
		out.Blockers = append(out.Blockers, "graph "+graphID+" has no owning checkout")
	case owner.State != store_sqlite.CheckoutStateReady:
		out.Blockers = append(out.Blockers,
			"checkout "+owner.AdminName+" is "+string(owner.State)+", so its corpus cannot be the family base")
	}

	checkouts, err := l.catalog.ListCheckouts(ctx, dedicated.FamilyID)
	if err != nil {
		return out, err
	}
	for _, checkout := range checkouts {
		if checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
			continue
		}
		detail := "checkout " + checkout.AdminName + " rebuilds its layers over " + dedicated.RepoPrefix
		route, routed, err := l.catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
		if err != nil {
			return out, err
		}
		if routed {
			detail += "; routed to " + route.GraphID + " today"
		}
		if l.hasCoordinator(checkout.CheckoutID) {
			detail += "; a coordinator is live for it"
		}
		out.Dependents = append(out.Dependents, reconcile.Dependent{
			Kind:   reconcile.DependentCheckout,
			ID:     checkout.CheckoutID,
			Detail: detail,
		})
	}
	out.Ready = len(out.Blockers) == 0
	return out, nil
}

// SetPrimary moves a family's base corpus to one dedicated graph.
//
// The catalog move is a compare-and-set on the family's primary epoch, so two
// actors deciding at once end with one primary rather than two half-installed
// ones. What follows is the part that costs something: every automatic
// checkout of the family has a stack composed over the OLD base, and none of
// it means anything over the new one.
//
// Each of those checkouts is rebuilt off-route and flipped in one write, so a
// query that materializes mid-transition reads either the whole old stack or
// the whole new one. Rebuilding through the ordinary cycle would not do: it
// repoints the route at the new graph first and builds afterwards, which
// leaves the checkout with a route naming a base it has no layers over for the
// length of a full build.
//
// A checkout that cannot rebuild inside its budget keeps the route it had and
// is reported. That route composes over the corpus of a graph that is no
// longer the primary, which is a real state of the world the checkout was in —
// and the next sweep, finding it still there, rebuilds it.
//
// Every dependent's coordinator therefore comes out of the registry BEFORE the
// compare-and-set, not when its own turn comes. The registry is the only thing
// that can hand a coordinator a cycle, and a cycle taken after the epoch has
// moved is an ordinary one: it repoints the route at the new base and clears
// both slots for the length of a full build, which is the window this whole
// flow exists to close.
func (l *CheckoutLifecycle) SetPrimary(ctx context.Context, graphID string) (SetPrimaryResult, error) {
	initial, err := l.PreviewSetPrimary(ctx, graphID)
	if err != nil {
		return SetPrimaryResult{}, err
	}
	// The physical build lane is always outside topology. Promotion and
	// demotion build first, then acquire family/checkout topology only at their
	// publication callback. Taking these in the opposite order here creates a
	// lock cycle: SetPrimary holds topology waiting for the lane while a
	// promotion holds the lane waiting for topology.
	releaseBuild := func() {}
	if gate := l.buildGate(); gate != nil {
		releaseBuild, err = gate.Acquire(ctx, ViewBuildRequired)
		if err != nil {
			return SetPrimaryResult{FamilyID: initial.FamilyID, GraphID: graphID},
				fmt.Errorf("indexer: wait for primary-switch build admission: %w", err)
		}
	}
	defer releaseBuild()
	familyTopology, err := l.AcquireCheckoutFamilyTopology(ctx, initial.FamilyID)
	if err != nil {
		return SetPrimaryResult{FamilyID: initial.FamilyID, GraphID: graphID}, err
	}
	defer familyTopology.Release()

	var preview SetPrimaryPreview
	var topology *CheckoutTopologyToken
	for {
		candidate, err := l.PreviewSetPrimary(ctx, graphID)
		if err != nil {
			return SetPrimaryResult{}, err
		}
		out := SetPrimaryResult{FamilyID: candidate.FamilyID, GraphID: graphID}
		if !candidate.Ready {
			return out, fmt.Errorf("%w: %s: %s", ErrPrimaryNotReady, graphID,
				strings.Join(candidate.Blockers, "; "))
		}
		if candidate.CurrentGraphID == graphID {
			return out, nil
		}

		topology, err = l.AcquireCheckoutTopology(ctx, primaryDependentIDs(candidate)...)
		if err != nil {
			return out, err
		}
		confirmed, confirmErr := l.PreviewSetPrimary(ctx, graphID)
		if confirmErr != nil {
			topology.Release()
			return out, confirmErr
		}
		if confirmed.CurrentGraphID == graphID {
			topology.Release()
			return out, nil
		}
		if !confirmed.Ready {
			topology.Release()
			return out, fmt.Errorf("%w: %s: %s", ErrPrimaryNotReady, graphID,
				strings.Join(confirmed.Blockers, "; "))
		}
		if !samePrimaryPreview(candidate, confirmed) {
			topology.Release()
			if err := ctx.Err(); err != nil {
				return out, err
			}
			continue
		}
		preview = confirmed
		break
	}
	defer topology.Release()
	out := SetPrimaryResult{FamilyID: preview.FamilyID, GraphID: graphID}
	endBatch := l.beginBatch()
	defer func() {
		// Session invalidation may synchronously trigger reconciliation. Release
		// topology first so the coalesced notification cannot re-enter this
		// family's own primary-switch fence.
		topology.Release()
		familyTopology.Release()
		// The coalesced invalidation below may synchronously reconcile and
		// acquire the build lane. Release it after topology but before the
		// notification edge to avoid self-deadlock.
		releaseBuild()
		endBatch()
	}()

	// A move that then fails the compare-and-set leaves every dependent
	// coordinator-less with its route untouched, so the automatic views keep
	// serving exactly what they served and the next sweep starts them again.
	for _, dependent := range preview.Dependents {
		l.dropCoordinatorFenced(dependent.ID)
	}

	err = l.catalog.SetPrimaryDedicatedGraph(ctx, store_sqlite.SetPrimaryDedicatedGraphRequest{
		FamilyID:             preview.FamilyID,
		GraphID:              graphID,
		ExpectedPrimaryEpoch: preview.PrimaryEpoch,
		LastSeen:             l.now().Unix(),
	})
	if err != nil {
		return out, err
	}

	for _, dependent := range preview.Dependents {
		checkout, found, err := l.catalog.GetCheckout(ctx, dependent.ID)
		if err != nil || !found {
			continue
		}
		if err := l.rehomeCheckoutFenced(ctx, checkout, graphID); err != nil {
			out.Stale = append(out.Stale, checkout.CheckoutID)
			out.Errors = append(out.Errors, fmt.Errorf("checkout %s: %w", checkout.AdminName, err))
			l.logger.Warn("checkout lifecycle: a checkout could not rebuild onto the new primary",
				zap.String("checkout", checkout.CheckoutID),
				zap.String("graph", graphID), zap.Error(err))
			continue
		}
		out.Rebuilt = append(out.Rebuilt, checkout.CheckoutID)
	}
	l.notifyTrackedSetChanged()
	return out, nil
}

func primaryDependentIDs(preview SetPrimaryPreview) []string {
	ids := make([]string, 0, len(preview.Dependents))
	for _, dependent := range preview.Dependents {
		if dependent.ID != "" {
			ids = append(ids, dependent.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func samePrimaryPreview(left, right SetPrimaryPreview) bool {
	return left.FamilyID == right.FamilyID &&
		left.GraphID == right.GraphID &&
		left.RepoPrefix == right.RepoPrefix &&
		left.CurrentGraphID == right.CurrentGraphID &&
		left.PrimaryEpoch == right.PrimaryEpoch &&
		slices.Equal(primaryDependentIDs(left), primaryDependentIDs(right))
}

// rehomeCheckout rebuilds one automatic checkout's stack over a new base and
// installs it in one route write.
//
// The running coordinator is dropped first, and the replacement is not
// registered until the rebuild has landed. Both halves matter: a coordinator
// built for the old primary stamps the old primary's repo prefix onto
// everything it builds, and a registered coordinator's loop would race the
// rebuild to the route with a cycle that clears both slots. The drop is
// repeated here rather than left to the caller's sweep of the dependents,
// because a reconciliation running beside the move can put one back.
func (l *CheckoutLifecycle) rehomeCheckoutFenced(
	ctx context.Context, checkout store_sqlite.Checkout, graphID string,
) error {
	l.dropCoordinatorFenced(checkout.CheckoutID)
	coordinator, err := l.buildCoordinator(ctx, graphID, checkout)
	if err != nil {
		return err
	}
	if coordinator == nil {
		return fmt.Errorf("indexer: graph %s cannot back a coordinator yet", graphID)
	}
	bounded, cancel := context.WithTimeout(ctx, rebuildBudget)
	defer cancel()
	if _, err := coordinator.prepareRehomeToAdmitted(bounded, graphID, coordinator.installStack); err != nil {
		_ = coordinator.Close()
		l.oweRetirement(coordinator.DrainRetirements()...)
		return err
	}
	if !l.installCoordinatorWithHeadFenced(
		checkout.CheckoutID, coordinator, checkoutHeadIdentity{}, false,
	) {
		return fmt.Errorf("indexer: checkout %s coordinator moved during primary switch", checkout.CheckoutID)
	}
	return nil
}

// hasCoordinator reports whether this process is running one checkout's build
// loop — the registered coordinator, or the one a transition is driving a
// rebuild with before it registers anything.
func (l *CheckoutLifecycle) hasCoordinator(checkoutID string) bool {
	if l == nil {
		return false
	}
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	if _, registered := l.coordinators[checkoutID]; registered {
		return true
	}
	return l.runningLocked(checkoutID)
}
