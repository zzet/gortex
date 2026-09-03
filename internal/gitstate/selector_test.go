package gitstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// selectorRepo builds a repository with one commit on refs/heads/main and
// returns its directory.
func selectorRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, filepath.Join(tempRoot(t), "repo"))
}

func headCommit(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "HEAD^{commit}")
}

func headTree(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "HEAD^{tree}")
}

// TestResolveViewSelectorReadsEveryRefNamespace covers the three namespaces a
// view selector may name, and both tag flavours. Every one of them has to land
// on the same commit and tree, because they all point at the same commit.
func TestResolveViewSelectorReadsEveryRefNamespace(t *testing.T) {
	dir := selectorRepo(t)
	commit := headCommit(t, dir)
	tree := headTree(t, dir)

	git(t, dir, "tag", "lightweight")
	git(t, dir, "tag", "-a", "-m", "annotated", "annotated")
	git(t, dir, "update-ref", "refs/remotes/origin/main", commit)

	for _, ref := range []string{
		"refs/heads/main",
		"refs/tags/lightweight",
		"refs/tags/annotated",
		"refs/remotes/origin/main",
	} {
		resolved, err := ResolveViewSelector(context.Background(), dir, ViewSelectorGitRef, ref)
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		if resolved.FullRef != ref {
			t.Fatalf("resolve %s echoed ref %q", ref, resolved.FullRef)
		}
		if resolved.CommitOID != commit {
			t.Fatalf("resolve %s = commit %s, want %s", ref, resolved.CommitOID, commit)
		}
		if resolved.TreeOID != tree {
			t.Fatalf("resolve %s = tree %s, want %s", ref, resolved.TreeOID, tree)
		}
	}
}

// TestResolveViewSelectorRejectsATagOnATree pins the difference between "the
// selector names nothing here" and "the selector names something that is not a
// commit". A tag on a tree resolves fine as an object and still cannot back a
// view, and the two tag flavours reach that answer by different routes: the
// lightweight tag IS the tree, the annotated one is a tag object that will not
// peel.
func TestResolveViewSelectorRejectsATagOnATree(t *testing.T) {
	dir := selectorRepo(t)
	tree := headTree(t, dir)

	git(t, dir, "tag", "treetag", tree)
	git(t, dir, "tag", "-a", "-m", "tree", "treeannotated", tree)

	for _, ref := range []string{"refs/tags/treetag", "refs/tags/treeannotated"} {
		_, err := ResolveViewSelector(context.Background(), dir, ViewSelectorGitRef, ref)
		if !errors.Is(err, ErrRefNotCommit) {
			t.Fatalf("resolve %s = %v, want ErrRefNotCommit", ref, err)
		}
		if errors.Is(err, ErrRefNotAvailableLocally) {
			t.Fatalf("resolve %s reported an availability failure for an object that is present", ref)
		}
	}
}

// TestResolveViewSelectorReportsAbsentRefsAndObjects covers the availability
// half: a ref nobody created, and an object id nothing in the store holds.
func TestResolveViewSelectorReportsAbsentRefsAndObjects(t *testing.T) {
	dir := selectorRepo(t)
	absentOID := strings.Repeat("deadbeef", 5)

	cases := []struct {
		name  string
		kind  ViewSelectorKind
		value string
	}{
		{"absent branch", ViewSelectorGitRef, "refs/heads/never-created"},
		{"absent tag", ViewSelectorGitRef, "refs/tags/never-created"},
		{"absent commit", ViewSelectorCommit, absentOID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveViewSelector(context.Background(), dir, tc.kind, tc.value)
			if !errors.Is(err, ErrRefNotAvailableLocally) {
				t.Fatalf("resolve %s = %v, want ErrRefNotAvailableLocally", tc.value, err)
			}
		})
	}
}

// TestResolveViewSelectorCommitKind covers the commit selector: a commit id
// resolves to itself plus its tree, and an id naming any other kind of object
// is refused as not-a-commit rather than peeled towards one.
func TestResolveViewSelectorCommitKind(t *testing.T) {
	dir := selectorRepo(t)
	commit := headCommit(t, dir)
	tree := headTree(t, dir)

	resolved, err := ResolveViewSelector(context.Background(), dir, ViewSelectorCommit, commit)
	if err != nil {
		t.Fatalf("resolve commit: %v", err)
	}
	if resolved.FullRef != "" {
		t.Fatalf("a commit selector named ref %q", resolved.FullRef)
	}
	if resolved.CommitOID != commit || resolved.TreeOID != tree {
		t.Fatalf("resolve commit = %+v, want commit %s tree %s", resolved, commit, tree)
	}

	// The tag object is the interesting non-commit: peeling it WOULD reach a
	// commit, and the commit selector still refuses it.
	git(t, dir, "tag", "-a", "-m", "annotated", "annotated")
	tagOID := git(t, dir, "rev-parse", "refs/tags/annotated")
	for _, oid := range []string{tree, tagOID} {
		if _, err := ResolveViewSelector(context.Background(), dir, ViewSelectorCommit, oid); !errors.Is(err, ErrRefNotCommit) {
			t.Fatalf("resolve object %s = %v, want ErrRefNotCommit", oid, err)
		}
	}
}

// TestResolveViewSelectorNeverRunsGitForAShortName is the no-DWIM claim, and
// it is checked by making git unavailable rather than by reading the code: the
// only git on PATH records every invocation, so an empty record is proof that
// the name was refused before any lookup could disambiguate it.
//
// The names are all ones that WOULD resolve if they reached git — the
// repository really is on main, really has the tag, and really has a HEAD.
func TestResolveViewSelectorNeverRunsGitForAShortName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the recording git shim is a POSIX shell script")
	}
	dir := selectorRepo(t)
	git(t, dir, "tag", "v1")

	log := filepath.Join(t.TempDir(), "invocations")
	shim := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >> " + strconv.Quote(log) + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the git shim: %v", err)
	}
	t.Setenv("PATH", shim)

	for _, name := range []string{"main", "v1", "HEAD", "main~1", "refs/heads/main..refs/tags/v1", "main@{1}", "refs/notes/commits"} {
		if _, err := ResolveViewSelector(context.Background(), dir, ViewSelectorGitRef, name); err == nil {
			t.Fatalf("resolve %q succeeded", name)
		}
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			body, _ := os.ReadFile(log)
			t.Fatalf("resolving %q ran git: %s", name, body)
		}
	}
}
