package goanalysis

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A pass over a package whose targeted warm-up listing is in flight waits
// (bounded) for that listing and runs warm-served instead of listing the
// same closure a second time; and the recent handle roots that order the
// targeted listings survive a restart.

var helperHandle = retentionHandle("helper/helper.go", "Wrap", 9, 13)

// targetedWaitFixture starts a warm-up whose first (targeted) listing, of
// the dirty helper package, is held until release is closed (other is
// dirty too when bothDirty, and queued behind it). It returns the provider,
// the channel releasing the held listing, a counter of the listings passes
// ran and one of the warm-up's listings naming helper.
func targetedWaitFixture(t *testing.T, bothDirty bool) (string, *Provider, chan struct{}, func() int, func() int) {
	t.Helper()
	root := typecheckCacheFixture(t)
	gitFixture(t, root)
	writeFile(t, root, "helper/helper.go", readFixture(t, root, "helper/helper.go")+"\n// dirty\n")
	if bothDirty {
		writeFile(t, root, "other/other.go", readFixture(t, root, "other/other.go")+"\n// dirty\n// more\n")
	}
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	var (
		mu          sync.Mutex
		passLists   int
		helperLists int
		first       = true
		started     = make(chan struct{})
		release     = make(chan struct{})
		startedOnce sync.Once
	)
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if cfg.Mode&packages.NeedExportFile != 0 {
			if !warmupListingFlags(cfg) {
				mu.Lock()
				passLists++
				mu.Unlock()
			} else {
				mu.Lock()
				for _, pattern := range patterns {
					if pattern == "example.com/scope/helper" {
						helperLists++
					}
				}
				hold := first
				first = false
				mu.Unlock()
				if hold {
					startedOnce.Do(func() { close(started) })
					select {
					case <-release:
					case <-cfg.Context.Done():
						return nil, cfg.Context.Err()
					}
				}
			}
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the targeted listing never started")
	}
	count := func(n *int) func() int {
		return func() int {
			mu.Lock()
			defer mu.Unlock()
			return *n
		}
	}
	return root, p, release, count(&passLists), count(&helperLists)
}

var otherHandle = retentionHandle("other/other.go", "Other", 3, 5)

// TestPassAdoptsItsInFlightTargetedListing: the edit's pass joins the
// warm-up's running listing of its package and is served from it: one
// listing in total. helper's closure is below the threshold: a listing
// already running is adopted all the same, never restarted.
func TestPassAdoptsItsInFlightTargetedListing(t *testing.T) {
	root, p, release, passLists, helperLists := targetedWaitFixture(t, false)
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	c := retentionPass(t, p, root, helperHandle, true, "first touch during the targeted listing")
	require.Equal(t, "adopted", c.TargetedWait)
	require.Greater(t, c.TargetedWaitMs, int64(0))
	require.Equal(t, 0, passLists(), "the pass never lists the closure itself")
	require.Equal(t, 1, helperLists())
	require.Equal(t, 0, c.ClosureMisses, "miss %q", c.MissReason)
	require.True(t, c.WarmServed)
}

// TestPassAdoptionOutlastsTheBound: a targeted listing that runs longer
// than the wait bound is still adopted: one listing in total, and the
// pass's wait is the listing's remaining time (never the bound plus a
// second listing).
func TestPassAdoptionOutlastsTheBound(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_TARGETED_WAIT", "200ms")
	t.Setenv("GORTEX_GOTYPES_TARGETED_WAIT_MIN_FILES", "0")
	root, p, release, passLists, helperLists := targetedWaitFixture(t, false)
	passStart := time.Now()
	released := passStart.Add(1200 * time.Millisecond)
	time.AfterFunc(time.Until(released), func() { close(release) })
	c := retentionPass(t, p, root, helperHandle, true, "first touch, listing longer than the bound")
	require.Equal(t, "adopted", c.TargetedWait)
	require.Equal(t, 0, passLists(), "no second listing after the bound")
	require.Equal(t, 1, helperLists(), "one listing of the closure in total")
	remaining := released.Sub(passStart) // the listing's remaining time when the pass began
	require.GreaterOrEqual(t, c.TargetedWaitMs, remaining.Milliseconds()-100)
	require.Less(t, c.TargetedWaitMs, remaining.Milliseconds()+5000)
}

// TestPassQueuedBehindAnotherTargetedListingIsBounded: a pass whose
// package is still queued behind another targeted listing waits at most
// the bound for its own listing to start, then lists itself.
func TestPassQueuedBehindAnotherTargetedListingIsBounded(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_TARGETED_WAIT", "200ms")
	t.Setenv("GORTEX_GOTYPES_TARGETED_WAIT_MIN_FILES", "0")
	root, p, release, passLists, _ := targetedWaitFixture(t, true)
	defer close(release)
	c := retentionPass(t, p, root, otherHandle, true, "first touch of a queued package")
	require.Equal(t, "timeout", c.TargetedWait)
	require.Equal(t, 1, passLists())
}

// TestPassListsSmallClosuresItself: a package whose closure is below the
// threshold and whose listing has not started (it is queued behind
// another targeted listing) is listed by the pass at once, and the warm-up
// stops holding passes back for it.
func TestPassListsSmallClosuresItself(t *testing.T) {
	root, p, release, passLists, _ := targetedWaitFixture(t, true)
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	defer releaseOnce()
	// A pass that waited would be released after 5 s (and fail below).
	time.AfterFunc(5*time.Second, releaseOnce)
	c := retentionPass(t, p, root, otherHandle, true, "first touch of a small queued package")
	require.Equal(t, "small", c.TargetedWait)
	require.Less(t, c.TargetedWaitMs, int64(1000))
	require.Equal(t, 1, passLists())
	loadDir, _ := p.warmupEligible(root)
	p.warm.mu.Lock()
	defer p.warm.mu.Unlock()
	require.False(t, p.warm.entries[loadDir].pending[resolvedDir(root+"/other")], "the pass's listing retired the pending package")
}

// TestRecentRootsSurviveARestart: a new provider (a restarted daemon)
// reads the persisted recent roots, so the warm-up targets them first.
func TestRecentRootsSurviveARestart(t *testing.T) {
	root := typecheckCacheFixture(t)
	dir := t.TempDir()
	before := newTestProvider(t)
	before.SetRecentRootsDir(dir)
	before.warm.noteRoots(root, map[string]struct{}{root + "/other": {}})
	before.warm.noteRoots(root, map[string]struct{}{root + "/helper": {}})
	before.warm.persists.Wait()

	after := newTestProvider(t)
	after.SetRecentRootsDir(dir)
	queue, targeted, err := after.planWarmup(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, 2, targeted)
	require.Equal(t, []string{"example.com/scope/helper", "example.com/scope/other"}, queue[:2], "the persisted order leads: %v", queue)

	// Tests never persist into the daemon's cache directory by default.
	quiet := newTestProvider(t)
	quiet.warm.mu.Lock()
	defer quiet.warm.mu.Unlock()
	require.Empty(t, quiet.warm.rootsDirLocked())
}

// TestPassWaitsDuringEnumerationForALargeDirtyPackage: while the warm-up
// is still enumerating the module, a pass over a dirty package whose
// closure is above the threshold already waits for its listing (and then
// adopts it) instead of listing itself.
func TestPassWaitsDuringEnumerationForALargeDirtyPackage(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_TARGETED_WAIT_MIN_FILES", "1")
	root := typecheckCacheFixture(t)
	gitFixture(t, root)
	writeFile(t, root, "helper/helper.go", readFixture(t, root, "helper/helper.go")+"\n// dirty\n")
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	var (
		mu         sync.Mutex
		passLists  int
		enumerate  = make(chan struct{})
		enumerated = make(chan struct{})
		once       sync.Once
	)
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		switch {
		case cfg.Mode&packages.NeedExportFile != 0 && !warmupListingFlags(cfg):
			mu.Lock()
			passLists++
			mu.Unlock()
		case cfg.Mode&packages.NeedExportFile == 0 && len(patterns) == 1 && patterns[0] == "./...":
			once.Do(func() { close(enumerate) })
			<-enumerated
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	select {
	case <-enumerate:
	case <-time.After(30 * time.Second):
		t.Fatal("the enumeration never started")
	}
	time.AfterFunc(300*time.Millisecond, func() { close(enumerated) })
	c := retentionPass(t, p, root, helperHandle, true, "first touch during the enumeration")
	require.Contains(t, []string{"adopted", "merged"}, c.TargetedWait)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 0, passLists, "the pass never lists the closure itself")
	require.True(t, c.WarmServed)
}
