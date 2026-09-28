package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Gated: GX_FOLD_LARGE_TREE names a git clone of a real repository (the
// Gortex repository, for example). GX_FOLD_LARGE_FILES names the
// widely used files every edit rewrites (comma-separated; default
// internal/graph/store_sqlite/store.go); GX_FOLD_LARGE_PACE the pause between
// edits (a Go duration, default 1.5s; 0: each edit is sent as soon as the one
// before is accepted); GX_FOLD_LARGE_OUT the file prefix for the bounded
// parity report (editDeltaResidualCompare: 50 rows per group in the log, the
// full list capped at 32 MB); GX_PARITY_RESIDUALS the residual list. The test
// indexes the clone three times (the corpus, the primary per-save oracle and
// the clean oracle), so it takes minutes: run it once per change of the fold
// and before a measurement, not in the ordinary suite.
const (
	foldLargeTreeEnv  = "GX_FOLD_LARGE_TREE"
	foldLargeFilesEnv = "GX_FOLD_LARGE_FILES"
	foldLargePaceEnv  = "GX_FOLD_LARGE_PACE"
	foldLargeOutEnv   = "GX_FOLD_LARGE_OUT"
	// foldLargeResidualsEnv names the residual list file.
	foldLargeResidualsEnv = "GX_PARITY_RESIDUALS"
	// foldLargeEditsEnv is the number of burst edits (default 12).
	foldLargeEditsEnv = "GX_FOLD_LARGE_EDITS"
)

// newCoordinatorFixtureFromClone is newCoordinatorFixture over a clone of a
// real repository: the primary is a clone of src at its HEAD, the worktree a
// linked one on a feature branch, the corpus a whole index of the primary.
func newCoordinatorFixtureFromClone(t *testing.T, src string) *coordinatorFixture {
	t.Helper()
	builderIsolateGit(t)
	family := builderTempDir(t, "family")
	primary := filepath.Join(family, "primary")
	builderGit(t, family, "clone", "--quiet", "--no-hardlinks", src, primary)
	builderGit(t, primary, "checkout", "--quiet", "-B", "main")
	treeA := builderGit(t, primary, "rev-parse", "HEAD^{tree}")
	worktree := filepath.Join(family, coordinatorAdminName)
	builderGit(t, primary, "worktree", "add", "--quiet", "-b", "feature", worktree)

	storePath := filepath.Join(t.TempDir(), "base.sqlite")
	store := builderOpenStoreAt(t, storePath)
	t.Cleanup(func() { _ = store.Close() })
	started := time.Now()
	builderIndex(t, store, primary)
	t.Logf("indexed the clone in %v", time.Since(started).Round(time.Millisecond))

	f := &coordinatorFixture{
		t: t, store: store, storePath: storePath, catalog: store.Catalog(),
		leases: graphview.NewLeaseManager(), primary: primary, worktree: worktree,
		familyID: "family-coordinator", graphID: GraphIDFor(builderRepoPrefix),
		primaryID: "checkout-primary", checkoutID: "checkout-worktree", treeA: treeA,
	}
	f.writeCatalogIdentity()
	f.registerRosterOwner(t)
	return f
}

// foldLargeProfile starts a CPU and a block profile of edit n when
// GX_FOLD_LARGE_PROF names a file prefix and GX_FOLD_LARGE_PROF_EDIT names n
// (default 2); the returned func stops and writes them.
func foldLargeProfile(t *testing.T, n int) func() {
	t.Helper()
	prefix := os.Getenv("GX_FOLD_LARGE_PROF")
	want := 2
	if raw := os.Getenv("GX_FOLD_LARGE_PROF_EDIT"); raw != "" {
		want, _ = strconv.Atoi(raw)
	}
	if prefix == "" || n != want {
		return func() {}
	}
	cpu, err := os.Create(fmt.Sprintf("%s-edit%02d-cpu.pprof", prefix, n))
	if err != nil {
		t.Fatal(err)
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		t.Fatal(err)
	}
	runtime.SetBlockProfileRate(int(time.Millisecond))
	runtime.SetMutexProfileFraction(1)
	return func() {
		pprof.StopCPUProfile()
		_ = cpu.Close()
		for _, name := range []string{"block", "mutex"} {
			file, err := os.Create(fmt.Sprintf("%s-edit%02d-%s.pprof", prefix, n, name))
			if err == nil {
				_ = pprof.Lookup(name).WriteTo(file, 0)
				_ = file.Close()
			}
		}
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	}
}

// foldLargeLaps renders an edit delta's phase laps, CPU and store I/O.
func foldLargeLaps(delta *EditDeltaReport) string {
	data, _ := json.Marshal(struct {
		Phases, Laps, WAL any
	}{delta.Phases, delta.PhaseWriteTx, delta.PhaseWALBytes})
	return string(data)
}

// foldLargeEdit rewrites the burst's tail declaration of rel: the file's other
// bodies are unchanged (their clone rows are carried), and the whole file is
// re-derived, with every caller of its symbols.
func foldLargeEdit(t *testing.T, root, rel string, k int) {
	t.Helper()
	path := filepath.Join(root, rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	const marker = "\n// fold burst tail\n"
	src := string(raw)
	if i := strings.Index(src, marker); i >= 0 {
		src = src[:i]
	}
	src += fmt.Sprintf("%sfunc foldBurstTail%d() {}\n", marker, k)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// A fold still running when the next edit arrives, with the daemon's
// defaults and no step hook: the layers are large (widely used files of a
// real repository), not held. Twelve MCP edits, GX_FOLD_LARGE_PACE apart: at
// least one is accepted while a fold is in flight, no fold is canceled (or a
// canceled one is taken up again and lands), the chain never reaches its
// bound, and the view after the last landing equals a clean index except for
// the rows the primary per-save path differs on as well. Overrides, named:
// the loop is parked (Debounce of an hour, demand debounced) as in
// mcpChainFixtureDefaults; no retirement sweep runs (the fixture has no
// worker).
func TestSteppedFoldInFlightUnderLargeLayersWithTheDaemonsDefaults(t *testing.T) {
	src := os.Getenv(foldLargeTreeEnv)
	if src == "" {
		t.Skipf("set %s to a git clone of a real repository", foldLargeTreeEnv)
	}
	files := []string{"internal/graph/store_sqlite/store.go"}
	if raw := os.Getenv(foldLargeFilesEnv); raw != "" {
		files = strings.Split(raw, ",")
	}
	pace := 1500 * time.Millisecond
	if raw := os.Getenv(foldLargePaceEnv); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("%s: %v", foldLargePaceEnv, err)
		}
		pace = d
	}
	out := os.Getenv(foldLargeOutEnv)
	// Without GX_PARITY_RESIDUALS the burst runs alone: no primary oracle in
	// the process and no comparison after it (timing only).
	residuals := os.Getenv(foldLargeResidualsEnv)
	edits := 12
	if raw := os.Getenv(foldLargeEditsEnv); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("%s: %q", foldLargeEditsEnv, raw)
		}
		edits = n
	}
	f := newCoordinatorFixtureFromClone(t, src)
	// The primary per-save oracle: the same corpus indexed again, every edit
	// saved through IncrementalReindexPaths as the daemon's primary applies it.
	var primaryStore *store_sqlite.Store
	var primary *Indexer
	if residuals != "" {
		primaryStore = builderOpenStore(t, "fold-large-primary")
		builderIndex(t, primaryStore, f.primary)
		primary = primaryOnIndexedStore(t, primaryStore, config.Default().Index)
		defer primary.Close()
	}
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	// The daemon's build-lane predicate (installBuildLaneBusy): the store's
	// editing state, its marks and its checkpoints key on it.
	installDaemonBuildLaneBusy(f, c)
	if out := c.reconcile(context.Background()); out.Err != nil {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	if raw := os.Getenv("GX_FOLD_LARGE_WARMUP"); raw != "" {
		// An idle gap after the whole index, as an agent's first pause
		// gives the store: its deferred planner statistics run in it.
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("GX_FOLD_LARGE_WARMUP: %v", err)
		}
		time.Sleep(d)
		t.Logf("warm-up pause of %v after the whole index", d)
	}

	fold := func() string {
		c.compaction.mu.Lock()
		owed := compactionOwed(c.compaction.running)
		folding := len(c.compaction.foldingChain)
		c.compaction.mu.Unlock()
		switch {
		case folding > 0:
			return fmt.Sprintf("stepping(%d)", folding)
		case owed:
			return "queued"
		}
		return "none"
	}
	inFlight, maxDepth, capInline, capDirect := 0, 0, 0, 0
	probeOut := os.Getenv("GX_CLAIM_PROBE_OUT")
	for i := 0; i < edits; i++ {
		before := fold()
		statsBefore := c.DirtyChainCompactionStats()
		depthBefore := 0
		if route := f.route(); route.DirtyGenerationID > 0 {
			depthBefore = len(c.dirtyChainMembers(context.Background(), route.DirtyGenerationID))
		}
		if probeOut != "" {
			// The claim probe (an -overlay of the delta writer) appends to
			// the file this names, one per edit.
			_ = os.Setenv("GX_CLAIM_PROBE_OUT", fmt.Sprintf("%s.edit%02d", probeOut, i+1))
		}
		recordLastEditDelta(nil)
		stopProfile := foldLargeProfile(t, i+1)
		started := time.Now()
		out := mcpEdit(t, l, f, func() {
			for _, rel := range files {
				foldLargeEdit(t, f.worktree, strings.TrimSpace(rel), i)
			}
		})
		took := time.Since(started)
		stopProfile()
		if delta := LastEditDeltaReport(); delta != nil {
			t.Logf("edit %2d laps: %s", i+1, foldLargeLaps(delta))
		}
		after := fold()
		rels := make([]string, 0, len(files))
		for _, rel := range files {
			rels = append(rels, strings.TrimSpace(rel))
		}
		if primary == nil {
			// no oracle
		} else if _, err := primary.IncrementalReindexPaths(f.worktree, rels); err != nil {
			t.Fatalf("the primary's save of edit %d: %v", i+1, err)
		}
		if before != "none" {
			inFlight++
		}
		maxDepth = max(maxDepth, out.DirtyChainDepth)
		var rows int
		if out.DirtyWork != nil {
			rows = out.DirtyWork.PassNodes + out.DirtyWork.PassEdges
		}
		stats := c.DirtyChainCompactionStats()
		promoted := -1
		if delta := LastEditDeltaReport(); delta != nil {
			promoted = delta.EdgeClaimsPromoted
		}
		atCap := depthBefore >= maxChainWalkDepth
		how := "chained"
		switch {
		case atCap && out.DirtyParentGenerationID != 0:
			how = "folded inline at the cap"
			capInline++
		case atCap:
			how = "built direct at the cap"
			capDirect++
		case out.DirtyParentGenerationID == 0:
			how = "built direct (" + out.DirtyChainReason + ")"
		}
		t.Logf("edit %2d: %v, lane wait %v (held by %v), depth %d -> %d, %s, rows %d, promoted %d, fold at start %s, at end %s, landed during it %d",
			i+1, took.Round(time.Millisecond), out.Admission.Lane.Round(time.Millisecond),
			out.Admission.LaneHeldBy, depthBefore, out.DirtyChainDepth, how, rows, promoted, before, after, stats.Flipped-statsBefore.Flipped)
		if atCap {
			t.Errorf("edit %d found the chain at the cap (%d): %s", i+1, depthBefore, how)
		}
		if pace > 0 {
			time.Sleep(pace)
		}
	}
	if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := c.DirtyChainCompactionStats()
	t.Logf("large burst (pace %v, %d files per edit): %+v; %d edits accepted while a fold was queued or stepping; deepest chain %d; at the cap %d (inline %d, direct %d)",
		pace, len(files), stats, inFlight, maxDepth, capInline+capDirect, capInline, capDirect)
	if inFlight == 0 {
		t.Errorf("no edit arrived while a fold was in flight")
	}
	if stats.Flipped == 0 {
		t.Errorf("no fold landed: %+v", stats)
	}
	if stats.Canceled > 0 && stats.Flipped == 0 {
		t.Errorf("a fold was canceled and none was taken up again: %+v", stats)
	}
	// The exact check's cost on a chain of this file's size at the cap: no
	// fold runs (compaction closed), edits chain to the cap, the chain is
	// copied at once and then checked, each timed.
	c.compaction.mu.Lock()
	c.compaction.closed = true
	c.compaction.mu.Unlock()
	for k := 0; k < maxChainWalkDepth; k++ {
		route := f.route()
		if route.DirtyGenerationID > 0 && len(c.dirtyChainMembers(context.Background(), route.DirtyGenerationID)) >= maxChainWalkDepth {
			break
		}
		mcpEdit(t, l, f, func() {
			for _, rel := range files {
				foldLargeEdit(t, f.worktree, strings.TrimSpace(rel), 950+k)
			}
		})
	}
	ctx := context.Background()
	route := f.route()
	members := c.dirtyChainMembers(ctx, route.DirtyGenerationID)
	commit, _, err := f.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	copyStarted := time.Now()
	built, oldestFirst, err := c.flattenDirtyChainChecked(ctx, commit, commit, route.DirtyGenerationID, c.copyChainAtOnce, nil)
	if err != nil {
		t.Fatalf("copy the chain at the cap: %v", err)
	}
	copyTook := time.Since(copyStarted)
	checkStarted := time.Now()
	checkErr := c.verifyFlattenedChain(ctx, commit.GenerationID, oldestFirst, built.GenerationID)
	checkTook := time.Since(checkStarted)
	layer, _ := graphview.NewGenerationLayerContext(ctx, f.store.AtGeneration(built.GenerationID))
	paths := 0
	if layer != nil {
		paths = len(layer.FilePaths())
	}
	t.Logf("exact check at the cap: chain of %d over commit %d, fold %d claims %d paths; copy %v, check %v (budget %v), verdict %v",
		len(members), commit.GenerationID, built.GenerationID, paths, copyTook.Round(time.Millisecond), checkTook.Round(time.Millisecond), dirtyChainInlineFoldBudget, checkErr)
	if checkErr != nil {
		t.Errorf("the exact check refused a correct copy: %v", checkErr)
	}

	if primary == nil {
		t.Logf("no residual list: no oracle ran, no comparison is made")
		return
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	clean := builderOpenStore(t, "fold-large-clean")
	builderIndex(t, clean, f.worktree)
	changed := make([]string, 0, len(files))
	for _, rel := range files {
		changed = append(changed, builderGraphPath(builderRepoPrefix, strings.TrimSpace(rel)))
	}
	if defects := editDeltaResidualCompare(t, view.Reader, clean, primaryStore, changed, residuals, out); len(defects) > 0 {
		t.Errorf("%d delta defects after the burst", len(defects))
	}
}
