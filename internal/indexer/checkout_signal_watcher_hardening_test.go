package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type checkoutSourceSignalAttemptPhase int

const (
	checkoutSourceSignalAttemptReady checkoutSourceSignalAttemptPhase = iota
	checkoutSourceSignalAttemptFactoryError
	checkoutSourceSignalAttemptPreReadyError
	checkoutSourceSignalAttemptReadyTimeout
	checkoutSourceSignalAttemptPostReadyError
)

type checkoutSourceSignalScriptedBackend struct {
	phase   checkoutSourceSignalAttemptPhase
	ready   chan struct{}
	events  chan fswatcher.WatchEvent
	dropped chan fswatcher.WatchEvent
	done    chan struct{}
}

func (b *checkoutSourceSignalScriptedBackend) Watch(ctx context.Context) error {
	defer close(b.done)
	switch b.phase {
	case checkoutSourceSignalAttemptPreReadyError:
		return errors.New("scripted pre-ready failure")
	case checkoutSourceSignalAttemptReadyTimeout:
		<-ctx.Done()
		return ctx.Err()
	default:
		close(b.ready)
		if b.phase == checkoutSourceSignalAttemptPostReadyError {
			return errors.New("scripted post-ready failure")
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func (b *checkoutSourceSignalScriptedBackend) Events() <-chan fswatcher.WatchEvent  { return b.events }
func (b *checkoutSourceSignalScriptedBackend) Dropped() <-chan fswatcher.WatchEvent { return b.dropped }
func (b *checkoutSourceSignalScriptedBackend) Close()                               {}

type checkoutSourceSignalScriptedFactory struct {
	mu       sync.Mutex
	phases   []checkoutSourceSignalAttemptPhase
	attempts []time.Time
	backends []*checkoutSourceSignalScriptedBackend
}

func (f *checkoutSourceSignalScriptedFactory) New(
	_ string,
	ready chan struct{},
	events, dropped chan fswatcher.WatchEvent,
) (checkoutSourceSignalBackend, error) {
	f.mu.Lock()
	attempt := len(f.attempts)
	f.attempts = append(f.attempts, time.Now())
	phase := checkoutSourceSignalAttemptReady
	if attempt < len(f.phases) {
		phase = f.phases[attempt]
	}
	if phase == checkoutSourceSignalAttemptFactoryError {
		f.mu.Unlock()
		return nil, errors.New("scripted factory failure")
	}
	backend := &checkoutSourceSignalScriptedBackend{
		phase: phase, ready: ready, events: events, dropped: dropped, done: make(chan struct{}),
	}
	f.backends = append(f.backends, backend)
	f.mu.Unlock()
	return backend, nil
}

func (f *checkoutSourceSignalScriptedFactory) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func (f *checkoutSourceSignalScriptedFactory) Times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.attempts...)
}

func waitCheckoutSourceSignalAttempts(t testing.TB, factory *checkoutSourceSignalScriptedFactory, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for factory.Count() < count {
		if time.Now().After(deadline) {
			t.Fatalf("watcher attempts = %d, want at least %d", factory.Count(), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCheckoutSourceSignalWatcherRetriesEveryBackendFailurePhase(t *testing.T) {
	for _, test := range []struct {
		name  string
		phase checkoutSourceSignalAttemptPhase
	}{
		{name: "factory", phase: checkoutSourceSignalAttemptFactoryError},
		{name: "pre_ready", phase: checkoutSourceSignalAttemptPreReadyError},
		{name: "ready_timeout", phase: checkoutSourceSignalAttemptReadyTimeout},
		{name: "post_ready", phase: checkoutSourceSignalAttemptPostReadyError},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			factory := &checkoutSourceSignalScriptedFactory{phases: []checkoutSourceSignalAttemptPhase{test.phase, checkoutSourceSignalAttemptReady}}
			signal, records := checkoutSourceSignalRecorder(8)
			watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
			watchers.readyTimeout = 8 * time.Millisecond
			watchers.retryInitial = 2 * time.Millisecond
			watchers.retryMax = 8 * time.Millisecond
			t.Cleanup(watchers.StopAll)

			if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
				t.Fatalf("ensure watcher: %v", err)
			}
			waitCheckoutSourceSignalAttempts(t, factory, 2)
			waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
			if got := watchers.Len(); got != 1 {
				t.Fatalf("live registrations after retry = %d, want 1", got)
			}
		})
	}
}

func TestCheckoutSourceSignalWatcherRetryIsExponentialAndStopAllJoins(t *testing.T) {
	root := t.TempDir()
	factory := &checkoutSourceSignalScriptedFactory{phases: []checkoutSourceSignalAttemptPhase{
		checkoutSourceSignalAttemptFactoryError,
		checkoutSourceSignalAttemptFactoryError,
		checkoutSourceSignalAttemptFactoryError,
		checkoutSourceSignalAttemptFactoryError,
	}}
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, nil, zap.NewNop())
	watchers.retryInitial = 5 * time.Millisecond
	watchers.retryMax = 20 * time.Millisecond
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		t.Fatalf("ensure watcher: %v", err)
	}
	waitCheckoutSourceSignalAttempts(t, factory, 4)
	times := factory.Times()
	minimums := []time.Duration{3 * time.Millisecond, 7 * time.Millisecond, 15 * time.Millisecond}
	for i, minimum := range minimums {
		if delay := times[i+1].Sub(times[i]); delay < minimum {
			t.Fatalf("retry delay %d = %s, want >= %s", i+1, delay, minimum)
		}
	}

	watchers.StopAll()
	attempts := factory.Count()
	time.Sleep(2 * watchers.retryMax)
	if got := factory.Count(); got != attempts {
		t.Fatalf("attempts after StopAll = %d, want stable %d", got, attempts)
	}
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); !errors.Is(err, errCheckoutSourceSignalSetStopped) {
		t.Fatalf("Ensure after StopAll error = %v, want stopped", err)
	}
}

func TestCheckoutSourceSignalWatcherDropCancelsRetrySupervisor(t *testing.T) {
	root := t.TempDir()
	factory := &checkoutSourceSignalScriptedFactory{phases: []checkoutSourceSignalAttemptPhase{checkoutSourceSignalAttemptReadyTimeout}}
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, nil, zap.NewNop())
	watchers.readyTimeout = 20 * time.Millisecond
	watchers.retryInitial = time.Millisecond
	watchers.retryMax = 4 * time.Millisecond
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		t.Fatalf("ensure watcher: %v", err)
	}
	waitCheckoutSourceSignalAttempts(t, factory, 1)
	watchers.Drop("checkout-1")
	attempts := factory.Count()
	time.Sleep(3 * watchers.readyTimeout)
	if got := factory.Count(); got != attempts {
		t.Fatalf("attempts after Drop = %d, want stable %d", got, attempts)
	}
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live registrations after Drop = %d, want 0", got)
	}
}

func TestCheckoutSourceSignalWatcherCanonicalAliasEnsureReusesEpochAndBackend(t *testing.T) {
	realRoot := t.TempDir()
	aliasRoot := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("create root alias: %v", err)
	}
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(4)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
	t.Cleanup(watchers.StopAll)
	identity := checkoutSourceSignalTestIdentity(aliasRoot, 1)
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure aliased root: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	first := watchers.watchers[identity.checkoutID]
	watchers.mu.Unlock()

	identity.requestedRoot = realRoot
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure canonical-equivalent root: %v", err)
	}
	watchers.mu.Lock()
	second := watchers.watchers[identity.checkoutID]
	watchers.mu.Unlock()
	if second != first || second.epoch != first.epoch {
		t.Fatalf("canonical-equivalent root replaced registration: first=%p/%d second=%p/%d", first, first.epoch, second, second.epoch)
	}
	if got := factory.Count(); got != 1 {
		t.Fatalf("canonical-equivalent backend creations = %d, want 1", got)
	}
	select {
	case <-factory.Backend(0).stopped:
		t.Fatal("canonical-equivalent ensure stopped the original backend")
	default:
	}
}

func TestCheckoutSourceSignalWatcherSustainedStreamHasBoundedAcceleration(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(16)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
	watchers.quietWindow = 50 * time.Millisecond
	watchers.maxDelay = 30 * time.Millisecond
	t.Cleanup(watchers.StopAll)
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		t.Fatalf("ensure watcher: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	backend := factory.Backend(0)

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		deadline := time.Now().Add(90 * time.Millisecond)
		for time.Now().Before(deadline) {
			backend.events <- fswatcher.WatchEvent{Path: filepath.Join(root, "changed.go")}
		}
	}()
	waitCheckoutSourceSignal(t, records, "source-event-max-delay", time.Second)
	<-streamDone
	waitCheckoutSourceSignal(t, records, "source-event-final", time.Second)
	select {
	case extra := <-records:
		t.Fatalf("sustained stream produced more than max+final signals: %+v", extra)
	case <-time.After(3 * watchers.quietWindow):
	}
}

func TestCheckoutSourceSignalWatcherEnsureStopAllRaceLeavesNoBackend(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		root := t.TempDir()
		factory := &fakeCheckoutSourceSignalFactory{}
		watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, nil, zap.NewNop())
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = watchers.Ensure(checkoutSourceSignalTestIdentity(root, iteration))
		}()
		go func() {
			defer wg.Done()
			<-start
			watchers.StopAll()
		}()
		close(start)
		wg.Wait()
		if got := watchers.Len(); got != 0 {
			t.Fatalf("iteration %d: live registrations = %d, want 0", iteration, got)
		}
		for backend := 0; backend < factory.Count(); backend++ {
			select {
			case <-factory.Backend(backend).stopped:
			default:
				t.Fatalf("iteration %d: backend %d remained live", iteration, backend)
			}
		}
	}
}

func TestCheckoutLifecycleSourceWatcherPromotionDemotionDisappearanceAndPrimaryLoss(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(16)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
	coordinator := &CheckoutCoordinator{root: root, familyID: "family", graphID: "primary"}
	lifecycle := &CheckoutLifecycle{
		logger: zap.NewNop(), coordinators: map[string]*CheckoutCoordinator{"checkout": coordinator},
		checkoutSignalWatchers: watchers,
	}
	checkout := store_sqlite.Checkout{
		CheckoutID: "checkout", Incarnation: "inc", FamilyID: "family", RootPath: root,
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	firstEpoch := watchers.watchers[checkout.CheckoutID].epoch
	watchers.mu.Unlock()

	checkout.EffectiveMode = store_sqlite.CheckoutModeDedicated
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	if got := watchers.Len(); got != 0 {
		t.Fatalf("watchers after promotion = %d, want 0", got)
	}
	checkout.EffectiveMode = store_sqlite.CheckoutModeAutomatic
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	secondEpoch := watchers.watchers[checkout.CheckoutID].epoch
	watchers.mu.Unlock()
	if secondEpoch <= firstEpoch {
		t.Fatalf("demotion epoch = %d, want > %d", secondEpoch, firstEpoch)
	}

	lifecycle.dropCheckoutSourceSignalWatcher(checkout.CheckoutID)
	if got := watchers.Len(); got != 0 {
		t.Fatalf("watchers after disappearance = %d, want 0", got)
	}
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	lifecycle.stopCheckoutSourceSignalWatchers()
	if got := watchers.Len(); got != 0 {
		t.Fatalf("watchers after primary loss = %d, want 0", got)
	}
}

func TestCheckoutLifecycleSourceSignalGuardCanonicalAliasAndPrimaryAdmission(t *testing.T) {
	realRoot := t.TempDir()
	aliasParent := t.TempDir()
	aliasRoot := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("create root alias: %v", err)
	}
	canonicalRoot, err := canonicalCheckoutSourceSignalRoot(aliasRoot)
	if err != nil {
		t.Fatalf("canonicalize alias: %v", err)
	}

	for _, test := range []struct {
		name             string
		graphFamily      string
		primary          bool
		coordinatorGraph string
		coordinatorSwap  bool
		want             bool
	}{
		{name: "canonical_alias", graphFamily: "family", primary: true, coordinatorGraph: "primary", want: true},
		{name: "wrong_family", graphFamily: "other", primary: true, coordinatorGraph: "primary"},
		{name: "not_primary", graphFamily: "family", coordinatorGraph: "primary"},
		{name: "wrong_coordinator_graph", graphFamily: "family", primary: true, coordinatorGraph: "other"},
		{name: "coordinator_replaced", graphFamily: "family", primary: true, coordinatorGraph: "primary", coordinatorSwap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "catalog.db"))
			if err != nil {
				t.Fatalf("open catalog store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			catalog := store.Catalog()
			ctx := context.Background()
			if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
				FamilyID: "family", CommonDirIdentity: "common", State: "ready",
			}); err != nil {
				t.Fatalf("upsert family: %v", err)
			}
			if test.graphFamily != "family" {
				if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
					FamilyID: test.graphFamily, CommonDirIdentity: "other-common", State: "ready",
				}); err != nil {
					t.Fatalf("upsert graph family: %v", err)
				}
			}
			checkout := store_sqlite.Checkout{
				CheckoutID: "checkout", Incarnation: "inc", FamilyID: "family", RootPath: aliasRoot,
				State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic,
				EffectiveMode: store_sqlite.CheckoutModeAutomatic,
			}
			if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
				t.Fatalf("upsert checkout: %v", err)
			}
			if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
				GraphID: "primary", RepoPrefix: "repo", FamilyID: test.graphFamily,
				IsPrimaryBase: test.primary, State: "ready",
			}); err != nil {
				t.Fatalf("upsert graph: %v", err)
			}
			coordinator := &CheckoutCoordinator{root: realRoot, familyID: "family", graphID: test.coordinatorGraph}
			lifecycle := &CheckoutLifecycle{
				catalog: catalog, logger: zap.NewNop(),
				coordinators: map[string]*CheckoutCoordinator{"checkout": coordinator},
			}
			identity := checkoutSourceSignalIdentity{
				checkoutID: "checkout", incarnation: "inc", familyID: "family",
				requestedRoot: aliasRoot, canonicalRoot: canonicalRoot,
				primaryGraphID: "primary", coordinator: coordinator,
			}
			if test.coordinatorSwap {
				lifecycle.coordinators["checkout"] = &CheckoutCoordinator{root: realRoot, familyID: "family", graphID: "primary"}
			}
			if got := lifecycle.checkoutSourceSignalCurrent(identity); got != test.want {
				t.Fatalf("guard admission = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCheckoutLifecycleSourceSignalCloseGateIsReusable(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(8)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
	coordinator := &CheckoutCoordinator{root: root, familyID: "family", graphID: "primary"}
	lifecycle := &CheckoutLifecycle{
		logger: zap.NewNop(), coordinators: map[string]*CheckoutCoordinator{"checkout": coordinator},
		checkoutSignalWatchers: watchers,
	}
	checkout := store_sqlite.Checkout{
		CheckoutID: "checkout", Incarnation: "inc", FamilyID: "family", RootPath: root,
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)

	lifecycle.beginCheckoutSourceSignalClose()
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	lifecycle.stopCheckoutSourceSignalWatchers()
	if lifecycle.sourceSignalWatcherSet() != nil {
		t.Fatal("source watcher set was recreated while close gate held")
	}
	lifecycle.finishCheckoutSourceSignalClose()
	if lifecycle.sourceSignalWatcherSet() == nil {
		t.Fatal("source watcher set was not reusable after close returned")
	}
	lifecycle.stopCheckoutSourceSignalWatchers()
}

func TestCheckoutSourceSignalWatcherConcurrentSignalsRemainABAExact(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	var signals atomic.Int64
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, func(string, string) bool {
		signals.Add(1)
		return true
	}, zap.NewNop())
	identity := checkoutSourceSignalTestIdentity(root, 1)
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure watcher: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for factory.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	watchers.mu.Lock()
	registration := watchers.watchers[identity.checkoutID]
	watchers.mu.Unlock()
	watchers.Drop(identity.checkoutID)
	baseline := signals.Load()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchers.signalIfCurrent(registration, "stale")
		}()
	}
	wg.Wait()
	if got := signals.Load(); got != baseline {
		t.Fatalf("stale ABA callbacks emitted %d signals", got-baseline)
	}
	watchers.StopAll()
}

func TestCheckoutSourceSignalWatcherEnsureDropRaceJoinsPublishedBackend(t *testing.T) {
	root := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	created := make(chan *fakeCheckoutSourceSignalBackend, 1)
	factory := func(
		_ string, ready chan struct{}, events, dropped chan fswatcher.WatchEvent,
	) (checkoutSourceSignalBackend, error) {
		backend := &fakeCheckoutSourceSignalBackend{
			ready: ready, events: events, dropped: dropped,
			started: make(chan struct{}), stopped: make(chan struct{}),
		}
		created <- backend
		close(entered)
		<-release
		return backend, nil
	}
	watchers := newCheckoutSourceSignalWatcherSet(factory, func(checkoutSourceSignalIdentity) bool { return true }, nil, zap.NewNop())
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		t.Fatalf("ensure watcher: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("factory was not entered")
	}
	dropped := make(chan struct{})
	go func() {
		watchers.Drop("checkout-1")
		close(dropped)
	}()
	close(release)
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("Drop did not join the published backend")
	}
	backend := <-created
	select {
	case <-backend.stopped:
	default:
		t.Fatal("Drop returned before raced backend stopped")
	}
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live registrations after raced Drop = %d, want 0", got)
	}
	watchers.StopAll()
}

func TestCheckoutLifecycleCloseRejectsConcurrentEnsureAndAllowsReuseAfterReturn(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(8)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool { return true }, signal, zap.NewNop())
	coordinator := &CheckoutCoordinator{root: root, familyID: "family", graphID: "primary"}
	lifecycle := &CheckoutLifecycle{
		logger: zap.NewNop(), coordinators: map[string]*CheckoutCoordinator{"checkout": coordinator},
		coordinatorHeads:       map[string]checkoutHeadIdentity{},
		started:                map[string][]*CheckoutCoordinator{},
		checkoutSignalWatchers: watchers,
	}
	checkout := store_sqlite.Checkout{
		CheckoutID: "checkout", Incarnation: "inc", FamilyID: "family", RootPath: root,
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)

	// Hold the established retry lease so Close remains inside its admission
	// window long enough to deterministically race a fresh Ensure.
	lifecycle.retryMu.Lock()
	lifecycle.retryWG.Add(1)
	lifecycle.retryMu.Unlock()
	// The synthetic coordinator has no loop/done channel; remove it from the
	// Close snapshot while retaining the already-published source registration.
	lifecycle.coordMu.Lock()
	delete(lifecycle.coordinators, checkout.CheckoutID)
	lifecycle.coordMu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- lifecycle.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		lifecycle.checkoutSignalWatchMu.Lock()
		closing := lifecycle.checkoutSignalWatchClosing
		lifecycle.checkoutSignalWatchMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			lifecycle.retryWG.Done()
			t.Fatal("Close did not enter source admission gate")
		}
		time.Sleep(time.Millisecond)
	}
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	lifecycle.retryWG.Done()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after retry lease drained")
	}
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live registrations after Close = %d, want 0", got)
	}
	lifecycle.checkoutSignalWatchMu.Lock()
	retained := lifecycle.checkoutSignalWatchers
	lifecycle.checkoutSignalWatchMu.Unlock()
	if retained != nil {
		t.Fatal("Close retained watcher set")
	}

	replacementFactory := &fakeCheckoutSourceSignalFactory{}
	replacementSignal, replacementRecords := checkoutSourceSignalRecorder(4)
	replacement := newCheckoutSourceSignalWatcherSet(replacementFactory.New, func(checkoutSourceSignalIdentity) bool { return true }, replacementSignal, zap.NewNop())
	lifecycle.coordMu.Lock()
	lifecycle.coordinators[checkout.CheckoutID] = coordinator
	lifecycle.coordMu.Unlock()
	lifecycle.checkoutSignalWatchMu.Lock()
	lifecycle.checkoutSignalWatchers = replacement
	lifecycle.checkoutSignalWatchMu.Unlock()
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, replacementRecords, "source-watch-ready", time.Second)
	lifecycle.stopCheckoutSourceSignalWatchers()
}

func TestCheckoutSourceSignalWatcherFactoryAttemptCountIsThreadSafe(t *testing.T) {
	factory := &checkoutSourceSignalScriptedFactory{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = factory.Count()
			_ = factory.Times()
		}()
	}
	wg.Wait()
	if got := fmt.Sprint(factory.Count()); got != "0" {
		t.Fatalf("unexpected attempt count %s", got)
	}
}
