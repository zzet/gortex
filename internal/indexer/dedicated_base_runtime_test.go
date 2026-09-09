package indexer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func dedicatedRuntimeFixture(t testing.TB) (*dedicatedBaseRuntime, *dedicatedBasePublisher, dedicatedBaseObservation, dedicatedBuilderFixtureRequest) {
	t.Helper()
	builder, request, _ := privateDedicatedBuilderFixture(t)
	runtime := &dedicatedBaseRuntime{store: builder.Store}
	publisher, err := runtime.install(context.Background(), store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: request.Identity.GraphID, Token: "runtime-authority-one",
		Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := dedicatedBaseObservation{
		Identity: store_sqlite.DedicatedBaseIdentity{TreeOID: request.Identity.TreeOID, ConfigHash: request.Identity.ConfigHash,
			ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion},
		RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
		ProvenanceCommitOID: request.Identity.ProvenanceCommitOID, CreatedAt: 1, Builder: *builder,
	}
	return runtime, publisher, observation, request
}

func dedicatedRuntimeObserver(runtime *dedicatedBaseRuntime, publisher *dedicatedBasePublisher, observation dedicatedBaseObservation) func(context.Context) (dedicatedBaseObservation, error) {
	return func(ctx context.Context) (dedicatedBaseObservation, error) {
		graph, found, err := runtime.store.Catalog().GetDedicatedGraph(ctx, publisher.authority.GraphID)
		if err != nil {
			return dedicatedBaseObservation{}, err
		}
		if !found {
			return dedicatedBaseObservation{}, fmt.Errorf("fixture graph absent")
		}
		observation.ExpectedActiveGenerationID = graph.ActiveGenerationID
		return observation, nil
	}
}

func dedicatedRuntimeWait(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatalf("wait for runtime fixture: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestDedicatedBaseRuntimeReadyReplayAndNoWrites(t *testing.T) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var publications atomic.Int32
	observation.PrePublish = func(context.Context, int64) error { publications.Add(1); return nil }
	first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if err != nil || first.Adoption.GenerationID <= 0 || first.Adoption.AlreadyAdopted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	observation.RootPath = "/nonexistent/dedicated-ready-reuse-must-not-open-git"
	for range 4 {
		reused, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
		if err != nil || reused.Adoption.GenerationID != first.Adoption.GenerationID || !reused.Adoption.AlreadyAdopted ||
			!reused.Report.Coalesced || reused.Claim.AttemptToken != first.Claim.AttemptToken || reused.Claim.Desire.Authority != first.Claim.Desire.Authority {
			t.Fatalf("reuse=%+v err=%v", reused, err)
		}
	}
	if publications.Load() != 1 {
		t.Fatalf("physical publication callbacks=%d", publications.Load())
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeAdvanceRequiredPreservesDesire(t *testing.T) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tree", "config", "extractor", "resolver"} {
		t.Run(field, func(t *testing.T) {
			changed := observation
			switch field {
			case "tree":
				changed.Identity.TreeOID = "different-committed-tree"
			case "config":
				changed.Identity.ConfigHash = "different-effective-context"
			case "extractor":
				changed.Identity.ExtractorVersions = "different-extractor"
			case "resolver":
				changed.Identity.ResolverVersion = "different-resolver"
			}
			_, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, changed))
			var advance *dedicatedBaseAdvanceRequiredError
			if !errors.Is(err, errDedicatedBaseAdvanceRequired) || !errors.As(err, &advance) || advance.ActiveGenerationID != first.Adoption.GenerationID {
				t.Fatalf("advance error=%v", err)
			}
		})
	}
	after, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("advance rejection changed desire: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeSupersededPublisherDoesNotObserve(t *testing.T) {
	runtime, old, observation, _ := dedicatedRuntimeFixture(t)
	ctx := context.Background()
	replacement, err := runtime.install(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: old.authority.GraphID, Owner: old.authority.Owner, ExpectedEpoch: old.authority.Epoch,
		ExpectedToken: old.authority.Token, Token: "replacement-runtime-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = old.ensureInitial(ctx, func(context.Context) (dedicatedBaseObservation, error) { called = true; return observation, nil })
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) || called || replacement.authority.Epoch <= old.authority.Epoch {
		t.Fatalf("superseded observer called=%v err=%v", called, err)
	}
}

func TestDedicatedBaseRuntimeFreshBuilderRequired(t *testing.T) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation)); err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	observation.Builder.Registry = nil // A retained builder from the previous call must not rescue this input.
	_, err = publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if !errors.Is(err, errDedicatedBaseRuntimeInput) {
		t.Fatalf("fresh builder error=%v", err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeExternalAdoptionDuringObservation(t *testing.T) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog := runtime.store.Catalog()
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: publisher.authority, Identity: observation.Identity})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "external-attempt", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := observation.Builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: observation.RootPath}); err != nil {
		t.Fatal(err)
	}
	_, err = publisher.ensureInitial(ctx, func(ctx context.Context) (dedicatedBaseObservation, error) {
		// The caller sampled active0. A public external adoption then wins before
		// this callback returns; no private SQL corruption or fake setter is used.
		if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
			return dedicatedBaseObservation{}, err
		}
		return observation, nil
	})
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("external active drift: %v", err)
	}
	p, _, err := catalog.DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil || p.Desire != desire || p.AttemptState != "adopted" {
		t.Fatalf("external adoption lost: %+v %v", p, err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation)); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeCancelAfterReadyBeforeAdoption(t *testing.T) {
	runtime, publisher, observation, _ := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()
	held := make(chan func(), 1)
	observation.PrePublish = func(ctx context.Context, _ int64) error {
		release, err := runtime.acquire(ctx, publisher.authority.GraphID)
		if err == nil {
			held <- release
		}
		return err
	}
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	done := make(chan outcome, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := publisher.ensureInitial(buildCtx, dedicatedRuntimeObserver(runtime, publisher, observation))
		done <- outcome{result, err}
	}()
	defer func() { cancelBuild(); wg.Wait() }()
	var release func()
	select {
	case release = <-held:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer release()
	var generationID int64
	dedicatedRuntimeWait(t, ctx, func() bool {
		p, found, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
		if err != nil || !found {
			return false
		}
		generationID = p.Claim.GenerationID
		row, found, err := runtime.store.Catalog().GetViewGeneration(ctx, generationID)
		return err == nil && found && row.State == store_sqlite.ViewGenerationReady
	})
	cancelBuild()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.result.Claim.GenerationID != generationID {
		t.Fatalf("cancel after ready=%+v %v", got.result, got.err)
	}
	release()
	observation.PrePublish = nil
	observation.RootPath = "/nonexistent/ready-retry"
	retry, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if err != nil || retry.Adoption.GenerationID != generationID || !retry.Report.Coalesced || retry.Adoption.AlreadyAdopted {
		t.Fatalf("guarded first adoption of ready retry=%+v err=%v", retry, err)
	}
}

func TestDedicatedBaseRuntimeCanceledFollowerDoesNotFailClaim(t *testing.T) {
	runtime, publisher, observation, _ := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leaderReady, releaseLeader := make(chan struct{}), make(chan struct{})
	observation.PrePublish = func(ctx context.Context, _ int64) error {
		close(leaderReady)
		select {
		case <-releaseLeader:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
		done <- err
	}()
	defer func() { cancel(); wg.Wait() }()
	select {
	case <-leaderReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	followerCtx, cancelFollower := context.WithCancel(ctx)
	defer cancelFollower()
	observed := make(chan struct{})
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	follower := make(chan outcome, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := publisher.ensureInitial(followerCtx, func(ctx context.Context) (dedicatedBaseObservation, error) {
			value, err := dedicatedRuntimeObserver(runtime, publisher, observation)(ctx)
			close(observed)
			return value, err
		})
		follower <- outcome{result, err}
	}()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Once its observation gate is gone, this call has completed Claim and
	// released the gate before entering/waiting for physical work.
	dedicatedRuntimeWait(t, ctx, func() bool { runtime.mu.Lock(); defer runtime.mu.Unlock(); return len(runtime.gates) == 0 })
	cancelFollower()
	got := <-follower
	if !errors.Is(got.err, context.Canceled) || got.result.Claim.GenerationID <= 0 {
		t.Fatalf("follower=%+v %v", got.result, got.err)
	}
	p, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil || p.AttemptState != "building" || p.Claim.AttemptToken != got.result.Claim.AttemptToken {
		t.Fatalf("follower poisoned claim=%+v %v", p, err)
	}
	close(releaseLeader)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeGateSerializationCancellationAndCleanup(t *testing.T) {
	runtime := &dedicatedBaseRuntime{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := runtime.acquire(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, err := runtime.acquire(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	other()
	waitCtx, cancelWait := context.WithCancel(ctx)
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		acquired, err := runtime.acquire(waitCtx, "one")
		if acquired != nil {
			acquired()
		}
		done <- err
	}()
	defer func() { cancelWait(); wg.Wait() }()
	dedicatedRuntimeWait(t, ctx, func() bool { runtime.mu.Lock(); defer runtime.mu.Unlock(); return runtime.gates["one"].refs == 2 })
	cancelWait()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued waiter=%v", err)
	}
	release()
	release()
	runtime.mu.Lock()
	remaining := len(runtime.gates)
	runtime.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("leaked gates=%d", remaining)
	}
}

func TestDedicatedBaseRuntimeGateConcurrentABA(t *testing.T) {
	runtime := &dedicatedBaseRuntime{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var active, overlaps atomic.Int32
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				release, err := runtime.acquire(ctx, "shared")
				if err != nil {
					errs <- err
					return
				}
				if active.Add(1) != 1 {
					overlaps.Add(1)
				}
				active.Add(-1)
				release()
				release()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if overlaps.Load() != 0 || len(runtime.gates) != 0 {
		t.Fatalf("overlap=%d gates=%d", overlaps.Load(), len(runtime.gates))
	}
}

func TestDedicatedBaseRuntimeOrdersObservationBeforeRecord(t *testing.T) {
	runtime, publisher, observation, _ := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	oldObserving, sampleOld := make(chan struct{}), make(chan struct{})
	oldPhysical, finishOld := make(chan struct{}), make(chan struct{})
	newPhysical, finishNew := make(chan struct{}), make(chan struct{})
	newObserving := make(chan struct{})
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	oldDone, newDone := make(chan outcome, 1), make(chan outcome, 1)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	old := observation
	old.PrePublish = func(ctx context.Context, _ int64) error {
		close(oldPhysical)
		select {
		case <-finishOld:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := publisher.ensureInitial(ctx, func(ctx context.Context) (dedicatedBaseObservation, error) {
			close(oldObserving)
			select {
			case <-sampleOld:
				return old, nil
			case <-ctx.Done():
				return dedicatedBaseObservation{}, ctx.Err()
			}
		})
		oldDone <- outcome{result, err}
	}()
	select {
	case <-oldObserving:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	newer := observation
	newer.Identity.ConfigHash = "newly-observed-effective-context"
	newer.PrePublish = func(ctx context.Context, _ int64) error {
		close(newPhysical)
		select {
		case <-finishNew:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := publisher.ensureInitial(ctx, func(ctx context.Context) (dedicatedBaseObservation, error) {
			close(newObserving)
			// Hold the next observation until the old claim has entered physical
			// work, making the stale completion order deterministic.
			select {
			case <-oldPhysical:
				return newer, nil
			case <-ctx.Done():
				return dedicatedBaseObservation{}, ctx.Err()
			}
		})
		newDone <- outcome{result, err}
	}()
	dedicatedRuntimeWait(t, ctx, func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.gates[publisher.authority.GraphID].refs == 2
	})
	select {
	case <-newObserving:
		t.Fatal("new observer ran outside graph gate")
	default:
	}
	close(sampleOld)
	select {
	case <-oldPhysical:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-newPhysical:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Both physical builds can reach preparation while the old one is blocked:
	// the observation gate is not retained across build/flight waits.
	p, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil || p.Desire.Identity != newer.Identity || p.Desire.Epoch != 2 {
		t.Fatalf("new observation not recorded last: %+v %v", p, err)
	}
	close(finishOld)
	older := <-oldDone
	if !errors.Is(older.err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("old build adopted after newer observation: %+v %v", older.result, older.err)
	}
	close(finishNew)
	newest := <-newDone
	if newest.err != nil || newest.result.Adoption.GenerationID != p.Claim.GenerationID {
		t.Fatalf("newest=%+v %v", newest.result, newest.err)
	}
}

func BenchmarkDedicatedBaseRuntimeReadyReplay(b *testing.B) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(b)
	ctx := context.Background()
	first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if err != nil {
		b.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		b.Fatal(err)
	}
	observation.RootPath = "/nonexistent/benchmark-ready-replay"
	observer := dedicatedRuntimeObserver(runtime, publisher, observation)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		reused, err := publisher.ensureInitial(ctx, observer)
		if err != nil || reused.Adoption.GenerationID != first.Adoption.GenerationID || !reused.Adoption.AlreadyAdopted ||
			!reused.Report.Coalesced || reused.Claim.AttemptToken != first.Claim.AttemptToken {
			b.Fatalf("ready replay=%+v %v", reused, err)
		}
	}
	b.StopTimer()
	if err := check(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkDedicatedBaseRuntimeGate(b *testing.B) {
	b.Run("serial_shared", func(b *testing.B) {
		runtime := &dedicatedBaseRuntime{}
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			release, err := runtime.acquire(ctx, "one")
			if err != nil {
				b.Fatal(err)
			}
			release()
		}
	})
	b.Run("parallel_independent", func(b *testing.B) {
		runtime := &dedicatedBaseRuntime{}
		ctx := context.Background()
		var ids atomic.Int64
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			graphID := fmt.Sprintf("graph-%d", ids.Add(1))
			for pb.Next() {
				release, err := runtime.acquire(ctx, graphID)
				if err != nil {
					b.Error(err)
					return
				}
				release()
			}
		})
	})
}
