package indexer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/reconcile"
)

type cleanupFinishActorKey struct{}

// This fixture starts at the completed producer/payload boundary. It does not
// claim to exercise MI untracking, configuration deletion, or physical purge.
// Catalog journals, reconciler calls, lifecycle finalization, ref-view drains,
// owner lease finalization and same-identity re-registration are all real.
type lifecycleFinishBoundaryHooks struct {
	l                                           *CheckoutLifecycle
	prepareMu                                   sync.Mutex
	firstPurge, allowFirstPurge                 chan struct{}
	lateFinish, allowLateFinish                 chan struct{}
	replacementRelease, allowReplacementRelease chan struct{}
	purgeOnce, lateOnce, replacementOnce        sync.Once
	legacyFinishes                              atomic.Int64
}

func (h *lifecycleFinishBoundaryHooks) prepareDrainedState(ctx context.Context, graphID string) error {
	h.prepareMu.Lock()
	defer h.prepareMu.Unlock()
	runtime := h.l.repositoryCleanup
	runtime.mu.Lock()
	state := runtime.states[graphID]
	runtime.mu.Unlock()
	if state != nil {
		return nil
	}
	identity, owner, found, err := h.l.closeRepositoryAdmission(ctx, graphID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("fixture graph %s disappeared before local close", graphID)
	}
	drained := make(chan struct{})
	close(drained)
	state = &repositoryCleanupState{
		graphID: graphID, identity: identity, owner: owner,
		refs:      h.l.closeRepositoryRefViews(identity.RepoPrefix),
		lane:      &repositoryCleanupLane{owner: h.l.mi, prefix: identity.RepoPrefix, done: drained, finalized: true},
		publisher: drained, initialized: true, producersDone: drained,
		attemptDone: make(chan struct{}), waitReady: drained,
	}
	runtime.mu.Lock()
	runtime.states[graphID] = state
	runtime.mu.Unlock()
	return nil
}

func (h *lifecycleFinishBoundaryHooks) BeginGraphCleanup(ctx context.Context, graphID string) error {
	if err := h.prepareDrainedState(ctx, graphID); err != nil {
		return err
	}
	return (cleanupHooks{l: h.l}).BeginGraphCleanup(ctx, graphID)
}

// Optional V2 seam; the baseline Reconciler ignores this method and uses its
// existing graph-ID notification below. This keeps the same regression source
// valid before and after the captured-state repair.
func (h *lifecycleFinishBoundaryHooks) BeginGraphCleanupAttempt(ctx context.Context, graphID string) (func(), error) {
	if err := h.prepareDrainedState(ctx, graphID); err != nil {
		return nil, err
	}
	hook, ok := any(cleanupHooks{l: h.l}).(interface {
		BeginGraphCleanupAttempt(context.Context, string) (func(), error)
	})
	if !ok {
		return nil, fmt.Errorf("Reconciler selected captured-finish mode without a lifecycle implementation")
	}
	finish, err := hook.BeginGraphCleanupAttempt(ctx, graphID)
	if finish == nil {
		return nil, err
	}
	actor, _ := ctx.Value(cleanupFinishActorKey{}).(string)
	return func() {
		if actor == "forget" {
			h.pauseLateFinish()
		}
		finish()
	}, err
}

func (h *lifecycleFinishBoundaryHooks) PurgeCheckoutLayers(ctx context.Context, _, _ string) error {
	if ctx.Value(cleanupFinishActorKey{}) != "forget" {
		return nil
	}
	h.purgeOnce.Do(func() { close(h.firstPurge) })
	select {
	case <-h.allowFirstPurge:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *lifecycleFinishBoundaryHooks) ReleaseGraph(ctx context.Context, graphID string) error {
	if ctx.Value(cleanupFinishActorKey{}) == "replacement" {
		h.replacementOnce.Do(func() { close(h.replacementRelease) })
		select {
		case <-h.allowReplacementRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runtime := h.l.repositoryCleanup
	runtime.mu.Lock()
	state := runtime.states[graphID]
	runtime.mu.Unlock()
	if state == nil {
		return fmt.Errorf("fixture has no captured cleanup state for %s", graphID)
	}
	state.mu.Lock()
	state.payloadPurged = true // no physical corpus was installed by this fixture
	state.mu.Unlock()
	return nil
}

func (h *lifecycleFinishBoundaryHooks) pauseLateFinish() {
	h.lateOnce.Do(func() { close(h.lateFinish); <-h.allowLateFinish })
}

func (h *lifecycleFinishBoundaryHooks) RepositoryCleanupAttemptFinished(graphIDs []string) {
	// retire_graph completes first; forget_checkout then deletes the last old
	// journal and pauses before delivering its deferred lifecycle notification.
	if h.legacyFinishes.Add(1) == 2 {
		h.pauseLateFinish()
	}
	notifier, ok := any(cleanupHooks{l: h.l}).(interface{ RepositoryCleanupAttemptFinished([]string) })
	if !ok {
		panic("Reconciler delivered an obsolete graph-ID notification to a captured-only lifecycle")
	}
	notifier.RepositoryCleanupAttemptFinished(graphIDs)
}

func TestRepositoryLateCleanupFinishCannotSignalRetrackedState(t *testing.T) {
	for _, separate := range []bool{false, true} {
		name := "same_reconciler"
		if separate {
			name = "separate_reconcilers"
		}
		t.Run(name, func(t *testing.T) {
			l, identity := repositoryAdmissionFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			owner, found, err := l.catalog.GetCheckout(ctx, identity.CheckoutID)
			if err != nil || !found {
				t.Fatalf("healthy owner: found=%v err=%v", found, err)
			}
			graph, found, err := l.catalog.GetDedicatedGraph(ctx, identity.GraphID)
			if err != nil || !found {
				t.Fatalf("healthy graph: found=%v err=%v", found, err)
			}
			l.mi = &MultiIndexer{}
			// No retry goroutine is started: drive the exact finalizer explicitly
			// while actors are held at deterministic journal/finish boundaries.
			l.repositoryCleanup = &repositoryCleanupRuntime{states: make(map[string]*repositoryCleanupState), wake: make(chan struct{}, 1)}
			hooks := &lifecycleFinishBoundaryHooks{l: l,
				firstPurge: make(chan struct{}), allowFirstPurge: make(chan struct{}),
				lateFinish: make(chan struct{}), allowLateFinish: make(chan struct{}),
				replacementRelease: make(chan struct{}), allowReplacementRelease: make(chan struct{}),
			}
			cfg := reconcile.Config{AvailabilityGrace: time.Second, RemovalGrace: time.Second}
			first, err := reconcile.New(l.catalog, hooks, cfg)
			if err != nil {
				t.Fatal(err)
			}
			second := first
			if separate {
				second, err = reconcile.New(l.catalog, hooks, cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			l.rec = first
			if err := l.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
				t.Fatal(err)
			}
			read, err := l.AcquireRepositoryRead(identity.GraphID)
			if err != nil {
				t.Fatal(err)
			}
			read.Release()
			assertPending := func(want bool) {
				t.Helper()
				got, err := l.rec.RepositoryCleanupPending(ctx, identity.GraphID, identity.CheckoutID, identity.Incarnation, identity.FamilyID)
				if err != nil || got != want {
					t.Fatalf("durable pending=%v want=%v err=%v", got, want, err)
				}
			}
			assertPending(false)
			var actors sync.WaitGroup
			var purgeOnce, finishOnce, replacementOnce sync.Once
			unblockPurge := func() { purgeOnce.Do(func() { close(hooks.allowFirstPurge) }) }
			unblockFinish := func() { finishOnce.Do(func() { close(hooks.allowLateFinish) }) }
			unblockReplacement := func() { replacementOnce.Do(func() { close(hooks.allowReplacementRelease) }) }
			defer func() { cancel(); unblockPurge(); unblockFinish(); unblockReplacement(); actors.Wait() }()
			await := func(ch <-chan struct{}, label string) {
				t.Helper()
				select {
				case <-ch:
				case <-ctx.Done():
					t.Fatalf("%s: %v", label, ctx.Err())
				}
			}
			forgetResult := make(chan error, 1)
			forgetDone := make(chan struct{})
			actors.Add(1)
			go func() {
				defer actors.Done()
				defer close(forgetDone)
				_, err := first.ForgetCheckoutExplicit(context.WithValue(ctx, cleanupFinishActorKey{}, "forget"), identity.CheckoutID, identity.Incarnation, identity.FamilyID, identity.GraphID)
				forgetResult <- err
			}()
			await(hooks.firstPurge, "old forget purge")
			// A different stable cleanup ID completes while the first still owns
			// a durable journal. Its completion legitimately wakes the old state.
			if err := second.RetireDedicatedGraph(ctx, identity.GraphID); err != nil {
				t.Fatal(err)
			}
			oldState := l.repositoryCleanup.states[identity.GraphID]
			if oldState == nil || !channelClosed(oldState.attemptDone) {
				t.Fatal("independent completion did not notify old state")
			}
			assertPending(true)
			if pending, err := l.finalizeRepositoryCleanups(ctx, identity.RepoPrefix); err != nil || !pending {
				t.Fatalf("remaining old journal did not hold finalizer: pending=%v err=%v", pending, err)
			}
			unblockPurge()
			await(hooks.lateFinish, "last old journal deleted, finish held")
			assertPending(false)
			if entries, err := l.catalog.ListCleanupEntries(ctx); err != nil || len(entries) != 0 {
				t.Fatalf("old journals remain: %+v err=%v", entries, err)
			}
			if pending, err := l.finalizeRepositoryCleanups(ctx, identity.RepoPrefix); err != nil || pending {
				t.Fatalf("actual old finalization: pending=%v err=%v", pending, err)
			}
			if !oldState.finalized || len(l.repositoryCleanup.states) != 0 {
				t.Fatal("old cleanup state was not actually removed")
			}
			if err := l.catalog.UpsertCheckout(ctx, owner); err != nil {
				t.Fatal(err)
			}
			if err := l.catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
				t.Fatal(err)
			}
			if err := l.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
				t.Fatal(err)
			}
			replacementRead, err := l.AcquireRepositoryRead(identity.GraphID)
			if err != nil {
				t.Fatal(err)
			}
			replacementRead.Release()
			assertPending(false)
			replacementResult := make(chan error, 1)
			replacementDone := make(chan struct{})
			actors.Add(1)
			go func() {
				defer actors.Done()
				defer close(replacementDone)
				replacementResult <- second.RetireDedicatedGraph(context.WithValue(ctx, cleanupFinishActorKey{}, "replacement"), identity.GraphID)
			}()
			await(hooks.replacementRelease, "replacement's own release hook")
			newState := l.repositoryCleanup.states[identity.GraphID]
			if newState == nil || newState == oldState || newState.owner == oldState.owner {
				t.Fatal("same-identity retrack did not create distinct cleanup capabilities")
			}
			if channelClosed(newState.attemptDone) {
				t.Fatal("replacement was already finished before old callback")
			}
			assertPending(true)
			unblockFinish()
			await(forgetDone, "old finish delivery")
			if err := <-forgetResult; err != nil {
				t.Errorf("old forget completion: %v", err)
			}
			if channelClosed(newState.attemptDone) {
				t.Error("old graph-ID finish signaled the replacement cleanup state")
			}
			assertPending(true) // Do not turn a wrong wakeup into a data-loss claim.
			unblockReplacement()
			await(replacementDone, "replacement's own completion")
			if err := <-replacementResult; err != nil {
				t.Fatal(err)
			}
			if !channelClosed(newState.attemptDone) {
				t.Fatal("replacement's own callback did not signal completion")
			}
			assertPending(false)
			if pending, err := l.finalizeRepositoryCleanups(ctx, identity.RepoPrefix); err != nil || pending {
				t.Fatalf("replacement finalization: pending=%v err=%v", pending, err)
			}
			if len(l.repositoryCleanup.states) != 0 || len(l.repositoryOwners) != 0 || len(l.repositoryClosing) != 0 {
				t.Fatal("completed cleanup retained local state")
			}
			if _, found, err := l.catalog.GetDedicatedGraph(ctx, identity.GraphID); err != nil || found {
				t.Fatalf("replacement cleanup left graph: found=%v err=%v", found, err)
			}
		})
	}
}
