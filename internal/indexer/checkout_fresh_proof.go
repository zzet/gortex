package indexer

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Reasons a CheckoutFreshProof could not prove the route current. They are
// diagnostics, not refusals: every one of them means "ask the coordinator for a
// refresh ticket", which is the path that can still make the route current.
const (
	FreshProofNoCoordinator     = "no_coordinator"
	FreshProofCheckoutNotReady  = "checkout_not_ready"
	FreshProofRouteNotActive    = "route_not_active"
	FreshProofCohortStale       = "dependency_cohort_stale"
	FreshProofSnapshotDiffers   = "snapshot_differs"
	FreshProofRouteMoved        = "route_moved_during_proof"
	FreshProofCheckoutRootMoved = "checkout_root_moved"
	FreshProofCheckoutChanged   = "checkout_changed_during_proof"
)

// freshProofSampled, when set, runs after the proof's working-copy sample and
// before the proof compares it with the route. It is a test seam: it lets a
// test move the route or the checkout row inside the window the proof has to
// notice, which no external caller can hit deterministically.
var freshProofSampled func()

// CheckoutFreshProof is the outcome of proving, with one working-copy sample
// taken after the caller asked, that a checkout's active route already
// describes its working copy.
type CheckoutFreshProof struct {
	// Fresh is true exactly when the route active at RouteEpoch names a commit
	// generation built for the sampled HEAD tree under this coordinator's
	// current identity, and a servable dirty generation rooted at it whose
	// LowerViewFingerprint equals the sample's fingerprint.
	Fresh bool
	// Reason names why Fresh is false; empty when it is true.
	Reason string
	// The route snapshot the proof is a statement about.
	CommitGenerationID int64
	DirtyGenerationID  int64
	RouteEpoch         int64
	Incarnation        string
	// Fingerprint is the sampled working-copy fingerprint.
	Fingerprint string
	// SampledAt is when the proof asked for its working-copy sample. The
	// sample it was given began at or after Since — the instant the caller's
	// request arrived — which is the whole claim; it may be a sample another
	// request started (DirtySampler.SampleSince), never one that began
	// before this request arrived.
	SampledAt time.Time
	// Since is the arrival instant the sample had to begin at or after: the
	// request's arrival when the caller supplied one
	// (WithFreshRequestArrival), otherwise SampledAt.
	Since time.Time
	// Sample is how long the proof waited for its working-copy sample,
	// lease wait included.
	Sample time.Duration

	// snapshot is the sample that refused the proof (Reason ==
	// FreshProofSnapshotDiffers) and checkoutID the checkout it is of. Only
	// RequestCheckoutRefreshAfterProof reads them: the sample was taken after
	// the caller's request arrived, which is exactly what a refresh ticket
	// needs to be admitted against, so the ticket need not take another.
	snapshot   *gitstate.DirtySnapshot
	checkoutID string
}

// ReusableSample reports whether the proof carries a working-copy sample a
// refresh ticket can be admitted against (RequestCheckoutRefreshAfterProof).
func (p CheckoutFreshProof) ReusableSample() bool {
	return p.snapshot != nil && p.checkoutID != "" && p.Reason == FreshProofSnapshotDiffers
}

// ProveCheckoutFresh answers a require_fresh request without a refresh ticket
// when the route is already current.
//
// A ticket costs the coordinator's quiet window plus a settle cycle: at least
// the debounce (150 ms by default) and three working-copy samples, even for a
// tree that has not moved since the last publication. The facts that cycle
// establishes for a settled tree are exactly these, so they are established
// here directly, lock-free, against one sample taken now:
//
//   - the checkout row is the same ready automatic incarnation at the same
//     root (the predicate completeCheckoutRefreshTickets applies);
//   - the route is active and names both slots;
//   - checkRoutedSnapshot accepts the sample against that route: the commit
//     layer's identity is the one this coordinator would mint for the
//     sampled HEAD tree (so configuration and cohort match too), the dirty
//     layer is servable, carries the sampled fingerprint, and is rooted at
//     the commit layer;
//   - the route did not move while the proof ran.
//
// It never publishes and never waits. Anything short of all of the above is
// reported with a reason, and the caller falls back to RequestCheckoutRefresh,
// whose publication path is unchanged. The dependency cohort is not described
// here: a cohort the coordinator already knows to be stale is refused rather
// than refreshed, because describing it takes the daemon-wide roster lease the
// loop owns.
func (l *CheckoutLifecycle) ProveCheckoutFresh(ctx context.Context, checkoutID, expectedRoot string) (CheckoutFreshProof, error) {
	var proof CheckoutFreshProof
	if l == nil || l.catalog == nil || checkoutID == "" || expectedRoot == "" {
		return proof, fmt.Errorf("%w: checkout identity and root are required", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return proof, err
	}
	if !found || !sameMutationRoot(checkout.RootPath, expectedRoot) {
		return proof, fmt.Errorf("%w: checkout root changed", ErrCheckoutMutationStale)
	}
	proof.Incarnation = checkout.Incarnation
	if !freshProofCheckoutReady(checkout) {
		proof.Reason = FreshProofCheckoutNotReady
		return proof, nil
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil || !rootInfo.IsDir() {
		return proof, fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
	}
	l.coordMu.Lock()
	c, closing := l.coordinators[checkoutID], l.coordinatorClosing
	l.coordMu.Unlock()
	if closing || c == nil || c.sampler == nil || !sameMutationRoot(c.root, expectedRoot) || c.lifetimeContext().Err() != nil {
		proof.Reason = FreshProofNoCoordinator
		return proof, nil
	}
	return c.proveFresh(ctx, checkout, rootInfo)
}

func (c *CheckoutCoordinator) proveFresh(ctx context.Context, checkout store_sqlite.Checkout, rootInfo os.FileInfo) (CheckoutFreshProof, error) {
	proof := CheckoutFreshProof{Incarnation: checkout.Incarnation}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return proof, err
	}
	if !found || route.State != store_sqlite.RouteActive || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		proof.Reason = FreshProofRouteNotActive
		return proof, nil
	}
	proof.CommitGenerationID, proof.DirtyGenerationID, proof.RouteEpoch = route.CommitGenerationID, route.DirtyGenerationID, route.RouteEpoch
	if c.cohortKnownStale() {
		proof.Reason = FreshProofCohortStale
		return proof, nil
	}
	proof.SampledAt = time.Now()
	proof.Since = freshRequestSince(ctx, proof.SampledAt)
	// Any sample whose git status began at or after the request arrived
	// proves the same thing a sample begun now would: the working copy at
	// some instant after the request, which is all require_fresh promises.
	// Concurrent requests on one checkout therefore share one in-flight
	// sample instead of queueing for one each — but only a sample that began
	// after every sharer arrived (SampleSince refuses an older one).
	sample, err := c.sampler.SampleSince(ctx, proof.Since)
	proof.Sample = time.Since(proof.SampledAt)
	if err != nil {
		return proof, err
	}
	proof.Fingerprint = sample.Fingerprint
	if freshProofSampled != nil {
		freshProofSampled()
	}
	if err := c.checkRoutedSnapshot(ctx, route, sample); err != nil {
		proof.Reason = FreshProofSnapshotDiffers
		proof.snapshot, proof.checkoutID = &sample, c.checkoutID
		return proof, nil
	}
	after, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return proof, err
	}
	if !found || after != route {
		proof.Reason = FreshProofRouteMoved
		return proof, nil
	}
	current, found, err := c.catalog.GetCheckout(ctx, c.checkoutID)
	if err != nil {
		return proof, err
	}
	if !found || current.Incarnation != checkout.Incarnation || !sameMutationRoot(current.RootPath, c.root) {
		proof.Reason = FreshProofCheckoutChanged
		return proof, nil
	}
	if !freshProofCheckoutReady(current) {
		proof.Reason = FreshProofCheckoutNotReady
		return proof, nil
	}
	currentInfo, err := checkoutRootFileInfo(current.RootPath)
	if err != nil || !os.SameFile(currentInfo, rootInfo) {
		proof.Reason = FreshProofCheckoutRootMoved
		return proof, nil
	}
	proof.Fresh = true
	return proof, nil
}

// RequestCheckoutRefreshAfterProof is the ticket path a refused proof falls
// back to. When the proof found the working copy differs from the route, the
// ticket is admitted against the proof's own sample
// (RequestCheckoutRefreshFromSample) — it was taken after the caller's
// request arrived, of this checkout — so the fallback costs no second
// working-copy sample before admission. Any other proof (fresh, refused for
// another reason, of another checkout, or the zero value) takes an ordinary
// RequestCheckoutRefresh, which samples for itself. Completion is unchanged
// either way: the coordinator completes the ticket only against a sample its
// cycle takes after admission.
//
// When the proof lent no sample (it was refused before sampling, e.g. the
// route was not active because the edit's own build was in flight) and ctx
// names the request's arrival (WithFreshRequestArrival), the ticket is
// admitted against a sample some other caller already took at or after that
// arrival, or, when there is none, without a sample at all (bound at
// completion: the first sample the coordinator takes after admission decides
// it). A ticket's capture sample only has to postdate the request it answers;
// its completion still waits for a post-admission sample — for a ticket
// admitted while the edit's own build is in flight, that build's pre-publish
// sample (completeCheckoutRefreshTickets).
func (l *CheckoutLifecycle) RequestCheckoutRefreshAfterProof(
	ctx context.Context, checkoutID, expectedRoot string, proof CheckoutFreshProof,
) (*CheckoutRefreshTicket, error) {
	if proof.ReusableSample() && proof.checkoutID == checkoutID {
		return l.RequestCheckoutRefreshFromSample(ctx, checkoutID, expectedRoot, *proof.snapshot)
	}
	if ctx != nil {
		if since := freshRequestSince(ctx, time.Time{}); !since.IsZero() {
			if sampler := l.freshProofSampler(checkoutID, expectedRoot); sampler != nil {
				// A sample another request already took after this one
				// arrived admits it for free. Otherwise the ticket is admitted
				// without one (bound at completion): the cycle's first sample
				// begun after admission — the in-flight build's own
				// pre-publish fence, when the edit's build is what withdrew
				// the route — proves it, so no sample is taken here.
				if sample, _, ok := sampler.LatestSampleSince(since); ok {
					return l.RequestCheckoutRefreshFromSample(ctx, checkoutID, expectedRoot, sample)
				}
				return l.requestBoundCheckoutRefresh(ctx, checkoutID, expectedRoot)
			}
		}
	}
	return l.RequestCheckoutRefresh(ctx, checkoutID, expectedRoot)
}

// freshProofSampler is the live coordinator's working-copy sampler for the
// checkout at expectedRoot, nil when there is none (closing, not yet
// activated, another root).
func (l *CheckoutLifecycle) freshProofSampler(checkoutID, expectedRoot string) *gitstate.DirtySampler {
	if l == nil || checkoutID == "" || expectedRoot == "" {
		return nil
	}
	l.coordMu.Lock()
	c, closing := l.coordinators[checkoutID], l.coordinatorClosing
	l.coordMu.Unlock()
	if closing || c == nil || c.sampler == nil || !sameMutationRoot(c.root, expectedRoot) || c.lifetimeContext().Err() != nil {
		return nil
	}
	return c.sampler
}

type freshRequestArrivalKey struct{}

// WithFreshRequestArrival records on ctx the instant the require_fresh
// request it serves arrived (taken with time.Now() in this process, so it
// carries a monotonic reading). ProveCheckoutFresh and
// RequestCheckoutRefreshAfterProof then accept any working-copy sample that
// began at or after it — never one that began earlier — so concurrent
// requests of one checkout share a sample. A zero instant records nothing.
func WithFreshRequestArrival(ctx context.Context, arrived time.Time) context.Context {
	if ctx == nil || arrived.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, freshRequestArrivalKey{}, arrived)
}

// FreshRequestArrival returns the arrival WithFreshRequestArrival recorded on
// ctx, and whether one was.
func FreshRequestArrival(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	arrived, ok := ctx.Value(freshRequestArrivalKey{}).(time.Time)
	return arrived, ok && !arrived.IsZero()
}

// freshRequestSince is the instant a sample must have begun at or after for
// the request ctx serves: its recorded arrival, or fallback when none was
// recorded. An arrival later than fallback cannot be the arrival of a request
// that is already being served, so fallback (the caller's own "now") is used.
func freshRequestSince(ctx context.Context, fallback time.Time) time.Time {
	if ctx == nil {
		return fallback
	}
	arrived, _ := ctx.Value(freshRequestArrivalKey{}).(time.Time)
	switch {
	case arrived.IsZero():
		return fallback
	case !fallback.IsZero() && arrived.After(fallback):
		return fallback
	default:
		return arrived
	}
}

// freshProofCheckoutReady is the checkout-row predicate a refresh ticket must
// pass before it completes (completeCheckoutRefreshTickets).
func freshProofCheckoutReady(checkout store_sqlite.Checkout) bool {
	return checkout.State == store_sqlite.CheckoutStateReady &&
		checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic &&
		checkout.DesiredMode == store_sqlite.CheckoutModeAutomatic &&
		checkout.ActiveIntentTransitionID == "" &&
		checkout.UnavailableSince == 0 &&
		checkout.RemovalDetectedAt == 0
}

// cohortKnownStale reports whether the coordinator already knows its cached
// dependency cohort no longer describes the topology — the condition under
// which the settle preflight would re-describe it before comparing
// identities. It only reads.
func (c *CheckoutCoordinator) cohortKnownStale() bool {
	token := c.cohort.topologyToken()
	c.revisionMu.RLock()
	defer c.revisionMu.RUnlock()
	return c.cohortStale || token != c.cohortTopology
}
