package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
)

// Background cycles over a moving working tree (checkout_motion.go) and the
// linked-worktree file watcher that reports the motion (checkout_watch.go).

// motionRepo is a git repository with one committed file and one dirty file in
// pkg/, so a working-copy sample has a dirty path and a dirty directory.
func motionRepo(t *testing.T) (string, *gitstate.DirtySampler) {
	t.Helper()
	builderIsolateGit(t)
	root := builderTempDir(t, "motion")
	builderGit(t, root, "init", "--initial-branch=main")
	builderWriteTree(t, root, map[string]string{
		"pkg/a.go":   "package pkg\n\nfunc A() {}\n",
		"other/b.go": "package other\n\nfunc B() {}\n",
	})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "base")
	builderWriteTree(t, root, map[string]string{"pkg/a.go": "package pkg\n\nfunc A() { _ = 1 }\n"})
	sampler, err := gitstate.NewDirtySampler(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return root, sampler
}

// startMotionCycle runs one coordinator cycle through the real admission path
// over a real sampler, with the checkout marked as watched. The cycle's work
// is the barrier (it stands in for the build). prepare runs on the coordinator
// before the cycle starts.
func startMotionCycle(t *testing.T, gate *ViewBuildGate, root string, sampler *gitstate.DirtySampler, prepare func(*CheckoutCoordinator), work func(ctx context.Context)) (*CheckoutCoordinator, <-chan CheckoutCycle) {
	t.Helper()
	// An empty catalog: the cycle's observed-change record reads the route.
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "motion.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	lifetime, cancel := context.WithCancel(t.Context())
	outcomes := make(chan CheckoutCycle, 1)
	c := &CheckoutCoordinator{
		checkoutID:     "moving",
		root:           root,
		sampler:        sampler,
		catalog:        store.Catalog(),
		quiet:          300 * time.Millisecond,
		gate:           gate,
		logger:         zap.NewNop(),
		signal:         make(chan struct{}, 1),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) { return CheckoutCycle{}, false },
		cycleBarrier:   work,
		cycleDone:      func(out CheckoutCycle) { outcomes <- out },
	}
	// A watcher that reports nothing on its own: the test reports changes.
	c.motion.watch = &checkoutWatch{}
	if prepare != nil {
		prepare(c)
	}
	go func() {
		defer close(c.done)
		c.cycle(lifetime)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
			t.Error("cycle did not stop")
		}
	})
	return c, outcomes
}

func awaitMotionOutcome(t *testing.T, outcomes <-chan CheckoutCycle) CheckoutCycle {
	t.Helper()
	select {
	case out := <-outcomes:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("cycle reported no outcome")
		return CheckoutCycle{}
	}
}

func drainSignal(c *CheckoutCoordinator) {
	select {
	case <-c.signal:
	default:
	}
}

func requireRearmed(t *testing.T, c *CheckoutCoordinator) {
	t.Helper()
	select {
	case <-c.signal:
	default:
		t.Fatal("the held cycle did not re-arm the quiet window")
	}
}

// A background cycle that wakes while the watcher is still reporting changes
// is held before it samples, queues or builds, and the quiet window is armed
// again so the tree gets its cycle once it settles.
func TestABackgroundCycleIsHeldWhileTheWorkingTreeIsStillChanging(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	built := make(chan struct{}, 1)
	c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		c.noteFilesystemChange([]string{filepath.Join(root, "pkg", "a.go")})
		drainSignal(c)
	}, func(ctx context.Context) {
		built <- struct{}{}
		<-ctx.Done()
	})
	out := awaitMotionOutcome(t, outcomes)
	if !out.Held || !out.Rescheduled || out.Err != nil {
		t.Fatalf("cycle = %+v, want held and rescheduled", out)
	}
	select {
	case <-built:
		t.Fatal("a held cycle reached its build")
	default:
	}
	if stats := gate.Stats(); stats.AdmittedBackground != 0 {
		t.Fatalf("a held cycle took the build lane (%d background admissions)", stats.AdmittedBackground)
	}
	if taken := sampler.SamplesTaken(); taken != 0 {
		t.Fatalf("a cycle held by the quiet window sampled the working copy %d times", taken)
	}
	requireRearmed(t, c)
}

// A reported path that moved again after it was reported (its event not
// delivered yet) holds the cycle even though the quiet window has passed.
func TestABackgroundCycleIsHeldWhenAReportedPathMovedAgain(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	target := filepath.Join(root, "pkg", "a.go")
	c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		c.noteFilesystemChange([]string{target})
		drainSignal(c)
		c.motion.mu.Lock()
		c.motion.lastEvent = time.Now().Add(-time.Second)
		c.motion.mu.Unlock()
		// An atomic save: a new file renamed over the reported one.
		tmp := target + ".next"
		if err := os.WriteFile(tmp, []byte("package pkg\n\nfunc A() { _ = 2 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, target); err != nil {
			t.Fatal(err)
		}
	}, func(ctx context.Context) { <-ctx.Done() })
	out := awaitMotionOutcome(t, outcomes)
	if !out.Held || out.Err != nil {
		t.Fatalf("cycle = %+v, want held by the re-stat", out)
	}
	if stats := gate.Stats(); stats.AdmittedBackground != 0 {
		t.Fatalf("a held cycle took the build lane (%d background admissions)", stats.AdmittedBackground)
	}
	requireRearmed(t, c)
}

// A background cycle whose working-copy sample the tree moved under is held
// before it queues for the lane; a sample that fails for any other reason is
// left to the cycle to report as before.
func TestABackgroundCycleWhoseSampleTheTreeMovedUnderNeverQueues(t *testing.T) {
	root, sampler := motionRepo(t)
	moved := fmt.Errorf("gitstate: fingerprint dirty content in %s: %w: %w", root, gitstate.ErrDirtyUnavailable, fmt.Errorf("git dirty status changed while sampling: %w", gitstate.ErrDirtyMoved))
	for _, tc := range []struct {
		name string
		err  error
		held bool
	}{
		{"moved", moved, true},
		{"unavailable", fmt.Errorf("gitstate: read status: %w: not a git repository", gitstate.ErrDirtyUnavailable), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewViewBuildGate()
			gate.Open()
			building := make(chan struct{})
			c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
				c.holdSample = func(context.Context) error { return tc.err }
			}, func(ctx context.Context) {
				close(building)
				<-ctx.Done()
			})
			if tc.held {
				out := awaitMotionOutcome(t, outcomes)
				if !out.Held || out.Err != nil {
					t.Fatalf("cycle = %+v, want held", out)
				}
				if stats := gate.Stats(); stats.AdmittedBackground != 0 {
					t.Fatalf("a cycle whose sample was torn took the build lane (%d admissions)", stats.AdmittedBackground)
				}
				requireRearmed(t, c)
				return
			}
			select {
			case <-building:
			case <-time.After(3 * time.Second):
				t.Fatal("a cycle whose sample failed for another reason was held")
			}
		})
	}
}

// A refresh ticket's cycle is never held: a caller is waiting on it, and it
// completes only against a sample taken after it arrived anyway.
func TestATicketCycleIsNeverHeldByTheWorkingTreeMoving(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	building := make(chan struct{})
	startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		c.noteFilesystemChange([]string{filepath.Join(root, "pkg", "a.go")})
		if _, err := c.enqueueCheckoutRefresh(&checkoutRefreshRequest{
			ticket: &CheckoutRefreshTicket{Ticket: &MutationTicket{}}, done: make(chan MutationResult, 1),
		}, false); err != nil {
			t.Fatal(err)
		}
	}, func(ctx context.Context) {
		close(building)
		<-ctx.Done()
	})
	select {
	case <-building:
	case <-time.After(3 * time.Second):
		t.Fatal("a ticket's cycle was held while the working tree was changing")
	}
	if stats := gate.Stats(); stats.AdmittedInteractive != 1 {
		t.Fatalf("interactive admissions = %d, want the ticket's cycle", stats.AdmittedInteractive)
	}
}

// An admitted background build is abandoned as soon as the watcher reports a
// change to a path it reads (a dirty file's directory), not for a change
// elsewhere; it publishes nothing, is rescheduled, and re-arms the window.
func TestABackgroundBuildIsAbandonedWhenAPathItReadsChanges(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	building := make(chan struct{})
	canceled := make(chan time.Time, 1)
	c, outcomes := startMotionCycle(t, gate, root, sampler, nil, func(ctx context.Context) {
		close(building)
		<-ctx.Done()
		canceled <- time.Now()
	})
	select {
	case <-building:
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle was not admitted")
	}
	c.noteFilesystemChange([]string{filepath.Join(root, "third", "c.go")})
	select {
	case <-canceled:
		t.Fatal("a change to a path the build does not read abandoned it")
	case <-time.After(150 * time.Millisecond):
	}
	drainSignal(c)
	reported := time.Now()
	c.noteFilesystemChange([]string{filepath.Join(root, "pkg", "new.go")})
	select {
	case at := <-canceled:
		t.Logf("the build observed the abort %s after the change was reported", at.Sub(reported))
	case <-time.After(3 * time.Second):
		t.Fatal("a change in a directory the build reads did not abandon it")
	}
	out := awaitMotionOutcome(t, outcomes)
	if out.Err != nil || !out.Rescheduled || out.YieldedTo != treeMovedReason {
		t.Fatalf("abandoned cycle = %+v, want rescheduled with yielded_to=%s", out, treeMovedReason)
	}
	requireRearmed(t, c)
}

// A build past its commit point is never abandoned for movement.
func TestABackgroundBuildPastItsCommitPointIsNotAbandoned(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	committed := make(chan struct{})
	finish := make(chan struct{})
	result := make(chan bool, 1)
	c, _ := startMotionCycle(t, gate, root, sampler, nil, func(ctx context.Context) {
		reachBuildCommitPoint(ctx)
		close(committed)
		<-finish
		result <- ctx.Err() != nil
		<-ctx.Done()
	})
	select {
	case <-committed:
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle was not admitted")
	}
	c.noteFilesystemChange([]string{filepath.Join(root, "pkg", "a.go")})
	time.Sleep(100 * time.Millisecond)
	close(finish)
	if <-result {
		t.Fatal("a build past its commit point was abandoned")
	}
}

// Aborts for a change git does not see (the next sample has the aborted
// build's fingerprint) stop arming builds after maxTreeMovedAborts; a real
// change resets the run.
func TestMovementAbortsForChangesGitDoesNotSeeAreBounded(t *testing.T) {
	root, sampler := motionRepo(t)
	c := &CheckoutCoordinator{root: root, sampler: sampler, logger: zap.NewNop(), lifetime: t.Context()}
	c.motion.watch = &checkoutWatch{}
	started := time.Now()
	snap, err := sampler.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	arm := func() bool {
		_, y := c.armTreeMoveAbort(t.Context(), started)
		defer c.disarmTreeMoveAbort(y)
		return y != nil
	}
	for i := 1; i < maxTreeMovedAborts; i++ {
		c.motion.abortedFingerprint = snap.Fingerprint
		if !arm() {
			t.Fatalf("a build was not armed after %d unseen aborts", i)
		}
	}
	c.motion.abortedFingerprint = snap.Fingerprint
	if arm() {
		t.Fatalf("a build was armed after %d aborts git did not see", maxTreeMovedAborts)
	}
	c.motion.abortedFingerprint = "a-state-git-saw-move"
	if !arm() {
		t.Fatal("an abort git saw did not reset the run")
	}
}

// A window re-armed by a stream of signals is not extended past the coalesce
// cap: a checkout that never stops changing still gets a cycle.
func TestAQuietWindowIsNotExtendedPastTheCoalesceCap(t *testing.T) {
	f := newCoordinatorFixture(t)
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var first time.Time
		start := time.Now()
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			cycleDone: func(CheckoutCycle) {
				mu.Lock()
				if first.IsZero() {
					first = time.Now()
				}
				mu.Unlock()
			},
		})
		for time.Since(start) < 2*backgroundCoalesceCapFloor {
			c.Signal("storm")
			time.Sleep(100 * time.Millisecond)
		}
		synctest.Wait()
		mu.Lock()
		got := first
		mu.Unlock()
		if got.IsZero() {
			t.Fatalf("no cycle ran during %s of signals every 100 ms", 2*backgroundCoalesceCapFloor)
		}
		if after := got.Sub(start); after > backgroundCoalesceCapFloor+time.Second {
			t.Fatalf("first cycle ran %s after the storm began, want within the %s cap", after, backgroundCoalesceCapFloor)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// motionFixtureCycles records a live coordinator's cycles with their instants.
type motionFixtureCycles struct {
	mu     sync.Mutex
	cycles []CheckoutCycle
	at     []time.Time
}

func (r *motionFixtureCycles) record(out CheckoutCycle) {
	r.mu.Lock()
	r.cycles = append(r.cycles, out)
	r.at = append(r.at, time.Now())
	r.mu.Unlock()
}

// published waits for a cycle after since that routed a working-tree
// generation describing the checkout as it is now.
func (r *motionFixtureCycles) published(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, since time.Time, within time.Duration) time.Duration {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		var hit time.Time
		for i, out := range r.cycles {
			if r.at[i].After(since) && out.Err == nil && out.DirtyGenerationID > 0 && (out.DirtyBuilt || out.DirtyReused) {
				hit = r.at[i]
			}
		}
		r.mu.Unlock()
		if !hit.IsZero() {
			snap, err := c.sampler.Sample(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if row, found := f.generation(f.route().DirtyGenerationID); found && row.LowerViewFingerprint == snap.Fingerprint {
				return hit.Sub(since)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no generation describing the working tree was published within %s", within)
	return 0
}

func motionAtomicWrite(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".save"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// liveMotionCoordinator starts a fixture coordinator with the production quiet
// window, no poll, and — through the lifecycle's wiring — a file watcher. An
// optional cfg supplies the builder and the build gate.
func liveMotionCoordinator(t *testing.T, f *coordinatorFixture, cycles *motionFixtureCycles, cfg ...CheckoutCoordinatorConfig) *CheckoutCoordinator {
	t.Helper()
	var config CheckoutCoordinatorConfig
	if len(cfg) > 0 {
		config = cfg[0]
	}
	config.PollInterval, config.cycleDone = -1, cycles.record
	c := f.coordinator(t, config)
	c.Signal("initial")
	cycles.published(t, f, c, time.Time{}, 60*time.Second)
	(&CheckoutLifecycle{cfgWatchCheckouts: true, logger: zap.NewNop()}).watchCheckout(c, builderRepoPrefix, f.worktree)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.motion.mu.Lock()
		attached := c.motion.watch != nil
		c.motion.mu.Unlock()
		if attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the checkout's watcher was not attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let the backend's startup replay pass before the test writes.
	time.Sleep(500 * time.Millisecond)
	return c
}

// A plain save in a linked worktree — no query, no MCP edit, no poll — is
// published by the checkout's own watcher.
func TestALinkedWorktreeSaveIsPublishedWithoutAQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a native file watcher")
	}
	t.Setenv(checkoutWatchDisableEnv, "")
	f := newCoordinatorFixture(t)
	cycles := &motionFixtureCycles{}
	c := liveMotionCoordinator(t, f, cycles)
	saved := time.Now()
	motionAtomicWrite(t, filepath.Join(f.worktree, "helper.go"), "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	took := cycles.published(t, f, c, saved, 5*time.Second)
	t.Logf("save published %s after the write (quiet window %s)", took, c.quiet)
	if took > 2*time.Second {
		t.Fatalf("save published %s after the write, want about the quiet window plus one build", took)
	}
}

// A working tree written every 300 ms produces held and abandoned background
// cycles, never a failed one, and its final state is published once the
// writes stop.
func TestAWorkingTreeWrittenEvery300msCoalescesWithoutFailedBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a native file watcher")
	}
	t.Setenv(checkoutWatchDisableEnv, "")
	f := newCoordinatorFixture(t)
	cycles := &motionFixtureCycles{}
	c := liveMotionCoordinator(t, f, cycles)
	cycles.mu.Lock()
	before := len(cycles.cycles)
	cycles.mu.Unlock()
	files := []string{"core.go", "helper.go", "island.go", "extra.go"}
	writes := 0
	var lastWrite time.Time
	for i := 0; i < 14; i++ {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		name := files[i%len(files)]
		motionAtomicWrite(t, filepath.Join(f.worktree, name), fmt.Sprintf("package fixture\n\nfunc Edit%d() {}\n", i))
		lastWrite = time.Now()
		writes++
	}
	took := cycles.published(t, f, c, lastWrite, 5*time.Second)

	cycles.mu.Lock()
	defer cycles.mu.Unlock()
	held, abandoned, built, failed := 0, 0, 0, 0
	for _, out := range cycles.cycles[before:] {
		switch {
		case out.Err != nil:
			failed++
			t.Errorf("cycle failed: %v", out.Err)
		case out.Held:
			held++
		case out.YieldedTo == treeMovedReason:
			abandoned++
		case out.DirtyBuilt:
			built++
		}
	}
	t.Logf("%d writes: %d cycles (%d held, %d abandoned, %d built, %d failed); final state published %s after the last write",
		writes, len(cycles.cycles)-before, held, abandoned, built, failed, took)
	if failed != 0 {
		t.Fatalf("%d background cycles failed over a moving working tree", failed)
	}
	// A held cycle samples the moving tree but never admits build work.
	// Count abandoned and completed builds, rather than watcher wakes, when
	// checking that the writes were coalesced.
	if admitted := abandoned + built; admitted >= writes {
		t.Fatalf("%d admitted builds for %d writes: the changes were not coalesced", admitted, writes)
	}
	if took > 2*time.Second {
		t.Fatalf("the final state was published %s after the last write, want about the quiet window plus one build", took)
	}
}

// A background cycle that has not queued for the lane yet steps aside the
// moment a refresh ticket of its checkout arrives: the loop runs one cycle at
// a time, and the ticket's cycle covers the same working tree.
func TestABackgroundCycleStepsAsideForATicketOfItsCheckout(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	inPreflight := make(chan struct{})
	c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		// A slow preflight: it ends only when its context does.
		c.cyclePreflight = func(ctx context.Context) (CheckoutCycle, bool) {
			close(inPreflight)
			<-ctx.Done()
			return CheckoutCycle{}, false
		}
	}, func(ctx context.Context) { <-ctx.Done() })
	select {
	case <-inPreflight:
	case <-time.After(3 * time.Second):
		t.Fatal("the background cycle never reached its preflight")
	}
	arrived := time.Now()
	if _, err := c.enqueueCheckoutRefresh(&checkoutRefreshRequest{
		ticket: &CheckoutRefreshTicket{Ticket: &MutationTicket{}}, done: make(chan MutationResult, 1),
	}, false); err != nil {
		t.Fatal(err)
	}
	out := awaitMotionOutcome(t, outcomes)
	t.Logf("the background cycle stepped aside %s after the ticket arrived", time.Since(arrived))
	if out.Err != nil || !out.Rescheduled || out.YieldedTo != "refresh_ticket" {
		t.Fatalf("cycle = %+v, want it rescheduled for the refresh ticket", out)
	}
	if stats := gate.Stats(); stats.AdmittedBackground != 0 || stats.AdmittedInteractive != 0 {
		t.Fatalf("a cycle that stepped aside took the build lane: %+v", stats)
	}
}

// Every background build the moving tree abandoned in a row doubles the quiet
// time the next admission needs, and the coalesce cap with it; a build that
// runs to its end resets both.
func TestAbandonedBackgroundBuildsBackOffTheirAdmission(t *testing.T) {
	root, sampler := motionRepo(t)
	c := &CheckoutCoordinator{root: root, sampler: sampler, quiet: 300 * time.Millisecond, logger: zap.NewNop(), lifetime: t.Context(), signal: make(chan struct{}, 1)}
	c.motion.watch = &checkoutWatch{}
	baseQuiet, baseCap := c.backgroundAdmissionBounds()
	for i := 1; i <= 4; i++ {
		c.treeMovedCycle(CheckoutCycle{Err: context.Canceled}, cycleAdmission{}, "watcher")
		quiet, capped := c.backgroundAdmissionBounds()
		want := min(i, 3)
		if quiet != baseQuiet<<want || capped != baseCap<<want {
			t.Fatalf("after %d abandoned builds: quiet %s cap %s, want %s and %s", i, quiet, capped, baseQuiet<<want, baseCap<<want)
		}
	}
	c.motion.mu.Lock()
	c.motion.lastEvent = time.Now().Add(-2 * baseQuiet)
	c.motion.burstStart = c.motion.lastEvent
	c.motion.mu.Unlock()
	ctx := withCycleStart(t.Context(), time.Now())
	if _, held := c.holdBackgroundCycle(ctx); !held {
		t.Fatalf("a cycle %s after the last change was admitted while the backoff asks for %s", 2*baseQuiet, baseQuiet<<3)
	}
	c.settleTreeMoveAborts()
	if quiet, capped := c.backgroundAdmissionBounds(); quiet != baseQuiet || capped != baseCap {
		t.Fatalf("a completed build left the backoff at quiet %s cap %s", quiet, capped)
	}
	if _, held := c.holdBackgroundCycle(ctx); held {
		t.Fatal("a cycle past the quiet window was held after the backoff reset")
	}
}
