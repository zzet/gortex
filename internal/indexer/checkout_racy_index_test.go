package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// A checkout whose index is racily clean when its coordinator starts (a fresh
// `git worktree add`, every entry stamped in the index's own second) is healed
// by the coordinator's start, before any edit samples it.
// racifyWorktreeIndex puts the fixture worktree's index in the racily clean
// state a fresh `git worktree add` leaves: every entry recorded in the index
// file's own second.
func racifyWorktreeIndex(t *testing.T, f *coordinatorFixture) {
	t.Helper()
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	err := filepath.WalkDir(f.worktree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".git" {
			return err
		}
		return os.Chtimes(path, past, past)
	})
	if err != nil {
		t.Fatal(err)
	}
	builderGit(t, f.worktree, "update-index", "-q", "--refresh")
	index := strings.TrimSpace(builderGit(t, f.worktree, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	if err := os.Chtimes(index, past, past); err != nil {
		t.Fatal(err)
	}
	probe, err := gitstate.NewDirtySampler(f.worktree, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if report := probe.RacyIndex(); !report.NeedsRefresh() {
		t.Fatalf("the fixture's index is not racily clean: %+v", report)
	}
}

func worktreeIndexRacy(t *testing.T, f *coordinatorFixture) bool {
	t.Helper()
	probe, err := gitstate.NewDirtySampler(f.worktree, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return probe.RacyIndex().NeedsRefresh()
}

func awaitWorktreeIndexHealed(t *testing.T, f *coordinatorFixture, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for worktreeIndexRacy(t, f) {
		if time.Now().After(deadline) {
			t.Fatalf("%s left the index racily clean", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A checkout whose index is racily clean when its coordinator starts is
// healed by the coordinator's start.
func TestCheckoutCoordinatorStartHealsARacilyCleanIndex(t *testing.T) {
	f := newCoordinatorFixture(t)
	racifyWorktreeIndex(t, f)
	f.coordinator(t, CheckoutCoordinatorConfig{Debounce: time.Hour, debounceDemand: true})
	awaitWorktreeIndexHealed(t, f, "the coordinator's start")
}

// The start-time refresh waits for an edit admitted on the checkout (a
// coordinator activated by an edit starts while that edit is admitted), and
// runs once the edit is done.
func TestCheckoutCoordinatorIndexRefreshWaitsForTheEdit(t *testing.T) {
	f := newCoordinatorFixture(t)
	// Control the start-time refresh precondition independently of whether
	// fixture creation and the index rewrite land in the same wall-clock second.
	racifyWorktreeIndex(t, f)
	c := f.coordinator(t, CheckoutCoordinatorConfig{Debounce: time.Hour, debounceDemand: true})
	awaitWorktreeIndexHealed(t, f, "the coordinator's start")
	racifyWorktreeIndex(t, f)
	if !c.admitSourceMutation() {
		t.Fatal("could not admit a source mutation")
	}
	done := make(chan struct{})
	go func() { c.healRacyIndex(t.Context()); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if !worktreeIndexRacy(t, f) {
		c.releaseSourceMutation()
		t.Fatal("the index was refreshed while an edit was admitted")
	}
	c.releaseSourceMutation()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the refresh never ran after the edit")
	}
	if worktreeIndexRacy(t, f) {
		t.Fatal("the refresh after the edit left the index racily clean")
	}
}

// At daemon start every ready checkout of a family is healed, dormant ones
// (no coordinator) included.
func TestLifecycleStartHealsDormantCheckoutIndexes(t *testing.T) {
	f := newCoordinatorFixture(t)
	racifyWorktreeIndex(t, f)
	l := &CheckoutLifecycle{catalog: f.catalog}
	l.healFamilyRacyIndexes(t.Context(), f.familyID)
	if worktreeIndexRacy(t, f) {
		t.Fatal("the start-time sweep left a registered checkout racily clean")
	}
}
