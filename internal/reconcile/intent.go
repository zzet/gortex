package reconcile

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Row states this package writes and reads back.
//
// The catalog stores them as free-form TEXT, so agreeing on the spelling in
// one place is what keeps a "is the primary ready" test from silently never
// matching because a writer spelled it differently.
const (
	// FamilyStateReady is a family whose common-dir identity is known.
	FamilyStateReady = "family_ready"
	// GraphStateReady is a dedicated graph that can serve queries.
	GraphStateReady = "graph_ready"
)

// ErrIntentNotRevocable reports that a checkout is still wanted by a tracking
// intent that an explicit forget may not withdraw. Removing the checkout
// anyway would tear down something another source asked for and would be
// re-created by the next pass over that source.
var ErrIntentNotRevocable = errors.New("reconcile: tracking intent cannot be revoked by an explicit forget")

// revocableIntentKinds is the single policy list shared by the public
// predicate and the catalog's atomic revocation transaction.
var revocableIntentKinds = []store_sqlite.IntentSourceKind{
	store_sqlite.IntentSourceCLITrack,
	store_sqlite.IntentSourceMCPTrack,
	store_sqlite.IntentSourceManualConfig,
}

// RevocableIntent reports whether an explicit forget may withdraw an intent.
//
// The rule is about who owns the decision. An intent someone typed — a CLI
// track, a tool call, a line in the configuration — is withdrawn by the same
// person asking for the opposite. An intent that exists because the checkout
// belongs to something else (a tracked project) is not: the membership is
// still true after the forget, so revoking it here would only be undone.
func RevocableIntent(kind store_sqlite.IntentSourceKind) bool {
	for _, candidate := range revocableIntentKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// IntentRevocation is what a forget preflight found and did.
type IntentRevocation struct {
	// Revoked are the intents this preflight marked inactive.
	Revoked []store_sqlite.TrackingIntent
	// Blocked are the active intents that may not be revoked here. A
	// non-empty Blocked means the forget must not proceed.
	Blocked []store_sqlite.TrackingIntent
}

// Blocked reports whether the preflight refused the forget.
func (r IntentRevocation) IsBlocked() bool { return len(r.Blocked) > 0 }

// RevokeTrackingIntents withdraws every revocable intent on a checkout and
// reports the ones it may not touch.
//
// It writes nothing when a non-revocable intent is present: a preflight that
// half-revoked and then refused would leave the checkout tracked by fewer
// reasons than the caller can see, which is exactly the drift the catalog
// exists to prevent.
func (r *Reconciler) RevokeTrackingIntents(ctx context.Context, checkoutID string) (IntentRevocation, error) {
	if checkoutID == "" {
		return IntentRevocation{}, fmt.Errorf("%w: revoking intents needs a checkout id", ErrSagaTarget)
	}
	revoked, blocked, err := r.catalog.RevokeTrackingIntents(
		ctx,
		checkoutID,
		r.now().Unix(),
		revocableIntentKinds,
	)
	out := IntentRevocation{Revoked: revoked, Blocked: blocked}
	if err != nil {
		return out, err
	}
	if out.IsBlocked() {
		return out, fmt.Errorf("%w: checkout %s is still wanted by %d intent(s)",
			ErrIntentNotRevocable, checkoutID, len(out.Blocked))
	}
	return out, nil
}

// DependentKind names what kind of row depends on a checkout.
type DependentKind string

const (
	// DependentCheckout is another checkout of the family that is served
	// from the primary rather than from a graph of its own.
	DependentCheckout DependentKind = "checkout"
	// DependentRefView is a named view rooted in the checkout's graph.
	DependentRefView DependentKind = "ref_view"
	// DependentRoute is a checkout route pointing at the graph.
	DependentRoute DependentKind = "route"
	// DependentGraph is a dedicated graph — the corpus a checkout's nodes
	// live in.
	DependentGraph DependentKind = "graph"
	// DependentLayer is one built payload generation of a checkout.
	DependentLayer DependentKind = "layer"
	// DependentFamily is the repository family row itself, which goes when
	// the retirement leaves it with no graph to be served from.
	DependentFamily DependentKind = "family"
)

// Dependent is one row that only exists because a checkout does.
type Dependent struct {
	Kind DependentKind
	// ID is the dependent row's own identifier.
	ID string
	// Detail is a short human-readable statement of what it is.
	Detail string
}

// Dependents enumerates what retiring a checkout would take with it.
//
// It is the preview a caller shows before an explicit forget. Automatic
// checkouts — the ones that would be served from another checkout's primary
// graph rather than from one of their own — are enumerated even though
// nothing mints them yet: the rule is a property of the catalog rows, so a
// preview written against the rows keeps telling the truth once they exist.
func (r *Reconciler) Dependents(ctx context.Context, checkoutID string) ([]Dependent, error) {
	if checkoutID == "" {
		return nil, nil
	}
	checkout, ok, err := r.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !ok {
		return nil, err
	}
	owned, err := r.ownedGraph(ctx, checkout.FamilyID, checkoutID)
	if err != nil {
		return nil, err
	}
	var out []Dependent
	if owned == nil {
		return out, nil
	}

	if owned.IsPrimaryBase {
		siblings, err := r.catalog.ListCheckouts(ctx, checkout.FamilyID)
		if err != nil {
			return nil, err
		}
		for _, sibling := range siblings {
			if sibling.CheckoutID == checkoutID ||
				sibling.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
				continue
			}
			out = append(out, Dependent{
				Kind:   DependentCheckout,
				ID:     sibling.CheckoutID,
				Detail: "checkout " + sibling.AdminName + " is served from this primary graph",
			})
		}
	}

	views, err := r.catalog.ListRefViews(ctx, owned.GraphID)
	if err != nil {
		return nil, err
	}
	for _, view := range views {
		out = append(out, Dependent{
			Kind:   DependentRefView,
			ID:     view.RefViewID,
			Detail: "view " + view.SelectorValue + " is rooted in this graph",
		})
	}

	route, present, err := r.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	if present {
		out = append(out, Dependent{
			Kind:   DependentRoute,
			ID:     route.CheckoutID,
			Detail: "queries for this checkout are routed to graph " + route.GraphID,
		})
	}
	return out, nil
}

// RetireOutcome names what RetireCheckout decided.
type RetireOutcome string

const (
	// OutcomeNoIdentity means the catalog holds no such checkout, so there
	// was nothing to retire.
	OutcomeNoIdentity RetireOutcome = "no_identity"
	// OutcomeForgotten means the checkout was removed.
	OutcomeForgotten RetireOutcome = "forgotten"
	// OutcomeTransitionPending means the checkout could not be retired yet
	// and the request was recorded as a pending intent transition instead.
	OutcomeTransitionPending RetireOutcome = "transition_pending"
)

// RetireCheckout withdraws the reason a checkout is served, following the
// lifecycle's own rule rather than the caller's guess.
//
// A checkout that is reachable, does not hold its family's primary base, and
// has a ready primary elsewhere in the family can stop being served on its
// own terms: it drops to the family's automatic lane. Nothing builds that
// lane yet, so today the drop reduces to forgetting the checkout — but the
// decision lives here, so the day automatic views exist the same call starts
// demoting instead of deleting without any caller changing.
//
// Every other shape — an unreachable checkout, the primary's own owner, a
// family with no other primary to fall back on — is not something a
// configuration edit may delete. The request is recorded as a pending intent
// transition and the rows stay exactly as they were.
func (r *Reconciler) RetireCheckout(ctx context.Context, checkoutID, incarnation, cause string) (RetireOutcome, error) {
	if checkoutID == "" || incarnation == "" {
		return "", fmt.Errorf("%w: retiring a checkout needs a checkout id and an incarnation", ErrSagaTarget)
	}
	if cause == "" {
		return "", fmt.Errorf("%w: retiring a checkout needs a cause", ErrSagaTarget)
	}
	checkout, ok, err := r.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return "", err
	}
	if !ok {
		return OutcomeNoIdentity, nil
	}
	if checkout.Incarnation != incarnation {
		return "", fmt.Errorf("%w: checkout %s is at incarnation %s, not %s",
			store_sqlite.ErrCatalogStaleGuard, checkoutID, checkout.Incarnation, incarnation)
	}

	graphs, err := r.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return "", err
	}
	var owned, primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkoutID {
			owned = &graphs[i]
		}
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
		}
	}

	demotable := checkout.State == store_sqlite.CheckoutStateReady &&
		(owned == nil || !owned.IsPrimaryBase) &&
		primary != nil &&
		primary.OwnerCheckoutID != checkoutID &&
		primary.State == GraphStateReady
	if demotable {
		if err := r.ForgetCheckout(ctx, checkoutID, incarnation); err != nil {
			return "", err
		}
		return OutcomeForgotten, nil
	}

	transition := store_sqlite.IntentTransition{
		TransitionID:       uuid.NewV7().String(),
		CheckoutID:         checkoutID,
		Cause:              cause,
		PriorDesiredMode:   checkout.DesiredMode,
		PriorEffectiveMode: checkout.EffectiveMode,
		RequestedMode:      store_sqlite.CheckoutModeAutomatic,
		PriorCheckoutState: checkout.State,
		State:              store_sqlite.IntentTransitionPending,
		CreatedAt:          r.now().Unix(),
		LastProgress:       r.now().Unix(),
	}
	if err := r.catalog.BeginIntentTransition(ctx, transition); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogIntentTransitionActive) {
			// The checkout already carries the one transition slot it is
			// allowed. Recording a second would be refused anyway, and the
			// standing one already says the checkout is on its way out.
			return OutcomeTransitionPending, nil
		}
		return "", err
	}
	return OutcomeTransitionPending, nil
}
