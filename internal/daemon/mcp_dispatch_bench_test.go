package daemon

import (
	"context"
	"testing"
)

// BenchmarkDispatchAdmission isolates admission/release bookkeeping, including
// the production metrics path. semaphore_only is a primitive lower bound, not
// the former full dispatcher or an end-to-end performance comparison.
func BenchmarkDispatchAdmission(b *testing.B) {
	b.Run("semaphore_only", func(b *testing.B) {
		slots := newMCPDispatchSlots(8)
		b.ReportAllocs()
		for b.Loop() {
			slots <- struct{}{}
			<-slots
		}
	})
	for _, class := range [...]MCPDispatchClass{MCPDispatchFast, MCPDispatchMutating, MCPDispatchSlow} {
		b.Run(class.String(), func(b *testing.B) {
			owner := &mcpDispatchCoordinator{}
			pool := owner.pool(newMCPDispatchSlots(8))
			ctx := withMCPDispatchQueueWait(context.Background(), 0)
			b.ReportAllocs()
			for b.Loop() {
				grant, err := pool.acquire(ctx, class, "one-session")
				if err != nil {
					b.Fatal(err)
				}
				grant.complete()
			}
		})
	}
}
