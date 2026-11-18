package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The real admitted ticket keeps warmup/WAL demand while a parked cycle owes
// its reply. Its demand must not prevent a stepped copy from committing.
func TestCheckoutTicketDemandKeepsForegroundWorkWhileFoldCommits(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	for i := 0; i < 3; i++ {
		mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	before := f.route()
	ctx, cancel := context.WithCancel(t.Context())
	first, err := l.RequestCheckoutRefresh(ctx, f.checkoutID, f.worktree)
	cancel() // the accepted ticket outlives its requesting RPC
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckoutRefreshPending(t, first)
	assertCheckoutRefreshPending(t, second)
	if !f.store.WriteWanted() {
		t.Fatal("queued tickets lost global write demand")
	}
	if busy, _ := l.foregroundActivity().ForegroundWork(); busy != "refresh_ticket" {
		t.Fatalf("queued ticket foreground work=%q, want refresh_ticket", busy)
	}
	commit, found := f.generation(before.CommitGenerationID)
	if !found {
		t.Fatal("missing selected commit")
	}
	bounded, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	// This uses the same stepped backend and exact verification as the real
	// inline copy; no one-shot or test-only backend bypass is installed.
	built, err := c.flattenDirtyChainOver(bounded, commit, commit, before.DirtyGenerationID, c.copyChainInline())
	if err != nil || built.GenerationID == 0 {
		t.Fatalf("fold while real tickets wait: generation=%d err=%v", built.GenerationID, err)
	}
	if f.route() != before {
		t.Fatal("copying a building fold must not change the served route")
	}
	if !f.store.WriteWanted() {
		t.Fatal("fold consumed pending ticket demand")
	}
	assertCheckoutRefreshPending(t, first)
	assertCheckoutRefreshPending(t, second)
	c.cycle(t.Context())
	for _, ticket := range []*CheckoutRefreshTicket{first, second} {
		result := awaitCheckoutRefresh(t, ticket)
		if result.Err != nil || result.AppliedGeneration != uint64(f.route().DirtyGenerationID) {
			t.Fatalf("coalesced ticket did not finish on the exact served view: %+v", result)
		}
	}
	if f.store.WriteWanted() {
		t.Fatal("coalesced completion retained write demand")
	}
}

// Synchronous Refresh carries no ticket. This control uses EnqueueRefresh and
// the coordinator's real cycle so the inline fold runs while its own long-lived
// announcement remains held. The unchanged three-second inline budget and
// exact before-publish/background checks must permit chaining, not fallback.
func TestAdmittedCheckoutTicketAtTheCapFoldsItsOwnChain(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	mcpChainAtTheCap(t, f, l)
	verified, wrong := inlineFoldsVerified.Load(), inlineFoldMismatches.Load()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	chainBurstEdit(t, f, maxChainWalkDepth)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ticket, err := m.EnqueueRefresh(ctx, filepath.Join(f.worktree, "island.go"))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if !f.store.WriteWanted() {
		t.Fatal("admitted ticket must hold global demand before its inline fold")
	}
	var at CheckoutCycle
	c.cycleDone = func(out CheckoutCycle) { at = out }
	bounded, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	c.cycle(bounded)
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || result.RequestedGeneration != ticket.Ticket.Generation || result.AppliedGeneration != uint64(at.DirtyGenerationID) {
		t.Fatalf("ticket did not publish the exact generation: result=%+v cycle=%+v", result, at)
	}
	if at.DirtyParentGenerationID == 0 || !at.DirtyParentPreferred || at.DirtyChainDepth != 2 {
		t.Fatalf("real ticket failed to chain over its verified inline fold: parent=%d preferred=%t depth=%d reason=%s", at.DirtyParentGenerationID, at.DirtyParentPreferred, at.DirtyChainDepth, at.DirtyChainReason)
	}
	if err := c.waitDirtyChainCompactions(bounded); err != nil {
		t.Fatal(err)
	}
	if inlineFoldsVerified.Load()-verified != 1 || inlineFoldMismatches.Load() != wrong {
		t.Fatal("ticket inline fold did not pass exact background verification")
	}
	if f.route().DirtyGenerationID != at.DirtyGenerationID || f.store.WriteWanted() {
		t.Fatal("ticket result disagrees with the served route or retained global demand")
	}
}
