package gitstate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/pathkey"
)

// isolateGit pins the git environment for the whole test: no user or
// system config, fixed identity, no credential prompts. Both the
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
	t.Setenv("GIT_AUTHOR_NAME", "gitstate test")
	t.Setenv("GIT_AUTHOR_EMAIL", "gitstate@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "gitstate test")
	t.Setenv("GIT_COMMITTER_EMAIL", "gitstate@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// tempRoot returns a temp directory with every symlink resolved. On
// macOS t.TempDir() hands back a path under /var, which is a symlink to
// /private/var; git reports the resolved spelling, so fixtures are
// built under the resolved root to keep path comparisons exact.
func tempRoot(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	return realPath(t, t.TempDir())
}

// realPath resolves symlinks in p, falling back to a cleaned p when the
// path does not exist.
func realPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return resolved
}

// git runs a git command inside dir and fails the test on error. An
// empty dir runs git without -C, for commands that create their own
// directory.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tryGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// tryGit runs a git command and returns its combined output plus the
// error, for callers that expect a command to be able to fail.
func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// initRepo creates a git repository at dir with one commit on branch
// "main" and returns dir.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	initBareRepo(t, dir, false)
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	git(t, dir, "add", "--", "seed.txt")
	git(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

// initBareRepo creates an empty repository at dir, bare or not, on
// branch "main".
func initBareRepo(t *testing.T, dir string, bare bool) string {
	t.Helper()
	args := []string{"init", "-q", "-b", "main"}
	if bare {
		args = append(args, "--bare")
	}
	args = append(args, "--", dir)
	git(t, "", args...)
	return dir
}

// addWorktree creates a linked worktree at path. Extra args go before
// the path (for example "-b", "feature/foo" or "--detach").
func addWorktree(t *testing.T, repo, path string, extra ...string) string {
	t.Helper()
	args := append([]string{"worktree", "add", "-q"}, extra...)
	args = append(args, "--", path)
	git(t, repo, args...)
	return path
}

// samePath reports whether two spellings name the same directory.
// WorktreeRecord.Path is git's own spelling, which uses "/" separators
// on every platform, while a fixture path is built with the host
// separator. Every production consumer folds the two before comparing,
// so the fixtures identify records through the same fold.
func samePath(a, b string) bool {
	return pathkey.EqualPaths(a, b)
}

// recordFor returns the inventory record for path.
func recordFor(t *testing.T, inv *FamilyInventory, path string) WorktreeRecord {
	t.Helper()
	for _, r := range inv.Records {
		if samePath(r.Path, path) {
			return r
		}
	}
	var paths []string
	for _, r := range inv.Records {
		paths = append(paths, strconv.Quote(r.Path))
	}
	t.Fatalf("no record for %q; inventory has %s", path, strings.Join(paths, ", "))
	return WorktreeRecord{}
}

// hasRecord reports whether the inventory carries a record for path.
func hasRecord(inv *FamilyInventory, path string) bool {
	for _, r := range inv.Records {
		if samePath(r.Path, path) {
			return true
		}
	}
	return false
}
