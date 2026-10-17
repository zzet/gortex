package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

const checkoutPublicationPressurePrefix = "publication-pressure"

type checkoutPublicationScale struct {
	dirtyFiles       int
	symbolsPerFile   int
	pressureNodes    int
	pressureEdges    int
	iterationTimeout time.Duration
}

type checkoutPublicationCycleEvent struct {
	cycle CheckoutCycle
	at    time.Time
}

type checkoutPublicationAdmission struct {
	started      time.Time
	admitted     time.Time
	begin        time.Duration
	prepare      time.Duration
	diskEdit     time.Duration
	enqueueClose time.Duration
}

type checkoutPublicationObservation struct {
	result    MutationResult
	cycle     CheckoutCycle
	barrierAt time.Time
	cycleAt   time.Time
	ticketAt  time.Time
}

type checkoutPublicationTotals struct {
	begin                 time.Duration
	prepare               time.Duration
	diskEdit              time.Duration
	enqueueClose          time.Duration
	admission             time.Duration
	admissionToBarrier    time.Duration
	barrierToCycle        time.Duration
	publicationE2E        time.Duration
	mutationE2E           time.Duration
	exactQuery            time.Duration
	routeEpochDelta       int64
	commitBuilds          int64
	dirtyBuilds           int64
	composedViewNodeCount int64
	composedViewEdgeCount int64
}

func BenchmarkCheckoutPublication(b *testing.B) {
	if os.Getenv("GORTEX_PUBLICATION_BENCH") != "1" {
		b.Skip("set GORTEX_PUBLICATION_BENCH=1 to run the real checkout publication benchmark")
	}
	scale := checkoutPublicationBenchmarkScale(b)
	for _, arm := range []struct {
		name     string
		pressure bool
	}{
		{name: "small_store"},
		{name: "unrelated_store_pressure", pressure: true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			checkoutPublicationRun(b, scale, arm.pressure)
		})
	}
}

func checkoutPublicationRun(b *testing.B, scale checkoutPublicationScale, pressure bool) {
	b.Helper()
	ctx := context.Background()
	barrierEvents := make(chan time.Time, 16)
	cycleEvents := make(chan checkoutPublicationCycleEvent, 16)
	f := newCoordinatorFixture(b)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(b, CheckoutCoordinatorConfig{
		Gate:     gate,
		Debounce: 300 * time.Millisecond,
		dirtyBarrier: func() {
			barrierEvents <- time.Now()
		},
		cycleDone: func(cycle CheckoutCycle) {
			cycleEvents <- checkoutPublicationCycleEvent{cycle: cycle, at: time.Now()}
		},
	})
	// f.coordinator also installs a cleanup. This later cleanup runs first and
	// makes the loop join an explicit part of this harness's teardown.
	b.Cleanup(func() {
		if err := c.Close(); err != nil {
			b.Errorf("close checkout coordinator: %v", err)
		}
	})
	initial := c.reconcile(ctx)
	if initial.Err != nil || initial.DirtyGenerationID == 0 {
		b.Fatalf("initial reconcile: %+v", initial)
	}

	pressureNodes, pressureEdges := 0, 0
	if pressure {
		pressureNodes, pressureEdges = scale.pressureNodes, scale.pressureEdges
		checkoutPublicationSeedPressure(b, f.store, pressureNodes, pressureEdges)
	}

	cleanRoute := f.route()
	previousVersion := "PublicationVersion000000"
	for file := 0; file < scale.dirtyFiles; file++ {
		version := ""
		if file == 0 {
			version = previousVersion
		}
		checkoutPublicationWriteFile(b, f.worktree, checkoutPublicationFileName(file), checkoutPublicationSource(file, scale.symbolsPerFile, version))
	}
	baseline := c.reconcile(ctx)
	if baseline.Err != nil || baseline.CommitBuilt || !baseline.DirtyBuilt {
		b.Fatalf("establish dirty baseline: %+v", baseline)
	}
	baselineRoute := f.route()
	if baselineRoute.State != store_sqlite.RouteActive || baselineRoute.CommitGenerationID != cleanRoute.CommitGenerationID || baselineRoute.DirtyGenerationID == cleanRoute.DirtyGenerationID {
		b.Fatalf("unexpected dirty baseline route: clean=%+v dirty=%+v", cleanRoute, baselineRoute)
	}
	lifecycle := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	// Direct setup reconciles use the same barrier callback as timed loop
	// cycles. They are expected setup observations, not stale timed work.
	checkoutPublicationDrainEvents(barrierEvents, cycleEvents)

	var totals checkoutPublicationTotals
	iteration := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		checkoutPublicationAssertNoEvents(b, barrierEvents, cycleEvents)
		before := f.route()
		iteration++
		version := fmt.Sprintf("PublicationVersion%06d", iteration)
		ticket, admission := checkoutPublicationAdmit(b, lifecycle, f, before, scale, version)
		observation := checkoutPublicationAwait(b, ticket, barrierEvents, cycleEvents, scale.iterationTimeout)
		publicationDone := observation.cycleAt
		if observation.ticketAt.After(publicationDone) {
			publicationDone = observation.ticketAt
		}
		checkoutPublicationAssertCycle(b, before, f.route(), ticket, observation)

		totals.begin += admission.begin
		totals.prepare += admission.prepare
		totals.diskEdit += admission.diskEdit
		totals.enqueueClose += admission.enqueueClose
		totals.admission += admission.admitted.Sub(admission.started)
		if observation.barrierAt.Before(admission.admitted) {
			b.Fatalf("dirty barrier preceded admission close: barrier=%s admitted=%s", observation.barrierAt, admission.admitted)
		}
		if observation.cycleAt.Before(observation.barrierAt) {
			b.Fatalf("cycle completed before dirty barrier: cycle=%s barrier=%s", observation.cycleAt, observation.barrierAt)
		}
		totals.admissionToBarrier += observation.barrierAt.Sub(admission.admitted)
		totals.barrierToCycle += observation.cycleAt.Sub(observation.barrierAt)
		totals.publicationE2E += publicationDone.Sub(admission.admitted)
		totals.mutationE2E += publicationDone.Sub(admission.started)
		totals.routeEpochDelta += f.route().RouteEpoch - before.RouteEpoch
		if observation.cycle.CommitBuilt {
			totals.commitBuilds++
		}
		if observation.cycle.DirtyBuilt {
			totals.dirtyBuilds++
		}

		// Querying the exact composed view can scan and allocate independently of
		// publication. Keep it outside the benchmark timer and report it on its
		// own so it cannot inflate the publication measurements.
		b.StopTimer()
		queryStarted := time.Now()
		reader := checkoutPublicationComposedReader(b, f, f.route().CommitGenerationID, f.route().DirtyGenerationID)
		if nodes := reader.FindNodesByName(version); len(nodes) == 0 {
			b.Fatalf("published symbol %q is absent from the exact composed view", version)
		}
		if nodes := reader.FindNodesByName(previousVersion); len(nodes) != 0 {
			b.Fatalf("removed symbol %q remains in the exact composed view: %d nodes", previousVersion, len(nodes))
		}
		totals.exactQuery += time.Since(queryStarted)
		totals.composedViewNodeCount += int64(reader.NodeCount())
		totals.composedViewEdgeCount += int64(reader.EdgeCount())
		previousVersion = version
		b.StartTimer()
	}
	b.StopTimer()

	samples := float64(b.N)
	checkoutPublicationReportDuration(b, "begin-ns/op", totals.begin, samples)
	checkoutPublicationReportDuration(b, "prepare-ns/op", totals.prepare, samples)
	checkoutPublicationReportDuration(b, "disk-edit-ns/op", totals.diskEdit, samples)
	checkoutPublicationReportDuration(b, "enqueue-close-ns/op", totals.enqueueClose, samples)
	checkoutPublicationReportDuration(b, "admission-ns/op", totals.admission, samples)
	checkoutPublicationReportDuration(b, "admission-to-dirty-barrier-ns/op", totals.admissionToBarrier, samples)
	checkoutPublicationReportDuration(b, "dirty-barrier-to-cycle-done-ns/op", totals.barrierToCycle, samples)
	checkoutPublicationReportDuration(b, "publication-e2e-ns/op", totals.publicationE2E, samples)
	checkoutPublicationReportDuration(b, "mutation-e2e-ns/op", totals.mutationE2E, samples)
	checkoutPublicationReportDuration(b, "exact-query-ns/op", totals.exactQuery, samples)
	b.ReportMetric(float64(totals.routeEpochDelta)/samples, "route-epoch-delta/op")
	b.ReportMetric(float64(totals.commitBuilds)/samples, "commit-builds/op")
	b.ReportMetric(float64(totals.dirtyBuilds)/samples, "dirty-builds/op")
	b.ReportMetric(float64(totals.composedViewNodeCount)/samples, "view-nodes/op")
	b.ReportMetric(float64(totals.composedViewEdgeCount)/samples, "view-edges/op")
	b.ReportMetric(float64(pressureNodes), "pressure-nodes")
	b.ReportMetric(float64(pressureEdges), "pressure-edges")
}

func checkoutPublicationAdmit(b *testing.B, lifecycle *CheckoutLifecycle, f *coordinatorFixture, before store_sqlite.CheckoutRoute, scale checkoutPublicationScale, version string) (*CheckoutRefreshTicket, checkoutPublicationAdmission) {
	b.Helper()
	ctx := context.Background()
	timing := checkoutPublicationAdmission{started: time.Now()}
	started := time.Now()
	mutation, err := lifecycle.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, before.RouteEpoch)
	timing.begin = time.Since(started)
	if err != nil {
		b.Fatalf("begin checkout mutation: %v", err)
	}
	defer mutation.Close()

	started = time.Now()
	if err := mutation.Prepare(ctx); err != nil {
		b.Fatalf("prepare checkout mutation: %v", err)
	}
	timing.prepare = time.Since(started)

	started = time.Now()
	checkoutPublicationWriteFile(b, f.worktree, checkoutPublicationFileName(0), checkoutPublicationSource(0, scale.symbolsPerFile, version))
	timing.diskEdit = time.Since(started)

	started = time.Now()
	ticket, err := mutation.EnqueueRefresh(ctx, filepath.Join(f.worktree, checkoutPublicationFileName(0)))
	if err != nil {
		b.Fatalf("enqueue checkout refresh: %v", err)
	}
	mutation.Close()
	timing.admitted = time.Now()
	timing.enqueueClose = time.Since(started)
	return ticket, timing
}

func checkoutPublicationAwait(b *testing.B, ticket *CheckoutRefreshTicket, barrierEvents <-chan time.Time, cycleEvents <-chan checkoutPublicationCycleEvent, timeout time.Duration) checkoutPublicationObservation {
	b.Helper()
	var observation checkoutPublicationObservation
	ticketDone := ticket.Ticket.Done
	barrierDone := barrierEvents
	cycleDone := cycleEvents
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for ticketDone != nil || barrierDone != nil || cycleDone != nil {
		select {
		case result, ok := <-ticketDone:
			if !ok {
				b.Fatal("checkout refresh ticket closed without a result")
			}
			observation.result = result
			observation.ticketAt = time.Now()
			ticketDone = nil
		case at := <-barrierDone:
			observation.barrierAt = at
			barrierDone = nil
		case event := <-cycleDone:
			observation.cycle = event.cycle
			observation.cycleAt = event.at
			cycleDone = nil
		case <-timer.C:
			b.Fatalf("checkout publication exceeded the %s deadlock bound", timeout)
		}
	}
	return observation
}

func checkoutPublicationAssertCycle(b *testing.B, before, after store_sqlite.CheckoutRoute, ticket *CheckoutRefreshTicket, observation checkoutPublicationObservation) {
	b.Helper()
	cycle := observation.cycle
	result := observation.result
	if cycle.Err != nil || cycle.Deferred || cycle.Rescheduled {
		b.Fatalf("checkout cycle did not complete directly: %+v", cycle)
	}
	if cycle.CommitBuilt || !cycle.DirtyBuilt || cycle.Recomposed {
		b.Fatalf("single dirty-file edit rebuilt the wrong layers: %+v", cycle)
	}
	if cycle.CommitGenerationID != before.CommitGenerationID || cycle.DirtyGenerationID == before.DirtyGenerationID {
		b.Fatalf("unexpected generation transition: before=%+v cycle=%+v", before, cycle)
	}
	if after.State != store_sqlite.RouteActive || after.CommitGenerationID != before.CommitGenerationID || after.DirtyGenerationID != cycle.DirtyGenerationID || after.RouteEpoch <= before.RouteEpoch {
		b.Fatalf("unexpected active route after publication: before=%+v after=%+v cycle=%+v", before, after, cycle)
	}
	if result.Err != nil || !result.Reindexed || result.RequestedGeneration != ticket.Ticket.Generation || result.AppliedGeneration != uint64(after.DirtyGenerationID) {
		b.Fatalf("checkout refresh ticket does not name the published generation: result=%+v ticket=%+v route=%+v", result, ticket, after)
	}
}

func checkoutPublicationDrainEvents(barrierEvents <-chan time.Time, cycleEvents <-chan checkoutPublicationCycleEvent) {
	for draining := true; draining; {
		select {
		case <-barrierEvents:
		default:
			draining = false
		}
	}
	for draining := true; draining; {
		select {
		case <-cycleEvents:
		default:
			draining = false
		}
	}
}

func checkoutPublicationAssertNoEvents(b *testing.B, barrierEvents <-chan time.Time, cycleEvents <-chan checkoutPublicationCycleEvent) {
	b.Helper()
	select {
	case at := <-barrierEvents:
		b.Fatalf("stale dirty-barrier notification before iteration: %s", at)
	default:
	}
	select {
	case event := <-cycleEvents:
		b.Fatalf("stale cycle notification before iteration: %+v", event.cycle)
	default:
	}
}

func checkoutPublicationComposedReader(t testing.TB, f *coordinatorFixture, commitGeneration, dirtyGeneration int64) graph.Reader {
	t.Helper()
	commit, err := graphview.NewGenerationLayer(f.store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatalf("open commit layer: %v", err)
	}
	base := graph.NewOverlaidViewWithLayer(f.store.AtGeneration(0), commit)
	if dirtyGeneration == 0 {
		return base
	}
	dirty, err := graphview.NewGenerationLayer(f.store.AtGeneration(dirtyGeneration))
	if err != nil {
		t.Fatalf("open dirty layer: %v", err)
	}
	return graph.NewOverlaidViewWithLayer(base, dirty)
}

func checkoutPublicationSeedPressure(t testing.TB, store graph.Store, nodeCount, edgeCount int) {
	t.Helper()
	const nodeBatchSize = 4096
	for start := 0; start < nodeCount; start += nodeBatchSize {
		end := min(start+nodeBatchSize, nodeCount)
		nodes := make([]*graph.Node, 0, end-start)
		for i := start; i < end; i++ {
			id := checkoutPublicationPressureNodeID(i)
			nodes = append(nodes, &graph.Node{
				ID:         id,
				Kind:       graph.KindFunction,
				Name:       fmt.Sprintf("PublicationPressure%06d", i),
				Language:   "go",
				RepoPrefix: checkoutPublicationPressurePrefix,
				FilePath:   fmt.Sprintf("pressure/file_%06d.go", i/128),
			})
		}
		store.AddBatch(nodes, nil)
	}
	if nodeCount == 0 {
		return
	}
	const edgeBatchSize = 8192
	for start := 0; start < edgeCount; start += edgeBatchSize {
		end := min(start+edgeBatchSize, edgeCount)
		edges := make([]*graph.Edge, 0, end-start)
		for i := start; i < end; i++ {
			from := i % nodeCount
			round := i / nodeCount
			to := (from*17 + round*131 + 1) % nodeCount
			edges = append(edges, &graph.Edge{
				From:     checkoutPublicationPressureNodeID(from),
				To:       checkoutPublicationPressureNodeID(to),
				Kind:     "calls",
				FilePath: fmt.Sprintf("pressure/file_%06d.go", from/128),
				Line:     i%127 + 1,
			})
		}
		store.AddBatch(nil, edges)
	}
}

func checkoutPublicationPressureNodeID(i int) string {
	return fmt.Sprintf("%s:function:%06d", checkoutPublicationPressurePrefix, i)
}

func checkoutPublicationWriteFile(t testing.TB, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write benchmark fixture %s: %v", name, err)
	}
}

func checkoutPublicationFileName(file int) string {
	return fmt.Sprintf("publication_dirty_%02d.go", file)
}

func checkoutPublicationSource(file, symbols int, version string) string {
	var source strings.Builder
	source.WriteString("package fixture\n\n")
	for symbol := 0; symbol < symbols; symbol++ {
		fmt.Fprintf(&source, "func PublicationStable%02d_%03d() int { return %d }\n", file, symbol, file*symbols+symbol)
	}
	if version != "" {
		fmt.Fprintf(&source, "\nfunc %s() int { return %d }\n", version, file+symbols)
	}
	return source.String()
}

func checkoutPublicationBenchmarkScale(b *testing.B) checkoutPublicationScale {
	b.Helper()
	var scale checkoutPublicationScale
	switch name := strings.TrimSpace(os.Getenv("GORTEX_PUBLICATION_SCALE")); name {
	case "", "quick":
		scale = checkoutPublicationScale{dirtyFiles: 4, symbolsPerFile: 32, pressureNodes: 5_000, pressureEdges: 20_000, iterationTimeout: 2 * time.Minute}
	case "realistic":
		scale = checkoutPublicationScale{dirtyFiles: 14, symbolsPerFile: 256, pressureNodes: 50_000, pressureEdges: 250_000, iterationTimeout: 10 * time.Minute}
	default:
		b.Fatalf("unknown GORTEX_PUBLICATION_SCALE %q (want quick or realistic)", name)
	}
	scale.dirtyFiles = checkoutPublicationPositiveEnv(b, "GORTEX_PUBLICATION_DIRTY_FILES", scale.dirtyFiles)
	scale.symbolsPerFile = checkoutPublicationPositiveEnv(b, "GORTEX_PUBLICATION_SYMBOLS_PER_FILE", scale.symbolsPerFile)
	scale.pressureNodes = checkoutPublicationPositiveEnv(b, "GORTEX_PUBLICATION_PRESSURE_NODES", scale.pressureNodes)
	scale.pressureEdges = checkoutPublicationPositiveEnv(b, "GORTEX_PUBLICATION_PRESSURE_EDGES", scale.pressureEdges)
	if value := strings.TrimSpace(os.Getenv("GORTEX_PUBLICATION_TIMEOUT")); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			b.Fatalf("invalid GORTEX_PUBLICATION_TIMEOUT %q", value)
		}
		scale.iterationTimeout = timeout
	}
	return scale
}

func checkoutPublicationPositiveEnv(b *testing.B, key string, fallback int) int {
	b.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		b.Fatalf("%s must be a positive integer, got %q", key, value)
	}
	return parsed
}

func checkoutPublicationReportDuration(b *testing.B, name string, total time.Duration, samples float64) {
	b.Helper()
	b.ReportMetric(float64(total.Nanoseconds())/samples, name)
}
