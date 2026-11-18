package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// An edit-driven (interactive) build that starts waiting for the lane while
// a background cycle is in the middle of a long durable drain gets the lane
// within the bound: the drain stops at its next sub-batch boundary, the
// background build fails with the cancellation and publishes nothing.
func TestBackgroundDrainGivesTheLaneToAnEditWithinTheBound(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "func F%d(x int) int {\n\treturn G%d(x) + %d\n}\n\nfunc G%d(x int) int {\n\treturn x * %d\n}\n\n", i, i, i, i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/big\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := builderOpenStore(t, "drain-yield")

	gate := NewViewBuildGate()
	gate.Open()
	releaseBackground, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	ctx, yield := armBackgroundLaneYield(context.Background(), gate, 0)
	if yield == nil {
		t.Fatal("no yield armed for the background holder")
	}

	var (
		mu      sync.Mutex
		subs    int
		granted = make(chan struct{}, 1)
	)
	prevRows, prevHook := drainYieldRows, drainSubBatchHook
	drainYieldRows = 2
	drainSubBatchHook = func() {
		mu.Lock()
		subs++
		first := subs == 1
		mu.Unlock()
		if first {
			// The edit arrives while the drain is flushing.
			go func() {
				release, err := gate.Acquire(context.Background(), ViewBuildInteractive)
				if err != nil {
					t.Error(err)
					return
				}
				granted <- struct{}{}
				release()
			}()
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Error("background drain did not yield to the waiting edit")
			}
		}
	}
	t.Cleanup(func() { drainYieldRows, drainSubBatchHook = prevRows, prevHook })

	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix(builderRepoPrefix)
	_, indexErr := idx.IndexCtx(ctx, dir)
	idx.Close()
	yield.close()
	releaseBackground()

	select {
	case <-granted:
	case <-time.After(time.Minute):
		t.Fatal("the edit never got the lane")
	}
	mu.Lock()
	drained := subs
	mu.Unlock()
	if drained != 1 {
		t.Fatalf("drain ran %d sub-batches after yielding, want exactly one", drained)
	}
	if !yield.Yielded() || indexErr == nil || !errors.Is(indexErr, context.Canceled) {
		t.Fatalf("the background build did not stop for the lane: yielded=%v err=%v", yield.Yielded(), indexErr)
	}
}
