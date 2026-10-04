package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
)

// TestRealRepoChainDepthBoundCost measures what a chain bound of 16 costs
// against 8 on the real repository: a chain of 16 real edit layers (one body
// edit per file, round-robin over editDeltaChainDefaultFiles) is built over a
// copy of the base store, then
//
//   - served reads through the composed view at depth 8 and at depth 16: a
//     package's file nodes, name lookups, in-edges of its nodes, per call;
//   - one more edit built over depth 8 and over depth 16 (the same file and
//     body edit), wall and CPU, with the per-layer keeper's counters.
//
// Opt-in as TestRealRepoChainedEditDelta (GX_DELTA_WARM_TREE,
// GX_DELTA_WARM_STORE); GX_DELTA_BOUND_PACKAGE (default internal/indexer),
// GX_DELTA_BOUND_REPS (default 20), GX_DELTA_BOUND_OUT (JSON).
func TestRealRepoChainDepthBoundCost(t *testing.T) {
	tree := os.Getenv("GX_DELTA_WARM_TREE")
	storePath := os.Getenv("GX_DELTA_WARM_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_DELTA_WARM_TREE / GX_DELTA_WARM_STORE are not set")
	}
	builderIsolateGit(t)
	if out := builderGit(t, tree, "status", "--porcelain"); out != "" {
		t.Fatalf("the clone at %s is not clean:\n%s", tree, out)
	}
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's configuration: %v", err)
	}
	pkg := os.Getenv("GX_DELTA_BOUND_PACKAGE")
	if pkg == "" {
		pkg = "internal/indexer"
	}
	reps := 20
	resetChainOverlayCaches()
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store := builderOpenStoreAt(t, copyPath)
	t.Cleanup(func() { _ = store.Close() })
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg.Index, Logger: editDeltaRealLogger(t)}
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatalf("open the checkout's sampler: %v", err)
	}
	ctx := context.Background()
	originals := map[string][]byte{}
	t.Cleanup(func() {
		for rel, body := range originals {
			_ = os.WriteFile(filepath.Join(tree, filepath.FromSlash(rel)), body, 0o644)
		}
	})
	edit := func(i int) string {
		rel := editDeltaChainDefaultFiles[i%len(editDeltaChainDefaultFiles)]
		full := filepath.Join(tree, filepath.FromSlash(rel))
		if _, ok := originals[rel]; !ok {
			body, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			originals[rel] = body
		}
		edited, _ := editDeltaRealBodyEdit(string(originals[rel]), i+1)
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		return rel
	}
	buildOver := func(chain []int64) (int64, time.Duration, time.Duration, *EditDeltaReport) {
		req := DirtyLayerRequest{
			Identity:     builderDirtyIdentity(),
			Base:         commitLayerBase{Reader: store, corpus: store, stack: []int64{editDeltaChainCommit}},
			CheckoutRoot: tree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			Sampler: sampler,
		}
		if len(chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, chain)
			if why != "" {
				t.Fatalf("the chain's manifest: %s", why)
			}
			parent := chain[len(chain)-1]
			req.Base = commitLayerBase{Reader: dirtyChainComposed(t, store, chain), corpus: store,
				stack: append([]int64{editDeltaChainCommit}, chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(chain)
		}
		recordLastEditDelta(nil)
		started, cpu := time.Now(), editDeltaProcessCPU()
		id, _, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("build over a chain of %d: %v", len(chain), err)
		}
		return id, time.Since(started), editDeltaProcessCPU() - cpu, LastEditDeltaReport()
	}
	var chain []int64
	for i := 0; i < 16; i++ {
		edit(i)
		id, _, _, _ := buildOver(chain)
		chain = append(chain, id)
	}

	var paths []string
	for n := range store.NodesByKind(graph.KindFile) {
		if n != nil && strings.HasPrefix(n.FilePath, builderRepoPrefix+"/"+pkg+"/") && !strings.Contains(strings.TrimPrefix(n.FilePath, builderRepoPrefix+"/"+pkg+"/"), "/") {
			paths = append(paths, n.FilePath)
		}
	}
	sort.Strings(paths)
	var ids, names []string
	seen := map[string]struct{}{}
	for _, nodes := range store.GetFileNodesByPaths(paths) {
		for _, n := range nodes {
			ids = append(ids, n.ID)
			if _, dup := seen[n.Name]; !dup && n.Name != "" {
				seen[n.Name] = struct{}{}
				names = append(names, n.Name)
			}
		}
	}
	sort.Strings(ids)
	if len(ids) > 2000 {
		ids = ids[:2000]
	}
	sort.Strings(names)
	if len(names) > 500 {
		names = names[:500]
	}
	type record struct {
		What  string  `json:"what"`
		Depth int     `json:"depth"`
		Us    float64 `json:"us_per_call,omitempty"`
		Wall  float64 `json:"wall_ms,omitempty"`
		CPU   float64 `json:"cpu_ms,omitempty"`
		Extra string  `json:"extra,omitempty"`
	}
	var records []record
	served := func(depth int) {
		view := dirtyChainComposed(t, store, chain[:depth])
		reads := map[string]func(){
			"served_file_nodes": func() {
				for _, p := range paths {
					view.GetFileNodes(p)
				}
			},
			"served_names": func() {
				for _, name := range names {
					view.FindNodesByName(name)
				}
			},
			"served_in_edges": func() { view.GetInEdgesByNodeIDs(ids) },
		}
		keys := make([]string, 0, len(reads))
		for k := range reads {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			reads[k]()
			started := time.Now()
			for r := 0; r < reps; r++ {
				reads[k]()
			}
			us := float64(time.Since(started).Microseconds()) / float64(reps)
			records = append(records, record{What: k, Depth: depth, Us: us})
			t.Logf("depth %2d %-18s %10.1f us/call", depth, k, us)
		}
	}
	served(8)
	served(16)
	for _, depth := range []int{8, 16} {
		edit(100)
		_, wall, cpu, report := buildOver(chain[:depth])
		extra := ""
		if report != nil {
			extra = fmt.Sprintf("overlaid=%d keeper(layers,rows,hits,loads,declined)=%v", report.ChainLayersOverlaid, report.ChainKeeper)
		}
		records = append(records, record{What: "edit_build", Depth: depth, Wall: ms(wall), CPU: ms(cpu), Extra: extra})
		t.Logf("depth %2d edit build: wall %.1f ms cpu %.1f ms %s", depth, ms(wall), ms(cpu), extra)
	}
	if out := os.Getenv("GX_DELTA_BOUND_OUT"); out != "" {
		if data, err := json.MarshalIndent(records, "", "  "); err == nil {
			_ = os.WriteFile(out, data, 0o644)
		}
	}
}
