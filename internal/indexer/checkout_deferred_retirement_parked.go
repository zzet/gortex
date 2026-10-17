package indexer

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A generation the catalog still references — a route slot, a ref view, an
// active graph pointer, a generation built on it — cannot be retired, and
// nothing about it changes until one of those references goes. The deferred
// sweep offered such generations again on every pass (one in the measured run
// was refused thirteen times, each refusal an error, a backoff and a Warn),
// and while any of them was pending the worker rescanned the catalog every
// second. A refused generation is now parked: it is skipped until a reference
// to it may have been released, or, as a floor for a release nothing reports,
// until retirementParkRecheck has passed. When only parked generations remain,
// the sweep reports nothing pending and the worker waits on its idle pause,
// which a release wakes.
//
// A release names the generations it may have freed: the base of a removed
// generation (the "based" reference its row held — reported by the Store for
// every removal, whichever path retired it), the generation a route slot
// stopped naming, the base a layer was rebased off, a ref view's or dedicated
// head's former generation, what a re-installed or repointed route or a moved
// dedicated active pointer named before. A generation refused for nothing but
// generations built on it (the common case: a parent waiting on its children)
// is unparked only by a release naming it. A release that cannot name what it
// freed (a withdrawn route, a deleted ref view or dedicated graph, a failed
// dedicated publication) unparks every parked generation.
// One release used to unpark them all — in a measured run about forty parents
// were each refused and re-parked eight times — though nothing about all but
// one of them had changed.
//
// A generation refused for anything else — a route slot, a ref view, an
// active pointer, a contract attachment, or a refusal that did not say — is
// unparked by any release at all, every removal included: some of those
// references go with writes nothing reports (a contract attachment is held by
// another generation's contract state, which that generation's sweep or a
// base-state update removes), and the next removal is when they were offered
// again before targeted releases existed.

// retirementParkRecheck is how long a parked generation waits without a hint.
const retirementParkRecheck = 5 * time.Minute

// retirementReleaseIDsMax bounds the per-generation release record. Past it the
// record is dropped and every park is released once, which is conservative.
const retirementReleaseIDsMax = 4096

// retirementReleases records the events that may release a generation
// reference: a sequence, the sequence of the last release that named no
// generation, and per generation the sequence of the last one that named it.
var retirementReleases struct {
	mu       sync.Mutex
	seq      int64
	wildcard int64
	byID     map[int64]int64
}

// deferredRetirementParked is the number of generations parked now, for the
// daemon's progress line.
var deferredRetirementParked atomic.Int64

// DeferredRetirementParked reports how many generations wait, parked, for a
// reference to be released.
func DeferredRetirementParked() int64 { return deferredRetirementParked.Load() }

// noteRetirementReferenceReleased records a release that cannot name what it
// freed: every parked generation is offered again.
func noteRetirementReferenceReleased() {
	retirementReleases.mu.Lock()
	retirementReleases.seq++
	retirementReleases.wildcard = retirementReleases.seq
	retirementReleases.mu.Unlock()
	notifyDeferredRetirementWork()
}

// noteRetirementReferencesReleased records a release of the references to
// ids: only those generations, if parked, are offered again.
func noteRetirementReferencesReleased(ids ...int64) {
	retirementReleases.mu.Lock()
	retirementReleases.seq++
	if retirementReleases.byID == nil {
		retirementReleases.byID = make(map[int64]int64)
	}
	for _, id := range ids {
		if id > 0 {
			retirementReleases.byID[id] = retirementReleases.seq
		}
	}
	if len(retirementReleases.byID) > retirementReleaseIDsMax {
		retirementReleases.byID = nil
		retirementReleases.wildcard = retirementReleases.seq
	}
	retirementReleases.mu.Unlock()
	notifyDeferredRetirementWork()
}

// retirementReleaseSeq is the release sequence now. An attempt reads it
// before it is refused and parks with it, so a release that lands between the
// refusal and the park still unparks.
func retirementReleaseSeq() int64 {
	retirementReleases.mu.Lock()
	defer retirementReleases.mu.Unlock()
	return retirementReleases.seq
}

// retirementReleasedSince reports a release that may have freed id after seq:
// one naming id (or everything), or — for a park waiting on a reference
// releases may not name — any release at all.
func retirementReleasedSince(id, seq int64, named bool) bool {
	retirementReleases.mu.Lock()
	defer retirementReleases.mu.Unlock()
	if !named {
		return retirementReleases.seq > seq
	}
	return retirementReleases.wildcard > seq || retirementReleases.byID[id] > seq
}

// retirementPark is one parked generation: the release sequence its refused
// attempt started at, the time it was parked, and whether only a release
// naming it can free it (it was refused for generations built on it alone).
// aside is the age the eligible debt had reached when this generation was
// parked, kept when that debt drained (retirementDebtDrained): a release
// brings this generation, and nothing else, back with that age.
type retirementPark struct {
	seq   int64
	since time.Time
	named bool
	aside time.Duration
}

// retirementStillReferenced reports a refusal for a reference the catalog
// still holds.
func retirementStillReferenced(err error) bool {
	return errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced)
}

// retirementRefusedOnlyBased reports a refusal that says the generation was
// held by nothing but generations built on it.
func retirementRefusedOnlyBased(err error) bool {
	var referenced *store_sqlite.GenerationReferencedError
	return errors.As(err, &referenced) && referenced.Refs.OnlyBased()
}

// parkReferencedRetirement parks a generation the catalog still references.
// seq is the release sequence read before the refused attempt, refusal the
// error the attempt was refused with.
func (l *CheckoutLifecycle) parkReferencedRetirement(generationID int64, now time.Time, seq int64, refusal error) {
	l.coordMu.Lock()
	if l.retirementParked == nil {
		l.retirementParked = make(map[int64]retirementPark)
	}
	previous, again := l.retirementParked[generationID]
	// Refused again: the age it kept, plus what it waited eligible since a
	// release brought it back, stays its own.
	aside := previous.aside
	if since, ok := l.retirementReleasedAged[generationID]; ok {
		aside = max(aside, now.Sub(time.Unix(0, since)))
		delete(l.retirementReleasedAged, generationID)
	}
	l.retirementParked[generationID] = retirementPark{
		seq: seq, since: now, named: retirementRefusedOnlyBased(refusal), aside: aside,
	}
	parked := len(l.retirementParked)
	deferredRetirementParked.Store(int64(parked))
	l.coordMu.Unlock()
	if l.logger != nil && !again {
		l.logger.Info("indexer: retirement parked a still-referenced generation",
			zap.Int64("generation", generationID), zap.Int("parked", parked))
	}
}

// retirementParkedNow reports whether a generation is still parked: no
// release naming it (or everything) since its refused attempt began, and the
// recheck floor not reached. A generation whose park lapsed is unparked (it
// is offered again); one a release unparked comes back on its own debt clock,
// started the age its park kept ago (agedReleasedRetirements).
func (l *CheckoutLifecycle) retirementParkedNow(generationID int64, now time.Time) bool {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	park, ok := l.retirementParked[generationID]
	if !ok {
		return false
	}
	released := retirementReleasedSince(generationID, park.seq, park.named)
	if !released && now.Sub(park.since) < retirementParkRecheck {
		return true
	}
	delete(l.retirementParked, generationID)
	deferredRetirementParked.Store(int64(len(l.retirementParked)))
	if released && park.aside > 0 {
		if l.retirementReleasedAged == nil {
			l.retirementReleasedAged = make(map[int64]int64)
		}
		l.retirementReleasedAged[generationID] = now.Add(-park.aside).UnixNano()
	}
	return false
}

// unparkRetirement forgets a generation (retired, or gone).
func (l *CheckoutLifecycle) unparkRetirement(generationID int64) {
	l.coordMu.Lock()
	delete(l.retirementParked, generationID)
	delete(l.retirementReleasedAged, generationID)
	deferredRetirementParked.Store(int64(len(l.retirementParked)))
	l.coordMu.Unlock()
}

// keepParkedDebtAge sets aside, for every generation parked while the debt
// that started at since was eligible, the age that debt had reached when the
// generation was parked. A generation parked before since keeps its own.
func (l *CheckoutLifecycle) keepParkedDebtAge(since time.Time) {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	for generationID, park := range l.retirementParked {
		if age := park.since.Sub(since); age > park.aside {
			park.aside = age
			l.retirementParked[generationID] = park
		}
	}
}

// agedReleasedRetirements is the released parked work in ordered whose own
// debt clock has aged past the starvation limit: the only work the age a park
// kept may carry past a stand-down. Debt beside it keeps the shared clock.
func (l *CheckoutLifecycle) agedReleasedRetirements(ordered []int64, now time.Time) []int64 {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	var aged []int64
	for _, generationID := range ordered {
		if since, ok := l.retirementReleasedAged[generationID]; ok && l.retirementClockAged(since, now) {
			aged = append(aged, generationID)
		}
	}
	return aged
}

// retirementAgedReleasePending applies every release that has landed on a
// park, then reports whether any released parked work is aged on its own
// clock: the pass then scans for it while young debt stands down.
func (l *CheckoutLifecycle) retirementAgedReleasePending(now time.Time) bool {
	l.coordMu.Lock()
	parked := make([]int64, 0, len(l.retirementParked))
	for generationID := range l.retirementParked {
		parked = append(parked, generationID)
	}
	l.coordMu.Unlock()
	for _, generationID := range parked {
		l.retirementParkedNow(generationID, now)
	}
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	for _, since := range l.retirementReleasedAged {
		if l.retirementClockAged(since, now) {
			return true
		}
	}
	return false
}

// retirementDebtSince is the oldest debt clock (unix nanos, 0 none) among
// ordered: the shared one, or a released generation's own.
func (l *CheckoutLifecycle) retirementDebtSince(ordered []int64) int64 {
	oldest := l.deferredRetirementEligibleSince.Load()
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	for _, generationID := range ordered {
		if since, ok := l.retirementReleasedAged[generationID]; ok && (oldest == 0 || since < oldest) {
			oldest = since
		}
	}
	return oldest
}

// retirementParkedCount is how many owed generations are parked now. A park
// whose generation is no longer owed — retired, or dropped by a path that
// never consults parks (the synchronous janitor, a checkout teardown) — is
// forgotten here; kept, it would count as parked work for the life of the
// process.
func (l *CheckoutLifecycle) retirementParkedCount() int {
	return l.pruneRetirementParked(l.owedRetirementSet())
}

// pruneRetirementParked forgets every park outside owed and returns how many
// are left.
func (l *CheckoutLifecycle) pruneRetirementParked(owed map[int64]struct{}) int {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	for generationID := range l.retirementParked {
		if _, ok := owed[generationID]; !ok {
			delete(l.retirementParked, generationID)
		}
	}
	for generationID := range l.retirementReleasedAged {
		if _, ok := owed[generationID]; !ok {
			delete(l.retirementReleasedAged, generationID)
		}
	}
	deferredRetirementParked.Store(int64(len(l.retirementParked)))
	return len(l.retirementParked)
}

// owedRetirementSet is every generation owed a retirement: the lifecycle's
// owed set and every registered coordinator's backlog.
func (l *CheckoutLifecycle) owedRetirementSet() map[int64]struct{} {
	l.coordMu.Lock()
	owed := make(map[int64]struct{}, len(l.owed))
	for generationID := range l.owed {
		owed[generationID] = struct{}{}
	}
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, coordinator := range l.coordinators {
		coordinators = append(coordinators, coordinator)
	}
	l.coordMu.Unlock()
	for _, coordinator := range coordinators {
		for _, generationID := range coordinator.pendingRetirementGenerations() {
			owed[generationID] = struct{}{}
		}
	}
	return owed
}

// The Store reports every committed release of a generation reference
// (store_sqlite.OnGenerationReferencesReleased): a removal's base, whichever
// path removed it, and the catalog writes that drop a route, ref view or
// dedicated reference.
func init() {
	store_sqlite.OnGenerationReferencesReleased(noteRetirementStoreRelease)
}

func noteRetirementStoreRelease(release store_sqlite.GenerationReferenceRelease) {
	switch {
	case release.Any:
		noteRetirementReferenceReleased()
	case len(release.Released) > 0 || release.Removed != 0:
		// A removal with no base names nothing, but it is still a release
		// for the parks that wait on any.
		noteRetirementReferencesReleased(release.Released...)
	}
}

// retirementOwedCounted is which generations the owed counter has counted,
// per Store: discovery, a coordinator's deferral and a refused offer can each
// meet the same generation, and the counter is meant to count it once. Past
// retirementOwedCountedMax the record starts over, which can count a
// generation twice at worst.
var retirementOwedCounted struct {
	mu  sync.Mutex
	ids map[retirementOwedKey]struct{}
}

type retirementOwedKey struct {
	store *store_sqlite.Store
	id    int64
}

const retirementOwedCountedMax = 4096

// countRetirementOwed counts a generation becoming owed a retirement, once.
func countRetirementOwed(store *store_sqlite.Store, generationID int64, why string) {
	key := retirementOwedKey{store: store, id: generationID}
	retirementOwedCounted.mu.Lock()
	if _, counted := retirementOwedCounted.ids[key]; counted {
		retirementOwedCounted.mu.Unlock()
		return
	}
	if retirementOwedCounted.ids == nil || len(retirementOwedCounted.ids) >= retirementOwedCountedMax {
		retirementOwedCounted.ids = make(map[retirementOwedKey]struct{})
	}
	retirementOwedCounted.ids[key] = struct{}{}
	retirementOwedCounted.mu.Unlock()
	viewmetrics.Count(viewmetrics.GenerationRetireOwedTotal, why)
}
