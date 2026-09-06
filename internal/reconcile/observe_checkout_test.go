package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestObserveCheckoutOnlyAllocatesSelectedAutomaticIdentity(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("known-gone", "inc-gone", "gone", store_sqlite.CheckoutModeAutomatic)
	before := f.checkouts()[0]
	f.git.setRecords(presentRecord("selected", "/repo/selected"), presentRecord("unselected", "/repo/unselected"))
	f.git.samples["/repo/selected"] = gitSampleExisting(volumeA)
	entry, err := f.rec.ObserveCheckout(t.Context(), before.FamilyID, "/repo/selected")
	if err != nil || !entry.Durable || entry.Action != ActionIdentityAllocated {
		t.Fatalf("observe: %+v %v", entry, err)
	}
	rows := f.checkouts()
	if len(rows) != 2 {
		t.Fatalf("observed unselected checkout: %+v", rows)
	}
	for _, row := range rows {
		if row.CheckoutID == before.CheckoutID {
			if row != before {
				t.Fatalf("observation advanced unrelated absence clocks: before=%+v after=%+v", before, row)
			}
		} else if row.CheckoutID != entry.CheckoutID || row.EffectiveMode != store_sqlite.CheckoutModeAutomatic || row.DesiredMode != store_sqlite.CheckoutModeAutomatic {
			t.Fatalf("not an automatic selected identity: %+v", row)
		}
	}
	again, err := f.rec.ObserveCheckout(t.Context(), before.FamilyID, "/repo/selected")
	if err != nil || again.CheckoutID != entry.CheckoutID || again.Incarnation != entry.Incarnation || len(f.checkouts()) != 2 {
		t.Fatalf("duplicate observation: %+v %v", again, err)
	}
}

func TestObserveCheckoutRequiresKnownMatchingFamilyAndPrimary(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("known", "inc-known", "known", store_sqlite.CheckoutModeAutomatic)
	familyID := f.checkouts()[0].FamilyID
	f.git.setRecords(presentRecord("selected", "/repo/selected"))
	f.git.samples["/repo/selected"] = gitSampleExisting(volumeA)
	if _, err := f.rec.ObserveCheckout(t.Context(), "unknown-family", "/repo/selected"); !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		t.Fatalf("unknown family: %v", err)
	}
	entry, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/selected")
	if err != nil || entry.Durable || len(f.checkouts()) != 1 {
		t.Fatalf("allocated without primary: %+v %v", entry, err)
	}
	f.seedPrimaryGraph("graph-primary")
	if _, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/not-listed"); err == nil {
		t.Fatal("accepted root Git never listed")
	}
	inventory := f.rec.inventory
	f.rec.inventory = func(ctx context.Context, root string) (*gitstate.FamilyInventory, error) {
		inv, err := inventory(ctx, root)
		if err == nil {
			copy := *inv
			copy.CommonDir = "/different-family/.git"
			inv = &copy
		}
		return inv, err
	}
	if _, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/selected"); err == nil || len(f.checkouts()) != 1 {
		t.Fatalf("accepted foreign inventory: %v", err)
	}
}

func TestObserveCheckoutAdoptsGuardedAllocationWinner(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("known", "inc-known", "known", store_sqlite.CheckoutModeAutomatic)
	familyID := f.checkouts()[0].FamilyID
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
	racing := &racingSampler{root: "/repo/wt", sample: f.git.sample, race: func() { f.seedCheckout("co-rival", "inc-rival", "wt", store_sqlite.CheckoutModeAutomatic) }}
	WithPathSampler(racing.sampleAndRace)(f.rec)
	entry, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/wt")
	if err != nil || entry.CheckoutID != "co-rival" || entry.Incarnation != "inc-rival" || !entry.Durable || len(f.checkouts()) != 2 {
		t.Fatalf("lost winner: %+v %v rows=%+v", entry, err, f.checkouts())
	}
}

func TestObserveCheckoutLeavesExistingDedicatedModeUntouched(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("dedicated", "inc-dedicated", "wt", store_sqlite.CheckoutModeDedicated)
	before := f.checkouts()[0]
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	entry, err := f.rec.ObserveCheckout(t.Context(), before.FamilyID, "/repo/wt")
	if err != nil || entry.CheckoutID != before.CheckoutID || f.checkouts()[0] != before {
		t.Fatalf("observation changed explicit intent/mode: %+v %v", entry, err)
	}
}

func TestObserveCheckoutReusesAndValidatesSuppliedInventory(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("known", "inc-known", "known", store_sqlite.CheckoutModeAutomatic)
	familyID := f.checkouts()[0].FamilyID
	f.git.setRecords(presentRecord("selected", "/repo/selected"))
	f.git.samples["/repo/selected"] = gitSampleExisting(volumeA)
	inv, err := f.rec.inventory(t.Context(), "/repo/selected")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.inventory = func(context.Context, string) (*gitstate.FamilyInventory, error) {
		t.Fatal("observer repeated the inventory probe")
		return nil, errors.New("unexpected inventory")
	}
	entry, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/selected", inv)
	if err != nil || !entry.Durable {
		t.Fatalf("supplied inventory: %+v %v", entry, err)
	}
	foreign := *inv
	foreign.CommonDir = "/foreign/.git"
	if _, err := f.rec.ObserveCheckout(t.Context(), familyID, "/repo/selected", &foreign); err == nil {
		t.Fatal("supplied foreign inventory was accepted")
	}
}
