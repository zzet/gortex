package indexer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A mutation can withdraw a capped chain while its scheduled fold queues.
// Copy the accepted predecessor while the real mutation lease owns cycleMu;
// the route must remain pending until that lease publishes its new disk state.
func TestWithdrawnDirtyChainCompactsBeforeMutationRefresh(t *testing.T) {
	tree := builderTreeA()
	tree["pending.go"] = "package fixture\n\nfunc Pending() {}\n"
	f, c, l := mcpChainFixture(t, tree, false)
	var pending strings.Builder
	pending.WriteString("package fixture\n\n")
	for i := 0; i < 256; i++ {
		fmt.Fprintf(&pending, "func Pending%d() {}\n", i)
	}
	var trigger CheckoutCycle
	for i := 0; i < maxChainWalkDepth; i++ {
		trigger = mcpEdit(t, l, f, func() {
			if i == 0 {
				builderWriteFile(t, f.worktree, "pending.go", pending.String())
			}
			// Short bodies keep deferred clone follow-up outside this core
			// publication fixture; full parity still compares all metadata.
			chainEditIsland(t, f, i)
		})
	}
	if trigger.DirtyChainDepth != maxChainWalkDepth {
		t.Fatalf("depth %d, want the physical cap %d", trigger.DirtyChainDepth, maxChainWalkDepth)
	}
	chainAssertFlat(t, f, "accepted-withdrawn-input")
	accepted, _ := f.generation(trigger.DirtyGenerationID)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	entered, permit, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	unpark := sync.OnceFunc(func() { close(permit) })
	var mutation *CheckoutMutation
	closeMutation := func() {
		if mutation != nil {
			mutation.Close()
			mutation = nil
		}
	}
	var report DirtyChainCompaction
	c.compaction.mu.Lock()
	c.compaction.closed = false
	c.compaction.barrier = func(ctx context.Context) {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-permit:
		case <-ctx.Done():
		}
	}
	c.compaction.mu.Unlock()
	t.Cleanup(func() {
		cancel()
		unpark()
		closeMutation()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("withdrawn compaction worker did not stop")
		}
	})
	go func() {
		report = c.compactDirtyChain(ctx, trigger)
		close(done)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("compaction did not reach admission barrier")
	}
	var err error
	mutation, err = l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	withdrawn := f.route()
	if withdrawn.State != store_sqlite.RoutePending || withdrawn.DirtyGenerationID != 0 {
		t.Fatalf("mutation did not withdraw route: %+v", withdrawn)
	}
	unpark()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("copy waited for the mutation's cycle lock")
	}
	if report.Outcome != dirtyChainCompactionPreferred || report.GenerationID == 0 {
		t.Fatalf("withdrawn copy = %+v, want a verified preferred parent", report)
	}
	if got := f.route(); got != withdrawn {
		t.Fatalf("background copy changed pending route: before %+v after %+v", withdrawn, got)
	}
	fold, found := f.generation(report.GenerationID)
	if !found || fold.State != store_sqlite.ViewGenerationReady || fold.BaseGenerationID != withdrawn.CommitGenerationID ||
		fold.LowerViewFingerprint != accepted.LowerViewFingerprint {
		t.Fatalf("fold lost accepted predecessor identity: %+v", fold)
	}
	requireFullManifest(t, f, fold.GenerationID)

	// Refuse stale handoffs without changing the newly installed parent.
	source := withdrawn
	source.DirtyGenerationID = trigger.DirtyGenerationID
	built := dirtyLayerBuild{GenerationID: fold.GenerationID, Key: logicalDirtyKey(fold, withdrawn.CommitGenerationID)}
	stale := source
	stale.RouteEpoch--
	if c.offerWithdrawnCompactionParent(ctx, stale, built) {
		t.Fatal("stale pending epoch accepted a handoff")
	}
	if c.offerWithdrawnCompactionParent(ctx, source, built) {
		t.Fatal("obsolete source overwrote a newer preferred parent")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if c.offerWithdrawnCompactionParent(canceled, source, built) {
		t.Fatal("canceled handoff accepted")
	}
	c.compaction.mu.Lock()
	preferred := c.compaction.preferred
	c.compaction.closed = true // Only this explicitly joined worker owns the fold.
	c.compaction.mu.Unlock()
	if preferred != fold.GenerationID {
		t.Fatalf("refused handoff changed preferred parent to %d", preferred)
	}

	// The foreground must sample the new bytes against the accepted fold; it
	// cannot certify the copied predecessor as the freshly edited checkout.
	builderWriteFile(t, f.worktree, "island.go", chainIslandTwo)
	out, err := mutation.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh over withdrawn copy: %v (%+v)", err, out)
	}
	closeMutation()
	if out.DirtyParentGenerationID != fold.GenerationID || out.DirtyChainDepth != 2 ||
		out.DirtyWork == nil || out.DirtyWork.PlanChanged != 1 {
		t.Fatalf("one-file refresh rebuilt the accepted frontier: %+v; work %+v", out, out.DirtyWork)
	}
	current, _ := f.generation(out.DirtyGenerationID)
	if current.LowerViewFingerprint == accepted.LowerViewFingerprint {
		t.Fatal("foreground reused predecessor fingerprint after the edit")
	}
	chainAssertBuildsOnFold(t, f, fold.GenerationID)
	chainAssertFlat(t, f, "withdrawn-fold")
}

func TestWithdrawnCompactionParentRejectsChangedAuthority(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < dirtyChainCompactionDepth; i++ {
		trigger = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	ctx := t.Context()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	route := f.route()
	c.compaction.mu.Lock()
	c.compaction.closed = false
	c.compaction.mu.Unlock()
	parent, release := c.withdrawnCompactionParent(ctx, route, trigger)
	release()
	if parent != trigger.DirtyGenerationID {
		t.Fatalf("valid accepted parent = %d, want %d", parent, trigger.DirtyGenerationID)
	}
	cases := []struct {
		name   string
		change func(*store_sqlite.CheckoutRoute)
	}{
		{"graph", func(r *store_sqlite.CheckoutRoute) { r.GraphID = "another-graph" }},
		{"commit", func(r *store_sqlite.CheckoutRoute) { r.CommitGenerationID++ }},
		{"active", func(r *store_sqlite.CheckoutRoute) { r.State = store_sqlite.RouteActive }},
		{"nonempty", func(r *store_sqlite.CheckoutRoute) { r.DirtyGenerationID = trigger.DirtyGenerationID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := route
			tc.change(&changed)
			parent, release := c.withdrawnCompactionParent(ctx, changed, trigger)
			release()
			if parent != 0 {
				t.Fatalf("changed authority accepted parent %d", parent)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	parent, release = c.withdrawnCompactionParent(canceled, route, trigger)
	release()
	if parent != 0 {
		t.Fatal("canceled worker accepted parent")
	}
	c.compaction.mu.Lock()
	c.compaction.preferred = route.CommitGenerationID // Servable, but not this checkout's dirty layer.
	c.compaction.mu.Unlock()
	parent, release = c.withdrawnCompactionParent(ctx, route, trigger)
	release()
	if parent != 0 {
		t.Fatal("non-dirty preferred generation accepted")
	}
	c.compaction.mu.Lock()
	c.compaction.preferred = trigger.DirtyGenerationID
	c.compaction.closed = true
	c.compaction.mu.Unlock()
	parent, release = c.withdrawnCompactionParent(ctx, route, trigger)
	release()
	if parent != 0 {
		t.Fatal("closed coordinator accepted parent")
	}
	if got := f.route(); got != route {
		t.Fatalf("refused candidates changed route: %+v", got)
	}
}
