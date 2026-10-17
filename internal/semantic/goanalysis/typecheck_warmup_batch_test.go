package goanalysis

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// TestWarmupPlanOrdersRecentRootsThenLargest: the warm-up lists the
// checkout's recent handle roots first, then the packages with the most
// files, whose first pass would pay the longest listing.
func TestWarmupPlanOrdersRecentRootsThenLargest(t *testing.T) {
	root := typecheckCacheFixture(t)
	p := newTestProvider(t)
	queue, targeted, err := p.planWarmup(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 0, targeted, "no dirty file, no recent root")
	require.Equal(t, []string{"example.com/scope/impl", "example.com/scope/api", "example.com/scope/helper", "example.com/scope/other"}, queue,
		"impl has two files; the rest one each, by path")

	p.warm.noteRoots(root, map[string]struct{}{filepath.Join(root, "other"): {}})
	p.warm.noteRoots(root, map[string]struct{}{filepath.Join(root, "helper"): {}})
	queue, targeted, err = p.planWarmup(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 2, targeted)
	require.Equal(t, []string{"example.com/scope/helper", "example.com/scope/other", "example.com/scope/impl", "example.com/scope/api"}, queue,
		"the newest handle root first, then the older one, then by size")
}

// TestTypecheckCacheWarmupBatchesKeepProgress: the warm-up lists the module
// a batch at a time and merges each batch as it completes, so a pass that
// preempts it mid-way loses only the batch in flight: the resumed warm-up
// never lists a merged batch again, and ends warm.
func TestTypecheckCacheWarmupBatchesKeepProgress(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	cached.warm.quiet = 50 * time.Millisecond
	cached.warm.batchSize = 1
	t.Cleanup(func() { _ = cached.Close() })
	var (
		mu       sync.Mutex
		batches  [][]string
		started  = make(chan struct{})
		canceled = make(chan struct{})
	)
	cached.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			batches = append(batches, append([]string(nil), patterns...))
			second := len(batches) == 2
			mu.Unlock()
			if second {
				close(started)
				<-cfg.Context.Done()
				close(canceled)
				return nil, cfg.Context.Err()
			}
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	<-started
	// The first batch (impl, the largest package) is merged already.
	st := cached.typecheckState(root, goManifestDigest(root))
	st.mu.Lock()
	require.NotNil(t, st.meta["example.com/scope/impl"], "the first batch was merged before the second started")
	st.mu.Unlock()

	handlePassMatchesFull(t, "pass during the warm-up", cached, root, otherHandleGraph, "ext::go:strings::Builder")
	select {
	case <-canceled:
	case <-time.After(time.Minute):
		t.Fatal("the pass's listing did not preempt the warm-up")
	}
	status := waitWarm(t, cached, root)
	require.Equal(t, warmupWarm, status.State)
	require.Equal(t, 1, status.Preemptions)
	require.Equal(t, 0, status.Remaining)
	mu.Lock()
	defer mu.Unlock()
	listed := map[string]int{}
	for _, b := range batches {
		for _, path := range b {
			listed[path]++
		}
	}
	require.Equal(t, 1, listed["example.com/scope/impl"], "a merged batch is never listed again: %v", batches)
	require.Equal(t, 0, listed["example.com/scope/helper"], "impl's batch listed helper with its dependencies: %v", batches)
	require.Equal(t, 0, listed["example.com/scope/other"], "the pass listed other: %v", batches)
}

// TestTypecheckCacheWarmupStopsWaitingForABusyDaemon: foreground work that
// never ends (other checkouts' builds) defers a touched checkout's warm-up
// only up to the maximum deferral; then it lists anyway and ends warm, and
// its first packages are pre-parsed, so the first pass over them reuses
// their siblings' syntax.
func TestTypecheckCacheWarmupStopsWaitingForABusyDaemon(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	activity.set("checkout_build")
	p := scheduledProvider(t, activity)
	p.warm.maxDeferral = 300 * time.Millisecond

	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	time.Sleep(100 * time.Millisecond)
	status := warmupStatus(p, root)
	require.Equal(t, warmupDeferred, status.State, "busy daemon: deferred first")
	status = waitWarm(t, p, root)
	require.Equal(t, warmupWarm, status.State)
	require.True(t, status.Forced)
	require.Equal(t, 0, status.Preemptions, "a forced warm-up does not yield to other checkouts' work")

	c := handlePassMatchesFull(t, "first pass after the forced warm-up", p, root, scopeHandleGraph,
		"edge api/api.go::Measure -calls-> impl/impl.go::Double")
	require.Equal(t, 1, c.ClosureHits)
	require.True(t, c.WarmServed)
	require.GreaterOrEqual(t, c.FilesReused, 1, "impl's sibling was pre-parsed by the warm-up")

	// An untouched checkout is never forced.
	other := typecheckCacheFixture(t)
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(other, cachedScope))
	time.Sleep(700 * time.Millisecond)
	st := warmupStatus(p, other)
	require.Equal(t, warmupDeferred, st.State)
	require.False(t, st.Forced)
	require.True(t, strings.Contains(st.DeferredBy, "checkout_build") || st.DeferredBy == "idle", st.DeferredBy)
}
