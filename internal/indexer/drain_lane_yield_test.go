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
	const bound = 2 * time.Second
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
		mu        sync.Mutex
		subs      int
		requested time.Time
		granted   = make(chan time.Time, 1)
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
			mu.Lock()
			requested = time.Now()
			mu.Unlock()
			go func() {
				release, err := gate.Acquire(context.Background(), ViewBuildInteractive)
				if err != nil {
					t.Error(err)
					return
				}
				granted <- time.Now()
				release()
			}()
		}
		time.Sleep(40 * time.Millisecond) // a slow flush, as on a loaded host
	}
	t.Cleanup(func() { drainYieldRows, drainSubBatchHook = prevRows, prevHook })

	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix(builderRepoPrefix)
	_, indexErr := idx.IndexCtx(ctx, dir)
	idx.Close()
	yield.close()
	releaseBackground()

	var got time.Time
	select {
	case got = <-granted:
	case <-time.After(time.Minute):
		t.Fatal("the edit never got the lane")
	}
	mu.Lock()
	waited, drained := got.Sub(requested), subs
	mu.Unlock()
	if requested.IsZero() {
		t.Fatal("the fixture's drain never ran a sub-batch")
	}
	if waited > bound {
		t.Fatalf("the edit waited %s for the lane (drain ran %d sub-batches), want <= %s", waited, drained, bound)
	}
	if !yield.Yielded() || indexErr == nil || !errors.Is(indexErr, context.Canceled) {
		t.Fatalf("the background build did not stop for the lane: yielded=%v err=%v", yield.Yielded(), indexErr)
	}
}
