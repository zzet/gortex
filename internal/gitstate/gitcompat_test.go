package gitstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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

// rejectPathFormat is the shim prologue for a git old enough to reject
// `--path-format`, which is how the fallback branch is reached.
const rejectPathFormat = `
for arg in "$@"; do
  case "$arg" in
    --path-format=*)
      echo "error: unknown option path-format" >&2
      exit 129
      ;;
  esac
done
dir=""
if [ "$1" = "-C" ]; then dir="$2"; fi
`

func TestResolveFamilyDirsFallsBackWhenPathFormatIsRejected(t *testing.T) {
	withGitShim(t, rejectPathFormat+`
# The plain form prints the common dir relative to the working
# directory git was run in.
printf '%s/.git\n.git\n' "$dir"
`)

	gitDir, commonDir, err := resolveFamilyDirs(context.Background(), "/repos/example")
	if err != nil {
		t.Fatalf("resolveFamilyDirs: %v", err)
	}
	want := filepath.Join("/repos/example", ".git")
	if gitDir != want {
		t.Errorf("gitDir = %q, want %q", gitDir, want)
	}
	if commonDir != want {
		t.Errorf("commonDir = %q, want %q — the relative answer was not anchored", commonDir, want)
	}
}

func TestResolveFamilyDirsFailsWhenBothFormsFail(t *testing.T) {
	withGitShim(t, rejectPathFormat+`
echo "fatal: not a git repository" >&2
exit 128
`)

	if _, _, err := resolveFamilyDirs(context.Background(), "/repos/example"); err == nil {
		t.Fatal("expected an error when both rev-parse forms fail")
	}
}

func TestResolveFamilyDirsRejectsUnexpectedOutput(t *testing.T) {
	withGitShim(t, rejectPathFormat+`
printf '%s/.git\n' "$dir"
`)

	if _, _, err := resolveFamilyDirs(context.Background(), "/repos/example"); err == nil {
		t.Fatal("expected an error when rev-parse prints one path instead of two")
	}
}

func TestInventoryUnavailableWhenPorcelainNULIsUnsupported(t *testing.T) {
	// A git that cannot emit the NUL-delimited listing cannot produce a
	// trustworthy inventory, so Inventory reports it as unavailable
	// rather than falling back to line splitting.
	withGitShim(t, `
dir=""
if [ "$1" = "-C" ]; then dir="$2"; fi
case "$*" in
  *"worktree list"*)
    echo "error: unknown switch z" >&2
    exit 129
    ;;
esac
printf '%s/.git\n%s/.git\n' "$dir" "$dir"
`)

	inv, err := Inventory(context.Background(), "/repos/example")
	if inv != nil {
		t.Errorf("expected a nil inventory, got %+v", inv)
	}
	if !errors.Is(err, ErrInventoryUnavailable) {
		t.Fatalf("err = %v, want ErrInventoryUnavailable", err)
	}
}
