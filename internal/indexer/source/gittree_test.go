package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// commitFixture builds the shared corpus inside a fresh repository,
// commits it, and returns the worktree directory and the tree id.
func commitFixture(t *testing.T) (string, contentFixture, string) {
	t.Helper()
	dir := tempRoot(t)
	initRepo(t, dir)
	fixture := buildContentFixture(t, dir)
	tree := commitAll(t, dir, "fixture")
	return dir, fixture, tree
}

// newTreeSource opens a tree source and closes it when the test ends.
func newTreeSource(t *testing.T, dir, tree string) *GitTreeSource {
	t.Helper()
	src, err := NewGitTreeSource(t.Context(), dir, tree)
	if err != nil {
		t.Fatalf("NewGitTreeSource: %v", err)
	}
	t.Cleanup(func() { src.Close() })
	return src
}

func TestGitTreeSourceMatchesTheCheckout(t *testing.T) {
	dir, fixture, tree := commitFixture(t)
	tree0 := newTreeSource(t, dir, tree)

	disk, err := NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	defer disk.Close()

	// Both sources enumerate the same namespace in the same order, so
	// one can stand in for the other.
	treePaths := walkPaths(t, tree0)
	if want := fixture.paths(); !slices.Equal(treePaths, want) {
		t.Fatalf("tree Walk paths =\n%q\nwant\n%q", treePaths, want)
	}
	if diskPaths := walkPaths(t, disk); !slices.Equal(treePaths, diskPaths) {
		t.Fatalf("tree Walk paths =\n%q\ndiffer from the checkout\n%q", treePaths, diskPaths)
	}

	for rel, want := range fixture.files {
		got, meta := readAll(t, tree0, rel)
		if got != want {
			t.Errorf("Open(%q) content = %q, want %q", rel, got, want)
		}
		diskContent, diskMeta := readAll(t, disk, rel)
		if got != diskContent {
			t.Errorf("Open(%q) content differs from the checkout: %q vs %q", rel, got, diskContent)
		}
		if meta.Size != diskMeta.Size {
			t.Errorf("Open(%q) size = %d, checkout reports %d", rel, meta.Size, diskMeta.Size)
		}
		if meta.Mode&0o111 != diskMeta.Mode&0o111 {
			t.Errorf("Open(%q) exec bit = %v, checkout reports %v", rel, meta.Mode, diskMeta.Mode)
		}
		if meta.Symlink {
			t.Errorf("Open(%q) reported a symlink", rel)
		}
	}
}

func TestGitTreeSourceReportsModesAndSymlinks(t *testing.T) {
	dir, _, tree := commitFixture(t)
	src := newTreeSource(t, dir, tree)

	executable, err := src.Stat("run.sh")
	if err != nil {
		t.Fatalf("Stat(run.sh): %v", err)
	}
	if executable.Mode != 0o755 {
		t.Errorf("Stat(run.sh) mode = %v, want 0755", executable.Mode)
	}
	plain, err := src.Stat("top.txt")
	if err != nil {
		t.Fatalf("Stat(top.txt): %v", err)
	}
	if plain.Mode != 0o644 {
		t.Errorf("Stat(top.txt) mode = %v, want 0644", plain.Mode)
	}

	link, err := src.Stat("link.txt")
	if err != nil {
		t.Fatalf("Stat(link.txt): %v", err)
	}
	if !link.Symlink || link.Mode&fs.ModeSymlink == 0 {
		t.Fatalf("Stat(link.txt) = %+v, want a symlink", link)
	}
	if link.SymlinkTarget != "a/b/nested.txt" {
		t.Errorf("Stat(link.txt) target = %q, want %q", link.SymlinkTarget, "a/b/nested.txt")
	}
	if link.Size != int64(len("a/b/nested.txt")) {
		t.Errorf("Stat(link.txt) size = %d, want the length of the target text", link.Size)
	}
	// The target is cached after the first read, and a second Stat
	// still reports it.
	if again, err := src.Stat("link.txt"); err != nil || again.SymlinkTarget != link.SymlinkTarget {
		t.Errorf("second Stat(link.txt) = %+v, %v", again, err)
	}
	// A tree source hands back the link blob rather than following the
	// link, which is the one place it differs from a checkout.
	if content, _ := readAll(t, src, "link.txt"); content != "a/b/nested.txt" {
		t.Errorf("Open(link.txt) content = %q, want the link target text", content)
	}
	// Walk reports the same resolved target it Stat reports.
	for _, m := range walkMetas(t, src) {
		if m.Path == "link.txt" && m.SymlinkTarget != link.SymlinkTarget {
			t.Errorf("Walk reported target %q, want %q", m.SymlinkTarget, link.SymlinkTarget)
		}
	}
}

func TestGitTreeSourceSkipsSubmoduleEntries(t *testing.T) {
	dir := tempRoot(t)
	initRepo(t, dir)
	requirePOSIX(t)
	writeFile(t, dir, "top.txt", "top\n", 0o644)
	git(t, dir, "add", "-A")
	// A gitlink records another repository's commit id; there is no
	// content in this repository to serve for it. It is staged after
	// the worktree files, since staging everything again would drop an
	// index entry that has no directory on disk.
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,sub")
	git(t, dir, "commit", "-q", "-m", "with submodule")
	tree := git(t, dir, "rev-parse", "HEAD^{tree}")

	// The fixture really does record a gitlink.
	if listing := git(t, dir, "ls-tree", "-r", "HEAD"); !strings.Contains(listing, "160000 commit") {
		t.Fatalf("fixture has no gitlink entry:\n%s", listing)
	}

	src := newTreeSource(t, dir, tree)
	if got, want := walkPaths(t, src), []string{"top.txt"}; !slices.Equal(got, want) {
		t.Fatalf("Walk paths = %q, want %q", got, want)
	}
	if _, err := src.Stat("sub"); !errors.Is(err, ErrNotInSource) {
		t.Errorf("Stat(sub) err = %v, want ErrNotInSource", err)
	}
	if rc, _, err := src.Open("sub"); !errors.Is(err, ErrNotInSource) {
		if rc != nil {
			rc.Close()
		}
		t.Errorf("Open(sub) err = %v, want ErrNotInSource", err)
	}
}

func TestNewGitTreeSourceRejectsMalformedObjectIDsBeforeRunningGit(t *testing.T) {
	dir := tempRoot(t)
	marker := filepath.Join(dir, "git-was-invoked")
	t.Setenv("GORTEX_SOURCE_TEST_MARKER", marker)
	withGitShim(t, `
: > "$GORTEX_SOURCE_TEST_MARKER"
exit 1
`)

	malformed := []string{
		"",
		"HEAD",
		"deadbeef",
		"--upload-pack=touch /tmp/pwned",
		strings.Repeat("0", 39),                 // one digit short
		strings.Repeat("A", 40),                 // hexadecimal, but not lower case
		strings.Repeat("0", 40) + " --help",     // an id with an option smuggled after it
		strings.Repeat("0", 40) + "^{tree}",     // a revision expression
		"../../../etc/passwd",                   // a path
		strings.Repeat("a", 65),                 // longer than any hash git has
		strings.Repeat("a", 40) + "\n" + "HEAD", // a second argument behind a newline
	}
	for _, oid := range malformed {
		src, err := NewGitTreeSource(t.Context(), dir, oid)
		if err == nil {
			src.Close()
			t.Errorf("NewGitTreeSource(%q) succeeded, want a rejection", oid)
			continue
		}
		if errors.Is(err, ErrObjectMissing) {
			t.Errorf("NewGitTreeSource(%q) err = %v, want a malformed-id error rather than ErrObjectMissing", oid, err)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("NewGitTreeSource(%q) invoked git before validating the object id", oid)
		}
	}

	// The marker really does fire for an id that passes validation, so
	// the assertions above are not vacuous.
	if _, err := NewGitTreeSource(t.Context(), dir, strings.Repeat("a", 40)); err == nil {
		t.Fatal("NewGitTreeSource with the shim succeeded, want the shim's failure")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the shim was never invoked for a well-formed id: %v", err)
	}
}

func TestNewGitTreeSourceReportsAMissingTree(t *testing.T) {
	dir := tempRoot(t)
	initRepo(t, dir)
	writeFile(t, dir, "top.txt", "top\n", 0o644)
	commitAll(t, dir, "seed")

	absent := strings.Repeat("1", 40)
	if _, err := NewGitTreeSource(t.Context(), dir, absent); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("NewGitTreeSource(absent tree) err = %v, want ErrObjectMissing", err)
	}

	// A blob id is well-formed and present, but it is not a tree.
	blob := git(t, dir, "rev-parse", "HEAD:top.txt")
	if _, err := NewGitTreeSource(t.Context(), dir, blob); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("NewGitTreeSource(blob) err = %v, want ErrObjectMissing", err)
	}

	// A directory that is not a repository is a different failure: the
	// setup is broken, not the object.
	notRepo := tempRoot(t)
	_, err := NewGitTreeSource(t.Context(), notRepo, strings.Repeat("1", 40))
	if err == nil {
		t.Fatal("NewGitTreeSource outside a repository succeeded")
	}
	if errors.Is(err, ErrObjectMissing) {
		t.Errorf("NewGitTreeSource outside a repository err = %v, want a plain failure", err)
	}

	// And a commit id is accepted, peeled to its tree.
	commit := git(t, dir, "rev-parse", "HEAD")
	src := newTreeSource(t, dir, commit)
	if got, want := walkPaths(t, src), []string{"top.txt"}; !slices.Equal(got, want) {
		t.Errorf("Walk paths = %q, want %q", got, want)
	}
}

func TestGitTreeSourceReportsAMissingBlob(t *testing.T) {
	dir := tempRoot(t)
	initRepo(t, dir)
	writeFile(t, dir, "keep.txt", "keep\n", 0o644)
	writeFile(t, dir, "doomed.txt", "doomed\n", 0o644)
	tree := commitAll(t, dir, "seed")

	// Prune one blob out of the object store, leaving the tree that
	// names it intact — the shape a partial clone or a damaged store
	// presents.
	oid := git(t, dir, "rev-parse", "HEAD:doomed.txt")
	loose := filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("remove loose object: %v", err)
	}

	src := newTreeSource(t, dir, tree)

	// Enumeration still works; the size is unknown rather than zero,
	// which would read as an empty file.
	meta, err := src.Stat("doomed.txt")
	if err != nil {
		t.Fatalf("Stat(doomed.txt): %v", err)
	}
	if meta.Size >= 0 {
		t.Errorf("Stat(doomed.txt) size = %d, want a negative unknown size", meta.Size)
	}

	if content, _ := readAll(t, src, "keep.txt"); content != "keep\n" {
		t.Fatalf("Open(keep.txt) content = %q", content)
	}
	rc, _, err := src.Open("doomed.txt")
	if !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("Open(doomed.txt) err = %v, want ErrObjectMissing", err)
	}
	if rc != nil {
		rc.Close()
		t.Error("Open(doomed.txt) returned a reader for a missing object")
	}
	// A missing object is a complete answer, so the batch protocol
	// keeps its place and the next read still works.
	if content, _ := readAll(t, src, "keep.txt"); content != "keep\n" {
		t.Errorf("Open(keep.txt) after a missing object = %q", content)
	}
	if got := src.spawnCount(); got != 1 {
		t.Errorf("spawnCount = %d, want 1", got)
	}
}

func TestGitTreeSourceReusesOneBatchProcess(t *testing.T) {
	dir := tempRoot(t)
	initRepo(t, dir)
	var paths []string
	for i := range 12 {
		rel := fmt.Sprintf("pkg/file%02d.txt", i)
		writeFile(t, dir, rel, fmt.Sprintf("content %d\n", i), 0o644)
		paths = append(paths, rel)
	}
	tree := commitAll(t, dir, "many files")

	src := newTreeSource(t, dir, tree)
	// The child is spawned on the first read, not at construction.
	if got := src.spawnCount(); got != 0 {
		t.Fatalf("spawnCount before any read = %d, want 0", got)
	}
	if _, err := src.Stat(paths[0]); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := src.spawnCount(); got != 0 {
		t.Fatalf("spawnCount after a Stat of a regular file = %d, want 0", got)
	}

	for i, rel := range paths {
		got, _ := readAll(t, src, rel)
		if want := fmt.Sprintf("content %d\n", i); got != want {
			t.Fatalf("Open(%q) = %q, want %q", rel, got, want)
		}
	}
	if got := src.spawnCount(); got != 1 {
		t.Errorf("spawnCount after %d reads = %d, want 1", len(paths), got)
	}
}

func TestGitTreeSourceConcurrentReads(t *testing.T) {
	dir := tempRoot(t)
	initRepo(t, dir)
	want := make(map[string]string)
	for i := range 8 {
		rel := fmt.Sprintf("pkg/file%02d.txt", i)
		content := strings.Repeat(fmt.Sprintf("line %d\n", i), 64)
		writeFile(t, dir, rel, content, 0o644)
		want[rel] = content
	}
	symlink(t, dir, "link.txt", "pkg/file00.txt")
	tree := commitAll(t, dir, "corpus")

	src := newTreeSource(t, dir, tree)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				for rel, expected := range want {
					rc, meta, err := src.Open(rel)
					if err != nil {
						t.Errorf("Open(%q): %v", rel, err)
						return
					}
					data, err := io.ReadAll(rc)
					rc.Close()
					if err != nil {
						t.Errorf("read %q: %v", rel, err)
						return
					}
					if string(data) != expected {
						t.Errorf("Open(%q) returned %d bytes, want %d", rel, len(data), len(expected))
						return
					}
					if meta.Size != int64(len(expected)) {
						t.Errorf("Open(%q) size = %d, want %d", rel, meta.Size, len(expected))
						return
					}
					if _, err := src.Stat("link.txt"); err != nil {
						t.Errorf("Stat(link.txt): %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if got := src.spawnCount(); got != 1 {
		t.Errorf("spawnCount = %d, want 1 shared child", got)
	}
}

func TestGitTreeSourceFallsBackWhenBatchCommandZIsUnsupported(t *testing.T) {
	dir, fixture, tree := commitFixture(t)
	real := realGit(t)
	withGitShim(t, fmt.Sprintf(`
real=%q
batch_command=0
dash_z=0
for arg in "$@"; do
  case "$arg" in
    --batch-command) batch_command=1 ;;
    -Z) dash_z=1 ;;
  esac
done
if [ "$batch_command" = 1 ] && [ "$dash_z" = 1 ]; then
  echo 'error: unknown switch Z' >&2
  exit 129
fi
exec "$real" "$@"
`, real))

	if probeBatchCommandZ(t.Context(), dir) {
		t.Fatal("the probe reported -Z support under a shim that rejects it")
	}

	src := newTreeSource(t, dir, tree)
	for rel, want := range fixture.files {
		if got, _ := readAll(t, src, rel); got != want {
			t.Errorf("Open(%q) = %q, want %q", rel, got, want)
		}
	}
	if got, want := walkPaths(t, src), fixture.paths(); !slices.Equal(got, want) {
		t.Errorf("Walk paths =\n%q\nwant\n%q", got, want)
	}
	if src.batch == nil {
		t.Fatal("no batch child was started")
	}
	if src.batch.command || src.batch.delim != '\n' {
		t.Errorf("batch dialect = {command:%v delim:%q}, want the newline-delimited fallback", src.batch.command, src.batch.delim)
	}
	if got := src.spawnCount(); got != 1 {
		t.Errorf("spawnCount = %d, want 1", got)
	}

	// A path the tree never named is answered from the entry map, before
	// the batch child is consulted at all.
	if _, err := readAllOpen(src, "absent.txt"); !errors.Is(err, ErrNotInSource) {
		t.Errorf("Open(absent.txt) err = %v, want ErrNotInSource", err)
	}

	// A blob the tree names but the object store no longer holds is
	// reported as a missing object in the fallback dialect too, and the
	// newline-framed protocol keeps its place: the next read still
	// answers over the same child.
	doomed := git(t, dir, "rev-parse", "HEAD:top.txt")
	loose := filepath.Join(dir, ".git", "objects", doomed[:2], doomed[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("remove loose object: %v", err)
	}
	rc, _, err := src.Open("top.txt")
	if !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("Open(top.txt) after pruning its blob = %v, want ErrObjectMissing", err)
	}
	if rc != nil {
		rc.Close()
		t.Error("Open(top.txt) returned a reader for a missing object")
	}
	want := fixture.files["a/sibling.txt"]
	if got, _ := readAll(t, src, "a/sibling.txt"); got != want {
		t.Errorf("Open(a/sibling.txt) after a missing object = %q, want %q", got, want)
	}
	if got := src.spawnCount(); got != 1 {
		t.Errorf("spawnCount after a missing object = %d, want 1", got)
	}
}

func TestGitTreeSourceIdentityAndClose(t *testing.T) {
	dir, _, tree := commitFixture(t)
	src, err := NewGitTreeSource(t.Context(), dir, tree)
	if err != nil {
		t.Fatalf("NewGitTreeSource: %v", err)
	}
	if want := "git:" + dir + "@" + tree; src.Identity() != want {
		t.Errorf("Identity = %q, want %q", src.Identity(), want)
	}
	if _, err := readAllOpen(src, "top.txt"); err != nil {
		t.Fatalf("Open(top.txt): %v", err)
	}
	child := src.batch.cmd
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close terminates and reaps the batch child rather than leaving it
	// behind holding a pipe.
	if child.ProcessState == nil || !child.ProcessState.Exited() {
		t.Errorf("batch child state after Close = %v, want an exited process", child.ProcessState)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := src.Open("top.txt"); err == nil {
		t.Error("Open after Close succeeded, want an error")
	}
	// Metadata that needs no object read still answers after Close.
	if _, err := src.Stat("top.txt"); err != nil {
		t.Errorf("Stat after Close: %v", err)
	}
}

func TestGitTreeSourceWalkHonoursContextCancellation(t *testing.T) {
	dir, _, tree := commitFixture(t)
	src := newTreeSource(t, dir, tree)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	visited := 0
	if err := src.Walk(cancelled, func(FileMeta) error { visited++; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk err = %v, want context.Canceled", err)
	}
	if visited != 0 {
		t.Errorf("Walk visited %d entries after cancellation", visited)
	}

	// An error from the callback stops the walk and is returned as-is.
	sentinel := errors.New("stop here")
	seen := 0
	err := src.Walk(t.Context(), func(FileMeta) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk err = %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("Walk visited %d entries after the callback failed", seen)
	}
}

// readAllOpen opens a path and returns its content, closing the reader.
func readAllOpen(src ContentSource, path string) ([]byte, error) {
	rc, _, err := src.Open(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
