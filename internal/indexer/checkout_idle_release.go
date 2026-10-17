package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Idle checkouts give their layers back.
//
// An automatic checkout that no session has selected and no edit has touched
// for idleCheckoutAfter keeps its registration and its route record, but not
// its layers: its coordinator is stopped, the route is set pending with both
// slots cleared (one guarded flip, so a cycle that moved the route first
// wins), and the generations it served are owed to the deferred sweep. The
// sweep retires them under its own rules — smallest first, a generation over
// its size bound only after its idle interval, nothing a lease, a route or a
// layer above still references — so the order is forced by references: the
// checkout's working-tree layers first, then its commit layer, and a
// dedicated base (a root other checkouts' commit layers stood on) only once
// the last layer standing on it is gone and no route names it.
//
// Opening the checkout again is the dormant-checkout path that exists
// already: the request finds the route pending, gets the labelled base
// fallback (an explicit selector gets view_building) and activates the
// coordinator, whose first cycle rebuilds the commit layer — a copy or reuse
// of a published commit layer when one matches the checkout's tree, a build
// otherwise — and then the working-tree layer. No request is ever answered
// from the released layers: they are no longer routed from the moment of the
// flip, and a reader holding a lease on them keeps its snapshot until it ends.
//
// Never released: the primary checkout (the owner of a dedicated graph), a
// checkout that is not automatic, a checkout activating or with a fold in
// flight, and a checkout with an uncommitted file modified within the idle
// limit (the edit may be the user's, made with no session open).

// defaultIdleCheckoutDays is the idle limit when none is configured: a week,
// so a checkout used on any working day of the week keeps its layers.
const defaultIdleCheckoutDays = 7

// idleCheckoutScanEvery is how often the release scan runs at most.
const idleCheckoutScanEvery = time.Hour

// idleCheckoutAfter reads GORTEX_IDLE_CHECKOUT_DAYS (default 7; 0 disables
// the release). Unparseable or negative input keeps the default.
func idleCheckoutAfter() time.Duration {
	days := defaultIdleCheckoutDays
	if raw := strings.TrimSpace(os.Getenv("GORTEX_IDLE_CHECKOUT_DAYS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

// idleCheckoutState is the lifecycle's record of checkout use. Selections
// are held in memory; the process start is the floor of every checkout's
// last use, so after a restart nothing is released before a full idle
// interval has passed in this process.
type idleCheckoutState struct {
	mu      sync.Mutex
	lastUse map[string]time.Time
	scanned time.Time
}

func (s *idleCheckoutState) noteUse(checkoutID string, at time.Time) {
	if checkoutID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUse == nil {
		s.lastUse = map[string]time.Time{}
	}
	if at.After(s.lastUse[checkoutID]) {
		s.lastUse[checkoutID] = at
	}
}

func (s *idleCheckoutState) used(checkoutID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUse[checkoutID]
}

// idleCheckoutInput is what the release decision reads about one checkout.
type idleCheckoutInput struct {
	Checkout store_sqlite.Checkout
	Primary  bool
	Busy     bool // activating, or a fold of its chain in flight
	// LastUse is the newest of its last selection, its newest generation's
	// creation (its last edit or head move) and the process start.
	LastUse time.Time
	// NewestUncommitted is the newest modification time of a file that
	// differs from HEAD; zero when the checkout is clean.
	NewestUncommitted time.Time
}

// Reasons of the release decision.
const (
	idleKeepDisabled     = "disabled"
	idleKeepPrimary      = "primary"
	idleKeepNotAutomatic = "not_automatic"
	idleKeepBusy         = "busy"
	idleKeepRecentEdits  = "recent_uncommitted_edits"
	idleKeepInUse        = "in_use"
	idleReleaseIdle      = "idle"
)

// decideIdleCheckout decides whether one checkout's layers are released.
func decideIdleCheckout(in idleCheckoutInput, after time.Duration, now time.Time) (release bool, reason string) {
	switch {
	case after <= 0:
		return false, idleKeepDisabled
	case in.Primary:
		return false, idleKeepPrimary
	case in.Checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic:
		return false, idleKeepNotAutomatic
	case in.Busy:
		return false, idleKeepBusy
	case !in.NewestUncommitted.IsZero() && now.Sub(in.NewestUncommitted) < after:
		return false, idleKeepRecentEdits
	case now.Sub(in.LastUse) < after:
		return false, idleKeepInUse
	}
	return true, idleReleaseIdle
}

// ReleaseIdleCheckouts releases the layers of every idle checkout (at most
// once per idleCheckoutScanEvery) and returns the checkouts it released.
func (l *CheckoutLifecycle) ReleaseIdleCheckouts(ctx context.Context) ([]string, error) {
	if l == nil || l.catalog == nil {
		return nil, nil
	}
	after := idleCheckoutAfter()
	now := l.clock()
	l.idle.mu.Lock()
	due := l.idle.scanned.IsZero() || now.Sub(l.idle.scanned) >= idleCheckoutScanEvery
	if due {
		l.idle.scanned = now
	}
	l.idle.mu.Unlock()
	if after <= 0 || !due {
		return nil, nil
	}
	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		l.logger.Debug("checkout lifecycle: idle release scan failed", zap.Error(err))
		return nil, err
	}
	var checkouts []store_sqlite.Checkout
	for _, family := range families {
		members, err := l.catalog.ListCheckouts(ctx, family.FamilyID)
		if err != nil {
			return nil, err
		}
		checkouts = append(checkouts, members...)
	}
	var released []string
	for _, checkout := range checkouts {
		if ctx.Err() != nil {
			return released, ctx.Err()
		}
		in, ok := l.idleCheckoutInput(ctx, checkout, after, now)
		if !ok {
			continue
		}
		release, reason := decideIdleCheckout(in, after, now)
		if !release {
			l.logger.Debug("checkout lifecycle: idle release kept a checkout",
				zap.String("checkout", checkout.CheckoutID), zap.String("reason", reason))
			continue
		}
		if l.releaseIdleCheckout(ctx, checkout, now.Sub(in.LastUse)) {
			released = append(released, checkout.CheckoutID)
		}
	}
	return released, nil
}

// idleCheckoutInput gathers the decision's input. The working tree is
// sampled only for a checkout already past the idle limit by use.
func (l *CheckoutLifecycle) idleCheckoutInput(ctx context.Context, checkout store_sqlite.Checkout, after time.Duration, now time.Time) (idleCheckoutInput, bool) {
	in := idleCheckoutInput{Checkout: checkout, LastUse: time.Unix(l.buildingRecoveryCutoff, 0)}
	if checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
		return in, true
	}
	owned, primary, err := l.familyGraphsFor(ctx, checkout)
	if err != nil {
		return in, false
	}
	// The owner of any dedicated graph, and the primary base's owner above all.
	in.Primary = owned != nil || (primary != nil && primary.OwnerCheckoutID == checkout.CheckoutID)
	l.coordMu.Lock()
	_, activating := l.coordinatorActivating[checkout.CheckoutID]
	coordinator := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	in.Busy = activating || (coordinator != nil && coordinator.compactionInFlight())
	if used := l.idle.used(checkout.CheckoutID); used.After(in.LastUse) {
		in.LastUse = used
	}
	rows, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{CheckoutID: checkout.CheckoutID, Limit: 64})
	if err != nil {
		return in, false
	}
	for _, row := range rows {
		if created := time.Unix(row.CreatedAt, 0); created.After(in.LastUse) {
			in.LastUse = created
		}
	}
	if now.Sub(in.LastUse) < after || in.Primary || in.Busy {
		return in, true
	}
	newest, err := newestUncommittedChange(ctx, checkout.RootPath)
	if err != nil {
		// A working tree that cannot be sampled is kept: its edits are
		// unknown.
		return in, false
	}
	in.NewestUncommitted = newest
	return in, true
}

// newestUncommittedChange is the newest modification time among the files
// that differ from HEAD (a deleted file counts as now: its deletion time is
// unknown).
func newestUncommittedChange(ctx context.Context, root string) (time.Time, error) {
	sampler, err := gitstate.NewDirtySampler(root, "", "")
	if err != nil {
		return time.Time{}, err
	}
	sample, err := sampler.Sample(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	for _, content := range sample.Contents {
		if content.HeadEqual {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(content.Path)))
		if err != nil {
			return time.Now(), nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, nil
}

// releaseIdleCheckout stops the checkout's coordinator, sets its route
// pending with both slots cleared, and owes the generations the route named
// to the sweep. It reports whether the route was released.
func (l *CheckoutLifecycle) releaseIdleCheckout(ctx context.Context, checkout store_sqlite.Checkout, idleFor time.Duration) bool {
	route, found, err := l.catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive ||
		(route.CommitGenerationID <= 0 && route.DirtyGenerationID <= 0) {
		return false
	}
	// The coordinator first: a live one would rebuild the cleared route on
	// its next cycle. Its owed retirements move to the lifecycle.
	l.dropCoordinator(checkout.CheckoutID)
	err = l.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID: checkout.CheckoutID, GraphID: route.GraphID,
		CommitGenerationID: 0, DirtyGenerationID: 0,
		State: store_sqlite.RoutePending, ExpectedRouteEpoch: route.RouteEpoch,
	})
	if err != nil {
		// A cycle moved the route first (it was not idle after all), or the
		// catalog refused: nothing was released.
		l.logger.Info("checkout lifecycle: idle release of a checkout refused",
			zap.String("checkout", checkout.CheckoutID), zap.Error(err))
		return false
	}
	owed := []int64{route.CommitGenerationID}
	if route.DirtyGenerationID > 0 && route.DirtyGenerationID != route.CommitGenerationID {
		owed = append(owed, route.DirtyGenerationID)
	}
	l.oweRetirement(owed...)
	l.logger.Info("checkout lifecycle: idle checkout's layers released",
		zap.String("checkout", checkout.CheckoutID), zap.Duration("idle", idleFor),
		zap.Int64("commit_generation", route.CommitGenerationID), zap.Int64("dirty_generation", route.DirtyGenerationID))
	return true
}
