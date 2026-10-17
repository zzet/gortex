package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// destinationProbeBackend uses the actual Store fold. The first successful
// Begin is released and refused to reproduce the same-destination retry gap.
// The second Release probes the published-but-not-yet-landed handoff.
type destinationProbeBackend struct {
	storeChainFoldBackend
	calls                            int
	to                               int64
	retryProtected, handoffProtected bool
}

func (b *destinationProbeBackend) protected(ctx context.Context, to int64) error {
	if !b.store.PayloadBuildFlightActive(to) {
		return fmt.Errorf("fold destination %d lost its physical owner", to)
	}
	_, err := b.store.RetirePayloadGenerationQuantum(ctx, to, nil)
	if !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		return fmt.Errorf("fold destination %d admitted retirement during handoff: %v", to, err)
	}
	return nil
}

func (b *destinationProbeBackend) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	b.calls++
	b.to = to
	fold, err := b.storeChainFoldBackend.BeginChainFold(ctx, chain, to, owner)
	if err != nil {
		return nil, err
	}
	if b.calls == 1 {
		if err := fold.Release(ctx); err != nil {
			return nil, err
		}
		if err := b.protected(ctx, to); err != nil {
			return nil, err
		}
		b.retryProtected = true
		return nil, store_sqlite.ErrChainFoldYielded
	}
	return destinationProbeFold{chainFoldSteps: fold, backend: b}, nil
}

type destinationProbeFold struct {
	chainFoldSteps
	backend *destinationProbeBackend
}

func (f destinationProbeFold) Release(ctx context.Context) error {
	if err := f.chainFoldSteps.Release(ctx); err != nil {
		return err
	}
	row, found, err := f.backend.store.Catalog().GetViewGeneration(ctx, f.backend.to)
	if err != nil || !found {
		return fmt.Errorf("fold destination lookup: found=%t err=%v", found, err)
	}
	if row.State == store_sqlite.ViewGenerationReady {
		if err := f.backend.protected(ctx, f.backend.to); err != nil {
			return err
		}
		f.backend.handoffProtected = true
	}
	return nil
}

func TestSteppedFoldDestinationOwnedAcrossRetryAndLandingWithoutLeases(t *testing.T) {
	f, c, trigger := destinationWithoutLeasesFixture(t)
	probe := &destinationProbeBackend{storeChainFoldBackend: storeChainFoldBackend{store: f.store}}
	c.compaction.backend = probe
	report := c.compactDirtyChain(t.Context(), trigger)
	if report.Err != nil || report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("compaction failed: %+v", report)
	}
	if probe.calls != 2 || !probe.retryProtected || !probe.handoffProtected {
		t.Fatalf("destination lifecycle calls=%d retry=%t handoff=%t", probe.calls, probe.retryProtected, probe.handoffProtected)
	}
	if f.store.PayloadBuildFlightActive(probe.to) {
		t.Fatal("completed landing leaked destination ownership")
	}
	if !c.retirementInUse(probe.to) {
		t.Fatal("landed destination did not transfer to retained/routed ownership")
	}
}

func TestSteppedFoldDestinationCancelledBeginAbandonsWithoutLeases(t *testing.T) {
	f, c, trigger := destinationWithoutLeasesFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &cancelDestinationBackend{storeChainFoldBackend: storeChainFoldBackend{store: f.store}, cancel: cancel}
	c.compaction.backend = probe
	report := c.compactDirtyChain(ctx, trigger)
	if !report.Canceled || probe.to == 0 {
		t.Fatalf("expected cancellation after output admission: %+v to=%d", report, probe.to)
	}
	if f.store.PayloadBuildFlightActive(probe.to) {
		t.Fatal("cancelled fold leaked destination owner")
	}
	row, found, err := f.catalog.GetViewGeneration(t.Context(), probe.to)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("cancelled output not abandoned: found=%t row=%+v err=%v", found, row, err)
	}
}

func destinationWithoutLeasesFixture(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, CheckoutCycle) {
	t.Helper()
	f := newCoordinatorFixtureWithTree(t, builderTreeA())
	gate := NewViewBuildGate()
	gate.Open()
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Gate: gate})
	// Join the fixture actor before manual cycles. Initial materialization
	// still uses its required lease manager; only the destination compaction
	// under test omits that optional coordinator manager.
	c.compaction.quiet = -1
	if out := c.reconcile(t.Context()); out.Err != nil {
		t.Fatalf("initial cycle: %+v", out)
	}
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		foldWiringEdits[i](t, f)
		trigger = c.reconcile(t.Context())
		if trigger.Err != nil {
			t.Fatalf("chain edit %d: %+v", i, trigger)
		}
	}
	if trigger.DirtyChainDepth != 4 {
		t.Fatalf("fixture chain depth=%d", trigger.DirtyChainDepth)
	}
	c.leases = nil
	return f, c, trigger
}

func TestSteppedFoldDestinationForeignOwnershipIsNotAbandoned(t *testing.T) {
	for _, alreadyReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "follower", true: "ready"}[alreadyReady], func(t *testing.T) {
			f, c, trigger := destinationWithoutLeasesFixture(t)
			commit, found, err := f.catalog.GetViewGeneration(t.Context(), trigger.CommitGenerationID)
			if err != nil || !found {
				t.Fatalf("commit lookup: found=%t err=%v", found, err)
			}
			destination := &foldDestinationOwnership{}
			ctx := context.WithValue(t.Context(), foldDestinationOwnershipKey{}, destination)
			var foreign *store_sqlite.PayloadBuildFlight
			var to int64
			t.Cleanup(func() { foreign.Complete(nil) })
			copier := func(ctx context.Context, chain []int64, id int64) (store_sqlite.GenerationCopyCounts, func(context.Context, bool), error) {
				to = id
				if alreadyReady {
					if err := f.catalog.SetViewGenerationState(ctx, id, store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationBuilding); err != nil {
						return store_sqlite.GenerationCopyCounts{}, nil, err
					}
				} else {
					var leader bool
					foreign, leader, _, err = f.store.JoinPayloadBuildFlight(ctx, id, false)
					if err != nil || !leader {
						return store_sqlite.GenerationCopyCounts{}, nil, fmt.Errorf("foreign admission: leader=%t err=%v", leader, err)
					}
				}
				return c.copyChainInSteps(ctx, chain, id)
			}
			_, err = c.flattenDirtyChainOver(ctx, commit, commit, trigger.DirtyGenerationID, copier)
			if !errors.Is(err, errFoldDestinationNotOwned) {
				t.Fatalf("expected ownership refusal, got %v", err)
			}
			if destination.flight != nil {
				t.Fatal("attempt claimed a foreign owner")
			}
			row, found, err := f.catalog.GetViewGeneration(t.Context(), to)
			want := store_sqlite.ViewGenerationBuilding
			if alreadyReady {
				want = store_sqlite.ViewGenerationReady
			}
			if err != nil || !found || row.State != want {
				t.Fatalf("foreign output was abandoned: found=%t state=%s want=%s err=%v", found, row.State, want, err)
			}
			if !alreadyReady && !f.store.PayloadBuildFlightActive(to) {
				t.Fatal("foreign leader was completed")
			}
		})
	}
}

type cancelDestinationBackend struct {
	storeChainFoldBackend
	cancel context.CancelFunc
	to     int64
}

func (b *cancelDestinationBackend) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	b.to = to
	if !b.store.PayloadBuildFlightActive(to) {
		return nil, fmt.Errorf("destination has no owner before Begin")
	}
	b.cancel()
	return b.storeChainFoldBackend.BeginChainFold(ctx, chain, to, owner)
}
