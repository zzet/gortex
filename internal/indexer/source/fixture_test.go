package source

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// isolateGit pins the git environment for the whole test: no user or
// system config, a fixed identity, no credential prompts. Both the
// fixture helpers and the package under test inherit it, so a
// developer's ~/.gitconfig cannot change what the tests see.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "source test")
	t.Setenv("GIT_AUTHOR_EMAIL", "source@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "source test")
	t.Setenv("GIT_COMMITTER_EMAIL", "source@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// requirePOSIX skips a test that depends on symlinks and executable
// bits, which Windows does not provide the same way.
func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixtures need POSIX symlinks and permission bits")
	}
}

// tempRoot returns a temp directory with every symlink resolved. On
// macOS t.TempDir() hands back a path under /var, which is a symlink to
// /private/var; git reports the resolved spelling, so fixtures are
// built under the resolved root to keep path comparisons exact.
func tempRoot(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}

// git runs a git command inside dir and fails the test on error. An
// empty dir runs git without -C, for commands that create their own
// directory.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates an empty repository at dir on branch "main".
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	isolateGit(t)
	git(t, "", "init", "-q", "-b", "main", "--", dir)
	return dir
}

// commitAll stages everything under dir and commits it, returning the
// resolved tree object id of the new commit.
func commitAll(t *testing.T, dir, message string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
	return git(t, dir, "rev-parse", "HEAD^{tree}")
}

// writeFile creates dir/rel with the given content and mode, making
// parent directories as needed.
func writeFile(t *testing.T, dir, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatalf("chmod %q: %v", rel, err)
	}
}

// symlink creates dir/rel as a symlink to target.
func symlink(t *testing.T, dir, rel, target string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.Symlink(filepath.FromSlash(target), full); err != nil {
		t.Fatalf("symlink %q -> %q: %v", rel, target, err)
	}
}

// newlineName is a file name containing a newline. It is the name that
// breaks any line-oriented parsing of git output, so the fixtures use
// it wherever the filesystem accepts it.
const newlineName = "new\nline.txt"

// contentFixture is the shared corpus both source implementations are
// checked against: nested directories, an executable, a symlink, a name
// with a space, a name with a newline, and a UTF-8 name.
type contentFixture struct {
	// files maps repo-relative path to content, for regular files only.
	files map[string]string
	// execs lists the paths whose executable bit must survive.
	execs []string
	// links maps a symlink path to its target text.
	links map[string]string
}

// paths returns every path of the fixture in the canonical walk order:
// lexicographic byte order of the full slash-separated path.
func (f contentFixture) paths() []string {
	var out []string
	for p := range f.files {
		out = append(out, p)
	}
	for p := range f.links {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// buildContentFixture populates dir with the shared corpus. The
// newline-named file is left out on a filesystem that rejects it.
func buildContentFixture(t *testing.T, dir string) contentFixture {
	t.Helper()
	requirePOSIX(t)
	f := contentFixture{
		files: map[string]string{
			"a/b/nested.txt": "nested content\n",
			"top.txt":        "top\n",
			"run.sh":         "#!/bin/sh\necho hi\n",
			"with space.txt": "spaced\n",
			// A name whose UTF-8 has no decomposed form, so a
			// normalizing filesystem cannot rewrite it under the test.
			"日本語.txt":         "utf-8 name\n",
			"a/b/c/deep.bin":  "\x00\x01\x02binary\xff",
			"a/sibling.txt":   "sibling\n",
			"a/b/nested2.txt": "second\n",
		},
		execs: []string{"run.sh"},
		links: map[string]string{"link.txt": "a/b/nested.txt"},
	}
	for rel, content := range f.files {
		mode := os.FileMode(0o644)
		if rel == "run.sh" {
			mode = 0o755
		}
		writeFile(t, dir, rel, content, mode)
	}
	for rel, target := range f.links {
		symlink(t, dir, rel, target)
	}
	// The newline-named file is best-effort: some filesystems refuse it.
	nl := filepath.Join(dir, newlineName)
	if err := os.WriteFile(nl, []byte("newline name\n"), 0o644); err == nil {
		f.files[newlineName] = "newline name\n"
	} else {
		t.Logf("skipping the newline-named file: %v", err)
	}
	return f
}

// withGitShim puts a stub `git` at the front of PATH for the test. It
// stands in for a git release whose option support differs from the one
// installed on the machine running the tests.
func withGitShim(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// realGit returns the absolute path of the installed git, for a shim
// that forwards everything it does not intercept.
func realGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve git path: %v", err)
	}
	return abs
}

// readAll opens path in src and returns its content.
func readAll(t *testing.T, src ContentSource, path string) (string, FileMeta) {
	t.Helper()
	rc, meta, err := src.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(data), meta
}

// walkPaths collects the paths a source yields, in walk order.
func walkPaths(t *testing.T, src ContentSource) []string {
	t.Helper()
	metas := walkMetas(t, src)
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Path)
	}
	return out
}

// walkMetas collects every entry a source yields, in walk order.
func walkMetas(t *testing.T, src ContentSource) []FileMeta {
	t.Helper()
	var out []FileMeta
	if err := src.Walk(t.Context(), func(m FileMeta) error {
		out = append(out, m)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}
