package indexer

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/search/trigram"
)

// The checkout searcher's tests run against the coordinator fixture, so the
// worktree is a real linked one and the generations the searcher reads its
// corpus from are the production builder's.

// worktreeWrite replaces one file in a checkout without committing it.
func worktreeWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// grepPaths returns the paths a search matched, sorted by the searcher's own
// order, so an assertion reads as a set of files rather than as line offsets.
func grepPaths(matches []trigram.Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Path)
	}
	return out
}

func fileContains(t *testing.T, path, want string) bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), want)
}

// TestCheckoutSearcherReadsTheCheckoutRootNotTheCanonicalOne is the defining
// claim: a routed checkout's text search answers about that checkout's working
// copy in both directions — it finds what only that copy holds, and it misses
// what only the canonical checkout still holds.
func TestCheckoutSearcherReadsTheCheckoutRootNotTheCanonicalOne(t *testing.T) {
	f := newCoordinatorFixture(t)

	// Two uncommitted differences from the tree both checkouts share: one file
	// gains a marker, another is removed.
	worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// worktree-only-marker\n}\n")
	if err := os.Remove(filepath.Join(f.worktree, "gone.go")); err != nil {
		t.Fatalf("remove gone.go from the worktree: %v", err)
	}

	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)

	searcher, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("build the checkout searcher: %v", err)
	}

	// The corpus is the whole checkout, not only what it differs by.
	if got := searcher.Grep("func Helper", 0); len(got) != 1 || got[0].Path != "helper.go" {
		t.Errorf("the unchanged part of the checkout is not searchable: %v", grepPaths(got))
	}

	// Found: content that exists in this working copy and nowhere else.
	got := searcher.Grep("worktree-only-marker", 0)
	if len(got) != 1 || got[0].Path != "island.go" {
		t.Errorf("the worktree's own edit did not answer: %v", grepPaths(got))
	}
	if fileContains(t, filepath.Join(f.primary, "island.go"), "worktree-only-marker") {
		t.Fatal("the canonical checkout holds the marker, so the hit above proves nothing")
	}

	// Missed: content the checkout removed and the canonical root still holds.
	if got := searcher.Grep("func Gone", 0); len(got) != 0 {
		t.Errorf("a file deleted in the worktree still answered: %v", grepPaths(got))
	}
	if !fileContains(t, filepath.Join(f.primary, "gone.go"), "func Gone") {
		t.Fatal("the canonical checkout no longer holds gone.go, so the miss above proves nothing")
	}
}

// TestCheckoutSearcherServesTheRoutedCheckoutThroughTheLifecycle pins the front
// door: the lifecycle finds the coordinator holding the checkout and answers
// literal and regexp searches through its searcher.
func TestCheckoutSearcherServesTheRoutedCheckoutThroughTheLifecycle(t *testing.T) {
	f := newCoordinatorFixture(t)
	worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// zephyr-marker\n}\n")

	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	l := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}

	ctx := context.Background()
	matches, served, err := l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "zephyr-marker", Limit: 10,
	})
	if err != nil || !served || len(matches) != 1 || matches[0].Path != "island.go" {
		t.Fatalf("literal search: served=%v err=%v matches=%v", served, err, grepPaths(matches))
	}

	matches, served, err = l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID,
		Query:      "zephyr-[a-z]+",
		Regexp:     regexp.MustCompile("zephyr-[a-z]+"),
		Limit:      10,
	})
	if err != nil || !served || len(matches) != 1 || matches[0].Path != "island.go" {
		t.Fatalf("regexp search: served=%v err=%v matches=%v", served, err, grepPaths(matches))
	}

	// A checkout no coordinator holds is not served from somewhere else.
	if _, served, err := l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: "checkout-nobody", Query: "zephyr-marker", Limit: 10,
	}); served || err != nil {
		t.Errorf("an unheld checkout answered: served=%v err=%v", served, err)
	}
}

// TestCheckoutSearcherRebuildsWhenTheWorkingTreeMoves pins the invalidation: a
// cycle that samples a different working tree re-keys the searcher, and the
// next search builds over what is on disk now.
//
// The whole test runs on virtual time. The coordinator's loop is what notices
// the edit, so the rebuild is driven the way production drives it — by a cycle
// — rather than by reaching into the cache.
func TestCheckoutSearcherRebuildsWhenTheWorkingTreeMoves(t *testing.T) {
	f := newCoordinatorFixture(t)

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		cycles := 0
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			cycleDone: func(CheckoutCycle) {
				mu.Lock()
				cycles++
				mu.Unlock()
			},
		})

		worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// marker-one\n}\n")
		c.Signal("first edit")
		time.Sleep(time.Second)
		synctest.Wait()

		first, err := c.textSearcher(context.Background())
		if err != nil {
			t.Fatalf("build the checkout searcher: %v", err)
		}
		if got := first.Grep("marker-one", 0); len(got) != 1 {
			t.Fatalf("the first working tree did not answer: %v", grepPaths(got))
		}

		worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// marker-two\n}\n")
		c.Signal("second edit")
		time.Sleep(time.Second)
		synctest.Wait()

		mu.Lock()
		ran := cycles
		mu.Unlock()
		if ran < 2 {
			t.Fatalf("%d cycles ran, want the edit's own cycle to have run", ran)
		}

		second, err := c.textSearcher(context.Background())
		if err != nil {
			t.Fatalf("rebuild the checkout searcher: %v", err)
		}
		if second == first {
			t.Fatal("the searcher was not rebuilt after the working tree moved")
		}
		if got := second.Grep("marker-two", 0); len(got) != 1 {
			t.Errorf("the current working tree did not answer: %v", grepPaths(got))
		}
		if got := second.Grep("marker-one", 0); len(got) != 0 {
			t.Errorf("the superseded content still answered: %v", grepPaths(got))
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCheckoutSearcherIsDroppedWithTheCoordinator pins the lifetime: the index
// lives exactly as long as the coordinator that owns the checkout, and the
// registry stops serving the checkout the moment the coordinator leaves.
func TestCheckoutSearcherIsDroppedWithTheCoordinator(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.coordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)

	l := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	ctx := context.Background()
	if _, served, err := l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "func Helper", Limit: 10,
	}); err != nil || !served {
		t.Fatalf("the routed checkout was not served: served=%v err=%v", served, err)
	}
	c.textMu.Lock()
	built := c.textIndex
	c.textMu.Unlock()
	if built == nil {
		t.Fatal("the search built no index to drop")
	}

	l.dropCoordinator(f.checkoutID)

	c.textMu.Lock()
	left := c.textIndex
	c.textMu.Unlock()
	if left != nil {
		t.Error("the checkout's index outlived its coordinator")
	}
	if _, served, err := l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "func Helper", Limit: 10,
	}); served || err != nil {
		t.Errorf("a dropped checkout still answered: served=%v err=%v", served, err)
	}
}
