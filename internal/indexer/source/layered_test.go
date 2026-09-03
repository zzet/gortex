package source

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// layeredFixture builds an upper and a lower checkout that disagree
// about some paths, plus the ownership predicate under test.
func layeredFixture(t *testing.T) (*LayeredSource, *FilesystemSource, *FilesystemSource) {
	t.Helper()
	upperDir := tempRoot(t)
	lowerDir := tempRoot(t)

	writeFile(t, upperDir, "app/main.go", "upper main\n", 0o644)
	writeFile(t, upperDir, "app/only-upper.go", "upper only\n", 0o644)
	writeFile(t, upperDir, "zz/late.go", "upper late\n", 0o644)
	// Held by the upper checkout but not claimed by it: the ownership
	// predicate, not the file's presence, decides who answers.
	writeFile(t, upperDir, "lib/stray.go", "upper stray\n", 0o644)

	writeFile(t, lowerDir, "app/main.go", "lower main\n", 0o644)
	writeFile(t, lowerDir, "app/only-lower.go", "lower only\n", 0o644)
	writeFile(t, lowerDir, "lib/util.go", "lower util\n", 0o644)
	writeFile(t, lowerDir, "README.md", "lower readme\n", 0o644)

	upper, err := NewFilesystemSource(upperDir)
	if err != nil {
		t.Fatalf("NewFilesystemSource(upper): %v", err)
	}
	lower, err := NewFilesystemSource(lowerDir)
	if err != nil {
		t.Fatalf("NewFilesystemSource(lower): %v", err)
	}
	owns := func(p string) bool {
		return strings.HasPrefix(p, "app/") || strings.HasPrefix(p, "zz/")
	}
	return NewLayeredSource(upper, owns, lower), upper, lower
}

func TestLayeredSourceRoutesByOwnership(t *testing.T) {
	src, _, _ := layeredFixture(t)
	defer src.Close()

	// An owned path is served by the upper layer even though the lower
	// layer has its own copy.
	if got, _ := readAll(t, src, "app/main.go"); got != "upper main\n" {
		t.Errorf("Open(app/main.go) = %q, want the upper layer's content", got)
	}
	if got, _ := readAll(t, src, "app/only-upper.go"); got != "upper only\n" {
		t.Errorf("Open(app/only-upper.go) = %q", got)
	}
	// An unowned path is served by the lower layer.
	if got, _ := readAll(t, src, "lib/util.go"); got != "lower util\n" {
		t.Errorf("Open(lib/util.go) = %q, want the lower layer's content", got)
	}
	if got, err := src.Stat("README.md"); err != nil || got.Path != "README.md" {
		t.Errorf("Stat(README.md) = %+v, %v", got, err)
	}

	// Routing, not merging: an owned path the upper layer does not hold
	// is absent, and an unowned path the upper layer does hold is not
	// consulted there.
	for _, p := range []string{"app/only-lower.go", "lib/stray.go"} {
		if _, err := src.Stat(p); !errors.Is(err, ErrNotInSource) {
			t.Errorf("Stat(%q) err = %v, want ErrNotInSource", p, err)
		}
		if rc, _, err := src.Open(p); !errors.Is(err, ErrNotInSource) {
			if rc != nil {
				rc.Close()
			}
			t.Errorf("Open(%q) err = %v, want ErrNotInSource", p, err)
		}
	}

	// A path outside either root is refused before routing.
	if _, err := src.Stat("../escape.go"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Stat(../escape.go) err = %v, want ErrOutsideRoot", err)
	}
}

func TestLayeredSourceWalksTheUnionInOrder(t *testing.T) {
	src, _, _ := layeredFixture(t)
	defer src.Close()

	want := []string{
		"README.md",         // lower, unowned
		"app/main.go",       // upper, owned, shadowing the lower copy
		"app/only-upper.go", // upper, owned, no lower counterpart
		"lib/util.go",       // lower, unowned
		"zz/late.go",        // upper, owned, after every lower path
	}
	metas := walkMetas(t, src)
	got := make([]string, 0, len(metas))
	for _, m := range metas {
		got = append(got, m.Path)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Walk paths =\n%q\nwant\n%q", got, want)
	}

	// The entry for a shadowed path is the upper layer's.
	for _, m := range metas {
		if m.Path == "app/main.go" && m.Size != int64(len("upper main\n")) {
			t.Errorf("Walk reported size %d for app/main.go, want the upper layer's entry", m.Size)
		}
	}

	// A callback error stops the walk and is returned unchanged.
	sentinel := errors.New("stop")
	seen := 0
	if err := src.Walk(t.Context(), func(FileMeta) error {
		seen++
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Walk err = %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("Walk visited %d entries after the callback failed", seen)
	}

	// Cancellation propagates from whichever layer is being walked.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := src.Walk(cancelled, func(FileMeta) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("Walk err = %v, want context.Canceled", err)
	}
}

func TestLayeredSourceWithoutOwnershipUsesTheLowerLayer(t *testing.T) {
	src, _, _ := layeredFixture(t)
	defer src.Close()
	upper, lower := src.upper, src.lower

	plain := NewLayeredSource(upper, nil, lower)
	if got, _ := readAll(t, plain, "app/main.go"); got != "lower main\n" {
		t.Errorf("Open(app/main.go) = %q, want the lower layer's content", got)
	}
	want := []string{"README.md", "app/main.go", "app/only-lower.go", "lib/util.go"}
	if got := walkPaths(t, plain); !slices.Equal(got, want) {
		t.Errorf("Walk paths = %q, want %q", got, want)
	}
}

func TestLayeredSourceIdentityAndClose(t *testing.T) {
	src, upper, lower := layeredFixture(t)

	want := "layered:" + upper.Identity() + "|" + lower.Identity()
	if src.Identity() != want {
		t.Errorf("Identity = %q, want %q", src.Identity(), want)
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Both layers really were closed.
	if _, err := upper.Stat("app/main.go"); err == nil {
		t.Error("the upper layer still answers after Close")
	}
	if _, err := lower.Stat("README.md"); err == nil {
		t.Error("the lower layer still answers after Close")
	}
	if err := src.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestLayeredSourceOverAGitTree(t *testing.T) {
	dir, fixture, tree := commitFixture(t)
	committed := newTreeSource(t, dir, tree)

	// A working copy that only owns one path stacked over the tree.
	scratch := tempRoot(t)
	writeFile(t, scratch, "top.txt", "edited\n", 0o644)
	edits, err := NewFilesystemSource(scratch)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	src := NewLayeredSource(edits, func(p string) bool { return p == "top.txt" }, committed)
	defer func() { _ = edits.Close() }()

	if got, _ := readAll(t, src, "top.txt"); got != "edited\n" {
		t.Errorf("Open(top.txt) = %q, want the working copy's content", got)
	}
	if got, _ := readAll(t, src, "a/b/nested.txt"); got != fixture.files["a/b/nested.txt"] {
		t.Errorf("Open(a/b/nested.txt) = %q, want the committed content", got)
	}
	// The union is the tree's namespace, with the one owned path served
	// from the working copy — the two implementations agree on order.
	if got, want := walkPaths(t, src), fixture.paths(); !slices.Equal(got, want) {
		t.Errorf("Walk paths =\n%q\nwant\n%q", got, want)
	}
}
