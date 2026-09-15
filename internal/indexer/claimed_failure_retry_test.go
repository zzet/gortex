package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Uses the actual advancement fixture/API already exercised in Mwd4Pq. It is
// additive to that fixture's candidate/landed tests, not a copied runtime or a
// source replacement overlay. These are proposed RED->GREEN acceptance tests;
// no execution is claimed in this private packet.
func TestClaimedPhysicalFailureReleasesSameDesireForRetry(t *testing.T) {
	for _, kind := range []string{"initial_full", "dedicated_delta"} {
		t.Run(kind, func(t *testing.T) {
			f := newDedicatedAdvanceFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var oldActive int64
			if kind == "dedicated_delta" {
				oldActive = f.ensure(t, ctx).Claim.GenerationID
				f.commitFile(t, "package dedicated\nfunc FailureRetryMarker() int { return 1 }\n")
			}
			want := errors.New("private physical leader failure")
			failed, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
				observation, err := f.observe(t, ctx)
				if err != nil {
					return observation, err
				}
				observation.PrePublish = func(context.Context, int64) error { return want }
				return observation, nil
			})
			if !errors.Is(err, want) || failed.Claim.GenerationID <= 0 || failed.Adoption.GenerationID != 0 {
				t.Fatalf("failure was not physical/no-adoption: %+v err=%v", failed, err)
			}
			catalog := f.builder.Store.Catalog()
			row, found, err := catalog.GetViewGeneration(ctx, failed.Claim.GenerationID)
			if err != nil || !found || row.State != "failed" {
				t.Fatalf("payload terminal state: %+v found=%v err=%v", row, found, err)
			}
			publication, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
			if err != nil || !found || publication.AttemptState != "failed" || publication.Claim.GenerationID != failed.Claim.GenerationID || publication.Claim.AttemptToken != failed.Claim.AttemptToken {
				t.Fatalf("physical failure left a poisoned or unrelated claim: %+v found=%v err=%v", publication, found, err)
			}
			binding, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
			if err != nil || !found || binding.ActiveGenerationID != oldActive {
				t.Fatalf("failed attempt moved old route: %+v found=%v err=%v", binding, found, err)
			}
			if f.builder.Store.PayloadBuildFlightActive(failed.Claim.GenerationID) {
				t.Fatal("failed physical flight retained")
			}
			retry, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) { return f.observe(t, ctx) })
			if err != nil {
				t.Fatalf("same-desire retry stayed poisoned: %v", err)
			}
			if retry.Claim.GenerationID <= failed.Claim.GenerationID || retry.Claim.AttemptToken == failed.Claim.AttemptToken || retry.Claim.Desire != failed.Claim.Desire || retry.Adoption.GenerationID != retry.Claim.GenerationID {
				t.Fatalf("retry did not allocate/adopt under unchanged desire: failed=%+v retry=%+v", failed, retry)
			}
			binding, found, err = catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
			if err != nil || !found || binding.ActiveGenerationID != retry.Claim.GenerationID {
				t.Fatalf("retry did not publish guarded route: %+v found=%v err=%v", binding, found, err)
			}
		})
	}
}

func TestClaimedCanceledPhysicalLeaderMarksAttemptFailed(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := make(chan int64, 1)
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		var finished outcome
		defer func() { done <- finished }()
		result, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
			observation, err := f.observe(t, ctx)
			if err != nil {
				return observation, err
			}
			observation.PrePublish = func(ctx context.Context, id int64) error { started <- id; <-ctx.Done(); return ctx.Err() }
			return observation, nil
		})
		finished = outcome{result, err}
	}()
	// Join before fixture Store.Close, including assertion failures.
	var finished outcome
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			<-done
		}
	})
	var id int64
	select {
	case id = <-started:
	case finished = <-done:
		joined = true
		t.Fatalf("leader did not reach physical stage: %+v", finished)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	finished = <-done
	joined = true
	if !errors.Is(finished.err, context.Canceled) || finished.result.Claim.GenerationID != id || finished.result.Adoption.GenerationID != 0 {
		t.Fatalf("physical cancellation outcome: %+v", finished)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Minute)
	defer checkCancel()
	publication, found, err := f.builder.Store.Catalog().DedicatedBasePublication(checkCtx, f.publisher.authority.GraphID)
	if err != nil || !found || publication.AttemptState != "failed" || publication.Claim.GenerationID != id {
		t.Fatalf("canceled leader skipped uncanceled terminal cleanup: %+v found=%v err=%v", publication, found, err)
	}
	if f.builder.Store.PayloadBuildFlightActive(id) {
		t.Fatal("canceled physical flight retained")
	}
	retry := f.ensure(t, checkCtx)
	if retry.Claim.GenerationID <= id {
		t.Fatalf("canceled-leader retry reused failed payload: %+v", retry)
	}
}

func TestClaimedStaleLeaderFailureCannotFailReplacement(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc SupersededFailureMarker() int { return 1 }\n")
	started := make(chan int64, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	want := errors.New("private stale physical leader failure")
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		var finished outcome
		defer func() { done <- finished }()
		result, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
			observation, err := f.observe(t, ctx)
			if err != nil {
				return observation, err
			}
			observation.PrePublish = func(ctx context.Context, id int64) error {
				started <- id
				select {
				case <-release:
					return want
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return observation, nil
		})
		finished = outcome{result, err}
	}()
	joined := false
	t.Cleanup(func() {
		unblock()
		cancel()
		if !joined {
			<-done
		}
	})
	var staleID int64
	select {
	case staleID = <-started:
	case result := <-done:
		joined = true
		t.Fatalf("stale owner did not reach barrier: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f.commitFile(t, "package dedicated\nfunc ReplacementFailureMarker() int { return 2 }\n")
	replacement := f.ensure(t, ctx)
	if replacement.Claim.GenerationID <= staleID || replacement.Claim.BaseGenerationID != initial.Claim.GenerationID {
		t.Fatalf("replacement fixture did not overtake blocked old claim: %+v", replacement)
	}
	unblock()
	failed := <-done
	joined = true
	if !errors.Is(failed.err, want) || failed.result.Adoption.GenerationID != 0 {
		t.Fatalf("stale failure outcome: %+v", failed)
	}
	catalog := f.builder.Store.Catalog()
	publication, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || publication.AttemptState != "adopted" || publication.Claim.GenerationID != replacement.Claim.GenerationID || publication.Claim.AttemptToken != replacement.Claim.AttemptToken {
		t.Fatalf("stale leader terminal notification clobbered replacement: %+v found=%v err=%v", publication, found, err)
	}
	binding, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || binding.ActiveGenerationID != replacement.Claim.GenerationID {
		t.Fatalf("stale failure moved active pointer: %+v found=%v err=%v", binding, found, err)
	}
	if f.builder.Store.PayloadBuildFlightActive(staleID) {
		t.Fatal("stale failed physical flight retained")
	}
}

func TestClaimedTerminalRowCrashStateReclaimsSameDesire(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	catalog := f.builder.Store.Catalog()
	publication, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || publication.AttemptState != "building" || publication.Claim.GenerationID <= 0 {
		t.Fatalf("initial allocated claim fixture: %+v found=%v err=%v", publication, found, err)
	}
	old := publication.Claim
	if f.builder.Store.PayloadBuildFlightActive(old.GenerationID) {
		t.Fatal("crash fixture unexpectedly owns a live physical flight")
	}
	// A still-BUILDING healthy row is not recovery permission: a fresh caller
	// must coalesce with this exact current association, not steal its payload.
	coalesced, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: old.Desire, ExpectedActiveGenerationID: old.ExpectedActiveGenerationID, AttemptToken: "private-live-claim-control",
		BaseGenerationID: old.BaseGenerationID, LayerID: old.LayerID, LowerViewFingerprint: old.LowerViewFingerprint, CreatedAt: 1,
	})
	if err != nil || coalesced.GenerationID != old.GenerationID || coalesced.AttemptToken != old.AttemptToken || coalesced.Status == "allocated" {
		t.Fatalf("healthy live claim was replaced: %+v err=%v", coalesced, err)
	}
	// Explicit persisted crash-boundary fixture: payload abandonment committed,
	// but the matching publication notification was lost before process exit.
	// No physical worker is active. The public setter shape is verified in the
	// byte-identical authored/landed parent-terminal test packet (1ed4211f...).
	if err := catalog.SetViewGenerationState(ctx, old.GenerationID, store_sqlite.ViewGenerationFailed); err != nil {
		t.Fatal(err)
	}
	poisoned, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || poisoned.AttemptState != "building" || poisoned.Claim.GenerationID != old.GenerationID {
		t.Fatalf("fixture did not isolate lost-notification state: %+v found=%v err=%v", poisoned, found, err)
	}
	retry, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) { return f.observe(t, ctx) })
	if err != nil {
		t.Fatalf("persisted failed-row/current-building attempt remains poisoned: %v", err)
	}
	if retry.Claim.GenerationID <= old.GenerationID || retry.Claim.AttemptToken == old.AttemptToken || retry.Claim.Desire != old.Desire || retry.Adoption.GenerationID != retry.Claim.GenerationID {
		t.Fatalf("recovery did not claim/adopt new payload under same desire: old=%+v retry=%+v", old, retry)
	}
	row, found, err := catalog.GetViewGeneration(ctx, old.GenerationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("recovery resurrected old failed payload: %+v found=%v err=%v", row, found, err)
	}
}
