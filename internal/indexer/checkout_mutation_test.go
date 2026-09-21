package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func newCheckoutMutationFixture(t testing.TB) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour})
	if out := c.reconcile(context.Background()); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	return f, c, l
}

func TestCheckoutMutationDryRunLeavesRouteUntouched(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	m.Close()
	if after := f.route(); after != before {
		t.Fatalf("dry run changed route: before=%+v after=%+v", before, after)
	}
}

func TestCheckoutMutationPublishesOnlyDirtyLayerAndPreservesPinnedView(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	pinned, err := materializer.MaterializeCheckout(t.Context(), f.checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatalf("prepare must be idempotent before refresh: %v", err)
	}
	if route := f.route(); route.State != store_sqlite.RoutePending || route.DirtyGenerationID != 0 || route.CommitGenerationID != before.CommitGenerationID {
		t.Fatalf("old exact route remained visible while source is changing: %+v", route)
	}
	if _, found := f.generation(before.DirtyGenerationID); !found {
		t.Fatal("prepare collected an old reader's pinned dirty generation")
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc EditedHelper() {}\n")
	out, err := m.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.CommitGenerationID != before.CommitGenerationID || out.DirtyGenerationID == before.DirtyGenerationID || !out.DirtyBuilt {
		t.Fatalf("unexpected mutation rebuild: %+v", out)
	}
	if route := f.route(); route.State != store_sqlite.RouteActive || route.DirtyGenerationID != out.DirtyGenerationID {
		t.Fatalf("successful refresh did not leave exact route: %+v", route)
	}
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	if err != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("source mutation changed primary: %v", err)
	}
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("fresh lease admitted a second unguarded disk commit: %v", err)
	}
}

func TestCheckoutMutationRejectsWrongRootEpochAndExternalChanges(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	for _, tc := range []struct {
		root  string
		epoch int64
	}{
		{f.primary, before.RouteEpoch},
		{f.worktree, before.RouteEpoch + 1},
		{f.worktree, 0},
	} {
		if m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, tc.root, tc.epoch); !errors.Is(err, ErrCheckoutMutationStale) {
			if m != nil {
				m.Close()
			}
			t.Fatalf("root=%s epoch=%d: %v", tc.root, tc.epoch, err)
		}
	}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternalEdit() {}\n")
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("external edit between begin and prepare was not refused: %v", err)
	}
	m.Close()
	if m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch); !errors.Is(err, ErrCheckoutMutationStale) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("stale disk at admission was not refused: %v", err)
	}
	if f.route() != before {
		t.Fatal("rejected mutations changed the catalog route")
	}
}

func TestCheckoutMutationCanceledRefreshLeavesPendingAndSignalsRetry(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc PendingEdit() {}\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh: %v", err)
	}
	m.Close()
	if route := f.route(); route.State != store_sqlite.RoutePending {
		t.Fatalf("failed refresh became active: %+v", route)
	}
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	if reason != "source mutation needs a dirty generation refresh" {
		t.Fatalf("missing retry signal: %q", reason)
	}
}

func TestCheckoutMutationRejectsReplacedRoot(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	oldRoot := f.worktree + "-original"
	if err := os.Rename(f.worktree, oldRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Keep the replacement under this fixture's temporary directory so
		// standard test cleanup owns its removal.
		_ = os.Rename(f.worktree, f.worktree+"-replacement")
		if err := os.Rename(oldRoot, f.worktree); err != nil {
			t.Errorf("restore fixture root: %v", err)
		}
	}()
	if err := os.CopyFS(f.worktree, os.DirFS(oldRoot)); err != nil {
		t.Fatal(err)
	}
	// This replacement precedes lifecycle discovery: catalog id/incarnation
	// and route epoch are unchanged, as are Git and source contents. The pinned
	// filesystem identity must refuse before a content comparison can accept it.
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) || !strings.Contains(err.Error(), "root was replaced") {
		t.Fatalf("replaced root admitted a source write: %v", err)
	}
	if f.route() != before {
		t.Fatal("root replacement refusal invalidated the original route")
	}
}

func TestCheckoutMutationCloseJoinsLeaseAndCancelsWaitingAdmission(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := c.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown did not wait for active writer: %v", err)
	}
	if err := m.Prepare(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("closing coordinator admitted a write: %v", err)
	}
	m.Close()
	if err := c.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lease, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch); err == nil {
		lease.Close()
		t.Fatal("closed coordinator admitted new source mutation")
	}
}

func TestCheckoutMutationAdmissionCancellationReleasesResources(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	// The cycle lock is the only thing admission waits on; the shared build
	// lane is not an admission stage (checkout_mutation_lane_test.go).
	c.cycleMu.Lock()
	block := c.cycleMu.Unlock
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch); !errors.Is(err, context.DeadlineExceeded) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("admission was not canceled: %v", err)
	}
	block()
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("canceled admission leaked %d leases", active)
	}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}

func TestCheckoutMutationAdmissionPanicReleasesResources(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	// A backend panic recovered by MCP must not strand the checkout route lock,
	// and admission must not have touched the shared build gate at all. A
	// missing coordinator catalog injects failure after the lock has been
	// acquired, without changing the production path.
	catalog := c.catalog
	c.catalog = nil
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		m, _ := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
		if m != nil {
			m.Close()
		}
	}()
	c.catalog = catalog
	if panicValue == nil {
		t.Fatal("fault injection did not panic")
	}
	if !c.cycleMu.TryLock() {
		t.Fatal("panic stranded the checkout route lock")
	}
	c.cycleMu.Unlock()
	if stats := c.gate.Stats(); stats.Active {
		t.Fatal("panic stranded the shared build gate")
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("panic leaked %d admissions", active)
	}
}

func BenchmarkCheckoutMutationDryRunAdmission(b *testing.B) {
	f, _, l := newCheckoutMutationFixture(b)
	epoch := f.route().RouteEpoch
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m, err := l.BeginCheckoutMutation(b.Context(), f.checkoutID, f.worktree, epoch)
		if err != nil {
			b.Fatal(err)
		}
		m.Close()
	}
}

// newCheckoutMutationAuthorityFixture is the mutation fixture with a real
// output-generation authority installed the way NewSharedServer installs it:
// on the MultiIndexer the lifecycle resolves through.
func newCheckoutMutationAuthorityFixture(t *testing.T) (*coordinatorFixture, *CheckoutLifecycle, *OutputGenerationAuthority) {
	t.Helper()
	f, _, l := newCheckoutMutationFixture(t)
	authority := NewOutputGenerationAuthority(f.leases)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetOutputGenerationAuthority(authority)
	l.mi = mi
	return f, l, authority
}

// TestCheckoutMutationNamesItsRoutedDirtyGeneration pins the checkout half of
// the single output-generation authority.
//
// A source edit against a checkout writes exactly one output generation: the
// routed DIRTY generation Prepare withdraws and Refresh republishes. Nothing
// else may be named — not the commit generation beside it, and not generation
// zero. The lease settles that receipt exactly once, on Close.
func TestCheckoutMutationNamesItsRoutedDirtyGeneration(t *testing.T) {
	f, l, authority := newCheckoutMutationAuthorityFixture(t)
	before := f.route()

	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	// The lease holds this checkout's cycle lock and a source-mutation
	// admission; a failed assertion must not leave the coordinator undrainable.
	closed := false
	defer func() {
		if !closed {
			m.Close()
		}
	}()
	receipt := m.Receipt()
	if receipt == nil {
		t.Fatal("a checkout source-edit lease opened no output-generation receipt")
	}
	target := receipt.Target()
	if target.Kind != OutputGenerationCheckout {
		t.Fatalf("target kind = %v, want checkout", target.Kind)
	}
	if target.Generation != before.DirtyGenerationID {
		t.Fatalf("target generation = %d, want the routed dirty generation %d", target.Generation, before.DirtyGenerationID)
	}
	if target.Generation == before.CommitGenerationID {
		t.Fatal("a source edit must not name the commit generation")
	}
	if target.CheckoutID != f.checkoutID || target.Incarnation == "" {
		t.Fatalf("target does not carry the complete checkout identity: %+v", target)
	}
	if target.OwnerKey != "checkout:"+f.checkoutID {
		t.Fatalf("owner key = %q, want the checkout", target.OwnerKey)
	}
	if entry := receipt.Entry(); entry != OutputEntryCheckoutSourceMutation {
		t.Fatalf("entry = %q, want the checkout source-mutation entry point", entry)
	}

	// A dry run fulfils nothing: it settles the receipt without claiming the
	// generation it named.
	m.Close()
	closed = true
	if err := m.ReceiptError(); err != nil {
		t.Fatalf("a dry run must settle cleanly: %v", err)
	}
	stats := authority.Stats()
	if stats.Issued != 1 || stats.Settled != 1 {
		t.Fatalf("stats = %+v, want one receipt issued and settled", stats)
	}
	if stats.LiveOwners != 0 {
		t.Fatalf("a settled lease must leave no live owner: %+v", stats)
	}
}

// TestCheckoutMutationRefusesASupersededGenerationBeforeItWrites is gate 6's
// last clause on the checkout lane, in its PREVENTIVE form: a lease whose
// routed dirty generation a newer mutation already took over never withdraws
// the route and never republishes it. The refusal comes before the bytes move,
// not after.
//
// Reachability, stated plainly: BeginCheckoutMutation holds the checkout's
// cycleMu from admission through Close (checkout_mutation.go, "Close now owns
// every acquired resource"), so two live receipts for one "checkout:<id>"
// owner cannot overlap in today's production shape — this test admits the
// newer receipt through the authority directly, which is what an asynchronous
// publication path (or a lease that outlives the cycle lock) would do. The
// check exists so the invariant is a property of the authority rather than an
// accident of the current locking.
func TestCheckoutMutationRefusesASupersededGenerationBeforeItWrites(t *testing.T) {
	f, l, authority := newCheckoutMutationAuthorityFixture(t)
	before := f.route()

	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	receipt := m.Receipt()
	if receipt == nil {
		m.Close()
		t.Fatal("a checkout source-edit lease opened no output-generation receipt")
	}

	// A newer mutation takes over this checkout's generation while the lease is
	// still open and has written nothing.
	newer, err := authority.Begin(t.Context(), OutputEntryCheckoutSourceMutation, receipt.Target())
	if err != nil {
		m.Close()
		t.Fatalf("admit the newer mutation: %v", err)
	}
	defer newer.Abandon()

	// Preventive: Prepare is refused, so the routed dirty generation is never
	// withdrawn.
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		m.Close()
		t.Fatalf("Prepare under a superseded receipt: got %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	after := f.route()
	if after.DirtyGenerationID != before.DirtyGenerationID || after.State != before.State || after.RouteEpoch != before.RouteEpoch {
		m.Close()
		t.Fatalf("a refused lease moved the route: before=%+v after=%+v", before, after)
	}

	// Refresh is fenced the same way, so a lease that somehow got past Prepare
	// still cannot republish the generation.
	if _, err := m.Refresh(t.Context()); err == nil {
		m.Close()
		t.Fatal("Refresh must refuse an unprepared, superseded lease")
	}

	// And the reporting half still holds: the lease settles without claiming
	// the generation.
	m.Close()
	if err := m.ReceiptError(); err != nil {
		t.Fatalf("a lease that wrote nothing abandons rather than reporting a refused fulfilment: %v", err)
	}
	stats := authority.Stats()
	if stats.Superseded != 1 {
		t.Fatalf("superseded count = %d, want 1 (the abandoned lease)", stats.Superseded)
	}
	if stats.LiveOwners != 1 {
		t.Fatalf("the surviving newer receipt must still hold the owner: %+v", stats)
	}
}

// TestCheckoutMutationRefusesToFulfilASupersededGeneration is the REPORTING
// half on the checkout lane: work whose authority a newer mutation took over
// while it ran cannot fulfil the generation it named, even though its own disk
// commit and rebuild succeeded. Those bytes are already written; the refusal
// exists so the lease does not report a fulfilled generation and the
// coordinator reschedules instead.
func TestCheckoutMutationRefusesToFulfilASupersededGeneration(t *testing.T) {
	f, l, authority := newCheckoutMutationAuthorityFixture(t)
	before := f.route()

	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		m.Close()
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc SupersededHelper() {}\n")
	if _, err := m.Refresh(t.Context()); err != nil {
		m.Close()
		t.Fatal(err)
	}

	// The takeover happens after the payload already ran: Prepare and Refresh
	// both completed, so only Complete can refuse.
	receipt := m.Receipt()
	if receipt == nil {
		m.Close()
		t.Fatal("a checkout source-edit lease opened no output-generation receipt")
	}
	newer, err := authority.Begin(t.Context(), OutputEntryCheckoutSourceMutation, receipt.Target())
	if err != nil {
		m.Close()
		t.Fatalf("admit the newer mutation: %v", err)
	}
	defer newer.Abandon()

	m.Close()
	if err := m.ReceiptError(); !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("superseded fulfilment: got %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	if got := authority.Stats().Superseded; got != 1 {
		t.Fatalf("superseded count = %d, want 1", got)
	}
}

// TestCheckoutMutationRefusesToPublishAGenerationTakenOverDuringTheBuildWait
// pins the fence at the REPUBLISH boundary.
//
// Refresh is the one lease operation that builds, so it is the one that queues
// for the shared build gate — an unbounded wait. A receipt validated only at
// the top of Refresh is therefore validated before that wait, and a newer
// mutation admitted while the lease sits in the queue would still get to
// republish the generation it no longer owns. The check taken after the gate
// and immediately before the build is what makes "a superseded receipt cannot
// publish" true rather than approximately true.
//
// The window is made deterministic by holding the gate's single build slot:
// Refresh is provably past its pre-gate check and provably before its build
// while the newer receipt is admitted.
func TestCheckoutMutationRefusesToPublishAGenerationTakenOverDuringTheBuildWait(t *testing.T) {
	f, l, authority := newCheckoutMutationAuthorityFixture(t)
	c := l.coordinators[f.checkoutID]
	before := f.route()

	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			m.Close()
		}
	}()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc TakenOverHelper() {}\n")
	receipt := m.Receipt()
	if receipt == nil {
		t.Fatal("a checkout source-edit lease opened no output-generation receipt")
	}
	target := receipt.Target()

	withdrawn := f.route()
	if withdrawn.DirtyGenerationID != 0 {
		t.Fatalf("Prepare did not withdraw the dirty route: %+v", withdrawn)
	}

	// Occupy the gate's single build slot, so the Refresh below is past its
	// pre-gate receipt check and parked in the queue.
	release, err := c.gate.Acquire(t.Context(), ViewBuildInteractive)
	if err != nil {
		t.Fatalf("hold the build gate: %v", err)
	}
	type refreshOutcome struct {
		out CheckoutCycle
		err error
	}
	done := make(chan refreshOutcome, 1)
	go func() {
		out, refreshErr := m.Refresh(context.Background())
		done <- refreshOutcome{out: out, err: refreshErr}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for c.gate.Stats().InteractiveQueued == 0 {
		if time.Now().After(deadline) {
			release()
			<-done
			t.Fatal("Refresh never queued for the shared build gate; the window under test does not exist")
		}
		time.Sleep(time.Millisecond)
	}

	// The takeover happens while the publish waits for the lane.
	newer, err := authority.Begin(t.Context(), OutputEntryCheckoutSourceMutation, target)
	if err != nil {
		release()
		<-done
		t.Fatalf("admit the newer mutation: %v", err)
	}
	defer newer.Abandon()
	release()

	outcome := <-done
	if !errors.Is(outcome.err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("Refresh under a receipt taken over during the build wait: got %v, want ErrOutputMutationReceiptSuperseded", outcome.err)
	}
	if after := f.route(); after.DirtyGenerationID != 0 || after.State != withdrawn.State {
		t.Fatalf("a superseded lease republished the generation it no longer owned: %+v", after)
	}

	// The lease still settles without claiming the generation, and the
	// withdrawn route is left for the coordinator to reschedule.
	m.Close()
	closed = true
	if err := m.ReceiptError(); err != nil {
		t.Fatalf("a lease that published nothing abandons rather than reporting a refused fulfilment: %v", err)
	}
	if got := authority.Stats().Superseded; got != 1 {
		t.Fatalf("superseded count = %d, want 1 (the abandoned lease)", got)
	}
}
