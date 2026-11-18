package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/testutil/parityresiduals"
)

// The residual list the window-6 harness reads (GX_PARITY_RESIDUALS names
// the file) holds against the kind-parity fixture: at every depth of the chain,
// plain and with go/types, every difference between the composed view and a
// clean index is excused by an entry, and entries with a partner excuse only
// paired rows. The same package does the comparison in the harness, so a
// shape this test accepts is one the harness accepts, and nothing else.
func TestKindParityResidualList(t *testing.T) {
	path := os.Getenv("GX_PARITY_RESIDUALS")
	if path == "" {
		t.Skip("set GX_PARITY_RESIDUALS to the residual list")
	}
	entries, err := parityresiduals.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, label, repoDir string, view graph.Reader, clean graph.Reader) {
		t.Helper()
		paths := residualListPaths(t, repoDir)
		res := parityresiduals.Compare(parityresiduals.Collect(view, paths), parityresiduals.Collect(clean, paths), entries)
		t.Logf("%s: %s", label, parityresiduals.Report(res))
		if bad := res.Unexcused(); len(bad) > 0 {
			t.Errorf("%s: %d differences no residual excuses", label, len(bad))
		}
	}
	t.Run("chain", func(t *testing.T) {
		repoDir, store := kindParityRepo(t, "residual-chain")
		builder := builderNewBuilder(store)
		h := newDirtyChainBuilder(t, builder, store, repoDir, true)
		h.compact = false
		for _, edit := range kindParityEdits() {
			applyKindParityEdit(t, repoDir, edit)
			_, _, chain := h.build()
			clean := builderOpenStore(t, fmt.Sprintf("residual-chain-clean-%d", len(chain)))
			builderIndex(t, clean, repoDir)
			check(t, fmt.Sprintf("depth %d/%s", len(chain), edit.name), repoDir, dirtyChainComposed(t, store, chain), clean)
		}
	})
	t.Run("semantic", func(t *testing.T) {
		f, c, mgr := semanticChainFixture(t, kindParityTree())
		coordinatorReconcile(t, c)
		for i, edit := range kindParityEdits() {
			applyKindParityEdit(t, f.worktree, edit)
			out := coordinatorReconcile(t, c)
			view := chainMaterialize(t, f)
			clean := builderOpenStore(t, fmt.Sprintf("residual-semantic-clean-%d", i))
			builderIndexSemantic(mgr)(t, clean, f.worktree)
			check(t, fmt.Sprintf("semantic depth %d/%s", out.DirtyChainDepth, edit.name), f.worktree, view.Reader, clean)
			view.Close()
		}
	})
}

// residualListPaths is every file of the working tree as a graph path.
func residualListPaths(t *testing.T, repoDir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(repoDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoDir, p)
		if err != nil {
			return err
		}
		out = append(out, builderGraphPath(builderRepoPrefix, filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
