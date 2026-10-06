package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/search/trigram"
)

// The checkout searcher over a working-tree chain: every generation of the
// chain contributes its claims to the corpus, not only the routed top, and a
// moved working tree patches the searcher path by path instead of rebuilding
// it over the whole checkout.

// TestCheckoutSearcherFindsAFileAddedInALowerChainedGeneration is the chained
// corpus defect: a file added by the chain's root and left alone by the child
// on top of it is part of the working copy the view serves, so a text search
// must find it even though the routed top generation claims nothing about it.
func TestCheckoutSearcherFindsAFileAddedInALowerChainedGeneration(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})

	worktreeWrite(t, f.worktree, "added.go", "package fixture\n\nfunc Added() {\n\t// lower-generation-marker\n}\n")
	first := coordinatorReconcile(t, c)
	if !first.DirtyBuilt || first.DirtyChainDepth != 1 {
		t.Fatalf("the first working-tree build = %+v, want a direct root", first)
	}

	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	second := coordinatorReconcile(t, c)
	if !second.DirtyBuilt || second.DirtyParentGenerationID != first.DirtyGenerationID || second.DirtyChainDepth != 2 {
		t.Fatalf("the second build = %+v, want a child of %d at depth 2", second, first.DirtyGenerationID)
	}

	searcher, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("build the checkout searcher: %v", err)
	}
	if got := searcher.Grep("lower-generation-marker", 0); len(got) != 1 || got[0].Path != "added.go" {
		t.Errorf("a file the chain root added is not searchable through the child: %v", grepPaths(got))
	}
	if got := searcher.Grep("first working-tree edit", 0); len(got) != 1 || got[0].Path != "helper.go" {
		t.Errorf("the child's own edit is not searchable: %v", grepPaths(got))
	}
	// And the whole answer is a clean index's.
	textAssertCleanParity(t, f, c, searcher, "chained-add")
}

// TestCheckoutSearcherHidesAFileDeletedInALowerChainedGeneration is the
// masking half: a file the chain root deleted stays out of the corpus when the
// child on top says nothing about it.
func TestCheckoutSearcherHidesAFileDeletedInALowerChainedGeneration(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	if err := os.Remove(filepath.Join(f.worktree, "gone.go")); err != nil {
		t.Fatalf("remove gone.go: %v", err)
	}
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	first := coordinatorReconcile(t, c)
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	second := coordinatorReconcile(t, c)
	if second.DirtyParentGenerationID != first.DirtyGenerationID {
		t.Fatalf("the second build = %+v, want a child of %d", second, first.DirtyGenerationID)
	}
	searcher, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("build the checkout searcher: %v", err)
	}
	if corpus := c.textState.corpus; corpus.has("gone.go") {
		t.Errorf("the corpus holds gone.go, which the chain root deleted: %v", corpus.paths())
	}
	textAssertCleanParity(t, f, c, searcher, "chained-delete")
}

// textQueries is the fixed query set the equality checks digest: literals
// that hit every file of the fixture, the markers the steps write, a query
// that hits nothing, and regexps with and without a trigram pre-filter, each
// unlimited and at small limits so the order of a limited answer is checked.
var textQueries = []struct {
	literal string
	re      *regexp.Regexp
}{
	{literal: "package fixture"},
	{literal: "func "},
	{literal: "Helper"},
	{literal: "Island"},
	{literal: "working-tree edit"},
	{literal: "text-step-marker"},
	{literal: "renamed-marker"},
	{literal: "no-such-literal-anywhere"},
	{literal: "func [A-Z][a-zA-Z]*", re: regexp.MustCompile(`func [A-Z][a-zA-Z]*`)},
	{literal: "", re: regexp.MustCompile(`^}$`)},
}

// textDigest runs the query set against a searcher and digests the answers in
// the order they were returned.
func textDigest(s *trigram.Searcher) (string, []string) {
	var lines []string
	for _, q := range textQueries {
		for _, limit := range []int{0, 1, 2} {
			var got []trigram.Match
			if q.re != nil {
				got = s.GrepRegexp(q.re, extractRegexLiterals(q.literal), "", limit)
			} else {
				got = s.Grep(q.literal, limit)
			}
			for _, m := range got {
				lines = append(lines, fmt.Sprintf("%q|%d|%s:%d:%s", q.literal, limit, m.Path, m.Line, m.Text))
			}
			lines = append(lines, fmt.Sprintf("%q|%d|n=%d", q.literal, limit, len(got)))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), lines
}

// textCleanSearcher indexes the worktree from scratch and builds a searcher
// over the file set that index holds: the oracle a checkout searcher must
// agree with.
func textCleanSearcher(t *testing.T, f *coordinatorFixture, label string) *trigram.Searcher {
	t.Helper()
	clean := builderOpenStore(t, "text-clean-"+label)
	builderIndex(t, clean, f.worktree)
	rows, err := clean.FileMetasForRepo(builderRepoPrefix)
	if err != nil {
		t.Fatalf("read the clean file inventory: %v", err)
	}
	var paths []string
	for _, row := range rows {
		if rel, owned := builderRelPath(builderRepoPrefix, row.FilePath); owned {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	return trigram.Build(f.worktree, paths)
}

// textAssertCleanParity requires the checkout searcher to answer the query set
// exactly as a searcher over a clean index of the worktree does.
func textAssertCleanParity(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, s *trigram.Searcher, label string) {
	t.Helper()
	got, gotLines := textDigest(s)
	want, wantLines := textDigest(textCleanSearcher(t, f, label))
	if got != want {
		t.Errorf("%s: the checkout searcher differs from a clean index:\n%s", label,
			strings.Join(parityDiff("text", gotLines, wantLines), "\n"))
	}
}

// textAssertPatchEqualsRebuild requires the patched searcher to answer the
// query set exactly as a searcher rebuilt over the same corpus does, and
// exactly as a clean index of the worktree does. It returns whether the first
// equality held, so the mutation check can observe it failing.
func textAssertPatchEqualsRebuild(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, label string) (bool, checkoutTextStats) {
	t.Helper()
	patched, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("%s: bring the checkout searcher up to date: %v", label, err)
	}
	stats := c.textSearchStats()
	c.textMu.Lock()
	rebuilt := trigram.Build(f.worktree, c.textState.corpus.paths())
	c.textMu.Unlock()
	got, gotLines := textDigest(patched)
	want, wantLines := textDigest(rebuilt)
	equal := got == want
	if !equal {
		t.Logf("%s: patched differs from rebuilt:\n%s", label, strings.Join(parityDiff("text", gotLines, wantLines), "\n"))
	}
	clean, cleanLines := textDigest(textCleanSearcher(t, f, label))
	if want != clean {
		t.Errorf("%s: the rebuilt corpus differs from a clean index:\n%s", label,
			strings.Join(parityDiff("text", wantLines, cleanLines), "\n"))
	}
	return equal, stats
}

// textStep is one edit of the patch-equals-rebuild sequence.
type textStep struct {
	name string
	edit func(t *testing.T, f *coordinatorFixture)
}

func textSteps() []textStep {
	return []textStep{
		{"first-edit", func(t *testing.T, f *coordinatorFixture) {
			builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
		}},
		{"add", func(t *testing.T, f *coordinatorFixture) {
			worktreeWrite(t, f.worktree, "aardvark.go", "package fixture\n\nfunc Aardvark() {\n\t// text-step-marker add\n}\n")
		}},
		{"modify", func(t *testing.T, f *coordinatorFixture) {
			worktreeWrite(t, f.worktree, "aardvark.go", "package fixture\n\nfunc Aardvark() {\n\t// text-step-marker modified\n\tHelper()\n}\n")
		}},
		{"delete", func(t *testing.T, f *coordinatorFixture) {
			if err := os.Remove(filepath.Join(f.worktree, "island.go")); err != nil {
				t.Fatalf("remove island.go: %v", err)
			}
		}},
		{"rename", func(t *testing.T, f *coordinatorFixture) {
			body, err := os.ReadFile(filepath.Join(f.worktree, "caller.go"))
			if err != nil {
				t.Fatalf("read caller.go: %v", err)
			}
			if err := os.Remove(filepath.Join(f.worktree, "caller.go")); err != nil {
				t.Fatalf("remove caller.go: %v", err)
			}
			worktreeWrite(t, f.worktree, "a_renamed.go", string(body)+"\n// renamed-marker\n")
		}},
		{"undo", func(t *testing.T, f *coordinatorFixture) {
			tree := builderTreeA()
			for _, name := range []string{"helper.go", "island.go", "caller.go"} {
				worktreeWrite(t, f.worktree, name, tree[name])
			}
			for _, name := range []string{"aardvark.go", "a_renamed.go"} {
				if err := os.Remove(filepath.Join(f.worktree, name)); err != nil {
					t.Fatalf("remove %s: %v", name, err)
				}
			}
			// Leave one edit so the checkout keeps a working-tree layer.
			worktreeWrite(t, f.worktree, "core.go", tree["core.go"]+"\n// text-step-marker undo\n")
		}},
	}
}

// TestCheckoutSearcherPatchEqualsRebuild drives add, modify, delete, rename and
// undo through a chained coordinator and requires, after every step, that the
// searcher was patched rather than rebuilt and that the patched searcher
// answers a fixed query set exactly as a rebuild over the same corpus and a
// clean index of the worktree do.
func TestCheckoutSearcherPatchEqualsRebuild(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	for i, step := range textSteps() {
		step.edit(t, f)
		cycle := coordinatorReconcile(t, c)
		if !cycle.DirtyBuilt {
			t.Fatalf("%s: the cycle built nothing: %+v", step.name, cycle)
		}
		equal, stats := textAssertPatchEqualsRebuild(t, f, c, step.name)
		if !equal {
			t.Errorf("%s: the patched searcher differs from a rebuild over the same corpus", step.name)
		}
		want := "patch"
		if i == 0 {
			want = textRebuildFirstUse
		}
		if stats.Last != want {
			t.Errorf("%s: the searcher's last update was %q, want %q (stats %+v, cycle depth %d reason %q)",
				step.name, stats.Last, want, stats, cycle.DirtyChainDepth, cycle.DirtyChainReason)
		}
		t.Logf("%s: depth=%d reason=%q last=%s paths=%d", step.name, cycle.DirtyChainDepth, cycle.DirtyChainReason, stats.Last, stats.LastPaths)
	}
	stats := c.textSearchStats()
	if stats.Rebuilds[textRebuildFirstUse] != 1 || len(stats.Rebuilds) != 1 {
		t.Errorf("rebuilds %v, want exactly the first use", stats.Rebuilds)
	}
}

// TestCheckoutSearcherPatchCheckFailsWhenAPathIsSkipped is the mutation check
// for the equality test: a patch that leaves out the one path an edit changed
// must be caught.
func TestCheckoutSearcherPatchCheckFailsWhenAPathIsSkipped(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	steps := textSteps()
	for _, step := range steps[:2] {
		step.edit(t, f)
		coordinatorReconcile(t, c)
		if equal, _ := textAssertPatchEqualsRebuild(t, f, c, step.name); !equal {
			t.Fatalf("%s: the unmutated patch already differs", step.name)
		}
	}
	c.textMu.Lock()
	c.textState.skipPatch = func(rel string) bool { return rel == "aardvark.go" }
	c.textMu.Unlock()
	steps[2].edit(t, f) // modify aardvark.go
	coordinatorReconcile(t, c)
	patched, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("bring the checkout searcher up to date: %v", err)
	}
	c.textMu.Lock()
	rebuilt := trigram.Build(f.worktree, c.textState.corpus.paths())
	c.textMu.Unlock()
	got, _ := textDigest(patched)
	want, _ := textDigest(rebuilt)
	if got == want {
		t.Fatal("a patch that skipped the modified path still digests equal to a rebuild: the equality check cannot fail")
	}
}

// TestCheckoutSearcherRebuildsWhenTheCommitMoves pins the rebuild arm: a commit
// in the worktree moves the route's commit generation, which no working-tree
// manifest describes, so the searcher is rebuilt over the whole corpus.
func TestCheckoutSearcherRebuildsWhenTheCommitMoves(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	coordinatorReconcile(t, c)
	if _, err := c.textSearcher(context.Background()); err != nil {
		t.Fatalf("build the checkout searcher: %v", err)
	}
	before := f.route().CommitGenerationID

	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "helper edit")
	worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// text-step-marker after commit\n}\n")
	coordinatorReconcile(t, c)
	if after := f.route().CommitGenerationID; after == before {
		t.Fatalf("the commit generation did not move (%d)", after)
	}
	equal, stats := textAssertPatchEqualsRebuild(t, f, c, "commit-moved")
	if !equal {
		t.Error("the searcher differs from a rebuild after the commit moved")
	}
	if stats.Last != textRebuildCommitMoved || stats.Rebuilds[textRebuildCommitMoved] != 1 {
		t.Errorf("stats %+v, want one rebuild for %s", stats, textRebuildCommitMoved)
	}
}

// TestCheckoutSearcherKeepsThePublishedStateUntilTheRouteMoves pins the key:
// a cycle records its sample before it builds, and a search in between must
// not re-key to a tree the route does not describe yet — the publication that
// follows changes the route and brings the patch.
func TestCheckoutSearcherKeepsThePublishedStateUntilTheRouteMoves(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	coordinatorReconcile(t, c)
	if _, err := c.textSearcher(context.Background()); err != nil {
		t.Fatalf("build the checkout searcher: %v", err)
	}
	worktreeWrite(t, f.worktree, "aardvark.go", "package fixture\n\n// text-step-marker pending\n")
	c.noteDirtyFingerprint("a-sample-the-route-does-not-describe-yet")
	s, err := c.textSearcher(context.Background())
	if err != nil {
		t.Fatalf("search before the publication: %v", err)
	}
	if stats := c.textSearchStats(); stats.Last != "patch" || stats.LastPaths != 0 {
		t.Errorf("a fingerprint the route does not describe yet did %s over %d paths, want an empty patch", stats.Last, stats.LastPaths)
	}
	if got := s.Grep("text-step-marker pending", 0); len(got) != 0 {
		t.Errorf("an unpublished file answered: %v", grepPaths(got))
	}
	coordinatorReconcile(t, c)
	if equal, stats := textAssertPatchEqualsRebuild(t, f, c, "after-publication"); !equal || stats.Last != "patch" || stats.LastPaths == 0 {
		t.Errorf("after the publication: equal=%v stats=%+v, want a non-empty patch equal to a rebuild", equal, stats)
	}
}
