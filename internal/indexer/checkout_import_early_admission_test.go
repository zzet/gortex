package indexer

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// One registered foreground request is enough to exercise the real initial
// grant boundary. There is no paced load, injected delay, wall-time latency or
// throughput assertion; the separate performance flow retains those oracles.
func TestImportQueuedInitialGrantPublishesWithoutPeriodicDemand(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 4
	t.Cleanup(func() { importInteractivePaths = oldPaths })
	fixture := newUnpublishedCommittedBaseFixture(t)
	fixture.publishBase(t)
	f := fixture.coordinatorFixture
	gate := NewViewBuildGate()
	gate.Open()
	outcomes := make(chan CheckoutCycle)
	cycleContinue := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var importing, queued atomic.Bool
	var once sync.Once
	probeDone := make(chan struct{})
	probeResult := make(chan error, 1)
	var probe sync.WaitGroup
	c := f.coordinatorWithLogger(t, CheckoutCoordinatorConfig{Gate: gate,
		cycleDone: func(out CheckoutCycle) {
			select {
			case outcomes <- out:
			case <-ctx.Done():
				return
			}
			select {
			case <-cycleContinue:
			case <-ctx.Done():
			}
		},
		dirtyBarrier: func() {
			if importing.Load() {
				select {
				case <-probeDone:
				case <-ctx.Done():
				}
			}
		},
	}, zap.NewNop())
	t.Cleanup(func() { cancel(); _ = c.Close(); probe.Wait() })
	if !c.awaitRacyIndexHeal(30 * time.Second) {
		t.Fatal("startup Git index refresh did not join")
	}
	requireImportReadSetProof(t, c)
	await := func() CheckoutCycle {
		select {
		case out := <-outcomes:
			return out
		case <-ctx.Done():
			t.Fatalf("controlled import did not publish: %v", ctx.Err())
			return CheckoutCycle{}
		}
	}
	c.Signal("initial working-tree layer")
	if out := await(); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("initial route: %+v", out)
	}
	select {
	case cycleContinue <- struct{}{}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	builderGit(t, f.primary, "checkout", "-q", "-b", "controlled-import")
	const files = 5
	for i := 0; i < files; i++ {
		builderWriteFile(t, f.primary, fmt.Sprintf("controlled_%d.go", i), fmt.Sprintf("package fixture\nfunc Controlled%d() int{return %d}\n", i, i))
	}
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-q", "-m", "controlled import")
	builderGit(t, f.primary, "checkout", "-q", "main")
	builderGit(t, f.worktree, "checkout", "controlled-import", "--", ".")
	c.cycleMu.Lock()
	c.importAdmissionBarrier = func(cycleCtx context.Context) {
		if !importing.Load() {
			return
		}
		once.Do(func() {
			probe.Add(1)
			go func() {
				defer probe.Done()
				defer close(probeDone)
				release, err := gate.AcquireRanked(ctx, ViewBuildInteractive, nil, func() int64 { return -1 })
				if err == nil {
					err = f.store.AtGeneration(0).SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "controlled-foreground"})
					release()
				}
				probeResult <- err
			}()
			for {
				gate.mu.Lock()
				found := false
				for _, w := range gate.interactive {
					if w.rank != nil && w.rank() == -1 {
						found = true
						break
					}
				}
				gate.mu.Unlock()
				if found {
					queued.Store(true)
					return
				}
				if cycleCtx.Err() != nil || ctx.Err() != nil {
					return
				}
				runtime.Gosched()
			}
		})
	}
	c.cycleMu.Unlock()
	importing.Store(true)
	c.Signal("one queued request at initial import grant")
	out := await()
	if out.Err != nil || !out.DirtyBuilt || out.DirtyBatchRemaining != files-1 {
		t.Fatalf("queued initial grant canceled private import or lost its first file: %+v", out)
	}
	if !queued.Load() {
		t.Fatal("foreground request did not actually queue before private preparation")
	}
	select {
	case err := <-probeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("queued foreground SQL did not complete")
	}
	claimed := 0
	for _, p := range claimedPaths(t, f.store, out.DirtyGenerationID) {
		if strings.HasPrefix(strings.TrimPrefix(p, builderRepoPrefix+"/"), "controlled_") {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("first import published %d paths, want one", claimed)
	}
}
