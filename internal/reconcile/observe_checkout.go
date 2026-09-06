package reconcile

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ObserveCheckout records only the selected working copy in an already known
// family. Unlike ReconcileFamily, it never advances other checkouts' absence
// clocks or runs a teardown saga. First-request discovery must not turn into
// administrative cleanup or a full-family indexing pass.
// sampled may carry the caller's just-read inventory; it is validated against
// the catalog exactly like an inventory read here, and never retained. This
// avoids a second whole-family Git probe in the first-request admission path.
func (r *Reconciler) ObserveCheckout(ctx context.Context, familyID, root string, sampled ...*gitstate.FamilyInventory) (CheckoutReport, error) {
	family, found, err := r.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil {
		return CheckoutReport{}, err
	}
	if !found {
		return CheckoutReport{}, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, familyID)
	}
	var inv *gitstate.FamilyInventory
	if len(sampled) == 0 {
		inv, err = r.inventory(ctx, root)
	} else if len(sampled) == 1 {
		inv = sampled[0]
	} else {
		return CheckoutReport{}, fmt.Errorf("observe checkout accepts at most one inventory")
	}
	if err := ValidateInventory(inv, err, family.CommonDirIdentity); err != nil {
		return CheckoutReport{}, err
	}
	var selected *gitstate.WorktreeRecord
	for i := range inv.Records {
		record := &inv.Records[i]
		if pathkey.EqualPaths(pathkey.CanonicalExistingRoot(record.Path), pathkey.CanonicalExistingRoot(root)) {
			selected = record
			break
		}
	}
	if selected == nil || selected.Bare {
		return CheckoutReport{}, fmt.Errorf("%w: selected path is not a working copy in this family", store_sqlite.ErrCatalogNotFound)
	}
	if entry, found, err := r.observedCheckout(ctx, familyID, selected); err != nil || found {
		return entry, err
	}
	graphs, err := r.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return CheckoutReport{}, err
	}
	pass := &familyPass{family: family, inventory: inv, now: r.now()}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			pass.primaryGraphID = graph.GraphID
			break
		}
	}
	entry, err := r.observeNew(ctx, pass, selected)
	if err != nil {
		return entry, err
	}
	if entry.Action == ActionGuardLost {
		// A simultaneous topology event may have won the guarded allocation.
		// Adopt only that same admin/root identity; never allocate a duplicate.
		if current, found, err := r.observedCheckout(ctx, familyID, selected); err != nil || found {
			return current, err
		}
	}
	return entry, nil
}

func (r *Reconciler) observedCheckout(ctx context.Context, familyID string, record *gitstate.WorktreeRecord) (CheckoutReport, bool, error) {
	known, err := r.catalog.ListCheckouts(ctx, familyID)
	if err != nil {
		return CheckoutReport{}, false, err
	}
	for _, checkout := range known {
		if checkout.AdminName != record.AdminName {
			continue
		}
		if !pathkey.EqualPaths(pathkey.CanonicalExistingRoot(checkout.RootPath), pathkey.CanonicalExistingRoot(record.Path)) {
			return CheckoutReport{}, false, fmt.Errorf("%w: checkout admin name moved; wait for topology reconciliation", store_sqlite.ErrCatalogStaleGuard)
		}
		return CheckoutReport{CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation, AdminName: checkout.AdminName, RootPath: checkout.RootPath, Main: record.IsMain, Durable: true, State: checkout.State, Action: ActionObserved}, true, nil
	}
	return CheckoutReport{}, false, nil
}
