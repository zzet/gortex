package indexer

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graphview"
)

func TestDedicatedRuntimeDependencyRevisionReadyReplay(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "initial"
		if current {
			name = "current"
		}
		t.Run(name, func(t *testing.T) {
			runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			observation.Identity.DependencyRevision = "runtime-replay-revision-a"
			var publications atomic.Int32
			observation.PrePublish = func(context.Context, int64) error { publications.Add(1); return nil }
			first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
			if err != nil {
				t.Fatal(err)
			}
			row, found, err := runtime.store.Catalog().GetViewGeneration(ctx, first.Adoption.GenerationID)
			if err != nil || !found || row.DependencyRevision != observation.Identity.DependencyRevision {
				t.Fatalf("fixture did not persist the requested dependency identity: %+v found=%v err=%v", row, found, err)
			}
			before, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
			if err != nil {
				t.Fatal(err)
			}
			check, err := installDedicatedWriteAudit(ctx, runtime.store, request.StorePath)
			if err != nil {
				t.Fatal(err)
			}
			observation.RootPath = "/nonexistent/dependency-replay-must-not-reopen-source"
			var leases *graphview.LeaseManager
			if current {
				leases = graphview.NewLeaseManager()
			}
			for range 4 {
				replay, err := publisher.ensureObserved(ctx, dedicatedRuntimeObserver(runtime, publisher, observation), leases)
				if err != nil || replay.Adoption.GenerationID != first.Adoption.GenerationID || !replay.Adoption.AlreadyAdopted || !replay.Report.Coalesced || replay.Claim.AttemptToken != first.Claim.AttemptToken {
					t.Fatalf("unchanged dependency revision was not a ready replay: %+v err=%v", replay, err)
				}
			}
			after, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("ready replay changed publication: before=%+v after=%+v err=%v", before, after, err)
			}
			if publications.Load() != 1 {
				t.Fatalf("ready replay rebuilt physical output %d times", publications.Load())
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDedicatedRuntimeDependencyOnlyChangePreservesInitialGuard(t *testing.T) {
	runtime, publisher, observation, request := dedicatedRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	observation.Identity.DependencyRevision = "runtime-replay-revision-a"
	first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, runtime.store, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := observation
	changed.Identity.DependencyRevision = "runtime-replay-revision-b"
	changed.RootPath = "/nonexistent/dependency-change-must-not-full-rebuild"
	_, err = publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, changed))
	var advance *dedicatedBaseAdvanceRequiredError
	if !errors.Is(err, errDedicatedBaseAdvanceRequired) || !errors.As(err, &advance) || advance.ActiveGenerationID != first.Adoption.GenerationID || advance.Active != observation.Identity || advance.Observed != changed.Identity {
		t.Fatalf("dependency change lost exact active/observed identities: %+v err=%v", advance, err)
	}
	after, _, err := runtime.store.Catalog().DedicatedBasePublication(ctx, publisher.authority.GraphID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected initial advancement changed desire: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkDedicatedRuntimeDependencyRevisionReplay(b *testing.B) {
	for _, revision := range []string{"", "runtime-benchmark-revision"} {
		name := "legacy-empty"
		if revision != "" {
			name = "nonempty-revision"
		}
		b.Run(name, func(b *testing.B) {
			runtime, publisher, observation, request := dedicatedRuntimeFixture(b)
			ctx := context.Background()
			observation.Identity.DependencyRevision = revision
			first, err := publisher.ensureInitial(ctx, dedicatedRuntimeObserver(runtime, publisher, observation))
			if err != nil {
				b.Fatal(err)
			}
			check, err := installDedicatedWriteAudit(ctx, runtime.store, request.StorePath)
			if err != nil {
				b.Fatal(err)
			}
			observation.RootPath = "/nonexistent/dependency-benchmark-replay"
			observe := dedicatedRuntimeObserver(runtime, publisher, observation)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				replayed, err := publisher.ensureInitial(ctx, observe)
				if err != nil || replayed.Adoption.GenerationID != first.Adoption.GenerationID || !replayed.Adoption.AlreadyAdopted || !replayed.Report.Coalesced {
					b.Fatalf("ready replay: %+v err=%v", replayed, err)
				}
			}
			b.StopTimer()
			if err := check(); err != nil {
				b.Fatal(err)
			}
		})
	}
}
