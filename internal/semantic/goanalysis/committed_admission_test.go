package goanalysis

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/semantic"
)

// committedContext is the context of a committed tree's pass whose preempt
// cancels it, as the manager builds it.
func committedContext(t *testing.T) (context.Context, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	preempted := &atomic.Int32{}
	scope := semantic.CheckoutCompilerScope{Committed: true, Preempt: func() {
		preempted.Add(1)
		cancel(semantic.ErrCommittedPassPreempted)
	}}
	return semantic.WithCheckoutCompilerScope(ctx, scope), preempted
}

// An edit's load that finds the admission held by a committed tree's pass
// ends that pass and takes the admission as soon as the pass lets it go.
func TestEditLoadOvertakesACommittedPass(t *testing.T) {
	provider := &Provider{heavyGate: make(chan struct{}, 1), largeGate: make(chan struct{}, 1)}
	committedCtx, preempted := committedContext(t)
	releaseCommitted, err := provider.acquireHeavy(committedCtx, true)
	if err != nil {
		t.Fatalf("committed acquire: %v", err)
	}
	// The committed pass lets its admission go when its context ends, as a
	// cancelled load does.
	go func() {
		<-committedCtx.Done()
		releaseCommitted()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := provider.acquireHeavy(ctx, false)
	if err != nil {
		t.Fatalf("the edit's load waited behind the committed pass: %v", err)
	}
	release()
	if preempted.Load() != 1 {
		t.Errorf("preempted %d times, want 1", preempted.Load())
	}
	if cause := context.Cause(committedCtx); cause != semantic.ErrCommittedPassPreempted {
		t.Errorf("committed pass cause %v, want %v", cause, semantic.ErrCommittedPassPreempted)
	}
}

// A load that finds the admission free preempts nothing.
func TestFreeAdmissionPreemptsNothing(t *testing.T) {
	provider := &Provider{heavyGate: make(chan struct{}, 2), largeGate: make(chan struct{}, 1)}
	committedCtx, preempted := committedContext(t)
	releaseCommitted, err := provider.acquireHeavy(committedCtx, true)
	if err != nil {
		t.Fatalf("committed acquire: %v", err)
	}
	defer releaseCommitted()
	release, err := provider.acquireHeavy(context.Background(), false)
	if err != nil {
		t.Fatalf("edit acquire: %v", err)
	}
	release()
	if preempted.Load() != 0 {
		t.Errorf("a free admission preempted the committed pass %d times", preempted.Load())
	}
}

// A committed pass queued for the admission before an edit's load gives it
// back when it gets it while the edit's load waits: the edit's load found
// nothing to preempt (the pass was queued, not holding), so the pass must
// not keep what the queue order handed it.
func TestCommittedPassGivesWayToAWaitingEdit(t *testing.T) {
	provider := &Provider{heavyGate: make(chan struct{}, 1), largeGate: make(chan struct{}, 1)}
	holder, err := provider.acquireHeavy(context.Background(), false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	committedCtx, preempted := committedContext(t)
	committedDone := make(chan func(), 1)
	go func() {
		release, err := provider.acquireHeavy(committedCtx, false)
		if err != nil {
			t.Errorf("committed acquire: %v", err)
		}
		committedDone <- release
	}()
	// Let the committed pass queue on the admission first.
	time.Sleep(50 * time.Millisecond)
	editDone := make(chan func(), 1)
	go func() {
		release, err := provider.acquireHeavy(context.Background(), false)
		if err != nil {
			t.Errorf("edit acquire: %v", err)
		}
		editDone <- release
	}()
	for provider.committed.interactive.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	holder()
	select {
	case release := <-editDone:
		release()
	case release := <-committedDone:
		release()
		t.Fatal("the committed pass kept the admission while an edit's load waited for it")
	case <-time.After(5 * time.Second):
		t.Fatal("neither load was admitted")
	}
	select {
	case release := <-committedDone:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the committed pass was never admitted after the edit")
	}
	if preempted.Load() != 0 {
		t.Errorf("a pass that held nothing was preempted %d times", preempted.Load())
	}
}
