package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Chain depth and fold costs on a real repository, in process.
//
// Skipped unless GX_FOLD_REAL_TREE names a clean private clone and
// GX_FOLD_REAL_STORE its base store (the one TestRealRepoEditDelta indexes).
// It edits four reference files round-robin, one file per edit, each
// edit chained on the previous published generation as the coordinator
// chains them, for nine edits: edits 1 to 8 stand at depths 1 to 8, the ninth
// meets the bound. After edits 4 and 8 it folds the chain the way the
// coordinator's compaction does (FlattenGenerationChain into a new working-tree
// generation) and measures the fold; after every edit it measures one read
// of the composed chain. Every figure is wall time, process CPU and the WAL
// the step appended.
func TestRealRepoChainDepthAndFold(t *testing.T) {
	tree := os.Getenv("GX_FOLD_REAL_TREE")
	storePath := os.Getenv("GX_FOLD_REAL_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_FOLD_REAL_TREE / GX_FOLD_REAL_STORE are not set")
	}
	builderIsolateGit(t)
	if out := builderGit(t, tree, "status", "--porcelain"); out != "" {
		t.Fatalf("the clone at %s is not clean:\n%s", tree, out)
	}
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logger := editDeltaRealLogger(t)
	// GX_FOLD_REAL_BASE=dedicated: the corpus is a published dedicated base
	// generation (built into GX_FOLD_REAL_STORE on first use) and every build and
	// read stands on a materialized view of it, so the per-stack caches are on.
	var dedicatedBase int64
	if os.Getenv("GX_FOLD_REAL_BASE") == "dedicated" {
		dedicatedBase = editDeltaRealDedicatedBase(t, tree, storePath, cfg.Index, logger)
	}
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store := builderOpenStoreAt(t, copyPath)
	t.Cleanup(func() { _ = store.Close() })
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg.Index, Logger: logger}
	chains := newDirtyChainBuilder(t, builder, store, tree, true)
	chains.compact = false
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatal(err)
	}
	chains.sampler = sampler
	if dedicatedBase > 0 {
		chains.useDedicatedBase(dedicatedBase)
	}
	prevKey := ""

	files := editDeltaRealDefaultFiles
	originals := map[string][]byte{}
	for _, rel := range files {
		full := filepath.Join(tree, filepath.FromSlash(rel))
		src, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		originals[full] = src
	}
	t.Cleanup(func() {
		for full, src := range originals {
			_ = os.WriteFile(full, src, 0o644)
		}
	})
	ctx := context.Background()
	measure := func(step string, fn func()) {
		t.Helper()
		mark, cpu, started := store.WALWriteMark(), editDeltaProcessCPU(), time.Now()
		fn()
		wall, used := time.Since(started), editDeltaProcessCPU()-cpu
		wal := store_sqlite.WALWrittenBetween(mark, store.WALWriteMark())
		t.Logf("MEASURE %-28s wall %8.1f ms  cpu %8.1f ms  wal %10d bytes (reset=%v)", step,
			float64(wall.Microseconds())/1000, float64(used.Microseconds())/1000, wal.Bytes, wal.Reset)
	}
	fold := func(depth int) {
		chain := append([]int64(nil), chains.chain...)
		var foldedTo int64
		measure("fold depth "+strconv.Itoa(depth), func() {
			id, _, _, err := store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "layer-worktree-fold",
				CheckoutID: "checkout-fixture", GenerationKind: DirtyLayerGenerationKind, CreatedAt: time.Now().Unix(),
			})
			if err != nil {
				t.Fatal(err)
			}
			foldedTo = id
			counts, err := store.FlattenGenerationChain(ctx, chain, id)
			if err != nil {
				t.Fatalf("fold %v: %v", chain, err)
			}
			t.Logf("fold of %d layers into %d: %+v", len(chain), id, counts)
		})
		_ = store.Catalog().SetViewGenerationState(ctx, foldedTo, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding)
	}
	read := func(label string) {
		var composed graph.Reader
		if chains.open != nil {
			base, release, err := chains.open(ctx, chains.chain[len(chains.chain)-1])
			if err != nil {
				t.Fatalf("open the chain's view: %v", err)
			}
			defer release()
			composed = base
		} else {
			composed = dirtyChainComposed(t, store, chains.chain)
		}
		measure("read "+label, func() {
			nodes := composed.GetFileNodes("repo/internal/indexer/checkout_refresh.go")
			ids := make([]string, 0, len(nodes))
			for _, n := range nodes {
				ids = append(ids, n.ID)
			}
			_ = composed.GetOutEdgesByNodeIDs(ids)
			_ = composed.GetInEdgesByNodeIDs(ids)
			_ = composed.FindNodesByName("New")
		})
	}
	for i := 1; i <= 9; i++ {
		rel := files[(i-1)%len(files)]
		full := filepath.Join(tree, filepath.FromSlash(rel))
		current, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		edited, ok := editDeltaRealBodyEdit(string(current), 1)
		if !ok {
			t.Fatalf("%s has no function body", rel)
		}
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		fallbacks := len(chains.fallbacks)
		var chain []int64
		var depth int
		measure("edit "+strconv.Itoa(i)+" "+filepath.Base(rel), func() {
			recordLastEditDelta(nil)
			_, _, chain = chains.build()
			depth = len(chain)
		})
		reason := ""
		for r := range chains.fallbacks {
			reason += r + " "
		}
		t.Logf("edit %d: depth %d, fallbacks %d→%d %s", i, depth, fallbacks, len(chains.fallbacks), strings.TrimSpace(reason))
		if dedicatedBase > 0 {
			prevKey = editDeltaRealCheckStackKey(t, filepath.Base(rel), i, LastEditDeltaReport(), chain, prevKey)
		}
		read("depth " + strconv.Itoa(depth))
		if depth == 4 || depth == 8 {
			fold(depth)
		}
	}
}
