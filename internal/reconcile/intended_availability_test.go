package reconcile

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestIntendedWorktreeExpiryBecomesUnavailable(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, removed := range []bool{false, true} {
			name := "intent"
			if owned {
				name = "dedicated"
			}
			if removed {
				name += "/removed"
			} else {
				name += "/inaccessible"
			}
			t.Run(name, func(t *testing.T) {
				clock := &steppedClock{now: time.Unix(1900000000, 0)}
				f := newFixture(t, Default(), WithClock(clock.Now))
				f.seedPrimaryGraph("base")
				mode := store_sqlite.CheckoutModeAutomatic
				if owned {
					mode = store_sqlite.CheckoutModeDedicated
				}
				f.seedCheckout("wanted", "inc", "work", mode)
				if owned {
					f.seedOwnedGraph("wanted-graph", "wanted")
				} else {
					if err := f.catalog.UpsertTrackingIntent(context.Background(), store_sqlite.TrackingIntent{IntentID: "manual", CheckoutID: "wanted", SourceKind: store_sqlite.IntentSourceManualConfig, SourceLocator: "config", Active: true}); err != nil {
						t.Fatal(err)
					}
				}
				record := presentRecord("work", "/repo/work")
				f.git.setRecords(record)
				f.git.samples[record.Path] = gitSampleExisting(volumeA)
				f.reconcile()
				record.RootAccessible = false
				record.RootErr = fs.ErrNotExist
				f.git.samples[record.Path] = gitSampleAbsent(volumeA)
				if removed {
					f.git.setRecords()
				} else {
					f.git.setRecords(record)
				}
				f.reconcile()
				clock.advance(31 * time.Second)
				f.reconcile()
				row, ok, err := f.catalog.GetCheckout(context.Background(), "wanted")
				if err != nil || !ok || row.State != store_sqlite.CheckoutStateUnavailable {
					t.Fatalf("intended checkout not retained unavailable: %+v found=%v err=%v", row, ok, err)
				}
				for _, call := range f.hooks.snapshot() {
					if strings.HasPrefix(call, "release:") {
						t.Fatalf("intended graph released: %v", f.hooks.snapshot())
					}
				}
			})
		}
	}
}
