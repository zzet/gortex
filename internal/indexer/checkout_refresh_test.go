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

	"github.com/zzet/gortex/internal/gitstate"
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
	if time.Since(started) >= testTimingSlack(time.Second) {
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
	if time.Since(started) >= testTimingSlack(time.Second) {
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

type checkoutRefreshTestCommittedError struct{ checkoutRefreshTestCodedError }

func (checkoutRefreshTestCommittedError) Committed() bool { return true }

func TestCheckoutRefreshDoesNotRetryPermanentSQLiteFailures(t *testing.T) {
	if retryableCheckoutRefreshError(fmt.Errorf("committed: %w", checkoutRefreshTestCommittedError{checkoutRefreshTestCodedError(5)})) {
		t.Fatal("committed storage error was retried")
	}
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
	origin := time.Now()
	riderRecord := DefaultPublicationPhases().Begin(f.checkoutID, fmt.Sprintf("failed-cycle-rider-%d", origin.UnixNano()), "fresh_request", origin)
	second, err := l.RequestCheckoutRefresh(WithPublicationRecord(t.Context(), riderRecord), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("old physical build failed")
	cycleCtx := WithPublicationStamps(t.Context())
	markPublicationPhase(cycleCtx, PublicationPublished)
	c.completeCheckoutRefreshTickets(cycleCtx, through, CheckoutCycle{Err: want})
	if _, found := phaseOffsets(riderRecord.Snapshot())[PublicationPublished]; found {
		t.Fatal("a rider inherited an unverified failed cycle's publication")
	}
	if result := awaitCheckoutRefresh(t, first); !errors.Is(result.Err, want) || result.Reindexed {
		t.Fatalf("first: %+v", result)
	}
	assertCheckoutRefreshPending(t, second)
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, second); result.Err != nil || !result.Reindexed {
		t.Fatalf("new recovery: %+v", result)
	}
}

func TestCheckoutRefreshBindsCommittedContentAcrossOtherEdits(t *testing.T) {
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
			if differentFile {
				if result.Err != nil || !result.Reindexed || result.AppliedGeneration != uint64(f.route().DirtyGenerationID) {
					t.Fatalf("same committed bytes were not published: %+v", result)
				}
			} else if !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) || result.Reindexed || result.AppliedGeneration != 0 {
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
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			if stage == "after_enqueue" {
				builderGit(t, f.worktree, "switch", "-c", "external-same-commit")
			}
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
	if !f.store.WriteWanted() {
		t.Fatal("admitted ticket did not retain global write demand")
	}
	_ = c.Close()
	if f.store.WriteWanted() {
		t.Fatal("shutdown retained ticket write demand")
	}
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
	rootInfo, err := checkoutRootFileInfo(f.worktree)
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

func TestCheckoutMutationConcurrentAdmissionIsBoundedByCaller(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	epoch := f.route().RouteEpoch
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Hold the existing sampler lease so admission positively reaches its
	// urgent held-cycle pre-check before the caller's budget expires.
	releaseSample, err := c.sampler.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	type admissionResult struct {
		lease *CheckoutMutation
		err   error
	}
	done := make(chan admissionResult, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case got := <-done:
				if got.lease != nil {
					got.lease.Close()
				}
			case <-time.After(testTimingSlack(2 * time.Second)):
				t.Error("owned admission did not join after cancellation")
			}
		}
		releaseSample()
	}()
	go func() {
		lease, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, epoch)
		done <- admissionResult{lease: lease, err: err}
	}()
	witnessed := false
	for !witnessed {
		select {
		case got := <-done:
			joined = true
			if got.lease != nil {
				got.lease.Close()
			}
			t.Fatalf("admission returned before a live urgent pre-check wait was witnessed: error=%v", got.err)
		case <-ctx.Done():
			t.Fatal("caller budget expired before a live urgent pre-check wait was witnessed")
		default:
			witnessed = c.sampler.UrgentWaiting() > 0 && ctx.Err() == nil
			if !witnessed {
				time.Sleep(time.Millisecond)
			}
		}
	}
	var got admissionResult
	select {
	case got = <-done:
		joined = true
	case <-time.After(testTimingSlack(2 * time.Second)):
		t.Fatal("admission did not return after its caller deadline")
	}
	if got.lease != nil {
		got.lease.Close()
		t.Fatal("admitted concurrent source mutation")
	}
	if !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, ErrCheckoutMutationBusy) {
		t.Fatalf("admission did not preserve caller deadline: error=%v", got.err)
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

// A postcommit receipt must own its reserved ticket even when the whole-tree
// sampling lane cannot currently make progress. The coordinator takes that
// sample after the lease is released, and must then publish these same bytes.
func TestCheckoutRefreshCommittedAdmissionDoesNotSample(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err = m.Prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc OwnedCommittedHelper() {}\n")
			path := filepath.Join(f.worktree, "helper.go")
			original := c.refreshSample
			c.refreshSample = func(context.Context, time.Time) (gitstate.DirtySnapshot, time.Time, error) {
				t.Error("postcommit admission invoked whole-checkout sampler")
				return gitstate.DirtySnapshot{}, time.Time{}, context.DeadlineExceeded
			}
			record := &PublicationPhaseRecord{}
			ctx, cancel := context.WithCancel(WithPublicationRecord(t.Context(), record))
			cancel()
			var ticket *CheckoutRefreshTicket
			if batch {
				ticket, err = m.EnqueueBatchRefresh(ctx, []CheckoutBatchFile{{Path: path, SHA256: checkoutBatchHash(t, path)}})
			} else {
				ticket, err = m.EnqueueRefresh(ctx, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			c.refreshSample = original
			assertCheckoutRefreshPending(t, ticket)
			c.refreshMu.Lock()
			request := c.refreshWaiters[ticket.Ticket.Generation]
			reserved := c.refreshReserved
			c.refreshMu.Unlock()
			if request == nil || request.record != record || !request.bindAtCompletion || request.headUnbound || reserved != 0 {
				t.Fatalf("ticket did not take publication ownership: request=%+v reserved=%d", request, reserved)
			}
			m.Close()
			c.cycle(t.Context())
			result := awaitCheckoutRefresh(t, ticket)
			if result.Err != nil || !result.Reindexed {
				t.Fatalf("committed bytes did not publish: %+v", result)
			}
			if nodes := f.store.AtGeneration(int64(result.AppliedGeneration)).FindNodesByName("OwnedCommittedHelper"); len(nodes) != 1 {
				t.Fatalf("ticket generation lacks committed symbol: %+v", nodes)
			}
		})
	}
}

func TestCheckoutRefreshCompletionRejectsReplacedIdentity(t *testing.T) {
	for _, replacement := range []string{"incarnation", "root"} {
		t.Run(replacement, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc IdentityBoundHelper() {}\n")
			through := c.checkoutRefreshHighWater()
			out := c.reconcile(t.Context())
			if out.Err != nil || out.DirtyGenerationID == 0 {
				t.Fatalf("build: %+v", out)
			}
			published, started, err := c.refreshSampleSince(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			c.refreshSample = func(context.Context, time.Time) (gitstate.DirtySnapshot, time.Time, error) {
				return published, started, nil
			}

			if replacement == "incarnation" {
				row, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
				if err != nil || !found {
					t.Fatalf("checkout: found=%v err=%v", found, err)
				}
				row.Incarnation += "-replacement"
				if err := f.catalog.UpsertCheckout(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			} else {
				original := f.worktree + "-original"
				if err := os.Rename(f.worktree, original); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(f.worktree); _ = os.Rename(original, f.worktree) })
				if err := os.Mkdir(f.worktree, 0755); err != nil {
					t.Fatal(err)
				}
			}
			c.completeCheckoutRefreshTickets(t.Context(), through, out)
			result := awaitCheckoutRefresh(t, ticket)
			if !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) || result.Reindexed {
				t.Fatalf("replacement inherited committed publication: %+v", result)
			}
		})
	}
}

func TestCheckoutRefreshCannotCertifyPublishedOtherBytesAfterRestore(t *testing.T) {
	for _, mode := range []string{"source", "batch_present", "batch_absent"} {
		t.Run(mode, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			wanted := "package fixture\nfunc WantedHelper() {}\n"
			builderWriteFile(t, f.worktree, "helper.go", wanted)
			path := filepath.Join(f.worktree, "helper.go")
			var ticket *CheckoutRefreshTicket
			switch mode {
			case "batch_absent":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				ticket, err = m.EnqueueBatchRefresh(t.Context(), []CheckoutBatchFile{{Path: path, Absent: true}})
			case "batch_present":
				ticket, err = m.EnqueueBatchRefresh(t.Context(), []CheckoutBatchFile{{Path: path, SHA256: checkoutBatchHash(t, path)}})
			default:
				ticket, err = m.EnqueueRefresh(t.Context(), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc OtherHelper() {}\n")
			through := c.checkoutRefreshHighWater()
			out := c.reconcile(t.Context())
			if out.Err != nil || out.DirtyGenerationID == 0 {
				t.Fatalf("build other bytes: %+v", out)
			}
			sample, started, err := c.refreshSampleSince(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "batch_absent" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				builderWriteFile(t, f.worktree, "helper.go", wanted)
			}
			c.refreshSample = func(context.Context, time.Time) (gitstate.DirtySnapshot, time.Time, error) {
				return sample, started, nil
			}
			c.completeCheckoutRefreshTickets(t.Context(), through, out)
			result := awaitCheckoutRefresh(t, ticket)
			if !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) || result.Reindexed || result.AppliedGeneration != 0 {
				t.Fatalf("restored bytes certified another publication: %+v", result)
			}
		})
	}
}

func TestCheckoutRefreshPublicationProofUsesSampleHEADForCleanPaths(t *testing.T) {
	f, c, _ := newCheckoutMutationFixture(t)
	path := filepath.Join(f.worktree, "helper.go")
	// Restore this path to HEAD so it is omitted from the dirty manifest.
	builderGit(t, f.worktree, "restore", "--source=HEAD", "--worktree", "--", "helper.go")
	sample, _, err := c.refreshSampleSince(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range sample.Contents {
		if content.Path == "helper.go" {
			t.Fatalf("clean HEAD path unexpectedly has manifest entry: %+v", content)
		}
	}

	var blob string
	_, raw, err := checkoutRefreshFileHash(t.Context(), f.worktree, mustCheckoutRootInfo(t, f.worktree), path, &blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: path, SHA256: raw}, blob); err != nil {
		t.Fatalf("HEAD bytes: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: path, SHA256: "different"}, blob); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("other bytes inherited HEAD: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: path, Absent: true}, ""); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("present HEAD certified absent: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: filepath.Join(f.worktree, "never-existed.go"), Absent: true}, ""); err != nil {
		t.Fatalf("HEAD absence: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "checkout-alias")
	if err := os.Symlink(f.worktree, alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(alias, "helper.go")
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: aliasPath, SHA256: raw}, blob); err != nil {
		t.Fatalf("alias HEAD bytes: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: aliasPath, Absent: true}, ""); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("alias certified present HEAD path absent: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, sample, CheckoutBatchFile{Path: filepath.Join(t.TempDir(), "outside.go"), Absent: true}, ""); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("outside path certified absent: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted, _, err := c.refreshSampleSince(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, deleted, CheckoutBatchFile{Path: path, Absent: true}, ""); err != nil {
		t.Fatalf("sample deletion: %v", err)
	}
	if err := checkoutSampleContainsFile(t.Context(), f.worktree, deleted, CheckoutBatchFile{Path: path, SHA256: raw}, blob); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("deleted sample certified present: %v", err)
	}

}

func mustCheckoutRootInfo(t *testing.T, root string) os.FileInfo {
	t.Helper()
	info, err := checkoutRootFileInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
