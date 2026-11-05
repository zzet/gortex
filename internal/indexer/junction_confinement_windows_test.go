//go:build windows

package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/excludes"
)

// TestJunctionOutOfRepoIsExcluded is the junction form of the symlink admission
// tests. The cold walk and the watcher must agree, so both are asserted: a
// junction that only one of them refused would come back on the next edit.
func TestJunctionOutOfRepoIsExcluded(t *testing.T) {
	outside := t.TempDir()
	repo := t.TempDir()
	pkg := filepath.Join(repo, "pkg")
	if err := os.Mkdir(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	escaping := filepath.Join(repo, "leaves-repo")
	internal := filepath.Join(repo, "stays-in-repo")
	for link, target := range map[string]string{escaping: outside, internal: pkg} {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
		}
	}

	idx := newSymlinkTestIndexer(t, repo)
	w := &Watcher{indexer: idx, excludes: excludes.New(excludes.Builtin)}

	if !idx.shouldExclude(escaping, repo, false) {
		t.Fatal("the cold walk must refuse a junction whose target escapes the repo")
	}
	if !w.isExcluded(escaping) {
		t.Fatal("the watcher must refuse a junction whose target escapes the repo")
	}
	if idx.shouldExclude(internal, repo, false) {
		t.Fatal("the cold walk must keep a junction that stays inside the repo")
	}
	if w.isExcluded(internal) {
		t.Fatal("the watcher must keep a junction that stays inside the repo")
	}
}
