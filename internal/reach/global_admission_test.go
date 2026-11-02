package reach

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Before repository-scoped admission is introduced, SQLite lookups use the
// same global gate and resolver mutex as the existing base-topology reader.
// Cancellation must report the actual failed mutex acquisition, not detached
// admission borrowed from the later repository-topology implementation.
func TestGlobalReachAdmissionRetainsSQLiteMutex(t *testing.T) {
	store := openTopologyScopeStore(t)
	store.AddNode(&graph.Node{ID: "seed", Name: "seed", Kind: graph.KindFunction})
	mu := store.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var waits []CoordinationWait
	ctx = WithCoordinationWaitObserver(ctx, func(wait CoordinationWait) {
		waits = append(waits, wait)
	})
	_, _, _, _, truncated := LookupContext(ctx, store, "seed")
	if !truncated {
		t.Fatal("cancelled lookup must retain its conservative result")
	}
	for _, wait := range waits {
		if wait.Phase == "resolve_mutex_wait" {
			if !wait.Attempted || wait.Acquired {
				t.Fatalf("held SQLite mutex must report a cancelled acquisition: %+v", wait)
			}
			return
		}
	}
	t.Fatalf("actual SQLite mutex admission was not observed: %+v", waits)
}
