// Package reporoot locates the module root for tests that reach source files
// by path.
//
// The Makefile builds and tests with -trimpath so that every worktree shares
// one Go build cache entry per package instead of compiling its own copy
// keyed by absolute directory. Under -trimpath runtime.Caller reports a
// module-relative path, so a test can no longer find its own source on disk
// that way. `go test` starts a package's binary with the working directory
// set to that package's real directory — including the `go test -c` plus
// relocated-binary form the CI runner uses, which passes the package
// directory as the process directory — so the working directory is the
// anchor that survives trimming.
//
// The root is resolved once at init because several packages chdir inside
// their tests; a lookup at call time would resolve against whichever test
// ran last.
//
// "Root" means the nearest enclosing module, not the git checkout. A test
// under internal/thirdparty/renameio or internal/thirdparty/go-pointer, which
// carry their own go.mod, would resolve to that submodule.
package reporoot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var root, rootErr = find()

func find() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod at or above " + start)
		}
		dir = parent
	}
}

// Path joins elem onto the module root, failing the test if the root could
// not be resolved. With no elem it is the module root itself.
func Path(tb testing.TB, elem ...string) string {
	tb.Helper()
	if rootErr != nil {
		tb.Fatalf("locate module root: %v", rootErr)
	}
	return filepath.Join(append([]string{root}, elem...)...)
}
