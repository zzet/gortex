package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

// warmAtReadyFixture is a primary and one linked worktree that is a Go module
// of its own, with the lifecycle's enrichment manager running the go/types
// provider and every coordinator built under the given index.dirty_chain
// block. The worktree is NOT activated: the test does that, so the checkout
// becoming ready is the event under test.
type warmAtReadyFixture struct {
	*familyFixture
	provider *goanalysis.Provider
}

func newWarmAtReadyFixture(t *testing.T, name string) *warmAtReadyFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	ctx := context.Background()

	main := f.gitRepo(name + "-main")
	writeFile(t, filepath.Join(main, "go.mod"), "module example.com/warmatready\n\ngo 1.22\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-q", "-m", "module")
	worktree := f.worktreeOf(main, name+"-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: name + "-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators, "the observed worktree is dormant until it is selected")

	family := &familyFixture{
		lifecycleFixture: f,
		main:             main,
		worktree:         worktree,
		mainPrefix:       tracked.Prefix,
		primaryGraph:     tracked.GraphID,
		familyID:         tracked.FamilyID,
	}
	family.automatic = family.otherCheckout(tracked.CheckoutID)

	// Installed only after the primary is indexed, so the one go/types
	// client is the checkout's coordinator and its warm-up.
	mgr := goTypesManager(t)
	var provider *goanalysis.Provider
	for _, p := range mgr.AllProviders() {
		if gp, ok := p.(*goanalysis.Provider); ok {
			provider = gp
		}
	}
	require.NotNil(t, provider, "no go/types provider registered")
	// The ready-time warm-up waits for an idle daemon (longer for a checkout
	// nobody touched since the start); shorten both so the tests do not
	// wait the product's seconds.
	provider.SetWarmupIdle(50*time.Millisecond, 400*time.Millisecond)
	f.mi.semanticMgr = mgr
	// Build cycles are held the way the daemon holds them while it warms up,
	// so the checkout becomes ready with no working-tree build behind it: a
	// build's own enrichment stage also asks for the warm-up after its pass,
	// and would hide a missing ready-time one.
	gate := NewViewBuildGate()
	require.False(t, gate.IsOpen(), "a new build gate is already open")
	f.lc.SetBuildGate(gate)
	t.Cleanup(gate.Open)
	return &warmAtReadyFixture{familyFixture: family, provider: provider}
}

// holdGoList puts a go command first on PATH that blocks every `go list`
// until the returned release is called (other go commands pass straight
// through), and reports through listed whether a listing has begun.
func holdGoList(t *testing.T, provider *goanalysis.Provider) (listed func() bool, release func()) {
	t.Helper()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go binary not available in PATH")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "listed")
	open := filepath.Join(dir, "released")
	executable, err := os.Executable()
	require.NoError(t, err)
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	shim := filepath.Join(dir, name)
	if err := os.Link(executable, shim); err != nil {
		// The test executable and TempDir can be on different volumes.
		raw, err := os.ReadFile(executable)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(shim, raw, 0o755))
	}
	t.Setenv(goListHoldEnv, "1")
	t.Setenv(goListRealEnv, realGo)
	t.Setenv(goListMarkEnv, mark)
	t.Setenv(goListReleaseEnv, open)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	released := false
	release = func() {
		if !released {
			released = true
			require.NoError(t, os.WriteFile(open, nil, 0o644))
		}
	}
	// A test that fails before releasing must not leave a warm-up's go
	// command parked on the marker past the test.
	t.Cleanup(func() {
		// Join/cancel the held command before TempDir removes its executable.
		// On a failing test this also avoids forwarding a new compiler child.
		require.NoError(t, provider.Close())
		release()
	})
	listed = func() bool {
		_, err := os.Stat(mark)
		return err == nil
	}
	return listed, release
}

const (
	goListHoldEnv    = "GORTEX_INDEXER_TEST_GO_LIST_HOLD"
	goListRealEnv    = "GORTEX_INDEXER_TEST_REAL_GO"
	goListMarkEnv    = "GORTEX_INDEXER_TEST_GO_LIST_MARK"
	goListReleaseEnv = "GORTEX_INDEXER_TEST_GO_LIST_RELEASE"
)

// The running test binary is also a portable go-command shim. TestMain
// dispatches here before flag parsing, so genuine go arguments stay intact.
func runGoListHoldHelper() (int, bool) {
	if os.Getenv(goListHoldEnv) != "1" || (filepath.Base(os.Args[0]) != "go" && filepath.Base(os.Args[0]) != "go.exe") {
		return 0, false
	}
	for _, arg := range os.Args[1:] {
		if arg != "list" {
			continue
		}
		if err := os.WriteFile(os.Getenv(goListMarkEnv), nil, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		for {
			if _, err := os.Stat(os.Getenv(goListReleaseEnv)); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		break
	}
	cmd := exec.Command(os.Getenv(goListRealEnv), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), true
		}
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	return 0, true
}

// activateWithin selects a dormant checkout and waits for its coordinator to
// be registered, failing if that takes longer than within: the activation
// runs on its own goroutine, so a ready path that waited on the warm-up fails
// here instead of hanging the test.
func activateWithin(t *testing.T, f *lifecycleFixture, checkoutID string, within time.Duration) {
	t.Helper()
	accepted := make(chan bool, 1)
	go func() { accepted <- f.lc.ActivateCheckout(checkoutID, "test activation") }()
	deadline := time.After(within)
	for {
		select {
		case ok := <-accepted:
			require.True(t, ok, "activation of %s was rejected", checkoutID)
			accepted = nil
		case <-deadline:
			t.Fatalf("checkout %s was not ready within %s of its activation", checkoutID, within)
		case <-time.After(time.Millisecond):
		}
		if accepted == nil && f.lc.coordinatorRegistered(checkoutID) {
			return
		}
	}
}

// TestCheckoutReadyStartsTheCompilerWarmup: an automatic checkout becoming
// ready starts the background whole-module listing of its module, and
// readiness never waits
// on it: the checkout's coordinator is registered and the lifecycle's
// registry lock is free while that listing is still held in its go command.
func TestCheckoutReadyStartsTheCompilerWarmup(t *testing.T) {
	f := newWarmAtReadyFixture(t, "warmready")
	listed, release := holdGoList(t, f.provider)

	require.Empty(t, f.provider.CheckoutWarmups(), "a warm-up ran before the checkout was ready")
	activateWithin(t, f.lifecycleFixture, f.automatic.CheckoutID, 15*time.Second)

	deadline := time.Now().Add(time.Minute)
	for !listed() {
		if time.Now().After(deadline) {
			t.Fatalf("no warm-up listing began after the checkout became ready: %+v",
				f.provider.CheckoutWarmups())
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses := f.provider.CheckoutWarmups()
	require.Len(t, statuses, 1, "the ready checkout's module is not the one warm-up")
	require.Equal(t, "running", statuses[0].State,
		"the warm-up is not running while its listing is held")
	require.True(t, f.lc.coordinatorRegistered(f.automatic.CheckoutID),
		"the checkout is not ready while the warm-up's listing is held")
	require.True(t, f.lc.coordMu.TryLock(),
		"the lifecycle's registry lock is held while the warm-up's listing is held")
	f.lc.coordMu.Unlock()

	release()
	deadline = time.Now().Add(2 * time.Minute)
	for {
		statuses = f.provider.CheckoutWarmups()
		if len(statuses) == 1 && statuses[0].State == "warm" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ready checkout was not warmed: %+v", statuses)
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Positive(t, statuses[0].Packages, "the warm-up listed nothing: %+v", statuses[0])

	caches := f.provider.TypecheckCacheStatus()
	require.Len(t, caches, 1, "the warm-up retained no compiler state for the checkout")
	require.Equal(t, "warm", caches[0].Warmup.State)
	require.Positive(t, caches[0].Closure, "the retained state holds no listed closure: %+v", caches[0])
	root, err := filepath.EvalSymlinks(f.worktree)
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(caches[0].Dir)
	require.NoError(t, err)
	require.Equal(t, root, got, "the warmed state is not the ready checkout's module")
}

// TestCheckoutReadyWarmupYieldsToTheCheckoutsForegroundWork: the ready-time
// warm-up is handed the lifecycle's foreground-activity view, so it lists
// nothing while a refresh ticket waits on a coordinator, reports what it is
// deferred by and that the checkout was touched, and lists once the daemon
// has been idle.
func TestCheckoutReadyWarmupYieldsToTheCheckoutsForegroundWork(t *testing.T) {
	f := newWarmAtReadyFixture(t, "warmyield")
	listed, release := holdGoList(t, f.provider)
	activateWithin(t, f.lifecycleFixture, f.automatic.CheckoutID, 15*time.Second)

	// A waiting refresh ticket on the ready checkout, planted before the
	// untouched idle period (400ms) can run out. The loop never serves it:
	// build cycles are held by the closed gate.
	f.lc.coordMu.Lock()
	c := f.lc.coordinators[f.automatic.CheckoutID]
	f.lc.coordMu.Unlock()
	require.NotNil(t, c)
	c.refreshMu.Lock()
	c.refreshWaiters = map[uint64]*checkoutRefreshRequest{1 << 62: {}}
	c.refreshHighWater = 1 << 62
	c.refreshMu.Unlock()
	c.ticketDemand.Store(time.Now().UnixNano())
	removeTicket := func() {
		c.refreshMu.Lock()
		c.refreshWaiters = nil
		c.refreshMu.Unlock()
	}
	defer removeTicket()

	var status goanalysis.CheckoutWarmupStatus
	deadline := time.Now().Add(30 * time.Second)
	for {
		statuses := f.provider.CheckoutWarmups()
		if len(statuses) == 1 && statuses[0].DeferredBy == "refresh_ticket" {
			status = statuses[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ready-time warm-up never deferred to the waiting ticket: %+v", statuses)
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, "deferred", status.State)
	require.True(t, status.Touched, "a checkout with an admitted ticket is touched")
	time.Sleep(time.Second)
	require.False(t, listed(), "the warm-up listed while a refresh ticket waited")

	removeTicket()
	deadline = time.Now().Add(time.Minute)
	for !listed() {
		if time.Now().After(deadline) {
			t.Fatalf("no warm-up listing began once the daemon was idle: %+v", f.provider.CheckoutWarmups())
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
	deadline = time.Now().Add(2 * time.Minute)
	for {
		statuses := f.provider.CheckoutWarmups()
		if len(statuses) == 1 && statuses[0].State == "warm" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the checkout was not warmed: %+v", statuses)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
