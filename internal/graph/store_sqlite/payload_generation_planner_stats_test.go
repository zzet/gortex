package store_sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Publishing a view generation is the third runtime path that grows the
// physical store, and the only one that adds a whole checkout's payload in one
// step — the lead's store reached 1.69M nodes across 16 generations while its
// statistics still described the 592k of the cold load that created it.
//
// The boundary is at the END of PublishAndRoute, after the route flip, and not
// at the publish tail. Between the publish and the flip the generation is
// ready but unrouted, and a tens-of-seconds ANALYZE holding the write gate
// inside that window would widen a documented transient state and stall the
// checkout coordinator waiting behind it.
//
// The refresh is SCHEDULED at that boundary, not run there: sqlite_stat1 is one
// table for one database file, so the maintenance lane owns it and no
// generation's publish is charged for it. This case therefore settles the lane
// before reading the health back — that the publish still owes the refresh is
// the half of the contract it pins, and TestPublishAndRoute_PaysNoMaintenanceInsideItsWindow
// pins the other half.
//
// Completing at all is half the assertion: PublishAndRoute drains the payload
// writers by taking and releasing the write gate, and the checker takes that
// same non-reentrant gate. A checker placed anywhere that still holds it
// deadlocks instead of failing.
func TestPublishAndRoute_RefreshesPlannerStats(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	// Statistics that describe the store as it is before the generation.
	seedNamedGoReceiverFixture(store, "a", 100)
	writeIndexStateCounters(t, store, 0, "repo", 200, 100)
	refreshStatsNow(t, store)
	before := mustHealth(t, store)
	if before.Stale {
		t.Fatalf("fixture started stale: %s", before.Reason)
	}

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	// The generation's own counter row. It is written at the generation's
	// view_gen, which is exactly the row a view-scoped total would not see.
	if err := handle.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repo", NodeCount: 400, EdgeCount: 200,
	}); err != nil {
		t.Fatalf("SetRepoIndexState on the generation handle: %v", err)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}

	settleMaintenanceLane(t, store)
	after := mustHealth(t, store)
	if after.Refreshes <= before.Refreshes {
		t.Fatalf("publishing a generation that tripled the store refreshed nothing (%d -> %d refreshes); stale=%v reason=%q",
			before.Refreshes, after.Refreshes, after.Stale, after.Reason)
	}
	if !strings.HasPrefix(after.LastRefreshReason, "growth:") {
		t.Errorf("last refresh reason = %q, want the growth verdict the publish should have acted on", after.LastRefreshReason)
	}
	if after.Nodes.Actual != 600 {
		t.Errorf("counter sum = %d, want the 600 summed across the base and the published generation", after.Nodes.Actual)
	}
}
