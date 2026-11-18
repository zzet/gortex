package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// These are production loops over fifty distinct real Git checkouts/catalog
// identities. Git construction and start-time index healing are excluded from
// the close budget; no timer or fake done channel stands in for a live actor.
func TestCheckoutLifecycleCloseJoinsFiftyLiveCoordinatorsWithinBudget(t *testing.T) {
	f := newCoordinatorFixture(t)
	original, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	l := &CheckoutLifecycle{
		logger: zap.NewNop(), coordinators: make(map[string]*CheckoutCoordinator),
		started: make(map[string][]*CheckoutCoordinator),
	}
	t.Cleanup(func() { require.NoError(t, l.Close()) })
	actors := make([]*CheckoutCoordinator, 0, 50)
	for i := range 50 {
		fixture := *f
		if i > 0 {
			name := fmt.Sprintf("shutdown-%02d", i)
			fixture.worktree = filepath.Join(filepath.Dir(f.primary), name)
			builderGit(t, f.primary, "worktree", "add", "--detach", fixture.worktree)
			fixture.checkoutID = name
			row := original
			row.CheckoutID, row.Incarnation = name, "incarnation-"+name
			row.RootPath, row.AdminName, row.HeadRef = fixture.worktree, name, ""
			row.GitDir = builderGit(t, fixture.worktree, "rev-parse", "--absolute-git-dir")
			require.NoError(t, f.catalog.AllocateCheckout(t.Context(), row))
		}
		actor := fixture.coordinator(t, CheckoutCoordinatorConfig{Debounce: time.Hour, debounceDemand: true})
		require.NoError(t, actor.waitRacyIndexHeal(t.Context()))
		require.True(t, actor.Running())
		actors = append(actors, actor)
		l.trackStarted(fixture.checkoutID, actor)
		if i%2 == 0 { // Half remain off-route, as admitted transitions can.
			l.coordinators[fixture.checkoutID] = actor
		}
	}
	require.Equal(t, 50, l.LiveCoordinators(""))
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("fifty live coordinator loops did not join within three seconds")
	}
	t.Logf("joined fifty live coordinator loops in %s (construction excluded)", time.Since(started))
	for _, actor := range actors {
		require.False(t, actor.Running(), actor.checkoutID)
		if actor.admitSourceMutation() {
			actor.releaseSourceMutation()
			t.Fatal("closed actor reopened source admission")
		}
		select {
		case <-actor.racyHeal.done:
		default:
			t.Fatalf("%s returned before start-time Git worker joined", actor.checkoutID)
		}
	}
}

type shutdownBlockedAnalysis struct {
	entered chan struct{}
	release chan struct{}
}

func (*shutdownBlockedAnalysis) InvalidateSessionScopes() {}
func (n *shutdownBlockedAnalysis) RunAnalysis() {
	close(n.entered)
	<-n.release
}

func TestCheckoutLifecycleCancelsAllBeforeBlockedAnalysisJoin(t *testing.T) {
	shortAnalysisQuiet(t)
	f := newCoordinatorFixture(t)
	first := f.coordinator(t, CheckoutCoordinatorConfig{})
	second := f.coordinator(t, CheckoutCoordinatorConfig{})
	n := &shutdownBlockedAnalysis{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(n.release) }) }
	l := &CheckoutLifecycle{logger: zap.NewNop(), analysis: newLifecycleAnalysisLane(), notifier: n,
		coordinators: map[string]*CheckoutCoordinator{"first": first},
		started:      map[string][]*CheckoutCoordinator{"second": {second}},
	}
	t.Cleanup(func() { unblock(); require.NoError(t, l.Close()) })
	l.requestAnalysis(analysisRequest{unconditional: true})
	select {
	case <-n.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real analysis worker did not enter its controlled pass")
	}
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	for _, actor := range []*CheckoutCoordinator{first, second} {
		select {
		case <-actor.lifetimeContext().Done():
		case <-time.After(3 * time.Second):
			t.Fatal("analysis join prevented another producer receiving cancellation")
		}
		if actor.admitSourceMutation() {
			actor.releaseSourceMutation()
			t.Fatal("canceled actor reopened source admission")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("cancellation falsely certified the still-owned analysis pass: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("released analysis pass did not join")
	}
}

func TestCheckoutLifecycleCancelsLateAdmittedActorAndJoinsItsSource(t *testing.T) {
	f := newCoordinatorFixture(t)
	l := &CheckoutLifecycle{logger: zap.NewNop(), started: make(map[string][]*CheckoutCoordinator)}
	l.coordinatorStartWG.Add(1) // A constructor admitted before shutdown.
	l.Cancel()
	actor := f.coordinator(t, CheckoutCoordinatorConfig{})
	require.True(t, actor.admitSourceMutation())
	var release sync.Once
	unblock := func() { release.Do(actor.releaseSourceMutation) }
	t.Cleanup(func() { unblock(); require.NoError(t, l.Close()) })
	l.trackStarted("late", actor)
	l.coordinatorStartWG.Done()
	require.ErrorIs(t, actor.lifetimeContext().Err(), context.Canceled)
	if actor.admitSourceMutation() {
		actor.releaseSourceMutation()
		t.Fatal("late canceled actor reopened source admission")
	}
	admitted, already := l.beginCheckoutActivation("another")
	require.False(t, admitted)
	require.False(t, already)
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	// The actual source-owner counter remains held after the loop was
	// canceled; it must not be discarded with a late registry admission.
	require.Eventually(t, func() bool { return !actor.Running() }, 3*time.Second, time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("late actor's live source was dropped instead of joined: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("late admitted actor did not finish its actual source join")
	}
}

func TestCoordinatorCancelDuringWatchStartupJoinsLateAttachment(t *testing.T) {
	f := newCoordinatorFixture(t)
	actor := f.coordinator(t, CheckoutCoordinatorConfig{})
	entered := make(chan struct{})
	canceled := make(chan struct{})
	startRelease := make(chan struct{})
	watchRelease := make(chan struct{})
	watchCanceled := make(chan struct{})
	var startOnce, watchOnce sync.Once
	unblockStart := func() { startOnce.Do(func() { close(startRelease) }) }
	unblockWatch := func() { watchOnce.Do(func() { close(watchRelease) }) }
	t.Cleanup(func() { unblockStart(); unblockWatch(); require.NoError(t, actor.Close()) })
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	t.Cleanup(cancelWatch)
	watch := &checkoutWatch{cancel: cancelWatch, done: make(chan struct{}), backendDone: make(chan struct{})}
	go func() {
		<-watchCtx.Done()
		close(watchCanceled)
		close(watch.done) // Event delivery stopped; physical backend still owns work.
		<-watchRelease
		close(watch.backendDone)
	}()
	require.True(t, actor.startFilesystemWatch(func(ctx context.Context) (*checkoutWatch, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-startRelease
		return watch, nil // A backend becomes ready just after cancellation.
	}))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher startup was not admitted")
	}
	cancelDone := make(chan struct{})
	go func() { actor.Cancel(); close(cancelDone) }()
	select {
	case <-cancelDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel joined startup or the physical watcher")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("owned startup did not receive coordinator cancellation")
	}
	short, stop := context.WithCancel(context.Background())
	stop()
	require.ErrorIs(t, actor.CloseContext(short), context.Canceled, "timed-out join must retain watcher ownership")
	done := make(chan error, 1)
	go func() { done <- actor.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close dropped an admitted watcher startup: %v", err)
	default:
	}
	var called atomic.Bool
	require.False(t, actor.startFilesystemWatch(func(context.Context) (*checkoutWatch, error) {
		called.Store(true)
		return nil, nil
	}))
	require.False(t, called.Load(), "closed admission invoked a late startup callback")
	unblockStart()
	select {
	case <-watchCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("late watcher attachment was not physically canceled")
	}
	select {
	case err := <-done:
		t.Fatalf("event-loop exit falsely certified physical watcher exit: %v", err)
	default:
	}
	unblockWatch()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("released late watcher did not join")
	}
	actor.motion.mu.Lock()
	require.Nil(t, actor.motion.watch)
	actor.motion.mu.Unlock()
}

func TestContractCoordinatorCancelDoesNotJoinQueuedSourceRelease(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("r/@k")
	p.schedule("", []string{"r"}, "r", "k")
	p.waitStarts(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var releases atomic.Int32
	source := &ContractCaptureSource{onRelease: func() {
		releases.Add(1)
		close(entered)
		<-release
	}}
	_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{
		Actor:  NewContractActor("wt", []string{"r", "z"}),
		Key:    graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: "wt"},
		Inputs: schedulerInputs([]string{"r", "z"}, "w"), Source: source,
	})
	require.NoError(t, err)
	canceled := make(chan struct{})
	go func() { p.c.Cancel(); close(canceled) }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel waited for a queued source's physical release")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("queued source release was dropped")
	}
	require.ErrorIs(t, p.c.ctx.Err(), context.Canceled)
	short, stop := context.WithCancel(context.Background())
	stop()
	require.ErrorIs(t, p.c.CloseContext(short), context.Canceled, "undrained release cannot certify closure")
	unblock()
	require.NoError(t, p.c.Close())
	require.Equal(t, int32(1), releases.Load())
}

func TestContractRepairCancelDoesNotBlockCoordinatorCancellation(t *testing.T) {
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	f := newContractRepairFixture(t, func(_ *contractRepairFixture, repair *ContractStuckRepair) {
		repair.beforeLane = func(string) {
			if armed.Load() {
				enterOnce.Do(func() { close(entered) })
				<-release
			}
		}
	})
	t.Cleanup(unblock)
	f.strict(t, time.Minute)
	armed.Store(true)
	f.failBody(t, "consts.go", jobConsts+"// a failed apply before shutdown\n")
	f.r.Scan([]string{"fixture"})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real repair pass did not enter its controlled lane boundary")
	}
	canceled := make(chan struct{})
	go func() {
		f.r.Cancel()
		f.c.Cancel()
		close(canceled)
	}()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("repair join prevented coordinator cancellation")
	}
	require.ErrorIs(t, f.r.ctx.Err(), context.Canceled)
	require.ErrorIs(t, f.c.ctx.Err(), context.Canceled)
	done := make(chan struct{})
	go func() { f.r.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("repair cancellation falsely certified its still-owned pass")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("released repair pass did not join")
	}
	require.NoError(t, f.c.Close())
}
