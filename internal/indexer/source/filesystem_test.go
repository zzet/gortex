package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFilesystemSourceServesTheFixture(t *testing.T) {
	dir := tempRoot(t)
	fixture := buildContentFixture(t, dir)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	if got, want := walkPaths(t, src), fixture.paths(); !slices.Equal(got, want) {
		t.Fatalf("Walk paths =\n%q\nwant\n%q", got, want)
	}

	for rel, want := range fixture.files {
		got, meta := readAll(t, src, rel)
		if got != want {
			t.Errorf("Open(%q) content = %q, want %q", rel, got, want)
		}
		if meta.Path != rel {
			t.Errorf("Open(%q) meta.Path = %q", rel, meta.Path)
		}
		if meta.Size != int64(len(want)) {
			t.Errorf("Open(%q) meta.Size = %d, want %d", rel, meta.Size, len(want))
		}
		if meta.Symlink {
			t.Errorf("Open(%q) reported a symlink", rel)
		}
		wantExec := slices.Contains(fixture.execs, rel)
		if gotExec := meta.Mode&0o111 != 0; gotExec != wantExec {
			t.Errorf("Open(%q) mode = %v, exec = %v, want exec = %v", rel, meta.Mode, gotExec, wantExec)
		}
	}
}

func TestFilesystemSourceReportsSymlinksWithoutFollowingThem(t *testing.T) {
	dir := tempRoot(t)
	buildContentFixture(t, dir)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	meta, err := src.Stat("link.txt")
	if err != nil {
		t.Fatalf("Stat(link.txt): %v", err)
	}
	if !meta.Symlink || meta.Mode&fs.ModeSymlink == 0 {
		t.Fatalf("Stat(link.txt) = %+v, want a symlink", meta)
	}
	if meta.SymlinkTarget != "a/b/nested.txt" {
		t.Errorf("SymlinkTarget = %q, want %q", meta.SymlinkTarget, "a/b/nested.txt")
	}
	if meta.Size != int64(len("a/b/nested.txt")) {
		t.Errorf("Size = %d, want the length of the target text", meta.Size)
	}

	// A link to a directory resolves to something that is not content,
	// which the caller learns from Open rather than from a read error.
	symlink(t, dir, "dirlink", "a/b")
	if rc, _, err := src.Open("dirlink"); !errors.Is(err, ErrNotInSource) {
		if rc != nil {
			rc.Close()
		}
		t.Errorf("Open(dirlink) err = %v, want ErrNotInSource", err)
	}

	// Open is the one place a link is followed, and it hands back the
	// target's content while still describing the link itself.
	content, openMeta := readAll(t, src, "link.txt")
	if content != "nested content\n" {
		t.Errorf("Open(link.txt) content = %q, want the target's content", content)
	}
	if !openMeta.Symlink || openMeta.SymlinkTarget != meta.SymlinkTarget {
		t.Errorf("Open(link.txt) meta = %+v, want the same metadata Stat reports", openMeta)
	}
}

func TestFilesystemSourceRefusesPathsOutsideTheRoot(t *testing.T) {
	requirePOSIX(t)
	parent := tempRoot(t)
	dir := filepath.Join(parent, "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	writeFile(t, parent, "outside.txt", "secret\n", 0o644)
	writeFile(t, dir, "inside.txt", "public\n", 0o644)
	symlink(t, dir, "escape.txt", "../outside.txt")
	symlink(t, dir, "absolute.txt", filepath.Join(parent, "outside.txt"))
	symlink(t, dir, "sub/escape.txt", "../../outside.txt")

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	outside := []string{
		"../outside.txt",
		"sub/../../outside.txt",
		filepath.Join(parent, "outside.txt"),
		"/etc/hosts",
		"escape.txt",
		"absolute.txt",
		"sub/escape.txt",
	}
	for _, p := range outside {
		rc, _, err := src.Open(p)
		if !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Open(%q) err = %v, want ErrOutsideRoot", p, err)
		}
		if rc != nil {
			rc.Close()
			t.Errorf("Open(%q) returned a reader for content outside the root", p)
		}
	}
	for _, p := range []string{"../outside.txt", "/etc/hosts", "sub/../../outside.txt"} {
		if _, err := src.Stat(p); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Stat(%q) err = %v, want ErrOutsideRoot", p, err)
		}
	}

	// An escaping link is still describable: only its content is
	// refused.
	meta, err := src.Stat("escape.txt")
	if err != nil {
		t.Fatalf("Stat(escape.txt): %v", err)
	}
	if !meta.Symlink || meta.SymlinkTarget != "../outside.txt" {
		t.Errorf("Stat(escape.txt) = %+v, want the raw escaping target", meta)
	}

	// And a path that stays inside still resolves, so the confinement
	// is not simply refusing everything.
	if content, _ := readAll(t, src, "./inside.txt"); content != "public\n" {
		t.Errorf("Open(inside.txt) content = %q", content)
	}
}

func TestFilesystemSourceReportsAbsentAndNonFilePaths(t *testing.T) {
	dir := tempRoot(t)
	buildContentFixture(t, dir)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	cases := []string{
		"nope.txt",         // never existed
		"a/b/nope.txt",     // absent below a real directory
		"a",                // a directory is not content
		"top.txt/child",    // a path through a file
		".",                // the root itself
		"",                 // no path at all
		"has\x00nul.txt",   // not a usable name
		"a/b/../b/nope.md", // normalized, still absent
	}
	for _, p := range cases {
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
}

func TestFilesystemSourceWalkSkipsTheRepositoryGitDirOnly(t *testing.T) {
	dir := tempRoot(t)
	writeFile(t, dir, "main.go", "package main\n", 0o644)
	writeFile(t, dir, ".git/config", "[core]\n", 0o644)
	writeFile(t, dir, ".git/objects/ab/cdef", "object\n", 0o644)
	writeFile(t, dir, "vendor/dep/.git/config", "[core]\n", 0o644)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	want := []string{"main.go", "vendor/dep/.git/config"}
	if got := walkPaths(t, src); !slices.Equal(got, want) {
		t.Fatalf("Walk paths = %q, want %q", got, want)
	}

	// The skip is an enumeration rule, not an access rule: a caller
	// that asks for a path under .git still gets it.
	if content, _ := readAll(t, src, ".git/config"); content != "[core]\n" {
		t.Errorf("Open(.git/config) content = %q", content)
	}
}

func TestFilesystemSourceWalkOrdersDirectoriesWhereTheirPathsSort(t *testing.T) {
	dir := tempRoot(t)
	writeFile(t, dir, "src-x.go", "x\n", 0o644)
	writeFile(t, dir, "src.go", "s\n", 0o644)
	writeFile(t, dir, "src/a.go", "a\n", 0o644)
	writeFile(t, dir, "src/b/c.go", "c\n", 0o644)
	writeFile(t, dir, "srcz.go", "z\n", 0o644)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	// Byte order of the full path: '-' < '.' < '/' < 'z'.
	want := []string{"src-x.go", "src.go", "src/a.go", "src/b/c.go", "srcz.go"}
	if got := walkPaths(t, src); !slices.Equal(got, want) {
		t.Fatalf("Walk paths = %q, want %q", got, want)
	}
}

func TestFilesystemSourceWalkHonoursContextCancellation(t *testing.T) {
	dir := tempRoot(t)
	for _, rel := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		writeFile(t, dir, rel, "x\n", 0o644)
	}

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer src.Close()

	// Cancelled before the walk starts: nothing is visited.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	visited := 0
	if err := src.Walk(cancelled, func(FileMeta) error { visited++; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk err = %v, want context.Canceled", err)
	}
	if visited != 0 {
		t.Errorf("Walk visited %d entries after cancellation", visited)
	}

	// Cancelled from inside the callback: the walk stops there.
	midway, cancelMidway := context.WithCancel(t.Context())
	defer cancelMidway()
	seen := 0
	err = src.Walk(midway, func(FileMeta) error {
		seen++
		cancelMidway()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk err = %v, want context.Canceled", err)
	}
	if seen != 1 {
		t.Errorf("Walk visited %d entries, want 1 before the cancellation took effect", seen)
	}
}

func TestFilesystemSourceIdentityAndClose(t *testing.T) {
	dir := tempRoot(t)
	writeFile(t, dir, "a.txt", "a\n", 0o644)

	src, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	if want := "fs:" + dir; src.Identity() != want {
		t.Errorf("Identity = %q, want %q", src.Identity(), want)
	}
	// A relative spelling of the same directory is the same identity.
	rel, err := filepath.Rel(filepath.Dir(dir), dir)
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	t.Chdir(filepath.Dir(dir))
	relSrc, err := NewFilesystemSource(rel)
	if err != nil {
		t.Fatalf("NewFilesystemSource(relative): %v", err)
	}
	defer relSrc.Close()
	if relSrc.Identity() != src.Identity() {
		t.Errorf("Identity of the relative spelling = %q, want %q", relSrc.Identity(), src.Identity())
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := src.Stat("a.txt"); err == nil {
		t.Error("Stat after Close succeeded, want an error")
	}
}

func TestNewFilesystemSourceRejectsBadRoots(t *testing.T) {
	if _, err := NewFilesystemSource(""); err == nil {
		t.Error("NewFilesystemSource(\"\") succeeded, want an error")
	}
	dir := tempRoot(t)
	if _, err := NewFilesystemSource(filepath.Join(dir, "absent")); err == nil {
		t.Error("NewFilesystemSource(absent) succeeded, want an error")
	}
	writeFile(t, dir, "file.txt", "x\n", 0o644)
	if _, err := NewFilesystemSource(filepath.Join(dir, "file.txt")); err == nil {
		t.Error("NewFilesystemSource(file) succeeded, want an error")
	}
}
