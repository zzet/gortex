package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	block, err := c.gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
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
	// A backend panic recovered by MCP must not strand the shared build gate
	// or the checkout route lock. A missing coordinator catalog injects failure after
	// both locks have been acquired, without changing the production path.
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
