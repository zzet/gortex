package goanalysis

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// pauseListing is one module-batch listing a fake load saw: what answer-path
// work was in flight when it began.
type pauseListing struct {
	busy      string
	toolCalls int64
}

// recordWarmupListings installs a fake load that records every warm-up
// listing and then lists for real. onListing lets a test start
// answer-path work as a listing begins; the func it returns runs as that
// listing ends.
func recordWarmupListings(p *Provider, activity *fakeForeground, toolCalls *atomic.Int64, onListing func(n int) func()) (func() []pauseListing, *sync.Mutex) {
	var mu sync.Mutex
	var seen []pauseListing
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			busy, _ := activity.ForegroundWork()
			mu.Lock()
			seen = append(seen, pauseListing{busy: busy, toolCalls: toolCalls.Load()})
			n := len(seen)
			mu.Unlock()
			if onListing != nil {
				if after := onListing(n); after != nil {
					defer after()
				}
			}
		}
		return packages.Load(cfg, patterns...)
	}
	return func() []pauseListing {
		mu.Lock()
		defer mu.Unlock()
		return append([]pauseListing(nil), seen...)
	}, &mu
}

// TestWarmupListingsPauseWhileAnEditHoldsTheLane: a forced warm-up (one that
// stopped waiting for an idle daemon) lists no module batch while an edit
// cycle holds the lane, neither before its first listing nor between
// listings, and resumes once the lane is free, keeping its queue.
func TestWarmupListingsPauseWhileAnEditHoldsTheLane(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	activity.set("checkout_mutation")
	p := scheduledProvider(t, activity)
	p.warm.maxDeferral = 100 * time.Millisecond
	p.warm.maxPause = time.Minute
	p.warm.batchSize = 1
	var toolCalls atomic.Int64
	p.warm.toolCalls = toolCalls.Load
	listings, _ := recordWarmupListings(p, activity, &toolCalls, func(n int) func() {
		if n != 1 {
			return nil
		}
		// An edit takes the lane while the first batch lists; it holds
		// the lane for a while after that listing ends.
		activity.set("checkout_cycle")
		return func() {
			go func() {
				time.Sleep(400 * time.Millisecond)
				activity.set("")
			}()
		}
	})

	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	// Planning is not held back; the first module batch is.
	require.Eventually(t, func() bool {
		status := warmupStatus(p, root)
		return status.Forced && status.Remaining > 0
	}, time.Minute, 10*time.Millisecond, "the warm-up was never forced and planned")
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, listings(), "a forced warm-up listed while an edit held the lane")
	status := warmupStatus(p, root)
	require.True(t, status.Forced, "the fixture must force the warm-up: %+v", status)
	require.Equal(t, "checkout_mutation", status.DeferredBy)

	activity.set("")
	status = waitWarm(t, p, root)
	got := listings()
	require.GreaterOrEqual(t, len(got), 2, "the fixture must list more than one batch")
	for i, l := range got {
		require.Empty(t, l.busy, "listing %d ran while %q held the lane", i+1, l.busy)
	}
	require.GreaterOrEqual(t, status.Pauses, 2, "the warm-up paused before its first listing and between listings: %+v", status)
	require.Equal(t, len(got), status.Attempts, "a pause restarted a listing")
	require.Zero(t, status.Preemptions)
}

// TestWarmupListingsPauseWhileAToolCallIsInFlight: on an otherwise idle
// daemon, the next module batch waits for a tool call in flight.
func TestWarmupListingsPauseWhileAToolCallIsInFlight(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	p := scheduledProvider(t, activity)
	p.warm.batchSize = 1
	var toolCalls atomic.Int64
	p.warm.toolCalls = toolCalls.Load
	listings, _ := recordWarmupListings(p, activity, &toolCalls, func(n int) func() {
		if n != 1 {
			return nil
		}
		toolCalls.Add(1)
		return func() {
			go func() {
				time.Sleep(400 * time.Millisecond)
				toolCalls.Add(-1)
			}()
		}
	})

	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	status := waitWarm(t, p, root)
	got := listings()
	require.GreaterOrEqual(t, len(got), 2, "the fixture must list more than one batch")
	for i, l := range got[1:] {
		require.Zero(t, l.toolCalls, "listing %d ran while a tool call was in flight", i+2)
	}
	require.GreaterOrEqual(t, status.Pauses, 1, "%+v", status)
	require.Positive(t, status.PausedMs)
}

// TestWarmupPauseIsBounded: a tool call that never ends (one waiting on
// the warm-up, or a client holding a call open) holds a module batch back
// for the maximum deferral only.
func TestWarmupPauseIsBounded(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	p := scheduledProvider(t, activity)
	p.warm.maxDeferral = 200 * time.Millisecond
	var toolCalls atomic.Int64
	toolCalls.Store(1)
	p.warm.toolCalls = toolCalls.Load
	listings, _ := recordWarmupListings(p, activity, &toolCalls, nil)

	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	status := waitWarm(t, p, root)
	require.NotEmpty(t, listings())
	require.GreaterOrEqual(t, status.Pauses, 1, "%+v", status)
}

// A module batch whose background listing makes no progress within the stall
// deadline (a compile starved at the lowest priority) is retried at normal
// priority, and the warm-up completes.
func TestWarmupRetriesAStalledBackgroundListingAtNormalPriority(t *testing.T) {
	background, normal := warmupBuildFlags(), targetedWarmupBuildFlags()
	if strings.Join(background, " ") == strings.Join(normal, " ") {
		t.Skip("background and normal priority listings are indistinguishable on this host")
	}
	root := typecheckCacheFixture(t)
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	p.warm.poll = 10 * time.Millisecond
	p.warm.stall = 300 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	var mu sync.Mutex
	var stalled, retried int
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			switch strings.Join(cfg.BuildFlags, " ") {
			case strings.Join(background, " "):
				mu.Lock()
				first := stalled == 0
				if first {
					stalled++
				}
				mu.Unlock()
				if first {
					// A child that never progresses: only the deadline ends it.
					<-cfg.Context.Done()
					return nil, cfg.Context.Err()
				}
			case strings.Join(normal, " "):
				mu.Lock()
				retried++
				mu.Unlock()
			}
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	status := waitWarm(t, p, root)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, stalled, "the fixture must stall one background listing")
	require.GreaterOrEqual(t, retried, 1, "the stalled batch was not retried at normal priority")
	require.Equal(t, 1, status.Escalations)
}
