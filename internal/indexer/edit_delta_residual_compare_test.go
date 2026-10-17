package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testutil/parityresiduals"
)

// editDeltaResidualCompare compares a composed view with a clean whole index
// over the files of the packages that hold changed, with the residual list at
// residualsPath (lanes/j1/engine-residuals.md), and takes every difference the
// list does not excuse to the primary per-save store:
//
//   - a row only the view holds that the primary also holds, or a row only
//     the clean index holds that the primary also lacks, is the incremental
//     paths' behaviour: an engine shape the list does not name yet;
//   - any other unexcused row is the delta's own: a delta defect.
//
// The bounded report (counts per group, 50 rows each, then the unexcused rows
// with their verdict) goes to the test log and to out (when not empty); the
// full list to out+".full" (capped at 32 MB). It returns the delta defects.
//
// For a large-layer test on the real repository: view is the checkout's
// served (or composed) reader, clean a whole index of the worktree, primary
// the store of primaryOnIndexedStore after the same saves.
func editDeltaResidualCompare(t *testing.T, view, clean, primary graph.Reader, changed []string, residualsPath, out string) []string {
	t.Helper()
	entries, err := parityresiduals.Load(residualsPath)
	if err != nil {
		t.Fatalf("residual list: %v", err)
	}
	paths := residualComparePaths(clean, changed)
	res := parityresiduals.Compare(parityresiduals.Collect(view, paths), parityresiduals.Collect(clean, paths), entries)
	var prim parityresiduals.Rows
	if primary != nil {
		prim = parityresiduals.Collect(primary, paths)
	}
	holds := func(d parityresiduals.Difference) bool {
		rows := prim.Nodes[d.Kind]
		if d.Class == "edge" {
			rows = prim.Edges[d.Kind]
		}
		for _, r := range rows {
			if r == d.Row {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "residual compare over %d files of the changed files' packages:\n%s", len(paths), parityresiduals.ReportBounded(res, 50))
	var defects, engine []string
	for _, d := range res.Unexcused() {
		line := fmt.Sprintf("%s %s only-%s: %s", d.Class, d.Kind, d.Side, d.Row)
		switch {
		case primary == nil:
			defects = append(defects, "UNTRIANGULATED "+line)
		case (d.Side == "view") == holds(d):
			// view-only and the primary has it, or clean-only and the
			// primary lacks it: the incremental paths agree with the view.
			engine = append(engine, line)
		default:
			defects = append(defects, line)
		}
	}
	sort.Strings(engine)
	sort.Strings(defects)
	fmt.Fprintf(&b, "unexcused, the primary per-save path agrees with the view (engine shape, not on the list): %d\n", len(engine))
	for i, l := range engine {
		if i == 50 {
			fmt.Fprintf(&b, "    ... %d more\n", len(engine)-50)
			break
		}
		fmt.Fprintf(&b, "    %s\n", l)
	}
	fmt.Fprintf(&b, "unexcused, the view alone (delta defect): %d\n", len(defects))
	for i, l := range defects {
		if i == 50 {
			fmt.Fprintf(&b, "    ... %d more\n", len(defects)-50)
			break
		}
		fmt.Fprintf(&b, "    %s\n", l)
	}
	t.Log(b.String())
	if out != "" {
		_ = os.WriteFile(out, []byte(b.String()), 0o644)
		_ = parityresiduals.WriteFull(out+".full", res, 32<<20)
	}
	return defects
}

// residualComparePaths is every file the reader holds in the directories of
// changed (graph paths).
func residualComparePaths(r graph.Reader, changed []string) []string {
	dirs := map[string]struct{}{}
	for _, p := range changed {
		dirs[filepath.Dir(p)] = struct{}{}
	}
	var out []string
	for n := range r.NodesByKind(graph.KindFile) {
		if n == nil {
			continue
		}
		if _, ok := dirs[filepath.Dir(n.FilePath)]; ok {
			out = append(out, n.FilePath)
		}
	}
	sort.Strings(out)
	return out
}

// primaryOnIndexedStore is the primary per-save path over an already indexed
// store (a copy of the base the test indexed, not the store under test): an
// Indexer opened on it whose saves go through IncrementalReindexPaths, as the
// daemon's primary checkout applies them. The caller closes the Indexer.
func primaryOnIndexedStore(t *testing.T, store *store_sqlite.Store, cfg config.IndexConfig) *Indexer {
	t.Helper()
	idx := New(store, builderRegistry(), cfg, zap.NewNop())
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	return idx
}

// The comparison on the kind-parity chain, with the residual list
// (GX_PARITY_RESIDUALS): after every edit, nothing the list does not excuse,
// and so nothing a delta defect. It is the small twin of the large-layer
// test's use of editDeltaResidualCompare.
func TestEditDeltaResidualCompareOnTheChain(t *testing.T) {
	list := os.Getenv("GX_PARITY_RESIDUALS")
	if list == "" {
		t.Skip("set GX_PARITY_RESIDUALS to the residual list")
	}
	repoDir, store := kindParityRepo(t, "residual-compare")
	primaryStore := builderOpenStore(t, "residual-compare-primary")
	builderIndex(t, primaryStore, repoDir)
	primary := primaryOnIndexedStore(t, primaryStore, config.Default().Index)
	defer primary.Close()
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	for i, edit := range kindParityEdits() {
		applyKindParityEdit(t, repoDir, edit)
		if _, err := primary.IncrementalReindexPaths(repoDir, edit.paths()); err != nil {
			t.Fatal(err)
		}
		_, _, chain := h.build()
		clean := builderOpenStore(t, fmt.Sprintf("residual-compare-clean-%d", i))
		builderIndex(t, clean, repoDir)
		changed := make([]string, 0, len(edit.paths()))
		for _, p := range edit.paths() {
			changed = append(changed, builderGraphPath(builderRepoPrefix, p))
		}
		if defects := editDeltaResidualCompare(t, dirtyChainComposed(t, store, chain), clean, primaryStore, changed, list, ""); len(defects) > 0 {
			t.Errorf("depth %d/%s: %d delta defects", len(chain), edit.name, len(defects))
		}
	}
}
