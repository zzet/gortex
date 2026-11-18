package indexer

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// The shapes behind the three arg_of rows the large-layer run found only in
// the view at store.go:2751-2752 and :1619: a conversion of a local
// (`uint64(n)`), and a field passed to a package function
// (`stripCloneShingles(n.Meta)`), in a file the edit re-derives. The view,
// a clean whole index and the primary per-save path are compared with the
// residual list (GX_PARITY_RESIDUALS); every unexcused row is classed engine
// (the primary agrees with the view) or delta defect.
func TestArgOfShapesOfTheLargeRun(t *testing.T) {
	list := os.Getenv("GX_PARITY_RESIDUALS")
	if list == "" {
		t.Skip("set GX_PARITY_RESIDUALS to the residual list")
	}
	src := `package st

type Node struct{ Meta map[string]any }

type Estimate struct{ NodeBytes, EdgeBytes uint64 }

const perNode = 64

func strip(meta map[string]any) map[string]any { return meta }

func Size(nodes, edges int) Estimate {
	var est Estimate
	var n, e int
	n, e = nodes, edges
	est.NodeBytes = uint64(n) * perNode
	est.EdgeBytes = uint64(e) * perNode
	return est
}

func Insert(n *Node) map[string]any {
	return strip(n.Meta)
}
`
	tree := map[string]string{"go.mod": "module example.com/st\n\ngo 1.22\n", "st/store.go": src,
		"st/use.go": "package st\n\nfunc Use() Estimate { return Size(1, 2) }\n"}
	builderIsolateGit(t)
	store := builderOpenStore(t, "argof")
	repoDir := builderTempDir(t, "checkout-argof")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, tree)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	primaryStore := builderOpenStore(t, "argof-primary")
	builderIndex(t, primaryStore, repoDir)
	primary := primaryOnIndexedStore(t, primaryStore, config.Default().Index)
	defer primary.Close()
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	edited := strings.Replace(src, "\tvar est Estimate\n", "\tvar est Estimate\n\t_ = 1\n", 1)
	builderWriteFile(t, repoDir, "st/store.go", edited)
	if _, err := primary.IncrementalReindexPaths(repoDir, []string{"st/store.go"}); err != nil {
		t.Fatal(err)
	}
	id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	clean := builderOpenStore(t, "argof-clean")
	builderIndex(t, clean, repoDir)
	defects := editDeltaResidualCompare(t, dirtyChainComposed(t, store, []int64{id}), clean, primaryStore,
		[]string{builderGraphPath(builderRepoPrefix, "st/store.go")}, list, "")
	if len(defects) > 0 {
		t.Errorf("%d delta defects", len(defects))
	}
}
