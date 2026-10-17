package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRefViewLifetimeDetachedBuildIsOwnedUntilRelease(t *testing.T) {
	var lifetime refViewLifetime
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	selectionCtx, finishSelection, err := lifetime.admit(requestCtx, false)
	if err != nil {
		t.Fatal(err)
	}
	buildCtx, finishBuild, err := lifetime.admit(selectionCtx, true)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if selectionCtx.Err() == nil {
		t.Fatal("request cancellation did not cancel selection")
	}
	if buildCtx.Err() != nil {
		t.Fatal("request cancellation abandoned detached build")
	}
	finishSelection()
	done := lifetime.closeAdmission()
	<-buildCtx.Done()
	select {
	case <-done:
		t.Fatal("cleanup passed live build")
	default:
	}
	finishBuild()
	finishBuild()
	<-done
	if again := lifetime.closeAdmission(); again != done {
		t.Fatal("close returned a new drain")
	}
	if _, _, err := lifetime.admit(context.Background(), true); !errors.Is(err, ErrRefViewManagerClosed) {
		t.Fatalf("late build admitted: %v", err)
	}
}

func TestRefViewLifetimeAllocatedBeforeJoinCannotOutliveDrain(t *testing.T) {
	var lifetime refViewLifetime
	ctx, finish, err := lifetime.admit(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	allocated := make(chan struct{})
	resumeJoin := make(chan struct{})
	producerDone := make(chan struct{})
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(resumeJoin) }) }
	// Register before starting the producer or making any further assertion.
	// Failure cleanup fences first, then unblocks and joins the producer.
	t.Cleanup(func() {
		lifetime.closeAdmission()
		resume()
		select {
		case <-producerDone:
		case <-time.After(5 * time.Second):
			t.Error("allocated producer did not join during cleanup")
		}
		finish()
	})
	go func() {
		defer close(producerDone)
		defer finish()
		close(allocated)
		<-resumeJoin
		if ctx.Err() == nil {
			t.Error("late producer was not fenced")
		}
	}()
	select {
	case <-allocated:
	case <-time.After(5 * time.Second):
		t.Fatal("producer never reached the allocated-before-join barrier")
	}
	drain := lifetime.closeAdmission()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the admitted producer")
	}
	select {
	case <-drain:
		t.Fatal("cleanup passed allocated producer")
	default:
	}
	resume()
	select {
	case <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not finish after its join barrier was released")
	}
	select {
	case <-drain:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not complete after allocated producer exit")
	}
}

func TestRefViewLifetimeCloseAcquireLinearization(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		var lifetime refViewLifetime
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		var ctx context.Context
		var finish func()
		var acquireErr error
		var drain <-chan struct{}
		go func() {
			defer workers.Done()
			<-start
			ctx, finish, acquireErr = lifetime.admit(context.Background(), true)
		}()
		go func() { defer workers.Done(); <-start; drain = lifetime.closeAdmission() }()
		close(start)
		workers.Wait()
		if acquireErr == nil {
			<-ctx.Done()
			select {
			case <-drain:
				t.Fatal("admitted producer was not counted")
			default:
			}
			finish()
		} else if !errors.Is(acquireErr, ErrRefViewManagerClosed) {
			t.Fatal(acquireErr)
		}
		<-drain
		if _, _, err := lifetime.admit(context.Background(), false); !errors.Is(err, ErrRefViewManagerClosed) {
			t.Fatal("reopened after close")
		}
	}
}

func TestRefViewLifetimeHeartbeatTailPrecedesDrain(t *testing.T) {
	var lifetime refViewLifetime
	_, finish, err := lifetime.admit(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	heartbeatStopping := make(chan struct{})
	heartbeatStopped := make(chan struct{})
	allowHeartbeatStop := make(chan struct{})
	producerDone := make(chan struct{})
	var stopOnce sync.Once
	allowStop := func() { stopOnce.Do(func() { close(allowHeartbeatStop) }) }
	// Installed before the actor can enter its deliberately blocked tail.
	t.Cleanup(func() {
		lifetime.closeAdmission()
		allowStop()
		select {
		case <-producerDone:
		case <-time.After(5 * time.Second):
			t.Error("heartbeat producer did not join during cleanup")
		}
		finish()
	})
	go func() {
		defer close(producerDone)
		defer finish()
		defer func() {
			close(heartbeatStopping)
			<-allowHeartbeatStop
			close(heartbeatStopped)
		}()
	}()
	select {
	case <-heartbeatStopping:
	case <-time.After(5 * time.Second):
		t.Fatal("producer never reached the heartbeat tail barrier")
	}
	drain := lifetime.closeAdmission()
	select {
	case <-drain:
		t.Fatal("drain passed heartbeat tail")
	default:
	}
	allowStop()
	select {
	case <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not exit after heartbeat release")
	}
	select {
	case <-drain:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not complete after heartbeat producer exit")
	}
	select {
	case <-heartbeatStopped:
	default:
		t.Fatal("drain preceded heartbeat stop")
	}
}

func TestRefViewLifetimeClosingPrefixCannotCreateReplacement(t *testing.T) {
	lifecycle := &CheckoutLifecycle{}
	drain := lifecycle.closeRepositoryRefViews("repo")
	<-drain.done
	if _, err := lifecycle.refViewManager("repo", nil); !errors.Is(err, ErrRefViewManagerClosed) {
		t.Fatalf("replacement admitted: %v", err)
	}
	lifecycle.closeAllRefViews()
	if err := lifecycle.finalizeRepositoryRefViews(drain); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.refViewManager("repo", nil); !errors.Is(err, ErrRefViewManagerClosed) {
		t.Fatalf("finalization reopened shutdown: %v", err)
	}
}

func TestRefViewLifetimeFinalizerCannotRemoveReplacement(t *testing.T) {
	lifecycle := &CheckoutLifecycle{}
	old := lifecycle.closeRepositoryRefViews("repo")
	if err := lifecycle.finalizeRepositoryRefViews(old); err != nil {
		t.Fatal(err)
	}
	replacement := &RefViewManager{}
	lifecycle.refViews = map[string]*RefViewManager{"repo": replacement}
	if err := lifecycle.finalizeRepositoryRefViews(old); err != nil {
		t.Fatal(err)
	}
	if lifecycle.refViews["repo"] != replacement {
		t.Fatal("old finalizer removed replacement")
	}
	other := &CheckoutLifecycle{}
	if err := other.finalizeRepositoryRefViews(old); err == nil {
		t.Fatal("foreign finalizer accepted")
	}
	<-replacement.CloseAdmission()
}
