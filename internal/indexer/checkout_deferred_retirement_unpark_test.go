package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A release names what it may have freed, and only that is unparked; a
// release landing between a refusal and its park still unparks.
func TestRetirementReleaseUnparksOnlyTheNamedGeneration(t *testing.T) {
	l := &CheckoutLifecycle{}
	now := time.Now()
	seq := retirementReleaseSeq()
	l.parkReferencedRetirement(101, now, seq, basedRefusal)
	l.parkReferencedRetirement(202, now, seq, basedRefusal)
	noteRetirementReferencesReleased(101)
	require.False(t, l.retirementParkedNow(101, now), "the named generation stays parked")
	require.True(t, l.retirementParkedNow(202, now), "an unrelated generation was unparked")

	// Refused before a release, parked after it: the park carries the
	// sequence the attempt started at.
	refusedAt := retirementReleaseSeq()
	noteRetirementReferencesReleased(303)
	l.parkReferencedRetirement(303, now, refusedAt, basedRefusal)
	require.False(t, l.retirementParkedNow(303, now), "a release between refusal and park was lost")

	// A release that names nothing unparks everything, and the floor holds.
	noteRetirementReferenceReleased()
	require.False(t, l.retirementParkedNow(202, now))
	l.parkReferencedRetirement(404, now, retirementReleaseSeq(), basedRefusal)
	require.True(t, l.retirementParkedNow(404, now.Add(time.Minute)))
	require.False(t, l.retirementParkedNow(404, now.Add(retirementParkRecheck)))
}

// A parent with two children and an unrelated parked generation: retiring a
// child re-offers only the parent, never the unrelated one, and the parked
// work keeps its debt age aside.
func TestRetiringAChildUnparksOnlyItsParent(t *testing.T) {
	f := newDedicatedChainFixture(t)
	parent := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	first := f.base(store_sqlite.ViewGenerationSuperseded, parent)
	second := f.base(store_sqlite.ViewGenerationSuperseded, parent)
	unrelated := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	live := f.base(store_sqlite.ViewGenerationReady, unrelated)
	f.activate(live)
	l := f.lifecycle(-1)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "", time.Time{} }
	l.interactiveDemand = func() bool { return false }
	l.owed = map[int64]struct{}{parent: {}, first: {}, second: {}, unrelated: {}}
	// Both parents were refused before; they wait parked on their children.
	seq := retirementReleaseSeq()
	l.parkReferencedRetirement(parent, time.Now(), seq, basedRefusal)
	l.parkReferencedRetirement(unrelated, time.Now(), seq, basedRefusal)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())

	var mu sync.Mutex
	offers := map[int64]int{}
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		mu.Lock()
		offers[id]++
		mu.Unlock()
		return l.store.RetirePayloadGenerationQuantum(ctx, id, inUse)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		_, pending, err := l.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		if !pending {
			break
		}
	}
	require.NoError(t, ctx.Err(), "retirement did not converge")
	requireGenerationRetired(t, f.store, first)
	requireGenerationRetired(t, f.store, second)
	requireGenerationRetired(t, f.store, parent)
	requireCatalogGenerationPresent(t, f.store, unrelated)
	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, offers[unrelated], "a child's retirement re-offered an unrelated parked generation")
	require.True(t, l.retirementParkedNow(unrelated, time.Now()))
	// The eligible debt drained: its clock stops, and its age is kept aside
	// for the parked work a release may bring back.
	require.Zero(t, l.deferredRetirementEligibleSince.Load(), "drained debt must stop the clock")
	require.Greater(t, l.retirementParked[unrelated].aside, 2*time.Minute, "parked work must keep its debt age aside")
}

// A generation retired outside the deferred sweep — an offer that succeeds
// inline, the synchronous janitor, ref-view retention — releases its base all
// the same: the Store reports every removal's base, so a parked parent is
// re-offered at once instead of at the recheck floor, and nothing else is.
func TestAnInlineRetirementUnparksItsBase(t *testing.T) {
	f := newDedicatedChainFixture(t)
	parent := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	child := f.base(store_sqlite.ViewGenerationSuperseded, parent)
	unrelated := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	f.activate(f.base(store_sqlite.ViewGenerationReady, unrelated))
	l := f.lifecycle(-1)
	now := time.Now()
	seq := retirementReleaseSeq()
	l.parkReferencedRetirement(parent, now, seq, basedRefusal)
	l.parkReferencedRetirement(unrelated, now, seq, basedRefusal)
	require.NoError(t, f.store.RetirePayloadGeneration(t.Context(), child, nil))
	require.False(t, l.retirementParkedNow(parent, now), "the removed child's base stayed parked")
	require.True(t, l.retirementParkedNow(unrelated, now), "an unrelated generation was unparked")
}

// A park whose generation the synchronous janitor retired is forgotten, and
// with no other debt the clock stops: a stale park used to keep the debt
// "aged" for the life of the process, which skipped every later stand-down.
func TestAJanitorRetirementForgetsItsParkAndStopsTheDebtClock(t *testing.T) {
	f := newDedicatedChainFixture(t)
	parent := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	l := f.lifecycle(-1)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "", time.Time{} }
	l.interactiveDemand = func() bool { return false }
	l.owed[parent] = struct{}{}
	l.parkReferencedRetirement(parent, time.Now(), retirementReleaseSeq(), basedRefusal)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	require.Positive(t, l.sweepRetirements(t.Context()))
	requireGenerationRetired(t, f.store, parent)
	// Status reads the parks against what is still owed, before any pass.
	backlog, err := l.RetirementBacklog(t.Context())
	require.NoError(t, err)
	require.Zero(t, backlog.Parked, "status counted a park the janitor had retired")
	for pass := 0; pass < 3; pass++ {
		_, _, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
	}
	require.Zero(t, l.retirementParkedCount(), "a park outlived its generation")
	require.Zero(t, l.deferredRetirementEligibleSince.Load(), "the debt clock kept running with nothing owed")
	require.Empty(t, l.retirementReleasedAged)
	backlog, err = l.RetirementBacklog(t.Context())
	require.NoError(t, err)
	require.Zero(t, backlog.Parked)
	require.Zero(t, backlog.DebtAgeSeconds)
}

// Debt that arrives after an idle drain starts its own clock even while work
// stays parked (a generation can stay parked indefinitely), so
// it gets the stand-down young debt gets. Parked work a release brings back
// resumes the age it had.
func TestDebtAfterAnIdleDrainStartsItsOwnClockBesideParkedWork(t *testing.T) {
	f := newDedicatedChainFixture(t)
	pinned := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	f.activate(f.base(store_sqlite.ViewGenerationReady, pinned))
	l := f.lifecycle(-1)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	idle := func() (string, time.Time) { return "", time.Time{} }
	l.foregroundWork = idle
	l.interactiveDemand = func() bool { return false }
	l.owed[pinned] = struct{}{}
	old := time.Now().Add(-10 * time.Minute).UnixNano()
	l.deferredRetirementEligibleSince.Store(old)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		_, pending, err := l.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		if !pending {
			break
		}
	}
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, l.retirementParkedCount())
	require.Zero(t, l.deferredRetirementEligibleSince.Load(), "the drained debt kept its clock")
	aside := l.retirementParked[pinned].aside
	require.True(t, aside >= 10*time.Minute && aside < 11*time.Minute, "the age set aside is %s, want the 10 minutes the debt waited", aside)

	// New debt while a refresh ticket is in flight: young, so it waits.
	fresh := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	l.foregroundWork = func() (string, time.Time) { return "refresh_ticket", time.Now() }
	_, _, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	requireCatalogGenerationPresent(t, f.store, fresh)
	require.False(t, l.retirementDebtAged(time.Now()), "new debt inherited the parked work's age")

	// A release brings the parked work back with the age it had.
	l.foregroundWork = idle
	noteRetirementReferenceReleased()
	_, _, err = l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	require.NotEmpty(t, bursts)
	debtAge := bursts[len(bursts)-1].ContextMap()["debt_age"].(time.Duration)
	require.Greater(t, debtAge, 9*time.Minute, "released parked work restarted its clock")
}

// basedRefusal is a refusal held by nothing but generations built on the
// generation.
var basedRefusal = &store_sqlite.GenerationReferencedError{Refs: store_sqlite.ViewGenerationReferences{Based: true}}

// A generation refused for a reference no write may name — a route slot, a
// contract attachment, a refusal that did not say — is offered again on any
// release, a removal with no base included, as every removal used to offer
// every park. One refused for its children alone waits for a release naming
// it.
func TestAParkOnAnUnnamedReferenceUnparksOnAnyRelease(t *testing.T) {
	l := &CheckoutLifecycle{}
	now := time.Now()
	refusals := map[int64]error{
		501: &store_sqlite.GenerationReferencedError{GenerationID: 501, Refs: store_sqlite.ViewGenerationReferences{Routed: true}},
		502: &store_sqlite.GenerationReferencedError{GenerationID: 502, Refs: store_sqlite.ViewGenerationReferences{Based: true, ContractAttached: true}},
		503: fmt.Errorf("%w: generation 503", store_sqlite.ErrCatalogGenerationReferenced),
	}
	seq := retirementReleaseSeq()
	for id, refusal := range refusals {
		l.parkReferencedRetirement(id, now, seq, refusal)
	}
	l.parkReferencedRetirement(504, now, seq, basedRefusal)
	for id := range refusals {
		require.True(t, l.retirementParkedNow(id, now), "generation %d unparked with no release", id)
	}
	noteRetirementStoreRelease(store_sqlite.GenerationReferenceRelease{Removed: 999})
	for id := range refusals {
		require.False(t, l.retirementParkedNow(id, now), "a removal did not re-offer generation %d", id)
	}
	require.True(t, l.retirementParkedNow(504, now), "a release naming nothing unparked a generation waiting on its children")
}

// Only time spent eligible ages debt: parked work a release brings back an
// hour later resumes the age the debt had when it was parked, not that age
// plus the hour it sat parked — and only that generation resumes it.
func TestReleasedParkedDebtResumesOnlyTheAgeItWaitedEligible(t *testing.T) {
	l := &CheckoutLifecycle{owed: map[int64]struct{}{7: {}, 8: {}}, retirementStarvationLimit: time.Minute}
	now := time.Now()
	l.deferredRetirementEligibleSince.Store(now.Add(-10 * time.Second).UnixNano())
	l.parkReferencedRetirement(7, now, retirementReleaseSeq(), basedRefusal)
	l.retirementDebtDrained(now)
	require.Zero(t, l.deferredRetirementEligibleSince.Load())
	require.Equal(t, 10*time.Second, l.retirementParked[7].aside)

	later := now.Add(time.Hour)
	noteRetirementReferencesReleased(7)
	require.False(t, l.retirementParkedNow(7, later))
	l.noteEligibleRetirementDebt(later, true, true)
	require.Equal(t, later.Add(-10*time.Second).UnixNano(), l.retirementReleasedAged[7])
	require.Equal(t, later.UnixNano(), l.deferredRetirementEligibleSince.Load(), "the released age moved the shared clock")
	require.Empty(t, l.agedReleasedRetirements([]int64{7, 8}, later), "an hour spent parked aged the debt")
	require.Equal(t, []int64{7}, l.agedReleasedRetirements([]int64{7, 8}, later.Add(50*time.Second)))
	require.False(t, l.retirementDebtAged(later.Add(50*time.Second)), "the released age aged the debt beside it")
}

// A park releasing while young debt waits brings back only the released
// generation with the age its park kept: it retires, served alone, while the
// young debt beside it keeps the production stand-down and no burst touches
// it. One shared clock moved back by the parked age used to make every
// eligible generation aged on its first sighting.
func TestAReleasedParkAgesOnlyItselfBesideYoungDebt(t *testing.T) {
	f := newDedicatedChainFixture(t)
	released := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	young := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	f.activate(f.base(store_sqlite.ViewGenerationReady, 0))
	l := f.lifecycle(-1)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.interactiveDemand = func() bool { return false }
	// Parked while aged debt was eligible; that debt then drained.
	parkedAt := time.Now()
	l.owed[released] = struct{}{}
	l.deferredRetirementEligibleSince.Store(parkedAt.Add(-10 * time.Minute).UnixNano())
	l.parkReferencedRetirement(released, parkedAt, retirementReleaseSeq(), basedRefusal)
	l.retirementDebtDrained(parkedAt)
	require.Zero(t, l.deferredRetirementEligibleSince.Load())

	// New debt while the checkout is busy, then the park's reference goes.
	l.owed[young] = struct{}{}
	l.foregroundWork = func() (string, time.Time) { return "refresh_ticket", time.Now() }
	noteRetirementReferencesReleased(released)
	var mu sync.Mutex
	offers := map[int64]int{}
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		mu.Lock()
		offers[id]++
		mu.Unlock()
		return l.store.RetirePayloadGenerationQuantum(ctx, id, inUse)
	}
	standDowns := deferredRetirementPassStandDowns.Load()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for pass := 0; pass < 50 && ctx.Err() == nil; pass++ {
		_, pending, err := l.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		require.True(t, pending, "the young debt is still owed")
		_, found, err := f.catalog.GetViewGeneration(ctx, released)
		require.NoError(t, err)
		if !found {
			break
		}
	}
	require.NoError(t, ctx.Err())
	requireGenerationRetired(t, f.store, released)
	// Further passes keep standing the young debt down.
	for pass := 0; pass < 3; pass++ {
		_, pending, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		require.True(t, pending)
	}
	require.Greater(t, deferredRetirementPassStandDowns.Load(), standDowns, "young debt never stood down")
	mu.Lock()
	defer mu.Unlock()
	require.Positive(t, offers[released])
	require.Zero(t, offers[young], "a burst touched young debt beside released parked work")
	row, found, err := f.catalog.GetViewGeneration(t.Context(), young)
	require.NoError(t, err)
	require.True(t, found, "young debt was retired")
	require.Equal(t, store_sqlite.ViewGenerationSuperseded, row.State, "young debt was fenced")
	require.False(t, l.retirementDebtAged(time.Now()), "the released park aged the shared clock")
}
