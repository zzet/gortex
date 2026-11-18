//go:build performance

package indexer

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// The tighter watcher publication latency is a host measurement; ordinary CI
// independently retains the native no-query publication correctness check.
func TestPerformanceALinkedWorktreeSaveIsPublishedWithoutAQuery(t *testing.T) {
	took, quiet := linkedWorktreeSavePublication(t)
	t.Logf("save published %s after the write (quiet window %s)", took, quiet)
	if took > 2*time.Second {
		t.Fatalf("save published %s after the write, want about the quiet window plus one build", took)
	}
}

// A working tree written every 300 ms produces held and abandoned background
// cycles, never a failed one, and its final state is published once the
// writes stop.
func TestPerformanceAWorkingTreeWrittenEvery300msCoalescesWithoutFailedBuilds(t *testing.T) {
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
