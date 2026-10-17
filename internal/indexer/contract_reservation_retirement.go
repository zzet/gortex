package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// DeferContractReservationRetirement transfers only a failed payload's physical
// cleanup to the lifecycle's existing gated worker. The synchronous step keeps
// the reference fence, retired write seal and writer drain; no WAL wait or
// deletion runs on the contract actor's job slot. Durable retiring rows remain
// discoverable even if shutdown stops the worker before its next pass.
func (l *CheckoutLifecycle) DeferContractReservationRetirement(ctx context.Context, catalog *store_sqlite.Store, generation int64, inUse func(int64) bool) error {
	if l == nil || l.store == nil || catalog == nil || l.store.Path() == "" || catalog.Path() != l.store.Path() {
		return fmt.Errorf("%w: reservation retirement needs the lifecycle's exact store", store_sqlite.ErrCatalogInvalidValue)
	}
	owned := func(id int64) bool {
		return (inUse != nil && inUse(id)) || l.deferredRetirementInUse(id)
	}
	if err := catalog.FencePayloadGenerationRetirement(ctx, generation, owned); err != nil {
		return err
	}
	l.coordMu.Lock()
	if l.owed == nil {
		l.owed = make(map[int64]struct{})
	}
	l.owed[generation] = struct{}{}
	l.coordMu.Unlock()
	countRetirementOwed(l.store, generation, viewmetrics.OwedDiscovered)
	notifyDeferredRetirementWork()
	return nil
}
