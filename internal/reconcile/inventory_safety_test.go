package reconcile

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestInventoryFailureFreezesIntendedPrimaryClocks(t *testing.T) {
	clock := &steppedClock{now: time.Unix(1900000000, 0)}
	f := newFixture(t, Default(), WithClock(clock.Now))
	f.seedCheckout("primary", "inc-primary", gitstate.MainAdminName, store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("primary-graph", "primary")
	f.promoteToPrimary("primary-graph")
	f.git.setRecords(mainWorktreeRecord("/repo/@main"))
	f.git.samples["/repo/@main"] = gitSampleExisting(volumeA)
	f.reconcile()
	before := f.checkout("primary")
	f.git.err = fmt.Errorf("damaged linked worktree: %w", gitstate.ErrInventoryUnavailable)
	f.git.samples["/repo/@main"] = gitSampleAbsent(volumeA)
	f.reconcile()
	clock.advance(31 * time.Second)
	f.reconcile()
	row, exists, err := f.catalog.GetCheckout(context.Background(), "primary")
	if err != nil || !exists {
		t.Fatalf("inventory failure retired intended primary: exists=%v err=%v cleanup=%v", exists, err, f.hooks.snapshot())
	}
	if row.UnavailableSince != before.UnavailableSince || row.AvailabilityDeadline != before.AvailabilityDeadline || row.RemovalDetectedAt != before.RemovalDetectedAt || row.RemovalDeadline != before.RemovalDeadline {
		t.Fatalf("unknown family advanced clocks: before=%+v after=%+v", before, row)
	}
	if calls := f.hooks.snapshot(); len(calls) != 0 {
		t.Fatalf("inventory failure ran cleanup: %v", calls)
	}
	if _, exists, err := f.catalog.GetDedicatedGraph(context.Background(), "primary-graph"); err != nil || !exists {
		t.Fatalf("inventory failure removed primary graph: exists=%v err=%v", exists, err)
	}
}

func TestInventoryFailureUsesFreshGitMarker(t *testing.T) {
	for _, marker := range []bool{false, true} {
		fresh := sampleOnVolume(volumeA)
		fresh.GitMarkerExists = marker
		class := Classify(gitstate.ErrInventoryUnavailable, nil, PathEvidence{}, fresh)
		want := DispositionUnknown
		if marker {
			want = DispositionPresent
		}
		if class.Disposition != want {
			t.Fatalf("marker=%v: disposition=%s want=%s", marker, class.Disposition, want)
		}
	}
}

func TestDeletedFamilyVsUnreadableCommonDir(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "unreadable"
		if deleted {
			name = "deleted-mounted"
		}
		t.Run(name, func(t *testing.T) {
			clock := &steppedClock{now: time.Unix(1900000000, 0)}
			f := newFixture(t, Default(), WithClock(clock.Now))
			f.seedCheckout("primary", "primary-inc", "main", store_sqlite.CheckoutModeDedicated)
			f.seedOwnedGraph("base", "primary")
			f.promoteToPrimary("base")
			f.seedCheckout("automatic", "auto-inc", "work", store_sqlite.CheckoutModeAutomatic)
			f.git.samples[testCommonDir] = gitSampleExisting(volumeA)
			f.git.samples["/repo/main"] = gitSampleExisting(volumeA)
			f.git.samples["/repo/work"] = gitSampleExisting(volumeA)
			f.git.setRecords(presentRecord("main", "/repo/main"), presentRecord("work", "/repo/work"))
			f.reconcile()
			f.git.err = gitstate.ErrInventoryUnavailable
			f.git.samples["/repo/main"] = gitSampleAbsent(volumeA)
			f.git.samples["/repo/work"] = gitSampleAbsent(volumeA)
			f.git.samples[testCommonDir] = gitstate.PathEvidence{}
			if deleted {
				f.git.samples[testCommonDir] = gitSampleAbsent(volumeA)
			}
			f.reconcile()
			clock.advance(31 * time.Second)
			f.reconcile()
			primary := f.checkout("primary")
			want := store_sqlite.CheckoutStateReady
			if deleted {
				want = store_sqlite.CheckoutStateUnavailable
			}
			if primary.State != want {
				t.Fatalf("primary state=%s want=%s", primary.State, want)
			}
			_, auto, err := f.catalog.GetCheckout(context.Background(), "automatic")
			if err != nil || auto == deleted {
				t.Fatalf("deleted=%v automatic exists=%v err=%v", deleted, auto, err)
			}
		})
	}
}

func TestPrimaryRetirementRefusedLogsOnce(t *testing.T) {
	clock := &steppedClock{now: time.Unix(1900000000, 0)}
	core, logs := observer.New(zap.InfoLevel)
	f := newFixture(t, Default(), WithClock(clock.Now), WithLogger(zap.New(core)))
	f.seedCheckout("primary", "inc", "main", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("base", "primary")
	f.promoteToPrimary("base")
	f.reconcile()
	clock.advance(31 * time.Second)
	f.reconcile()
	clock.advance(31 * time.Second)
	f.reconcile()
	warnings := logs.FilterMessage("primary_retirement_refused").All()
	if len(warnings) != 1 || warnings[0].Level != zap.WarnLevel {
		t.Fatalf("primary retirement warnings=%+v", warnings)
	}
	if cause := warnings[0].ContextMap()["cause"]; cause != CauseReconcileRemoval {
		t.Fatalf("refused removal cause=%v", cause)
	}
}
