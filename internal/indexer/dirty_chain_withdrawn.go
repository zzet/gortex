package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A mutation withdraws its route before writing but retains its accepted dirty
// predecessor for the next build. Background compaction may copy that immutable
// predecessor too; it cannot publish a serving route for the newer disk state.
func (c *CheckoutCoordinator) withdrawnCompactionParent(ctx context.Context, route store_sqlite.CheckoutRoute, trigger CheckoutCycle) (int64, func()) {
	release := func() {}
	if ctx.Err() != nil || !steppedChainFoldEnabled || route.State != store_sqlite.RoutePending ||
		route.DirtyGenerationID != 0 || trigger.DirtyGenerationID <= 0 || route.CommitGenerationID != trigger.CommitGenerationID {
		return 0, release
	}
	c.compaction.mu.Lock()
	parent, closed := c.compaction.preferred, c.compaction.closed
	c.compaction.mu.Unlock()
	if closed || parent <= 0 {
		return 0, release
	}
	// Pin before validation: the retained top protects its ancestry, and the
	// explicit lease keeps every copied member alive through the handoff.
	if c.leases != nil {
		members := append(c.dirtyChainMembers(ctx, parent), route.CommitGenerationID)
		pin := c.leases.Acquire(members...)
		release = pin.Release
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found || !servableGeneration(commit.State) || commit.GraphID != route.GraphID {
		return 0, release
	}
	chain, valid, _, err := c.dirtyChainRoot(ctx, parent, commit, maxChainWalkDepth)
	if err != nil || !valid || len(chain) == 0 {
		return 0, release
	}
	current := c.dirtyIdentity(commit.GraphID, commit.GenerationID)
	top := chain[0]
	if top.ConfigHash != current.ConfigHash || top.ExtractorVersions != current.ExtractorVersions ||
		top.ResolverVersion != current.ResolverVersion || top.DependencyRevision != current.DependencyRevision {
		return 0, release
	}
	return parent, release
}

// The verified fold can become the lease's next parent before landing acquires
// cycleMu. Otherwise a source lease holding that lock could refresh on the old
// deep chain while the completed fold waited for the very same lock.
func (c *CheckoutCoordinator) offerWithdrawnCompactionParent(ctx context.Context, source store_sqlite.CheckoutRoute, built dirtyLayerBuild) bool {
	if ctx.Err() != nil {
		return false
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RoutePending || route.DirtyGenerationID != 0 ||
		route.RouteEpoch != source.RouteEpoch || route.CommitGenerationID != source.CommitGenerationID || route.GraphID != source.GraphID {
		return false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, built.GenerationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationReady ||
		row.CheckoutID != c.checkoutID || row.GraphID != source.GraphID || row.BaseGenerationID != source.CommitGenerationID {
		return false
	}
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	if c.compaction.closed || ctx.Err() != nil || c.compaction.preferred != source.DirtyGenerationID {
		return false
	}
	// Retain before publishing the pointer; selection cannot consume it until
	// both are installed. A superseded worker never overwrites a newer parent.
	c.retainDirty(ctx, built.Key, built.GenerationID)
	c.compaction.preferred = built.GenerationID
	return true
}
