package search

import (
	"context"
	"testing"
	"time"
)

type lockWaitLegacyBackend struct {
	bundleCalls       int
	scopedBundleCalls int
	vectorCalls       int
}

func (*lockWaitLegacyBackend) Add(string, ...string) {}
func (*lockWaitLegacyBackend) Remove(string)         {}
func (*lockWaitLegacyBackend) Search(string, int) []SearchResult {
	return nil
}
func (*lockWaitLegacyBackend) Count() int { return 1 }
func (*lockWaitLegacyBackend) Close()     {}

func (b *lockWaitLegacyBackend) SearchSymbolBundles(string, int) []SymbolBundle {
	b.bundleCalls++
	return nil
}

func (b *lockWaitLegacyBackend) SearchSymbolBundlesScoped(string, []string, int) []SymbolBundle {
	b.scopedBundleCalls++
	return nil
}

func (b *lockWaitLegacyBackend) VectorChannelOnly(string, int) ([]string, ChannelTimings) {
	b.vectorCalls++
	return nil, ChannelTimings{}
}

type lockWaitCancelContext struct {
	context.Context
	initialCheck chan struct{}
	canceled     <-chan struct{}
}

func (c *lockWaitCancelContext) Err() error {
	select {
	case <-c.canceled:
		return context.Canceled
	default:
	}
	select {
	case c.initialCheck <- struct{}{}:
	default:
	}
	return nil
}

func TestSwappableContextForwardersRecheckCancellationAfterLockWait(t *testing.T) {
	tests := []struct {
		name  string
		call  func(*Swappable, context.Context)
		calls func(*lockWaitLegacyBackend) int
	}{
		{
			name: "bundles",
			call: func(swappable *Swappable, ctx context.Context) {
				swappable.SearchSymbolBundlesContext(ctx, "query", 5)
			},
			calls: func(backend *lockWaitLegacyBackend) int {
				return backend.bundleCalls
			},
		},
		{
			name: "scoped bundles",
			call: func(swappable *Swappable, ctx context.Context) {
				swappable.SearchSymbolBundlesScopedContext(ctx, "query", []string{"repo"}, 5)
			},
			calls: func(backend *lockWaitLegacyBackend) int {
				return backend.scopedBundleCalls
			},
		},
		{
			name: "vector",
			call: func(swappable *Swappable, ctx context.Context) {
				swappable.VectorChannelOnlyContext(ctx, "query", 5)
			},
			calls: func(backend *lockWaitLegacyBackend) int {
				return backend.vectorCalls
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &lockWaitLegacyBackend{}
			swappable := NewSwappable(inner)

			tt.call(swappable, context.Background())
			if got := tt.calls(inner); got != 1 {
				t.Fatalf("live legacy calls = %d, want 1", got)
			}

			initialCheck := make(chan struct{}, 1)
			canceled := make(chan struct{})
			ctx := &lockWaitCancelContext{
				Context:      context.Background(),
				initialCheck: initialCheck,
				canceled:     canceled,
			}
			done := make(chan struct{})

			swappable.mu.Lock()
			go func() {
				defer close(done)
				tt.call(swappable, ctx)
			}()

			select {
			case <-initialCheck:
			case <-time.After(2 * time.Second):
				close(canceled)
				swappable.mu.Unlock()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
				}
				t.Fatal("context forwarder did not reach its pre-lock cancellation check")
			}

			close(canceled)
			swappable.mu.Unlock()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("context forwarder did not return after the lock was released")
			}
			if got := tt.calls(inner); got != 1 {
				t.Fatalf("legacy calls after lock-wait cancellation = %d, want 1", got)
			}
		})
	}
}
