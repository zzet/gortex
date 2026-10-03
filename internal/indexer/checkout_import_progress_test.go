package indexer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/tstypes"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// A committed-base import must publish every file while interactive requests
// continue arriving faster than one file can prepare, without replaying a
// successful import or blocking the interactive lane for that file's work.
func TestALargeWorkingTreeImportCompletesUnderSustainedInteractiveDemand(t *testing.T) {
	testSustainedImportProgress(t, false, false, false, false, false)
}

func TestALargeWorkingTreeImportWithInlineGoSemanticsCompletesUnderSustainedInteractiveDemand(t *testing.T) {
	testSustainedImportProgress(t, true, false, false, false, false)
}

// Catalog planning can outlast the same 40ms producer interval as private
// payload preparation. Preserve the original producer, bounds and parity
// oracle while making that pre-handoff delay deterministic.
func TestImportPreambleCompletesUnderSustainedInteractiveDemand(t *testing.T) {
	testSustainedImportProgress(t, false, true, false, false, false)
}

// An import's post-publication fold prepares its materialized ancestry before
// copying. This work must admit foreground requests while it is still private.
func TestImportFoldPreparationCompletesUnderSustainedInteractiveDemand(t *testing.T) {
	testSustainedImportProgress(t, false, false, true, false, false)
}

// Demand queued at the initial grant must not cancel an eligible import before
// it can establish private preparation. Keep the original sustained producer,
// file-by-file progress, foreground latency, deadline and parity checks.
func TestImportEarlyAdmissionQueuePreservesProgress(t *testing.T) {
	testSustainedImportProgress(t, false, true, false, true, false)
}

// Retained-route metadata cleanup must not monopolize the physical build lane.
// Keep the original twelve files, three folds, 40ms SQL producer and all bounds.
func TestImportPublicationTailAdmitsForegroundSQL(t *testing.T) {
	testSustainedImportProgress(t, false, false, false, false, true)
}

func testSustainedImportProgress(t *testing.T, inline, slowPreamble, slowFoldPlanning, earlyDemand, slowPublicationTail bool) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 4
	t.Cleanup(func() { importInteractivePaths = oldPaths })

	fixture := newUnpublishedCommittedBaseFixture(t)
	var manager *semantic.Manager
	if inline {
		manager = goTypesManager(t)
		for _, provider := range tstypes.DefaultProviders(zap.NewNop()) {
			manager.RegisterProvider(provider)
		}
		builderWriteFile(t, fixture.primary, "go.mod", "module example.com/fixture\n\ngo 1.22\n")
		baseFunctions := "package fixture\n\n"
		for i := 0; i < 16; i++ {
			baseFunctions += fmt.Sprintf("func BaseExtra%d() int { return %d }\n", i, i)
		}
		builderWriteFile(t, fixture.primary, "extra_base.go", baseFunctions)
		builderGit(t, fixture.primary, "add", "-A")
		builderGit(t, fixture.primary, "commit", "-q", "-m", "Go semantic baseline")
		builderGit(t, fixture.worktree, "reset", "--hard", "main")
		provider := manager.ProviderForLanguage("go")
		parallel, ok := provider.(interface {
			ConcurrentCheckoutPreparation(context.Context, string, string, semantic.CheckoutCompilerScope, []string) bool
		})
		if !ok || !parallel.ConcurrentCheckoutPreparation(t.Context(), fixture.worktree, builderRepoPrefix, semantic.CheckoutCompilerScope{HandleRoots: true}, []string{builderRepoPrefix + "/core.go"}) {
			t.Skip("actual Go provider has no concurrent admission for this ordinary module; conservative inline fallback remains enabled")
		}
		// The immutable baseline already carries real semantic facts, so the
		// final oracle can compare every node/edge with a clean semantic index.
		builderIndexSemantic(manager)(t, fixture.store, fixture.primary)
		fixture.newBuilder = func(store *store_sqlite.Store) *SparseGenerationBuilder {
			b := builderNewBuilder(store)
			b.Semantic = manager
			return b
		}
	}
	fixture.publishBase(t)
	f := fixture.coordinatorFixture
	admissionLogger, admissionLogs := newImportAdmissionLogCapture()
	builder := builderNewBuilder(f.store)
	builder.Semantic = manager
	builder.Logger = admissionLogger
	gate := NewViewBuildGate()
	gate.Open()
	outcomes := make(chan CheckoutCycle, 512)
	var importing atomic.Bool
	var publicationTailActive atomic.Bool
	var publicationTails, publicationTailSQL atomic.Int64
	var publicationTailWorst atomic.Int64
	var cycleContext context.Context
	c := f.coordinatorWithLogger(t, CheckoutCoordinatorConfig{
		Gate:      gate,
		Builder:   builder,
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
	}, admissionLogger)
	// Startup refresh intentionally writes the Git index. Join it before
	// this fixture's own checkout commands; a routed cycle is not its join.
	if !c.awaitRacyIndexHeal(30 * time.Second) {
		t.Fatal("startup Git index refresh did not finish before import setup")
	}
	c.cycleMu.Lock()
	c.cycleBarrier = func(ctx context.Context) { cycleContext = ctx }
	if earlyDemand {
		c.importAdmissionBarrier = queuedImportAdmissionProbe(t, c, gate, &importing)
	}
	if slowPreamble {
		c.importPreambleBarrier = func(ctx context.Context) {
			if !importing.Load() {
				return
			}
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		}
	}
	if slowFoldPlanning {
		c.importFoldPlanningBarrier = func(ctx context.Context) {
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		}
	}
	if slowPublicationTail {
		c.importPublicationTailBarrier = func(ctx context.Context) {
			if !importing.Load() {
				return
			}
			publicationTails.Add(1)
			publicationTailActive.Store(true)
			defer publicationTailActive.Store(false)
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		}
	}
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
	requireImportReadSetProof(t, c)
	c.Signal("first build")
	if out, ok := await(ctx); !ok || out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", out)
	}

	const imported = 12
	builderGit(t, f.primary, "checkout", "-q", "-b", "import-src")
	for i := 0; i < imported; i++ {
		source := fmt.Sprintf("package fixture\n\nfunc Imported%02d() int {\n\treturn %d\n}\n", i, i)
		if inline {
			source = fmt.Sprintf("package fixture\n\nfunc Imported%02d() Options {\n\tHelper()\n\tvalue := Options{}\n\treturn value\n}\n", i)
		}
		builderWriteFile(t, f.primary, fmt.Sprintf("imported_%02d.go", i), source)
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
	type slowAdmission struct {
		probe          int
		asked, granted time.Time
		wait           time.Duration
		beforeAcquire  time.Time
		beforeStats    ViewBuildGateStats
	}
	var slowAdmissions []slowAdmission // Single producer, read after probeDone.
	var worstAdmission slowAdmission
	var droppedSlowAdmissions int
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
				// Existing-state snapshot precedes the unchanged measured Acquire interval.
				beforeAcquire := time.Now()
				beforeStats := gate.Stats()
				asked := time.Now()
				release, err := gate.Acquire(probeCtx, ViewBuildInteractive)
				if err != nil {
					if probeCtx.Err() == nil {
						probeErr = err
					}
					return
				}
				waits = append(waits, time.Since(asked))
				if wait := waits[len(waits)-1]; wait >= 50*time.Millisecond {
					// Snapshot the grant while this probe still owns its lease.
					// The original measured Acquire interval above is unchanged.
					sample := slowAdmission{
						probe: len(waits), asked: asked, granted: gate.Stats().ActiveSince, wait: wait,
						beforeAcquire: beforeAcquire, beforeStats: beforeStats,
					}
					if len(slowAdmissions) < 64 {
						slowAdmissions = append(slowAdmissions, sample)
					} else {
						droppedSlowAdmissions++
					}
					if sample.wait > worstAdmission.wait {
						worstAdmission = sample
					}
				}
				// Exercise the shared writer while the import payload is private.
				err = f.store.AtGeneration(0).SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "interactive-probe"})
				if err == nil && publicationTailActive.Load() {
					publicationTailSQL.Add(1)
					elapsed := time.Since(asked).Nanoseconds()
					for {
						before := publicationTailWorst.Load()
						if elapsed <= before || publicationTailWorst.CompareAndSwap(before, elapsed) {
							break
						}
					}
				}
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
	for _, sample := range slowAdmissions {
		returned := sample.asked.Add(sample.wait)
		validGrant := !sample.granted.IsZero() && !sample.granted.Before(sample.asked) && !sample.granted.After(returned)
		t.Logf("slow interactive admission: probe=%d asked=%s granted=%s returned=%s total=%s grant_valid=%t request_to_grant=%s grant_to_return=%s",
			sample.probe, sample.asked.UTC().Format(time.RFC3339Nano), sample.granted.UTC().Format(time.RFC3339Nano), returned.UTC().Format(time.RFC3339Nano), sample.wait, validGrant, sample.granted.Sub(sample.asked), returned.Sub(sample.granted))
	}
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
	folds, links, yields, compilerPasses := 0, 0, 0, 0
	for i, cy := range cycles {
		if cy.out.YieldedTo != "" {
			yields++
			continue
		}
		if !cy.out.DirtyBuilt {
			continue
		}
		links++
		if goTypesRan(cy.out.DirtyWork) {
			if inline {
				t.Logf("import compiler cycle %d: %+v", i, cy.out.DirtyWork.CompilerContext)
			}
			compilerPasses++
		}
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
	if slowPublicationTail {
		t.Logf("publication-tail witness: parks=%d actual foreground SQL commits while parked=%d maxAcquireAndSQL=%s", publicationTails.Load(), publicationTailSQL.Load(), time.Duration(publicationTailWorst.Load()))
		if publicationTails.Load() == 0 || publicationTailSQL.Load() == 0 {
			t.Error("no actual foreground SQL committed while retained-route tail remained parked")
		}
		if time.Duration(publicationTailWorst.Load()) > 100*time.Millisecond {
			t.Error("foreground admission plus real SQL exceeded100ms during retained-route metadata tail")
		}
	}
	var worst time.Duration
	for _, w := range waits {
		worst = max(worst, w)
	}
	t.Logf("import: %d cycles, %d file links, %d folds, %d yields; %d interactive probes, worst wait %s (all: %v)",
		len(cycles), links, folds, yields, len(waits), worst, waits)
	if worst > 100*time.Millisecond {
		// Snapshotting the displaced holder after Acquire would name this
		// interactive probe instead. These pre-Acquire snapshots are bounded
		// observations, not proof that the holder stayed unchanged throughout.
		for _, sample := range append(slowAdmissions, worstAdmission) {
			t.Logf("import pre-Acquire observation: probe=%d sampled=%s asked=%s granted=%s returned=%s wait=%s active=%t active_since=%s holder=%+v interactive_queued=%d background_queued=%d admitted_interactive=%d admitted_background=%d yield_requests=%d yield_refusals=%d",
				sample.probe, sample.beforeAcquire.UTC().Format(time.RFC3339Nano), sample.asked.UTC().Format(time.RFC3339Nano), sample.granted.UTC().Format(time.RFC3339Nano), sample.asked.Add(sample.wait).UTC().Format(time.RFC3339Nano), sample.wait,
				sample.beforeStats.Active, sample.beforeStats.ActiveSince.UTC().Format(time.RFC3339Nano), sample.beforeStats.Holder,
				sample.beforeStats.InteractiveQueued, sample.beforeStats.BackgroundQueued, sample.beforeStats.AdmittedInteractive, sample.beforeStats.AdmittedBackground, sample.beforeStats.YieldRequests, sample.beforeStats.YieldRefusals)
		}
		logs, droppedLogs, writeCost := admissionLogs.snapshot()
		t.Logf("import admission diagnostics: slow_dropped=%d logs=%d logs_dropped=%d sink_write_cost=%s (excludes JSON encoding); coordinator/builder timestamps are existing-log emission times, not every lane boundary",
			droppedSlowAdmissions, len(logs), droppedLogs, writeCost)
		for _, entry := range logs {
			t.Logf("import existing phase log: %s", strings.TrimSpace(entry))
		}
		for i, cy := range cycles {
			var physical []GenerationPhase
			if cy.out.DirtyWork != nil {
				physical = cy.out.DirtyWork.Phases
			}
			t.Logf("import cycle diagnostic: cycle=%d started=%s generation=%d remaining=%d folded=%t plan=%v physical=%v",
				i, cy.out.cycleStarted.UTC().Format(time.RFC3339Nano), cy.out.DirtyGenerationID, cy.out.DirtyBatchRemaining, cy.out.ImportFolded, cy.out.PlanLaps, physical)
		}
		t.Errorf("an interactive build waited %s for the import, want ≤ 100ms", worst)
	}
	if inline {
		if compilerPasses != imported {
			t.Errorf("inline provider ran in %d/%d imported file publications", compilerPasses, imported)
		}
		view := chainMaterialize(t, f)
		knownCalls := 0
		for _, edge := range view.Reader.AllEdges() {
			if edge.Kind == graph.EdgeCalls && strings.Contains(edge.From, "::Imported") && strings.HasSuffix(edge.To, "::Helper") && edge.Origin == graph.OriginLSPResolved {
				knownCalls++
			}
		}
		view.Close()
		if knownCalls != imported {
			t.Errorf("known type-resolved imported calls=%d, want %d", knownCalls, imported)
		}
		t.Logf("inline compiler passes=%d resolved imported calls=%d", compilerPasses, knownCalls)
		if bindings := semanticReferenceBindingRows(t, f, "sustained-import-proof"); len(bindings) < imported {
			t.Errorf("reference named bindings=%d, want at least %d; parity must not compare empty sets", len(bindings), imported)
		}
		assertSemanticParity(t, f, manager, "sustained-import")
	} else if result := assertPropagationParity(t, f.store, routedStack(t, f, c), f.worktree, "import"); !result.ok() {
		t.Errorf("the imported working tree differs from a clean index: %v", result.Diffs)
	}
}

// The barrier identifies its own registered waiter under the gate lock; the
// original periodic producer cannot satisfy this witness. A negative rank tags
// the request without promoting it ahead of ordinary arrival order.
func queuedImportAdmissionProbe(t *testing.T, c *CheckoutCoordinator, gate *ViewBuildGate, importing *atomic.Bool) func(context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var requests sync.WaitGroup
	var sequence, queued atomic.Int64
	t.Cleanup(func() {
		importing.Store(false)
		cancel()
		c.Close() // No later cycle can add to requests once the loop has joined.
		joined := make(chan struct{})
		go func() { requests.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("early admission requests did not stop")
		}
		t.Logf("early admission actual queued-before-arm witnesses=%d", queued.Load())
		if !t.Skipped() && queued.Load() == 0 {
			t.Error("early admission had no registered pre-arm demand")
		}
	})
	return func(cycleCtx context.Context) {
		if !importing.Load() {
			return
		}
		tag := -sequence.Add(1)
		requests.Add(1)
		go func() {
			defer requests.Done()
			release, err := gate.AcquireRanked(ctx, ViewBuildInteractive, nil, func() int64 { return tag })
			if err == nil {
				release()
			}
		}()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			gate.mu.Lock()
			registered := false
			for _, waiter := range gate.interactive {
				if waiter.rank != nil && waiter.rank() == tag {
					registered = true
					break
				}
			}
			gate.mu.Unlock()
			if registered {
				queued.Add(1)
				return
			}
			select {
			case <-ticker.C:
			case <-cycleCtx.Done():
				return
			case <-timer.C:
				t.Error("early request did not register before yield arming")
				return
			}
		}
	}
}

// importAdmissionLogCapture is a bounded in-memory sink used only by the
// sustained-import fixture. Injection happens before coordinator construction,
// so constructor-spawned Git healing sees the same immutable logger pointer.
// No diagnostic I/O, SQL, sampling, stacks or sleeps run in the producer.
type importAdmissionLogCapture struct {
	mu        sync.Mutex
	lines     []string
	bytes     int
	dropped   int
	writeCost time.Duration
}

func newImportAdmissionLogCapture() (*zap.Logger, *importAdmissionLogCapture) {
	sink := &importAdmissionLogCapture{}
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "at"
	cfg.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(cfg), sink, zap.DebugLevel))
	return logger, sink
}

func (s *importAdmissionLogCapture) Write(p []byte) (int, error) {
	started := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 512*1024 {
		s.dropped++
	} else {
		// Keep the latest phases: a final publication can be the slow one.
		for len(s.lines) > 0 && (len(s.lines) >= 256 || s.bytes+len(p) > 512*1024) {
			s.bytes -= len(s.lines[0])
			copy(s.lines, s.lines[1:])
			s.lines[len(s.lines)-1] = ""
			s.lines = s.lines[:len(s.lines)-1]
			s.dropped++
		}
		s.lines = append(s.lines, string(p))
		s.bytes += len(p)
	}
	s.writeCost += time.Since(started)
	return len(p), nil
}

func (*importAdmissionLogCapture) Sync() error { return nil }

func (s *importAdmissionLogCapture) snapshot() ([]string, int, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...), s.dropped, s.writeCost
}
