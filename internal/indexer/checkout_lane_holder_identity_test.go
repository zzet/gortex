package indexer

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A checkout cycle holding the build lane says which checkout's tree it is
// building and why it runs, and a cycle that waited on it logs both: the
// holder of a slow admission is named by its working tree and reason, not
// only by an identity no log line maps back to a checkout.
func TestTheLaneHolderIsNamedByItsTreeAndReason(t *testing.T) {
	_, c, _ := newCheckoutMutationFixture(t)
	ctx := context.Background()
	builderWriteFile(t, c.root, "helper.go", chainHelperEdit)
	c.Signal("a named change")
	seen := make(chan *ViewBuildLaneHolder, 1)
	c.cycleBarrier = func(context.Context) {
		if h := c.gate.Stats().Holder; h != nil {
			copied := *h
			seen <- &copied
		} else {
			seen <- nil
		}
	}
	done := make(chan CheckoutCycle, 1)
	c.cycleDone = func(out CheckoutCycle) { done <- out }
	go c.cycle(ctx)
	var holder *ViewBuildLaneHolder
	select {
	case holder = <-seen:
	case <-time.After(30 * time.Second):
		t.Fatal("the cycle never reached admission")
	}
	<-done
	if holder == nil || holder.Kind != "checkout_cycle" || holder.Root != c.root || holder.Reason != "a named change" {
		t.Fatalf("the cycle's lane holder = %+v, want its root %q and reason", holder, c.root)
	}

	core, logs := observer.New(zap.InfoLevel)
	c.logger = zap.New(core)
	c.logSlowAdmission("waiting", 0, cycleAdmission{Lane: time.Second, LaneHeldBy: holder})
	entries := logs.FilterMessage("checkout coordinator: slow build admission").All()
	if len(entries) != 1 {
		t.Fatalf("slow admission records = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["lane_holder_root"] != c.root || fields["lane_holder_reason"] != "a named change" {
		t.Fatalf("the slow admission names the holder as %v / %v", fields["lane_holder_root"], fields["lane_holder_reason"])
	}
}

// A cycle that builds its working-tree slot logs one plan record carrying
// the reconcile's steps before the dirty slot (cohort, primary base, head
// sample, route, base recomposition, commit slot) beside the dirty slot's
// own (sample, routed, reuse lookup, parent selection): every read between
// admission and the build is lapped.
func TestTheDirtySlotPlanRecordCarriesEveryPlanStep(t *testing.T) {
	_, c, _ := newCheckoutMutationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	c.logger = zap.New(core)
	builderWriteFile(t, c.root, "helper.go", chainHelperEdit)
	done := make(chan CheckoutCycle, 1)
	c.cycleDone = func(out CheckoutCycle) { done <- out }
	go c.cycle(context.Background())
	select {
	case out := <-done:
		if !out.DirtyBuilt {
			t.Fatalf("the cycle = %+v, want a working-tree build", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the cycle never finished")
	}
	entries := logs.FilterMessage("checkout coordinator: dirty slot plan").All()
	if len(entries) != 1 {
		t.Fatalf("plan records = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	for _, step := range []string{"cohort", "primary_base", "head_sample", "ensure_route", "base_recompose", "commit_slot",
		"sample", "routed", "reuse_lookup", "parent_selection"} {
		if _, ok := fields[step]; !ok {
			t.Fatalf("the plan record lacks %q: %v", step, fields)
		}
	}
}
