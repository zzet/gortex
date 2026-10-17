package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A real context.AfterFunc registration seam, reached by actual RehomeTo only
// after admission. It pauses before the first build-gate/catalog operation.
type rehomeAdmissionLifetime struct {
	context.Context
	linked    chan struct{}
	allowLink chan struct{}
	once      sync.Once
}

// Hide the embedded cancelCtx's private lookup so context.AfterFunc uses this
// wrapper's explicit AfterFunc interface rather than bypassing the test seam.
func (*rehomeAdmissionLifetime) Value(any) any { return nil }

func (p *rehomeAdmissionLifetime) AfterFunc(fn func()) func() bool {
	p.once.Do(func() { close(p.linked) })
	<-p.allowLink
	return context.AfterFunc(p.Context, fn)
}

func TestCheckoutRehomeAfterCloseRefusesBeforeCatalogOrBuildAdmission(t *testing.T) {
	done := make(chan struct{})
	close(done)
	c := &CheckoutCoordinator{done: done}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Deliberately nil catalog, builder and gate: a stale externally retained
	// pointer must refuse before touching any of them, including nil lifetime.
	if _, err := c.RehomeTo(context.Background(), "retired-graph"); !errors.Is(err, context.Canceled) {
		t.Fatalf("late off-route build admitted: %v", err)
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("late call leaked admission count %d", active)
	}
}

func TestCheckoutCloseJoinsAlreadyAdmittedExternalRehome(t *testing.T) {
	ctx, stopRequest := context.WithTimeout(context.Background(), time.Minute)
	base, cancel := context.WithCancel(context.Background())
	probe := &rehomeAdmissionLifetime{Context: base, linked: make(chan struct{}), allowLink: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(probe.allowLink) })
	done := make(chan struct{})
	close(done) // The loop is already drained; only the external actor remains.
	c := &CheckoutCoordinator{done: done, lifetime: probe, cancelLifetime: cancel}
	var workers sync.WaitGroup
	// Unconditional cleanup precedes both workers and every assertion. In
	// particular, a failure of the second RehomeTo check cannot strand the first
	// actor in AfterFunc registration or Close waiting for its admission count.
	t.Cleanup(func() { cancel(); stopRequest(); unblock(); workers.Wait() })
	rehomeDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := c.RehomeTo(ctx, "graph")
		rehomeDone <- err
	}()
	select {
	case <-probe.linked:
	case err := <-rehomeDone:
		t.Fatalf("RehomeTo returned before lifetime-link barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 1 {
		t.Fatalf("RehomeTo not counted before gate/SQL: %d", active)
	}
	closeDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		closeDone <- c.Close()
	}()
	select {
	case <-base.Done(): // Close's permanent admission/cancellation fence committed.
	case err := <-closeDone:
		t.Fatalf("Close returned before its lifetime fence: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close passed admitted external actor: %v", err)
	default:
	}
	if _, err := c.RehomeTo(ctx, "graph"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second actor crossed closing fence: %v", err)
	}
	unblock()
	select {
	case err := <-rehomeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admitted actor did not cancel before catalog: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	workers.Wait()
	c.mu.Lock()
	active, drained := c.sourceMutations, c.sourceMutationsDrained
	c.mu.Unlock()
	if active != 0 || drained != nil {
		t.Fatalf("external lifetime not bounded after drain: active=%d drained=%v", active, drained)
	}
}

func TestCheckoutRehomeCanceledContextReleasesAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &CheckoutCoordinator{}
	if _, err := c.RehomeTo(ctx, "graph"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call=%v", err)
	}
	c.mu.Lock()
	active, drained := c.sourceMutations, c.sourceMutationsDrained
	c.mu.Unlock()
	if active != 0 || drained != nil {
		t.Fatalf("early return leaked external actor: active=%d drained=%v", active, drained)
	}
}
