package reconcile

import (
	"context"
	"fmt"
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
