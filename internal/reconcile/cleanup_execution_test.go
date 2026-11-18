package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Done is evaluated only by the waiting select in acquire. Err is inherited,
// so this is an exact queued-waiter barrier without polling or sleep.
type cleanupExecutionWaitContext struct {
	context.Context
	queued chan struct{}
	once   sync.Once
}

func (c *cleanupExecutionWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Done()
}

func awaitCleanupSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup barrier timed out")
	}
}

func TestCleanupExecutionStableIDExcludesSuccessorAndCleansState(t *testing.T) {
	var registry cleanupExecutionRegistry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldRelease, err := registry.acquire(ctx, "same-stable-id")
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelease()
	waiting := &cleanupExecutionWaitContext{Context: ctx, queued: make(chan struct{})}
	result := make(chan func(), 1)
	errResult := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := registry.acquire(waiting, "same-stable-id")
		if err != nil {
			errResult <- err
			return
		}
		result <- release
	}()
	t.Cleanup(func() { cancel(); oldRelease(); awaitCleanupSignal(t, done) })
	awaitCleanupSignal(t, waiting.queued)
	registry.mu.Lock()
	entry := registry.entries["same-stable-id"]
	refs, active := entry.refs, entry.active
	registry.mu.Unlock()
	if refs != 2 || !active {
		t.Fatalf("successor did not wait behind holder: refs=%d active=%v", refs, active)
	}
	select {
	case release := <-result:
		release()
		t.Fatal("successor entered old hook lifetime")
	default:
	}
	oldRelease()
	awaitCleanupSignal(t, done)
	select {
	case err := <-errResult:
		t.Fatal(err)
	default:
	}
	newRelease := <-result
	oldRelease() // retained idempotent release cannot affect the new holder
	registry.mu.Lock()
	active = registry.entries["same-stable-id"].active
	registry.mu.Unlock()
	if !active {
		t.Fatal("old release cleared successor")
	}
	newRelease()
	newRelease()
	registry.mu.Lock()
	remaining := len(registry.entries)
	registry.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("execution state leaked: %d", remaining)
	}
}

func TestCleanupExecutionCanceledWaiterDoesNotReleaseHolder(t *testing.T) {
	var registry cleanupExecutionRegistry
	release, err := registry.acquire(context.Background(), "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := &cleanupExecutionWaitContext{Context: ctx, queued: make(chan struct{})}
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		acquired, err := registry.acquire(waiting, "cleanup")
		if acquired != nil {
			acquired()
		}
		result <- err
	}()
	t.Cleanup(func() { cancel(); release(); awaitCleanupSignal(t, done) })
	awaitCleanupSignal(t, waiting.queued)
	cancel()
	awaitCleanupSignal(t, done)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	registry.mu.Lock()
	entry := registry.entries["cleanup"]
	refs, active := entry.refs, entry.active
	registry.mu.Unlock()
	if refs != 1 || !active {
		t.Fatalf("canceled waiter changed holder: refs=%d active=%v", refs, active)
	}
	release()
	if len(registry.entries) != 0 {
		t.Fatal("canceled queue leaked state")
	}
}

func TestCleanupExecutionIndependentIDsAndCanceledAdmission(t *testing.T) {
	var registry cleanupExecutionRegistry
	first, err := registry.acquire(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := registry.acquire(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	second()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := registry.acquire(ctx, "third"); release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller admitted: %v %v", release != nil, err)
	}
	first()
	if len(registry.entries) != 0 {
		t.Fatal("empty gate retained registrations")
	}
}

func BenchmarkCleanupExecutionUncontended(b *testing.B) {
	var registry cleanupExecutionRegistry
	ctx := context.Background()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		release, err := registry.acquire(ctx, "owner-cleanup")
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
	if len(registry.entries) != 0 {
		b.Fatal("benchmark retained completed owners")
	}
}
