package reconcile

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Closure is everything retiring one primary dedicated graph would take with
// it, and everything in its family that survives.
//
// It is the payload behind an untrack preview of a primary checkout. The
// caller shows it and asks; the confirm runs RetirePrimaryClosure with the
// epoch this preview read, so a primary that moved between the two is refused
// rather than retired on a stale picture.
type Closure struct {
	// GraphID is the primary dedicated graph the closure is rooted in.
	GraphID string
	// FamilyID is the family it belongs to.
	FamilyID string
	// OwnerCheckoutID is the checkout that owns the primary graph.
	OwnerCheckoutID string
	// RepoPrefix is the corpus the primary graph's nodes live under.
	RepoPrefix string
	// PrimaryEpoch is the family's compare-and-set token at preview time.
	PrimaryEpoch int64
	// Dependents are the rows the retirement removes, in the order the saga
	// removes them: the graph itself, then every automatic checkout that could
	// only be served because of it — with its route and its built layers —
	// then the owner's own stack and the owner identity, then the named views
	// rooted in the graph, and finally the family row when nothing is left to
	// serve it from.
	Dependents []Dependent
	// Preserved are the family's other dedicated graphs. They are served from
	// corpora of their own and survive the retirement.
	Preserved []Dependent
	// SoleGraph reports that the primary is the family's only dedicated graph,
	// so retiring it continues into a family teardown.
	SoleGraph bool
}

// PrimaryClosure enumerates what retiring one primary dedicated graph takes
// with it.
//
// Everything it reports is read from the catalog rows rather than derived from
// what happens to be running, so a preview taken while the daemon holds no
// coordinator at all still names the routes and layers a restart would find.
func (r *Reconciler) PrimaryClosure(ctx context.Context, graphID string) (Closure, error) {
	if graphID == "" {
		return Closure{}, fmt.Errorf("%w: a closure needs a graph id", ErrSagaTarget)
	}
	dedicated, ok, err := r.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return Closure{}, err
	}
	if !ok {
		return Closure{}, fmt.Errorf("%w: dedicated graph %s", store_sqlite.ErrCatalogNotFound, graphID)
	}
	out := Closure{
		GraphID:         dedicated.GraphID,
		FamilyID:        dedicated.FamilyID,
		OwnerCheckoutID: dedicated.OwnerCheckoutID,
		RepoPrefix:      dedicated.RepoPrefix,
	}
	family, ok, err := r.catalog.GetRepositoryFamily(ctx, dedicated.FamilyID)
	if err != nil {
		return Closure{}, err
	}
	if !ok {
		return Closure{}, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, dedicated.FamilyID)
	}
	out.PrimaryEpoch = family.PrimaryEpoch

	out.Dependents = append(out.Dependents, Dependent{
		Kind:   DependentGraph,
		ID:     dedicated.GraphID,
		Detail: "the family's primary corpus " + dedicated.RepoPrefix + " is retired",
	})

	checkouts, err := r.catalog.ListCheckouts(ctx, dedicated.FamilyID)
	if err != nil {
		return Closure{}, err
	}
	for _, checkout := range checkouts {
		if checkout.CheckoutID == dedicated.OwnerCheckoutID ||
			checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
			continue
		}
		out.Dependents = append(out.Dependents, Dependent{
			Kind:   DependentCheckout,
			ID:     checkout.CheckoutID,
			Detail: "checkout " + checkout.AdminName + " is served from this primary graph",
		})
		rows, err := r.checkoutStack(ctx, checkout)
		if err != nil {
			return Closure{}, err
		}
		out.Dependents = append(out.Dependents, rows...)
	}

	// The owner's own route and layers go too — a primary that was itself
	// demoted and re-promoted still carries them — and then the owner itself:
	// the saga forgets that checkout, because forgetting it is what releases
	// the graph. It is listed for the same reason every other removed row is,
	// so what a caller confirms is what the retirement takes.
	owner, present, err := r.catalog.GetCheckout(ctx, dedicated.OwnerCheckoutID)
	if err != nil {
		return Closure{}, err
	}
	if present {
		rows, err := r.checkoutStack(ctx, owner)
		if err != nil {
			return Closure{}, err
		}
		out.Dependents = append(out.Dependents, rows...)
		out.Dependents = append(out.Dependents, Dependent{
			Kind:   DependentCheckout,
			ID:     owner.CheckoutID,
			Detail: "checkout " + owner.AdminName + " owns this primary graph and is forgotten with it",
		})
	}

	views, err := r.catalog.ListRefViews(ctx, dedicated.GraphID)
	if err != nil {
		return Closure{}, err
	}
	for _, view := range views {
		out.Dependents = append(out.Dependents, Dependent{
			Kind:   DependentRefView,
			ID:     view.RefViewID,
			Detail: "view " + view.SelectorValue + " is rooted in this graph",
		})
	}

	graphs, err := r.catalog.ListDedicatedGraphs(ctx, dedicated.FamilyID)
	if err != nil {
		return Closure{}, err
	}
	for _, sibling := range graphs {
		if sibling.GraphID == dedicated.GraphID {
			continue
		}
		out.Preserved = append(out.Preserved, Dependent{
			Kind:   DependentGraph,
			ID:     sibling.GraphID,
			Detail: "corpus " + sibling.RepoPrefix + " is independent of this primary and is kept",
		})
	}
	out.SoleGraph = len(out.Preserved) == 0
	if out.SoleGraph {
		// Nothing is left to serve the family from, so the retirement carries
		// on into a teardown of every checkout it still holds and of the
		// family row itself. Naming it as a dependent is what puts it in front
		// of the caller as a row that goes rather than as a flag to interpret.
		out.Dependents = append(out.Dependents, Dependent{
			Kind:   DependentFamily,
			ID:     dedicated.FamilyID,
			Detail: "family " + dedicated.FamilyID + " has no other corpus, so it is torn down with this one",
		})
	}
	return out, nil
}

// checkoutStack enumerates one checkout's route and the layers built for it.
func (r *Reconciler) checkoutStack(
	ctx context.Context, checkout store_sqlite.Checkout,
) ([]Dependent, error) {
	var out []Dependent
	route, present, err := r.catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	if err != nil {
		return nil, err
	}
	if present {
		out = append(out, Dependent{
			Kind:   DependentRoute,
			ID:     route.CheckoutID,
			Detail: "queries for checkout " + checkout.AdminName + " are routed to graph " + route.GraphID,
		})
	}
	layers, err := r.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		CheckoutID: checkout.CheckoutID,
	})
	if err != nil {
		return nil, err
	}
	for _, layer := range layers {
		out = append(out, Dependent{
			Kind:   DependentLayer,
			ID:     fmt.Sprintf("%d", layer.GenerationID),
			Detail: layer.GenerationKind + " layer of checkout " + checkout.AdminName,
		})
	}
	return out, nil
}

// RetireDedicatedGraph gives up one non-primary dedicated graph without
// touching the checkout that owned it.
//
// It is what a demotion runs once the checkout is being served from the
// family's primary instead: the corpus and the views rooted in it are gone,
// the identity stays. The primary base is refused — retiring that one takes a
// closure with it, which is RetirePrimaryClosure's job and carries the epoch
// guard this call has no business bypassing.
func (r *Reconciler) RetireDedicatedGraph(ctx context.Context, graphID string) error {
	if graphID == "" {
		return fmt.Errorf("%w: retire_dedicated_graph needs a graph id", ErrSagaTarget)
	}
	target := sagaTarget{Kind: sagaRetireGraph, GraphID: graphID}
	dedicated, ok, err := r.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if ok {
		if dedicated.IsPrimaryBase {
			return fmt.Errorf("%w: graph %s is the primary base of family %s",
				ErrSagaTarget, graphID, dedicated.FamilyID)
		}
		target.FamilyID = dedicated.FamilyID
	}
	return r.enterSaga(ctx, target)
}
