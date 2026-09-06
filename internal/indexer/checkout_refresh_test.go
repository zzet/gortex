package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func queueCheckoutSourceEdit(t *testing.T, f *coordinatorFixture, l *CheckoutLifecycle, text string) *CheckoutRefreshTicket {
	t.Helper()
	m, err := l.BeginCheckoutMutation(context.Background(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", text)
	ticket, err := m.EnqueueRefresh(context.Background(), filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func assertCheckoutRefreshPending(t testing.TB, ticket *CheckoutRefreshTicket) {
	t.Helper()
	select {
	case result := <-ticket.Ticket.Done:
		t.Fatalf("ticket completed before requested publication: %+v", result)
	default:
	}
}

func awaitCheckoutRefresh(t testing.TB, ticket *CheckoutRefreshTicket) MutationResult {
	t.Helper()
	select {
	case result, ok := <-ticket.Ticket.Done:
		if !ok {
			t.Fatal("ticket closed without a result")
		}
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("refresh ticket never completed")
		return MutationResult{}
	}
}

func TestCheckoutRefreshEnqueueOutlivesRequestAndPublishesExactGeneration(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	primary, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc AsyncHelper() {}\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	ticket, err := m.EnqueueRefresh(ctx, filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("enqueue waited for a graph build")
	}
	checkoutID, incarnation := m.Identity()
	if ticket.CheckoutID != checkoutID || ticket.Incarnation != incarnation || ticket.Root != f.worktree || ticket.Ticket.Generation == 0 {
		t.Fatalf("incorrect ticket identity: %+v", ticket)
	}
	assertCheckoutRefreshPending(t, ticket)
	if _, err := m.Refresh(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("queued lease also admitted synchronous publication: %v", err)
	}
	if f.route().State != store_sqlite.RoutePending {
		t.Fatal("queued edit became exact before build")
	}
	m.Close()
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || result.RequestedGeneration != ticket.Ticket.Generation || result.AppliedGeneration != uint64(f.route().DirtyGenerationID) || result.AppliedGeneration == uint64(before.DirtyGenerationID) {
		t.Fatalf("refresh did not identify its exact generation: %+v route=%+v", result, f.route())
	}
	if after, err := os.ReadFile(filepath.Join(f.primary, "helper.go")); err != nil || string(after) != string(primary) {
		t.Fatalf("primary changed: %v", err)
	}
	if f.route().CommitGenerationID != before.CommitGenerationID {
		t.Fatal("dirty refresh replaced primary/commit layer")
	}
}

func TestCheckoutRefreshRecoveryAdmitsWhileSourceLeaseOwnsBuildGate(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc RecoveryHelper() {}\n")
	started := time.Now()
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("recovery waited on source build gate")
	}
	assertCheckoutRefreshPending(t, ticket)
	m.Close()
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("recovery: %+v", result)
	}
}

func TestCheckoutRefreshExistingLoopCompletesSourceAndMidBuildRecovery(t *testing.T) {
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Millisecond})
	if out := c.reconcile(t.Context()); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("initial: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	c.dirtyBarrier = func() { enteredOnce.Do(func() { close(entered) }); <-release }
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc BackgroundHelper() {}\n")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator did not consume admitted source ticket")
	}
	assertCheckoutRefreshPending(t, ticket)
	recovery, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("mid-build recovery rejected: %v", err)
	}
	assertCheckoutRefreshPending(t, recovery)
	releaseOnce.Do(func() { close(release) })
	for _, pending := range []*CheckoutRefreshTicket{ticket, recovery} {
		if result := awaitCheckoutRefresh(t, pending); result.Err != nil || !result.Reindexed {
			t.Fatalf("background publication: %+v", result)
		}
	}
}

func TestCheckoutRefreshDeferralAndTimeoutRemainPending(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc RetriedHelper() {}\n")
	through := c.checkoutRefreshHighWater()
	for _, out := range []CheckoutCycle{{Deferred: true}, {Rescheduled: true}, {Err: context.DeadlineExceeded}, {Err: ErrViewBuildQueueFull}, {Err: fmt.Errorf("busy: %w", checkoutRefreshTestCodedError(5))}, {Err: checkoutRefreshTestCodedError(6 | 1<<8)}} {
		c.completeCheckoutRefreshTickets(t.Context(), through, out)
		assertCheckoutRefreshPending(t, ticket)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("retry: %+v", result)
	}
}

type checkoutRefreshTestCodedError int

func (e checkoutRefreshTestCodedError) Error() string { return "injected SQLite error" }
func (e checkoutRefreshTestCodedError) Code() int     { return int(e) }

func TestCheckoutRefreshDoesNotRetryPermanentSQLiteFailures(t *testing.T) {
	for _, code := range []int{1, 7, 10, 13, 19} {
		if retryableCheckoutRefreshError(fmt.Errorf("storage: %w", checkoutRefreshTestCodedError(code))) {
			t.Fatalf("permanent SQLite failure %d was retried", code)
		}
	}
}

func TestCheckoutRefreshOldCycleCannotFailNewAdmission(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	first, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	through := c.checkoutRefreshHighWater()
	second, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("old physical build failed")
	c.completeCheckoutRefreshTickets(t.Context(), through, CheckoutCycle{Err: want})
	if result := awaitCheckoutRefresh(t, first); !errors.Is(result.Err, want) || result.Reindexed {
		t.Fatalf("first: %+v", result)
	}
	assertCheckoutRefreshPending(t, second)
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, second); result.Err != nil || !result.Reindexed {
		t.Fatalf("new recovery: %+v", result)
	}
}

func TestCheckoutRefreshSupersededContentAndSnapshotNeverClaimFresh(t *testing.T) {
	for _, differentFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "target_changed", true: "unrelated_snapshot_changed"}[differentFile], func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc RequestedHelper() {}\n")
			path := "helper.go"
			if differentFile {
				path = "extra.go"
			}
			builderWriteFile(t, f.worktree, path, "package fixture\nfunc DifferentHelper() {}\n")
			c.cycle(t.Context())
			result := awaitCheckoutRefresh(t, ticket)
			if !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) || result.Reindexed || result.AppliedGeneration != 0 {
				t.Fatalf("false exact success: %+v", result)
			}
			recovery, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
			if err != nil {
				t.Fatal(err)
			}
			c.cycle(t.Context())
			if result := awaitCheckoutRefresh(t, recovery); result.Err != nil || !result.Reindexed {
				t.Fatalf("recovery: %+v", result)
			}
		})
	}
}

func TestCheckoutRefreshNeverAdoptsAnExternalBranchSwitch(t *testing.T) {
	for _, stage := range []string{"before_prepare", "before_enqueue", "after_enqueue"} {
		t.Run(stage, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if stage == "before_prepare" {
				builderGit(t, f.worktree, "switch", "-c", "external-same-commit")
				if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
					t.Fatalf("branch change admitted: %v", err)
				}
				return
			}
			if err := m.Prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc BranchBoundHelper() {}\n")
			if stage == "before_enqueue" {
				builderGit(t, f.worktree, "switch", "-c", "external-same-commit")
			}
			ticket, err := m.EnqueueRefresh(t.Context(), filepath.Join(f.worktree, "helper.go"))
			if stage == "before_enqueue" {
				if !errors.Is(err, ErrCheckoutRefreshSuperseded) || ticket != nil {
					t.Fatalf("post-commit branch was adopted: ticket=%+v err=%v", ticket, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			builderGit(t, f.worktree, "switch", "-c", "external-same-commit")
			c.cycle(t.Context())
			if result := awaitCheckoutRefresh(t, ticket); !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) || result.Reindexed {
				t.Fatalf("post-admission branch was adopted: %+v", result)
			}
		})
	}
}

func TestCheckoutRefreshQueueReservationPrecedesDiskCommit(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c.refreshMu.Lock()
	c.refreshReserved = maxCheckoutRefreshTickets
	c.refreshMu.Unlock()
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutRefreshQueueFull) {
		t.Fatalf("prepare accepted full queue: %v", err)
	}
	if f.route() != before {
		t.Fatal("full queue invalidated route before rejecting disk commit")
	}
	c.refreshMu.Lock()
	c.refreshReserved = 0
	c.refreshMu.Unlock()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.refreshMu.Lock()
	c.refreshReserved = maxCheckoutRefreshTickets // Includes the source reservation.
	c.refreshMu.Unlock()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc ReservedHelper() {}\n")
	if _, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree); !errors.Is(err, ErrCheckoutRefreshQueueFull) {
		t.Fatalf("recovery stole reserved capacity: %v", err)
	}
	ticket, err := m.EnqueueRefresh(t.Context(), filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatalf("post-commit reserved enqueue failed: %v", err)
	}
	c.refreshMu.Lock()
	c.refreshReserved = 0 // Release the artificial recovery reservations.
	c.refreshMu.Unlock()
	m.Close()
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("reserved source: %+v", result)
	}
}

func TestCheckoutRefreshShutdownCompletesWaiters(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc StoppedHelper() {}\n")
	_ = c.Close()
	if result := awaitCheckoutRefresh(t, ticket); !errors.Is(result.Err, ErrCheckoutRefreshStopped) || result.Reindexed {
		t.Fatalf("stopped ticket: %+v", result)
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if len(c.refreshWaiters) != 0 || c.refreshReserved != 0 || !c.refreshClosed {
		t.Fatalf("retained ticket state: waiters=%d reservations=%d closed=%v", len(c.refreshWaiters), c.refreshReserved, c.refreshClosed)
	}
}

func TestCheckoutRefreshOperationalStoragePanicFailsTicketOnly(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	storageErr := &store_sqlite.StorageError{}
	func() {
		defer c.guardCheckoutRefreshCycle(t.Context(), c.checkoutRefreshHighWater())
		panic(storageErr)
	}()
	if result := awaitCheckoutRefresh(t, ticket); !errors.Is(result.Err, storageErr) || result.Reindexed {
		t.Fatalf("storage panic: %+v", result)
	}
	want := errors.New("programmer panic")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		defer c.guardCheckoutRefreshCycle(t.Context(), 0)
		panic(want)
	}()
	if recovered != want {
		t.Fatalf("programmer panic was swallowed: %v", recovered)
	}
}

func TestCheckoutRefreshHashRejectsOutsideAndReplacedPhysicalRoot(t *testing.T) {
	f, _, _ := newCheckoutMutationFixture(t)
	rootInfo, err := os.Stat(f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.worktree, filepath.Join(f.primary, "helper.go"), filepath.Join(f.worktree, ".git")} {
		if _, _, err := checkoutRefreshFileHash(t.Context(), f.worktree, rootInfo, path); err == nil {
			t.Fatalf("accepted unsafe target %q", path)
		}
	}
	original := f.worktree + "-removed"
	if err := os.Rename(f.worktree, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc Imposter() {}\n")
	if _, _, err := checkoutRefreshFileHash(t.Context(), f.worktree, rootInfo, filepath.Join(f.worktree, "helper.go")); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("accepted reused pathname: %v", err)
	}
}

func TestCheckoutMutationBusyAdmissionIsBounded(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	started := time.Now()
	other, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if other != nil {
		other.Close()
		t.Fatal("admitted concurrent source mutation")
	}
	if !errors.Is(err, ErrCheckoutMutationBusy) || time.Since(started) > 2*time.Second {
		t.Fatalf("unbounded or untyped admission: elapsed=%s error=%v", time.Since(started), err)
	}
}

func BenchmarkCheckoutRefreshAdmission(b *testing.B) {
	f, c, l := newCheckoutMutationFixture(b)
	checkout, _, err := l.catalog.GetCheckout(context.Background(), f.checkoutID)
	if err != nil {
		b.Fatal(err)
	}
	rootInfo, err := os.Stat(f.worktree)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		request, err := c.captureCheckoutRefresh(context.Background(), checkout, rootInfo, filepath.Join(f.worktree, "helper.go"))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := c.enqueueCheckoutRefresh(request, false); err != nil {
			b.Fatal(err)
		}
		c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
	}
}
