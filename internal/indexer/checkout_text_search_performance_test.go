//go:build performance

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestPerformanceCheckoutSearcherFirstSearchAfterEditTiming measures the searcher work of
// the first search after a one-file chained edit on the accumulated-edit
// fixture: a rebuild over the whole corpus (what every key change cost before
// the patch) against the patch. It runs over a clean start and over a
// checkout that already carries a dirty set of 200 files, so a patch that
// grew with the dirty set would show.
func TestPerformanceCheckoutSearcherFirstSearchAfterEditTiming(t *testing.T) {
	for _, predirty := range []int{0, 200} {
		t.Run(fmt.Sprintf("dirty=%d", predirty), func(t *testing.T) {
			textTimeFirstSearch(t, predirty)
		})
	}
}

func textTimeFirstSearch(t *testing.T, predirty int) {
	layout := accumulatedDirtyIndependent
	f := newCoordinatorFixtureWithTree(t, accumulatedDirtyTree(layout))
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	write := func(i int, body, comment bool) {
		if err := os.WriteFile(filepath.Join(f.worktree, accumulatedDirtyUnitPath(layout, i)),
			[]byte(accumulatedDirtyUnitSource(layout, i, body, comment)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if predirty > 0 {
		for i := 0; i < predirty; i++ {
			write(i, false, true)
		}
		coordinatorReconcile(t, c)
		if _, err := c.textSearcher(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var rebuilds, patches []time.Duration
	var patched []int
	for i := 0; i < 12; i++ {
		write(i, true, predirty > 0)
		cycle := coordinatorReconcile(t, c)

		// Rebuild: drop the cached searcher, as the old per-key rebuild did.
		c.textMu.Lock()
		saved, savedKey, savedState := c.textIndex, c.textKey, c.textState
		c.textIndex, c.textKey = nil, ""
		c.textState = checkoutTextState{}
		c.textMu.Unlock()
		start := time.Now()
		if _, err := c.textSearcher(ctx); err != nil {
			t.Fatal(err)
		}
		rebuilds = append(rebuilds, time.Since(start))
		if saved == nil {
			// The first edit has nothing to patch from: keep the rebuild.
			continue
		}
		// Patch: restore the searcher as it stood before the edit.
		c.textMu.Lock()
		c.textIndex, c.textKey, c.textState = saved, savedKey, savedState
		c.textMu.Unlock()
		start = time.Now()
		if _, err := c.textSearcher(ctx); err != nil {
			t.Fatal(err)
		}
		patches = append(patches, time.Since(start))
		stats := c.textSearchStats()
		if stats.Last != "patch" {
			t.Fatalf("edit %d: the timed update was %q, not a patch", i, stats.Last)
		}
		patched = append(patched, stats.LastPaths)
		if i == 1 {
			c.textMu.Lock()
			corpus, dirty := len(c.textState.corpus.paths()), len(c.textState.manifest)
			c.textMu.Unlock()
			t.Logf("fixture corpus: %d files, dirty set %d, chain depth %d", corpus, dirty, cycle.DirtyChainDepth)
		}
	}
	t.Logf("fixture first search after a one-file edit (dirty=%d): rebuild %s, patch %s, paths patched %v",
		predirty, textDurations(rebuilds), textDurations(patches), patched)
}

// textDurations renders min / median / max.
func textDurations(ds []time.Duration) string {
	if len(ds) == 0 {
		return "n=0"
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	return fmt.Sprintf("n=%d min %s p50 %s max %s", len(sorted),
		sorted[0].Round(time.Microsecond), sorted[len(sorted)/2].Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond))
}
