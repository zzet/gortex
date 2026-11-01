package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func retainedDispatchCompletion(t *testing.T, grant *mcpScheduledGrant) func() {
	t.Helper()
	release := grant.retainCompletion()
	if release == nil {
		t.Fatal("live dispatch did not retain its handler completion")
	}
	t.Cleanup(release)
	return release
}

func TestDispatchRetainedHandlersKeepDrainBoundAfterDispatcherReturns(t *testing.T) {
	pool := testDispatchPool(1)
	var releases []func()
	for i := range mcpDispatchDrainLimit + 1 {
		grant := takeScheduled(t, pool, MCPDispatchFast, fmt.Sprint(i))
		releases = append(releases, retainedDispatchCompletion(t, grant))
		grant.complete() // The outer firewall returned; the leaf did not.
	}
	if pool.owner.draining != 4 || len(pool.slots) != 1 || len(pool.owner.blocked) != 1 {
		t.Fatalf("physical work escaped drain bound: draining=%d held=%d blocked=%d", pool.owner.draining, len(pool.slots), len(pool.owner.blocked))
	}
	_, err := pool.acquire(withMCPDispatchQueueWait(t.Context(), 0), MCPDispatchFast, "sixth")
	if !errors.Is(err, errMCPDispatchSaturated) {
		t.Fatalf("full draining allowance lost its capacity charge: %v", err)
	}
	releases[0]() // One leaf exits; the charged fifth is promoted to draining.
	if pool.owner.draining != 4 || len(pool.slots) != 0 || len(pool.owner.blocked) != 0 {
		t.Fatal("leaf completion did not promote the capacity-charged cancellation")
	}
	keeper := takeScheduled(t, pool, MCPDispatchFast, "unrelated")
	releases[0]() // A late duplicate cannot release the keeper or another drain.
	if len(pool.slots) != 1 || pool.owner.draining != 4 {
		t.Fatal("duplicate completion changed another dispatch's ownership")
	}
	for _, release := range releases[1:] {
		release()
	}
	if pool.owner.draining != 0 || len(pool.slots) != 1 {
		t.Fatal("retained completion released unrelated live capacity")
	}
	keeper.complete()
}

func TestDispatchCompletionOwnerSurvivesDetachWithoutAdoptingReattach(t *testing.T) {
	pool := testDispatchPool(1)
	ctx := withMCPDispatchSlotLease(t.Context())
	lease := mcpDispatchSlotLeaseForContext(ctx)
	old := takeScheduled(t, pool, MCPDispatchFast, "old")
	attached := lease.attach(nil)
	attached.scheduled = old
	owned := WithMCPDispatchCompletionOwner(ctx)
	parent := RetainMCPDispatchCompletion(owned)
	if parent == nil {
		t.Fatal("outer firewall did not retain physical work")
	}
	t.Cleanup(parent)
	lease.detach(attached)
	old.complete()
	fresh := takeScheduled(t, pool, MCPDispatchFast, "fresh")
	replacement := lease.attach(nil)
	replacement.scheduled = fresh
	// A nested firewall can enter after its outer dispatch has detached. Its
	// inherited context must still name the old operation, not the new attach.
	nested := RetainMCPDispatchCompletion(WithMCPDispatchCompletionOwner(owned))
	if nested == nil {
		t.Fatal("nested firewall lost its captured physical owner")
	}
	t.Cleanup(nested)
	parent()
	if pool.owner.draining != 1 || len(pool.slots) != 1 {
		t.Fatal("parent return released a nested leaf or the replacement")
	}
	nested()
	if pool.owner.draining != 0 || len(pool.slots) != 1 {
		t.Fatal("nested leaf completion changed the replacement's ownership")
	}
	if RetainMCPDispatchCompletion(owned) != nil {
		t.Fatal("stale owner revived an already completed dispatch")
	}
	lease.detach(replacement)
	fresh.complete()
}

func TestDispatchCompletionOwnerFollowsSlowTransferButNotPassiveRelease(t *testing.T) {
	pool := testDispatchPool(3)
	ctx := withMCPDispatchSlotLease(withMCPDispatchQueueWait(t.Context(), 0))
	lease := mcpDispatchSlotLeaseForContext(ctx)
	mutation := takeScheduled(t, pool, MCPDispatchMutating, "edit")
	attached := lease.attach(nil)
	attached.scheduled = mutation
	owned := WithMCPDispatchCompletionOwner(ctx)
	if err := MoveMCPDispatchToSlowLane(owned, nil); err != nil {
		t.Fatal(err)
	}
	slow := lease.scheduledGrant(attached, mutation)
	t.Cleanup(slow.complete)
	release := RetainMCPDispatchCompletion(owned)
	if release == nil {
		t.Fatal("slow wait lost its physical completion owner")
	}
	t.Cleanup(release)
	slow.complete()
	if pool.draining[MCPDispatchSlow] != 1 || pool.active[MCPDispatchMutating] != 0 {
		t.Fatal("abandoned wait did not retain the transferred slow grant")
	}
	release()
	lease.detach(attached)
	passive := takeScheduled(t, pool, MCPDispatchMutating, "passive")
	attached = lease.attach(nil)
	attached.scheduled = passive
	passiveCtx := WithMCPDispatchCompletionOwner(ctx)
	if !ReleaseMCPDispatchSlot(passiveCtx) {
		t.Fatal("explicit passive release did not return capacity")
	}
	if RetainMCPDispatchCompletion(passiveCtx) != nil || len(pool.slots) != 0 {
		t.Fatal("passive wait revived its released grant")
	}
	if RetainMCPDispatchCompletion(context.Background()) != nil {
		t.Fatal("standalone request acquired daemon completion ownership")
	}
}

func TestDispatchRetainedCompletionConcurrentWithOuterReturn(t *testing.T) {
	pool := testDispatchPool(1)
	grant := takeScheduled(t, pool, MCPDispatchFast, "outer")
	parent := retainedDispatchCompletion(t, grant)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			release := grant.retainCompletion()
			if release == nil {
				t.Error("live parent lost nested completion ownership")
				return
			}
			grant.complete()
			release()
			release()
		})
	}
	group.Wait()
	parent()
	if pool.owner.draining != 0 || len(pool.slots) != 0 {
		t.Fatal("concurrent nested/outer completion leaked capacity")
	}
}
