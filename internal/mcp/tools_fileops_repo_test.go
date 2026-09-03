package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRepoPrefixLookup struct{ prefixes []string }

func (f fakeRepoPrefixLookup) RepoPrefixes() []string              { return f.prefixes }
func (f fakeRepoPrefixLookup) RepoRoot(p string) (string, bool)    { return "/" + p, true }
func (f fakeRepoPrefixLookup) LinkedWorktreeRoots(string) []string { return nil }

// TestMatchedRepoPrefix_LongestMatchWins pins the path→repo inference: a
// nested repo must win over its parent for a path under the child, and
// the result must not depend on RepoPrefixes() iteration order.
func TestMatchedRepoPrefix_LongestMatchWins(t *testing.T) {
	for _, order := range [][]string{{"a", "a/b"}, {"a/b", "a"}} {
		mi := fakeRepoPrefixLookup{prefixes: order}
		require.Equalf(t, "a/b", matchedRepoPrefix(mi, "a/b/internal/x.go"),
			"longest matching prefix must win (order %v)", order)
		require.Equal(t, "a", matchedRepoPrefix(mi, "a/main.go"))
	}
	require.Equal(t, "", matchedRepoPrefix(fakeRepoPrefixLookup{prefixes: []string{"a"}}, "c/x.go"),
		"a path under no tracked repo resolves to no prefix")
	require.Equal(t, "", matchedRepoPrefix(nil, "a/x.go"))
}

// fakeRepoRoots maps repo prefixes to real on-disk roots so the
// existence-gated anchoring in anchorUnprefixedExisting can be exercised
// against actual files.
type fakeRepoRoots struct{ roots map[string]string }

func (f fakeRepoRoots) RepoPrefixes() []string {
	ps := make([]string, 0, len(f.roots))
	for p := range f.roots {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}
func (f fakeRepoRoots) RepoRoot(p string) (string, bool)    { r, ok := f.roots[p]; return r, ok }
func (f fakeRepoRoots) LinkedWorktreeRoots(string) []string { return nil }

// TestAnchorUnprefixedExisting pins the multi-repo convenience: a bare
// repo-relative path is anchored only when exactly one tracked repo
// actually contains it. Zero matches (new file) and multiple matches
// (genuinely ambiguous) both refuse to guess.
func TestAnchorUnprefixedExisting(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	mustWrite := func(root, rel string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("package x\n"), 0o644))
	}
	mustWrite(a, "internal/x.go") // only in repo "alpha"
	mustWrite(a, "shared/y.go")   // in both repos
	mustWrite(b, "shared/y.go")   //
	mi := fakeRepoRoots{roots: map[string]string{"alpha": a, "beta": b}}

	// Unique existing match → anchored to the lone owning repo, with the
	// repo-prefixed relPath for session bookkeeping.
	abs, rel, n := anchorUnprefixedExisting(mi, viewPathRoot{}, "internal/x.go")
	require.Equal(t, 1, n)
	require.Equal(t, filepath.Join(a, "internal", "x.go"), abs)
	require.Equal(t, "alpha/internal/x.go", rel)

	// Present in two repos → ambiguous, caller must disambiguate.
	_, _, n = anchorUnprefixedExisting(mi, viewPathRoot{}, "shared/y.go")
	require.Equal(t, 2, n)

	// Missing everywhere (a new write target) → no anchor.
	_, _, n = anchorUnprefixedExisting(mi, viewPathRoot{}, "internal/new.go")
	require.Equal(t, 0, n)

	// A `..` traversal is rejected by the containment gate, not anchored.
	_, _, n = anchorUnprefixedExisting(mi, viewPathRoot{}, "../escape.go")
	require.Equal(t, 0, n)

	// An absolute path is never an "unprefixed" path — left to the caller.
	_, _, n = anchorUnprefixedExisting(mi, viewPathRoot{}, filepath.Join(a, "internal", "x.go"))
	require.Equal(t, 0, n)
}
