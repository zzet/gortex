package goanalysis

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// fakeForeground is a settable foreground-activity view.
type fakeForeground struct {
	mu      sync.Mutex
	busy    string
	last    time.Time
	touched map[string]bool
	polls   int
}

func (f *fakeForeground) ForegroundWork() (string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.busy, f.last
}

func (f *fakeForeground) CheckoutTouched(root string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touched[filepath.Clean(root)]
}

// set starts (busy != "") or ends foreground work; either is the latest
// instant foreground work was seen.
func (f *fakeForeground) set(busy string) {
	f.mu.Lock()
	f.busy = busy
	f.last = time.Now()
	f.mu.Unlock()
}

func (f *fakeForeground) touch(root string) {
	f.mu.Lock()
	if f.touched == nil {
		f.touched = map[string]bool{}
	}
	f.touched[filepath.Clean(root)] = true
	f.mu.Unlock()
}

// warmupListingFlags reports whether a packages.Load is a warm-up listing
// (a pass's listing carries no -p flag).
func warmupListingFlags(cfg *packages.Config) bool {
	return len(cfg.BuildFlags) >= 1 && strings.HasPrefix(cfg.BuildFlags[0], "-p=")
}

func scheduledProvider(t *testing.T, activity *fakeForeground) *Provider {
	t.Helper()
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	p.warm.poll = 10 * time.Millisecond
	p.SetWarmupIdle(150*time.Millisecond, 600*time.Millisecond)
	p.SetForegroundActivity(activity)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func warmupStatus(p *Provider, root string) CheckoutWarmupStatus {
	loadDir, _ := p.warmupEligible(root)
	return p.CheckoutWarmup(loadDir)
}

// TestTypecheckCacheWarmupWaitsForAnIdleDaemon: with the daemon's
// foreground-activity view installed, a warm-up asked for while foreground
// work is in flight lists nothing until that work is over and the daemon has
// been idle for the checkout's idle period.
func TestTypecheckCacheWarmupWaitsForAnIdleDaemon(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	activity.set("refresh_ticket")
	p := scheduledProvider(t, activity)
	var mu sync.Mutex
	var listedAt time.Time
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			if listedAt.IsZero() {
				listedAt = time.Now()
			}
			mu.Unlock()
		}
		return packages.Load(cfg, patterns...)
	}

	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	require.True(t, listedAt.IsZero(), "the warm-up listed while foreground work was in flight")
	mu.Unlock()
	status := warmupStatus(p, root)
	require.Equal(t, warmupDeferred, status.State)
	require.Equal(t, "refresh_ticket", status.DeferredBy)
	require.True(t, status.Touched)

	activity.set("")
	cleared := time.Now()
	status = waitWarm(t, p, root)
	mu.Lock()
	defer mu.Unlock()
	require.False(t, listedAt.IsZero())
	require.GreaterOrEqual(t, listedAt.Sub(cleared), 150*time.Millisecond,
		"the warm-up did not wait for the idle period after the foreground work ended")
	require.Equal(t, 1, status.Attempts)
}

// TestTypecheckCacheWarmupOfAnUntouchedCheckoutWaitsLonger: a checkout nobody
// touched since the daemon started is warmed only after the longer idle
// period.
func TestTypecheckCacheWarmupOfAnUntouchedCheckoutWaitsLonger(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.set("checkout_cycle")
	p := scheduledProvider(t, activity)
	var mu sync.Mutex
	var listedAt time.Time
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			if listedAt.IsZero() {
				listedAt = time.Now()
			}
			mu.Unlock()
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	time.Sleep(50 * time.Millisecond)
	activity.set("")
	cleared := time.Now()
	status := waitWarm(t, p, root)
	require.False(t, status.Touched)
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, listedAt.Sub(cleared), 600*time.Millisecond,
		"an untouched checkout was warmed before the daemon had been idle for the untouched idle period")
}

// TestTypecheckCacheWarmupYieldsToForegroundWork: foreground work that
// appears while the listing runs cancels it (its go command dies with the
// context); the warm-up lists again once the daemon is idle.
func TestTypecheckCacheWarmupYieldsToForegroundWork(t *testing.T) {
	root := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(root)
	p := scheduledProvider(t, activity)
	var mu sync.Mutex
	listings := 0
	started := make(chan struct{})
	canceled := make(chan struct{})
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			listings++
			first := listings == 1
			mu.Unlock()
			if first {
				close(started)
				<-cfg.Context.Done()
				close(canceled)
				return nil, cfg.Context.Err()
			}
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the warm-up never started on an idle daemon")
	}
	activity.set("interactive_build")
	select {
	case <-canceled:
	case <-time.After(time.Minute):
		t.Fatal("foreground work did not cancel the running listing")
	}
	status := warmupStatus(p, root)
	require.Equal(t, 1, status.Preemptions)
	require.Equal(t, "interactive_build", status.DeferredBy)

	activity.set("")
	status = waitWarm(t, p, root)
	require.Equal(t, 2, status.Attempts)
	require.Equal(t, 1, status.Preemptions)
	mu.Lock()
	require.Equal(t, 2, listings)
	mu.Unlock()
}

// TestTypecheckCacheWarmupsRunOneAtATime: warm-ups of several checkouts never
// list concurrently, and a touched checkout asked for later goes before an
// untouched one asked for first.
func TestTypecheckCacheWarmupsRunOneAtATime(t *testing.T) {
	untouched := typecheckCacheFixture(t)
	touched := typecheckCacheFixture(t)
	activity := &fakeForeground{}
	activity.touch(touched)
	activity.set("checkout_cycle")
	p := scheduledProvider(t, activity)
	p.SetWarmupIdle(50*time.Millisecond, 50*time.Millisecond)
	var mu sync.Mutex
	running, maxRunning := 0, 0
	var order []string
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			running++
			if running > maxRunning {
				maxRunning = running
			}
			order = append(order, filepath.Clean(cfg.Dir))
			mu.Unlock()
			defer func() {
				mu.Lock()
				running--
				mu.Unlock()
			}()
			time.Sleep(100 * time.Millisecond)
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(untouched, cachedScope))
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(touched, cachedScope))
	time.Sleep(50 * time.Millisecond)
	activity.set("")
	waitWarm(t, p, untouched)
	waitWarm(t, p, touched)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, maxRunning, "two warm-up listings ran at once")
	require.Len(t, order, 2)
	touchedDir, _ := p.warmupEligible(touched)
	require.Equal(t, filepath.Clean(touchedDir), order[0], "the touched checkout was not warmed first: %v", order)
}

// TestWarmupBuildFlagsRunToolsAtLowPriority: the warm-up's go command runs
// its tools through the host's lowest-priority wrapper, and a real listing
// with those flags succeeds (the fixture tests above list through them too).
func TestWarmupBuildFlagsRunToolsAtLowPriority(t *testing.T) {
	flags := warmupBuildFlags()
	require.True(t, strings.HasPrefix(flags[0], "-p="), "flags %v", flags)
	switch runtime.GOOS {
	case "windows", "plan9", "js", "wasip1":
		require.Len(t, flags, 1, "flags %v", flags)
		return
	case "darwin":
		require.Contains(t, flags, "-toolexec=/usr/sbin/taskpolicy -b")
	default:
		if wrapper := lowPriorityToolexec(); wrapper != "" {
			require.Contains(t, flags, "-toolexec="+wrapper)
			require.True(t, strings.HasSuffix(wrapper, "nice -n 19"), "wrapper %q", wrapper)
		}
	}
	root := typecheckCacheFixture(t)
	cfg := &packages.Config{
		Mode:       packages.NeedName | packages.NeedExportFile,
		Dir:        root,
		BuildFlags: flags,
	}
	pkgs, err := packages.Load(cfg, "./...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	for _, pkg := range pkgs {
		require.Empty(t, pkg.Errors, "%s", pkg.PkgPath)
		require.NotEmpty(t, pkg.ExportFile, "%s has no export data", pkg.PkgPath)
	}
}
