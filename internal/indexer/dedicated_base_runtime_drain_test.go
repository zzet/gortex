package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func dedicatedDrainOwner() store_sqlite.DedicatedBaseOwner {
	return store_sqlite.DedicatedBaseOwner{CheckoutID: "owner", Incarnation: "incarnation"}
}

func assertDedicatedDrainOpen(t testing.TB, drained <-chan struct{}) {
	t.Helper()
	select {
	case <-drained:
		t.Fatal("drain finished while an admitted actor still owns the runtime")
	default:
	}
}

func awaitDedicatedDrain(t testing.TB, drained <-chan struct{}) {
	t.Helper()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime drain did not complete")
	}
}

func TestDedicatedBaseRuntimeOwnerDrainIsIdempotent(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	release, err := r.admitOwner(t.Context(), "graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := r.confirmOwner("graph", owner); err != nil {
		t.Fatal(err)
	}
	drained, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil || again != drained {
		t.Fatalf("close did not coalesce: same=%v err=%v", again == drained, err)
	}
	assertDedicatedDrainOpen(t, drained)
	if _, err := r.admitOwner(t.Context(), "graph", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("new actor admitted while closing: %v", err)
	}
	release()
	release()
	awaitDedicatedDrain(t, drained)
	if _, err := r.admitOwner(t.Context(), "graph", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("idle owner resurrected after drain: %v", err)
	}
}

func TestDedicatedBaseRuntimeUnverifiedOwnerDoesNotPoisonAdmission(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	wrong := dedicatedDrainOwner()
	wrong.Incarnation = "stale-allegation"
	release, err := r.admitOwner(t.Context(), "graph", wrong)
	if err != nil {
		t.Fatal(err)
	}
	// Models a catalog validation refusal. No ownership confirmation follows.
	release()
	owner := dedicatedDrainOwner()
	release, err = r.admitOwner(t.Context(), "graph", owner)
	if err != nil {
		t.Fatalf("failed request poisoned legitimate installation: %v", err)
	}
	defer release()
	if err := r.confirmOwner("graph", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.admitOwner(t.Context(), "graph", wrong); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("confirmed owner mismatch not fenced: %v", err)
	}
	if _, err := r.CloseDedicatedBaseOwner("graph", wrong); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("wrong owner closed current publisher: %v", err)
	}
	next, err := r.admitOwner(t.Context(), "graph", owner)
	if err != nil {
		t.Fatalf("wrong-owner close changed admission: %v", err)
	}
	next()
}

func TestDedicatedBaseRuntimeCloseBeforeFirstAdmission(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	drained, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, drained)
	if _, err := r.admitOwner(t.Context(), "graph", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("first request resurrected forgotten owner: %v", err)
	}
}

func TestDedicatedBaseRuntimeAuthorizedReregistrationRevokesOldSlot(t *testing.T) {
	for _, sameIncarnation := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-owner-retrack", false: "new-incarnation"}[sameIncarnation], func(t *testing.T) {
			r := &dedicatedBaseRuntime{}
			owner := dedicatedDrainOwner()
			if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
				t.Fatal(err)
			}
			release, oldSlot, err := r.admitOwnerState(t.Context(), "graph", owner, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			drained, err := r.CloseDedicatedBaseOwner("graph", owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.RegisterDedicatedBaseOwner("graph", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
				t.Fatalf("registration bypassed outstanding actor: %v", err)
			}
			release()
			awaitDedicatedDrain(t, drained)
			newOwner := owner
			if !sameIncarnation {
				newOwner.Incarnation += "-replacement"
			}
			if err := r.RegisterDedicatedBaseOwner("graph", newOwner); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.admitOwnerState(t.Context(), "graph", owner, oldSlot); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
				t.Fatalf("held old publisher revived after re-registration: %v", err)
			}
			currentRelease, currentSlot, err := r.admitOwnerState(t.Context(), "graph", newOwner, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer currentRelease()
			if currentSlot == oldSlot {
				t.Fatal("registration reopened the old capability")
			}
			if err := r.RegisterDedicatedBaseOwner("graph", newOwner); err != nil {
				t.Fatal(err)
			}
			anotherRelease, again, err := r.admitOwnerState(t.Context(), "graph", newOwner, currentSlot)
			if err != nil {
				t.Fatal(err)
			}
			anotherRelease()
			if again != currentSlot {
				t.Fatal("idempotent live registration replaced admission")
			}
		})
	}
}

func TestDedicatedBaseRuntimePhysicalTailIsPartOfOwnerDrain(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	r := &dedicatedBaseRuntime{store: b.Store}
	publisher := &dedicatedBasePublisher{runtime: r, authority: claim.Desire.Authority}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	entered := make(chan struct{})
	allow := make(chan struct{})
	release := sync.OnceFunc(func() { close(allow) })
	finished := make(chan struct{})
	var result dedicatedBaseResult
	var buildErr error
	go func() {
		defer close(finished)
		result, buildErr = publisher.ensureInitial(ctx, func(context.Context) (dedicatedBaseObservation, error) {
			return dedicatedBaseObservation{
				Identity: claim.Desire.Identity, ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
				RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
				CreatedAt: 1, Builder: *b,
				PrePublish: func(ctx context.Context, _ int64) error {
					close(entered)
					select {
					case <-allow:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}, nil
		})
	}()
	// Registered after fixture cleanup so producer joining precedes Store.Close.
	t.Cleanup(func() { release(); cancel(); <-finished })
	select {
	case <-entered:
	case <-finished:
		t.Fatalf("physical producer exited before barrier: %v", buildErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	drained, err := r.CloseDedicatedBaseOwner(claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner)
	if err != nil {
		t.Fatal(err)
	}
	assertDedicatedDrainOpen(t, drained)
	// The observation gate is no longer held, but the physical actor is still
	// owned. This local-fence test does not simulate catalog closing: an already
	// admitted producer may finish. The lifecycle test owns durable refusal.
	release()
	awaitDedicatedDrain(t, finished)
	awaitDedicatedDrain(t, drained)
	if buildErr != nil || result.Adoption.GenerationID != claim.GenerationID {
		t.Fatalf("admitted physical producer did not finish: result=%+v err=%v", result, buildErr)
	}
}

func TestDedicatedBaseRuntimeOwnerCloseIsScopedAndShutdownJoinsAll(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	releaseA, err := r.admitOwner(t.Context(), "a", owner)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	releaseB, err := r.admitOwner(t.Context(), "b", owner)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	closedA, err := r.CloseDedicatedBaseOwner("a", owner)
	if err != nil {
		t.Fatal(err)
	}
	additionalB, err := r.admitOwner(t.Context(), "b", owner)
	if err != nil {
		t.Fatalf("unrelated graph was closed: %v", err)
	}
	additionalB()
	all := r.CloseDedicatedBaseAdmission()
	if r.CloseDedicatedBaseAdmission() != all {
		t.Fatal("shutdown did not reuse drain")
	}
	releaseA()
	awaitDedicatedDrain(t, closedA)
	assertDedicatedDrainOpen(t, all)
	if _, err := r.admitOwner(t.Context(), "new-graph", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("shutdown admitted new graph: %v", err)
	}
	releaseB()
	awaitDedicatedDrain(t, all)
}

func TestDedicatedBaseRuntimeCanceledGateWaitRemainsOwnedUntilRelease(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	unlock, err := r.acquire(t.Context(), "graph")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admitted := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		release, err := r.admitOwner(ctx, "graph", owner)
		if err != nil {
			finished <- err
			return
		}
		defer release()
		close(admitted)
		gateRelease, err := r.acquire(ctx, "graph")
		if gateRelease != nil {
			gateRelease()
		}
		finished <- err
	}()
	awaitDedicatedDrain(t, admitted)
	drained, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	assertDedicatedDrainOpen(t, drained)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued actor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued actor ignored cancellation")
	}
	awaitDedicatedDrain(t, drained)
	unlock()
}

func TestDedicatedBaseRuntimeConcurrentOwnerDrain(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	const actors = 64
	var admitted, finished sync.WaitGroup
	admitted.Add(actors)
	finished.Add(actors)
	allowRelease := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(allowRelease) })
	t.Cleanup(func() { unblock(); finished.Wait() })
	for i := 0; i < actors; i++ {
		go func() {
			defer finished.Done()
			release, err := r.admitOwner(t.Context(), "graph", owner)
			if err != nil {
				t.Error(err)
				admitted.Done()
				return
			}
			defer release()
			admitted.Done()
			<-allowRelease
		}()
	}
	admitted.Wait()
	drained, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	assertDedicatedDrainOpen(t, drained)
	unblock()
	finished.Wait()
	awaitDedicatedDrain(t, drained)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admittedActors != 0 || r.ownerAdmissions["graph"].active != 0 {
		t.Fatal("actor accounting did not drain")
	}
}

func TestDedicatedBaseRuntimeClosedPublisherSkipsObserverAndCatalogWrites(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	r := &dedicatedBaseRuntime{store: b.Store}
	publisher := &dedicatedBasePublisher{runtime: r, authority: claim.Desire.Authority}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	drained, err := r.CloseDedicatedBaseOwner(claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, drained)
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.ensureInitial(ctx, func(context.Context) (dedicatedBaseObservation, error) {
		t.Fatal("closed publisher invoked observer")
		return dedicatedBaseObservation{}, nil
	})
	if !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("publisher refusal: %v", err)
	}
	_, err = r.install(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: claim.Desire.Authority.GraphID, Owner: claim.Desire.Authority.Owner, Token: "must-not-install",
	})
	if !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("installation refusal: %v", err)
	}
	if err := check(); err != nil {
		t.Fatalf("closed runtime wrote catalog: %v", err)
	}
}

func TestDedicatedBaseRuntimeHeldPublisherCannotReopenRetrackedOwner(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	r := &dedicatedBaseRuntime{store: b.Store}
	graphID, owner := claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner
	if err := r.RegisterDedicatedBaseOwner(graphID, owner); err != nil {
		t.Fatal(err)
	}
	release, oldSlot, err := r.admitOwnerState(t.Context(), graphID, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	publisher := &dedicatedBasePublisher{runtime: r, authority: claim.Desire.Authority, admission: oldSlot}
	drained, err := r.CloseDedicatedBaseOwner(graphID, owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, drained)
	// Deliberately reuse the same graph and owner: comparing only their values
	// would revive this old capability after an explicit retrack.
	if err := r.RegisterDedicatedBaseOwner(graphID, owner); err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(t.Context(), request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.ensureInitial(t.Context(), func(context.Context) (dedicatedBaseObservation, error) {
		t.Fatal("held publisher observed replacement owner's source")
		return dedicatedBaseObservation{}, nil
	})
	if !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("held publisher was not revoked: %v", err)
	}
	if err := check(); err != nil {
		t.Fatalf("held publisher wrote replacement owner's catalog: %v", err)
	}
}

func BenchmarkDedicatedBaseRuntimeOwnerAdmission(b *testing.B) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	// Measure the confirmed live-owner path, not rejected-install slot allocation.
	if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	release, err := r.admitOwner(ctx, "graph", owner)
	if err != nil {
		b.Fatal(err)
	}
	release()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release, err := r.admitOwner(ctx, "graph", owner)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}

// Failed, unvalidated installation is not a durable owner registration. The
// confirmed healthy slot is an independent positive control and must survive.
func TestDedicatedBaseRuntimeInvalidInstallsDoNotRetainSlots(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	r := &dedicatedBaseRuntime{store: b.Store}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	authority := claim.Desire.Authority
	valid := store_sqlite.AcquireDedicatedBaseAuthorityRequest{GraphID: authority.GraphID, Owner: authority.Owner, Token: authority.Token}
	healthy, err := r.install(ctx, valid)
	if err != nil || healthy == nil || healthy.admission == nil || healthy.authority != authority {
		t.Fatalf("healthy install control: publisher=%+v err=%v", healthy, err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 128; n++ {
		invalid := valid
		invalid.GraphID = fmt.Sprintf("private-missing-owner-%d", n)
		publisher, err := r.install(ctx, invalid)
		if publisher != nil || !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			t.Fatalf("missing graph install was not rejected: publisher=%+v err=%v", publisher, err)
		}
		r.mu.Lock()
		slots, actors, gates := len(r.ownerAdmissions), r.admittedActors, len(r.gates)
		current := r.ownerAdmissions[authority.GraphID]
		preserved := current == healthy.admission && current.owner == authority.Owner && current.active == 0 && !current.closing
		r.mu.Unlock()
		if slots != 1 || actors != 0 || gates != 0 || !preserved {
			t.Fatalf("failed install retained state or damaged healthy owner: n=%d slots=%d actors=%d gates=%d preserved=%v", n, slots, actors, gates, preserved)
		}
	}
	if err := check(); err != nil {
		t.Fatalf("invalid installs changed catalog: %v", err)
	}
}

// Done is evaluated by acquire's waiting select; its prior Err checks use the
// embedded Context. Holding the graph gate makes this a deterministic barrier
// after owner admission but before either authority-validation transaction.
type dedicatedDrainInstallWaitContext struct {
	context.Context
	queued chan struct{}
	once   sync.Once
}

func (c *dedicatedDrainInstallWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Done()
}

func TestDedicatedBaseRuntimeConcurrentInvalidAndValidInstallPreservesSlot(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	authority := claim.Desire.Authority
	valid := store_sqlite.AcquireDedicatedBaseAuthorityRequest{GraphID: authority.GraphID, Owner: authority.Owner, Token: authority.Token}
	// Establish catalog fixture validity independently of the empty test runtime.
	control := &dedicatedBaseRuntime{store: b.Store}
	if publisher, err := control.install(ctx, valid); err != nil || publisher == nil || publisher.authority != authority {
		t.Fatalf("healthy authority control: publisher=%+v err=%v", publisher, err)
	}
	r := &dedicatedBaseRuntime{store: b.Store}
	unlock, err := r.acquire(ctx, authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); unlock(); workers.Wait() })
	validCtx := &dedicatedDrainInstallWaitContext{Context: ctx, queued: make(chan struct{})}
	invalidCtx := &dedicatedDrainInstallWaitContext{Context: ctx, queued: make(chan struct{})}
	invalid := valid
	invalid.Owner.Incarnation += "-invalid"
	type outcome struct {
		valid     bool
		publisher *dedicatedBasePublisher
		err       error
	}
	results := make(chan outcome, 2)
	workers.Add(2)
	go func() {
		defer workers.Done()
		publisher, err := r.install(validCtx, valid)
		results <- outcome{valid: true, publisher: publisher, err: err}
	}()
	go func() {
		defer workers.Done()
		publisher, err := r.install(invalidCtx, invalid)
		results <- outcome{publisher: publisher, err: err}
	}()
	awaitDedicatedDrain(t, validCtx.queued)
	awaitDedicatedDrain(t, invalidCtx.queued)
	r.mu.Lock()
	shared := r.ownerAdmissions[authority.GraphID]
	counted := shared != nil && shared.owner == (store_sqlite.DedicatedBaseOwner{}) && shared.active == 2 && r.admittedActors == 2 && len(r.ownerAdmissions) == 1
	r.mu.Unlock()
	if !counted {
		t.Fatal("concurrent installs did not share an unconfirmed counted slot")
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	workers.Wait()
	var installed *dedicatedBasePublisher
	for n := 0; n < 2; n++ {
		result := <-results
		if result.valid {
			if result.err != nil || result.publisher == nil || result.publisher.authority != authority {
				t.Fatalf("valid concurrent install failed: %+v", result)
			}
			installed = result.publisher
		} else if result.publisher != nil || !errors.Is(result.err, store_sqlite.ErrCatalogStaleGuard) {
			t.Fatalf("invalid concurrent install accepted: %+v", result)
		}
	}
	r.mu.Lock()
	current := r.ownerAdmissions[authority.GraphID]
	preserved := current == shared && current != nil && current == installed.admission && current.owner == authority.Owner && current.active == 0 && !current.closing && r.admittedActors == 0 && len(r.ownerAdmissions) == 1 && len(r.gates) == 0
	r.mu.Unlock()
	if !preserved {
		t.Fatal("invalid actor release detached or changed the confirmed valid slot")
	}
	release, exact, err := r.admitOwnerState(ctx, authority.GraphID, authority.Owner, installed.admission)
	if err != nil {
		t.Fatalf("installed publisher capability was revoked by invalid actor: %v", err)
	}
	release()
	if exact != shared {
		t.Fatal("concurrent installation changed captured admission identity")
	}
	if err := check(); err != nil {
		t.Fatalf("same-token valid replay or invalid actor changed catalog: %v", err)
	}
}
