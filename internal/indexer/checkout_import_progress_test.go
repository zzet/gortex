package indexer

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A committed-base import must publish every file while interactive requests
// continue arriving faster than one file can prepare, without replaying a
// successful import or blocking the interactive lane for that file's work.
func TestALargeWorkingTreeImportCompletesUnderSustainedInteractiveDemand(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 4
	t.Cleanup(func() { importInteractivePaths = oldPaths })

	f := newCommittedBaseFixture(t).coordinatorFixture
	gate := NewViewBuildGate()
	gate.Open()
	outcomes := make(chan CheckoutCycle, 512)
	var importing atomic.Bool
	var cycleContext context.Context
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate:      gate,
		cycleDone: func(out CheckoutCycle) { outcomes <- out },
		dirtyBarrier: func() {
			if importing.Load() {
				// A private file may take longer than the interactive interval.
				// Keep real payload work in flight through multiple requests.
				timer := time.NewTimer(150 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-cycleContext.Done():
				}
			}
		},
	})
	c.cycleMu.Lock()
	c.cycleBarrier = func(ctx context.Context) { cycleContext = ctx }
	c.cycleMu.Unlock()
	c.compaction.mu.Lock()
	c.compaction.quiet = -1
	var privateFolds atomic.Int64
	c.compaction.stepHook = func(foldCtx context.Context, step int) {
		if step != 0 {
			return
		}
		privateFolds.Add(1)
		// The target is private before its first copy step. A foreground
		// build must acquire the physical lane during this exact interval.
		probeCtx, cancel := context.WithTimeout(foldCtx, time.Second)
		defer cancel()
		release, err := gate.Acquire(probeCtx, ViewBuildInteractive)
		if err != nil {
			t.Errorf("private fold held build lane: %v", err)
			return
		}
		release()
		route := f.route()
		building := 0
		for _, row := range f.generations() {
			if row.GenerationKind == DirtyLayerGenerationKind && row.State == store_sqlite.ViewGenerationBuilding {
				building++
				if row.GenerationID == route.DirtyGenerationID {
					t.Errorf("unfinished fold %d became routed", row.GenerationID)
				}
			}
		}
		if building != 1 {
			t.Errorf("private fold targets=%d, want one", building)
		}
	}
	c.compaction.mu.Unlock()
	await := func(ctx context.Context) (CheckoutCycle, bool) {
		select {
		case out := <-outcomes:
			return out, true
		case <-ctx.Done():
			return CheckoutCycle{}, false
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c.Signal("first build")
	if out, ok := await(ctx); !ok || out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", out)
	}

	const imported = 12
	builderGit(t, f.primary, "checkout", "-q", "-b", "import-src")
	for i := 0; i < imported; i++ {
		builderWriteFile(t, f.primary, fmt.Sprintf("imported_%02d.go", i),
			fmt.Sprintf("package fixture\n\nfunc Imported%02d() int {\n\treturn %d\n}\n", i, i))
	}
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-q", "-m", "import source")
	builderGit(t, f.primary, "checkout", "-q", "main")
	builderGit(t, f.worktree, "checkout", "import-src", "--", ".")

	type importCycle struct {
		out   CheckoutCycle
		files []string
	}
	var cycles []importCycle
	complete := func(out CheckoutCycle) bool {
		return out.DirtyBuilt && out.DirtyBatchRemaining == 0 && !out.Rescheduled
	}
	record := func(out CheckoutCycle) {
		// The files a link imported are the imported paths its own generation
		// claims; a fold claims everything before it and is counted separately.
		var files []string
		if out.DirtyBuilt && !out.ImportFolded && out.DirtyGenerationID > 0 {
			for _, p := range claimedPaths(t, f.store, out.DirtyGenerationID) {
				if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
					files = append(files, rel)
				}
			}
		}
		cycles = append(cycles, importCycle{out: out, files: files})
		if out.Err != nil {
			t.Fatalf("import cycle failed: %+v", out)
		}
		if len(cycles) > 40 {
			for i, cy := range cycles {
				var phases []GenerationPhase
				if cy.out.DirtyWork != nil {
					phases = cy.out.DirtyWork.Phases
				}
				t.Logf("cycle %d: built=%v yield=%s plan=%v physical=%v", i, cy.out.DirtyBuilt, cy.out.YieldedTo, cy.out.PlanLaps, phases)
			}
			t.Fatal("sustained demand starved the import before durable progress")
		}
	}
	// Keep this producer alive until the final generation has published.
	// Completing only after stopping requests cannot satisfy the test.
	probeCtx, stopProbes := context.WithCancel(ctx)
	probeDone := make(chan struct{})
	var waits []time.Duration
	var probeErr error
	go func() {
		defer close(probeDone)
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-ticker.C:
				asked := time.Now()
				release, err := gate.Acquire(probeCtx, ViewBuildInteractive)
				if err != nil {
					if probeCtx.Err() == nil {
						probeErr = err
					}
					return
				}
				waits = append(waits, time.Since(asked))
				// Exercise the shared writer while the import payload is private.
				err = f.store.AtGeneration(0).SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "interactive-probe"})
				release()
				if err != nil {
					probeErr = err
					return
				}
			}
		}
	}()
	defer func() { stopProbes(); <-probeDone }()
	importing.Store(true)
	c.Signal("git checkout with sustained interactive demand")
	for {
		out, ok := await(ctx)
		if !ok {
			t.Fatal("the import did not complete while interactive demand remained active")
		}
		record(out)
		if complete(out) {
			break
		}
	}
	stopProbes()
	<-probeDone
	if probeErr != nil {
		t.Fatalf("interactive writer failed: %v", probeErr)
	}
	if len(waits) < imported {
		t.Fatalf("only %d interactive requests overlapped %d imported files", len(waits), imported)
	}
	if len(cycles) == 0 {
		t.Fatal("the import ran no cycle")
	}
	last := cycles[len(cycles)-1].out
	if last.Err != nil || !last.DirtyBuilt || last.DirtyBatchRemaining != 0 {
		t.Fatalf("the import did not complete: %+v", last)
	}

	seen := map[string]int{}
	folds, links, yields := 0, 0, 0
	for i, cy := range cycles {
		if cy.out.YieldedTo != "" {
			yields++
			continue
		}
		if !cy.out.DirtyBuilt {
			continue
		}
		links++
		if len(cy.files) > 1 {
			t.Errorf("import cycle %d imported %d files %v, want one", i, len(cy.files), cy.files)
		}
		for _, p := range cy.files {
			seen[p]++
		}
		if cy.out.ImportFolded {
			folds++
		}
		if cy.out.DirtyChainReason != "" && links > 1 {
			// The first file stands direct on the commit generation (the
			// clean layer it replaces is no smaller a parent); every later
			// one must chain.
			t.Errorf("import cycle %d fell back (%s): %+v", i, cy.out.DirtyChainReason, cy.out)
		}
	}
	// A link that was folded right after it was built claims its file in
	// the fold, not in its own generation, so the distinct count is checked
	// against the imported files the routed stack claims at the end.
	for p, n := range seen {
		if n > 1 {
			t.Errorf("%s was imported by %d links; a completed file is never imported again", p, n)
		}
	}
	if privateFolds.Load() == 0 {
		t.Error("no private stepped fold overlapped foreground admission")
	}
	if folds == 0 {
		t.Errorf("the import never folded its chain (%d links)", links)
	}
	routedImported := map[string]struct{}{}
	for _, id := range routedStack(t, f, c)[1:] {
		for _, p := range claimedPaths(t, f.store, id) {
			if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
				routedImported[rel] = struct{}{}
			}
		}
	}
	if len(routedImported) != imported {
		t.Errorf("the imported working tree claims %d imported files, want %d", len(routedImported), imported)
	}
	var worst time.Duration
	for _, w := range waits {
		worst = max(worst, w)
	}
	t.Logf("import: %d cycles, %d file links, %d folds, %d yields; %d interactive probes, worst wait %s (all: %v)",
		len(cycles), links, folds, yields, len(waits), worst, waits)
	if worst > 100*time.Millisecond {
		t.Errorf("an interactive build waited %s for the import, want ≤ 100ms", worst)
	}
	if result := assertPropagationParity(t, f.store, routedStack(t, f, c), f.worktree, "import"); !result.ok() {
		t.Errorf("the imported working tree differs from a clean index: %v", result.Diffs)
	}
}
