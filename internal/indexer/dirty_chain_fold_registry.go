package indexer

import (
	"context"
	"slices"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
)

// The contract registry across a fold.
//
// A chained edit files its final registry under the stack of the generation
// it publishes (editDeltaRegistryCarry), so the next edit finds it. A fold
// publishes a generation no delta built: the first edit over it would reload
// the registry from the view (951 ms on the daemon's repository). The fold
// serves exactly the view of the chain it replaced, so the registry filed for
// the chain's top is the fold's: handOverFoldRegistry files it again under the
// stack the fold makes, the layers above the fold included.

// handOverFoldRegistry files the registry kept for the stack
// commit·below·folded·above under commit·below·fold·above. below and above
// are the chain members under and over the folded ones, oldest first. It
// reports whether a registry was handed over.
func (c *CheckoutCoordinator) handOverFoldRegistry(ctx context.Context, commitGeneration int64, below, folded []int64, fold int64, above []int64) bool {
	if c == nil || c.repoPrefix == "" || fold <= 0 || len(folded) == 0 {
		return false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil || !found || !servableGeneration(row.State) {
		return false
	}
	view, err := c.baseViewMaterializer().MaterializeRefView(ctx, row.GraphID, commitGeneration)
	if err != nil {
		return false
	}
	base, _ := c.ancestryLayerBase(view).(commitLayerBase)
	view.Close()
	if len(base.stack) == 0 {
		// A commit view over the mutable base corpus keys no registry.
		return false
	}
	if !handOverFoldRegistryStack(c.store, c.repoPrefix, c.workspaceID, c.projectID, base.stack, below, folded, fold, above) {
		c.logger.Debug("checkout coordinator: no registry kept for the folded chain",
			zap.String("checkout", c.checkoutID), zap.Int64("folded_generation", fold))
		return false
	}
	return true
}

// handOverFoldRegistryStack files the registry kept for the stack
// commitStack·below·folded·above under commitStack·below·fold·above.
func handOverFoldRegistryStack(store any, repoPrefix, workspaceID, projectID string, commitStack, below, folded []int64, fold int64, above []int64) bool {
	stack := func(middle ...int64) commitLayerBase {
		s := slices.Clone(commitStack)
		s = append(s, below...)
		s = append(s, middle...)
		s = append(s, above...)
		return commitLayerBase{stack: s}
	}
	from, ok := editDeltaRegistryKey(stack(folded...), store, repoPrefix, workspaceID, projectID)
	if !ok {
		return false
	}
	to, ok := editDeltaRegistryKey(stack(fold), store, repoPrefix, workspaceID, projectID)
	if !ok {
		return false
	}
	list, ok := keptEditDeltaContractRegistry(from)
	if !ok {
		return false
	}
	storeEditDeltaContractRegistry(to, copyContracts(list))
	return true
}

// keptEditDeltaContractRegistry is the registry kept under key, if any.
func keptEditDeltaContractRegistry(key string) ([]contracts.Contract, bool) {
	editDeltaContractCache.Lock()
	defer editDeltaContractCache.Unlock()
	for i := len(editDeltaContractCache.entries) - 1; i >= 0; i-- {
		if editDeltaContractCache.entries[i].key == key {
			return editDeltaContractCache.entries[i].contracts, true
		}
	}
	return nil, false
}
