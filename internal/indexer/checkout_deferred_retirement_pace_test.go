package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The deferred retirement pace test: the production shape — a real linked
// worktree over real SQLite, the build gate installed and open on the
// coordinator and the lifecycle (the gated branch the daemon takes), the
// contract-core runtime on the builder, the native watcher, and the real
// worker loop — under a sustained edit stream with a require_fresh read every
// second, then idle. Two seams shorten production limits so the windows
// below fit the test and bind: the starvation limit (the 2-minute stand-down
// the daemon keeps while debt is young) and the idle burst (2 s in production;
// a short one makes the fixture's backlog take many bursts, so the order they
// take generations in and the cadence between them show). The 20 s edit idle
// is production's.

const (
	paceFiles      = 16
	paceFuncs      = 30
	paceStream     = 30 * time.Second
	paceStarvation = 10 * time.Second
	paceDrain      = 30 * time.Second
	paceIdleBurst  = 200 * time.Millisecond
	// paceResumeDrain bounds the drain once retirement resumes after the
	// stream and its seeded debt: back-to-back bursts clear it within the
	// bound (measured 5-8 s), one burst per one-second pass does not (49 s).
	// Under the race detector the bound is paceResumeDrainRace (measured
	// 18 s; one burst per pass took 97 s).
	paceResumeDrain     = 12 * time.Second
	paceResumeDrainRace = 3 * paceResumeDrain
	// paceExtraGenerations failed generations of paceExtraNodes nodes are
	// owed at the end of the stream (seedPaceDebt).
	paceExtraGenerations = 8
	paceExtraNodes       = 30000
	// paceSlack is the backlog that may remain: one build in flight, one
	// generation mid-burst, and the routed chain's depth.
	paceSlack = 4
)

// paceTree is a committed tree whose working-tree layers carry enough rows
// that the drain is measurable.
func paceTree() map[string]string {
	tree := map[string]string{}
	for i := 0; i < paceFiles; i++ {
		var b strings.Builder
		b.WriteString("package fixture\n")
		for j := 0; j < paceFuncs; j++ {
			fmt.Fprintf(&b, "\nfunc Pace%dF%d() {\n", i, j)
			if j > 0 {
				fmt.Fprintf(&b, "\tPace%dF%d()\n", i, j-1)
			}
			b.WriteString("}\n")
		}
		tree[fmt.Sprintf("pace%02d.go", i)] = b.String()
	}
	return tree
}

type paceSample struct {
	at       time.Time
	retiring int
	backlog  RetirementBacklog
}

type paceResult struct {
	tickets              []time.Duration
	ticketErrs           int
	started, lastWrite   time.Time
	drainedAt            time.Time
	samples              []paceSample
	scans, passes, downs int64
	sightings            int64
	m0, mEdit, mEnd      viewmetrics.Snapshot
	mEndAt               time.Time
	extra                []int64
	extraSeeded          time.Time
	extraLeft            int
	bursts               []observer.LoggedEntry
	cycleErrs            int
	leaked               []int64
}

// paceRun drives one stream. With retire the real worker loop runs beside it.
func paceRun(t *testing.T, retire bool) paceResult {
	t.Helper()
	t.Setenv(checkoutWatchDisableEnv, "")
	f := newCoordinatorFixtureWithTree(t, paceTree())
	builder := builderNewBuilder(f.store)
	// The daemon installs the contract-core runtime on every builder.
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	gate := NewViewBuildGate()
	gate.Open()
	cycles := &motionFixtureCycles{}
	c := liveMotionCoordinator(t, f, cycles, CheckoutCoordinatorConfig{Builder: builder, Gate: gate})
	installDaemonBuildLaneBusy(f, c)
	core, logs := observer.New(zap.InfoLevel)
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, leases: f.leases, logger: zap.New(core),
		owed: map[int64]struct{}{}, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}, cfgWatchCheckouts: true}
	l.SetBuildGate(gate)
	l.retirementStarvationLimit = paceStarvation
	l.retirementIdleBurst = paceIdleBurst

	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	if retire {
		workers.Add(1)
		go func() {
			defer workers.Done()
			DeferredRetirementLoop{
				Sweep: l.SweepDeferredRetirements, Logger: zap.New(core),
				BasePause: time.Second, MaxBackoff: time.Minute, IdlePause: 30 * time.Second,
				Progress: DeferredRetirementProgress, Wake: DeferredRetirementWake,
			}.Run(ctx)
		}()
	}
	var smu sync.Mutex
	var samples []paceSample
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			rows, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{})
			if err != nil {
				continue
			}
			s := paceSample{at: time.Now(), backlog: l.retirementBacklogFrom(rows, time.Now())}
			s.retiring = s.backlog.Retiring
			smu.Lock()
			samples = append(samples, s)
			smu.Unlock()
		}
	}()

	var res paceResult
	var tmu sync.Mutex
	var asks sync.WaitGroup
	// ask is a require_fresh read: a refresh ticket bound to a sample begun
	// after it arrived, retried while superseded, answered within 10 s.
	ask := func() {
		defer asks.Done()
		arrived := time.Now()
		deadline := arrived.Add(streamFreshWait)
		var err error
		for {
			var ticket *CheckoutRefreshTicket
			ticket, err = l.RequestBoundCheckoutRefresh(WithFreshRequestArrival(context.Background(), arrived), f.checkoutID, f.worktree)
			if err == nil {
				select {
				case result := <-ticket.Ticket.Done:
					err = result.Err
				case <-time.After(time.Until(deadline)):
					err = fmt.Errorf("not answered within %s", streamFreshWait)
				}
			}
			retry := errors.Is(err, ErrCheckoutRefreshSuperseded) || errors.Is(err, ErrCheckoutMutationBusy) || errors.Is(err, ErrCheckoutRefreshQueueFull)
			if !retry || time.Now().Add(25*time.Millisecond).After(deadline) {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		tmu.Lock()
		res.tickets = append(res.tickets, time.Since(arrived))
		if err != nil {
			res.ticketErrs++
		}
		tmu.Unlock()
	}

	cycles.mu.Lock()
	mark := len(cycles.cycles)
	cycles.mu.Unlock()
	res.m0 = viewmetrics.Read()
	scans0, passes0, downs0 := deferredRetirementScans.Load(), res.m0.Counters[viewmetrics.RetirementPassesTotal], deferredRetirementPassStandDowns.Load()
	sightings0 := deferredRetirementFirstSightings.Load()
	res.started = time.Now()
	for i := 0; time.Since(res.started) < paceStream; i++ {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		name := filepath.Join(f.worktree, fmt.Sprintf("pace%02d.go", i%paceFiles))
		if err := appendToFile(name, fmt.Sprintf("\nfunc PaceEdit%d() {}\n", i)); err != nil {
			t.Fatal(err)
		}
		res.lastWrite = time.Now()
		if i%10 == 0 {
			asks.Add(1)
			go ask()
		}
	}
	res.mEdit = viewmetrics.Read()
	res.scans = deferredRetirementScans.Load() - scans0
	res.passes = res.mEdit.Counters[viewmetrics.RetirementPassesTotal] - passes0
	res.downs = deferredRetirementPassStandDowns.Load() - downs0
	res.sightings = deferredRetirementFirstSightings.Load() - sightings0
	cycles.published(t, f, c, res.lastWrite, 60*time.Second)
	asks.Wait()

	if retire {
		// What the stream leaves is drained in about a second, too little for
		// the idle drain's order (A4), cadence (A2's resume bound) and duty
		// (A5) to show. Failed generations large enough to take several idle
		// bursts each are owed on top of it, as a failed build leaves them.
		res.extra = seedPaceDebt(t, f, c, l)
		res.extraSeeded = time.Now()
		// A2/A3: the backlog, parked work included, drains within twice the
		// production bound after the last write: the edit idle, the
		// starvation limit and the drain.
		bound := res.lastWrite.Add(2 * (deferredRetirementEditIdle + paceStarvation + paceDrain))
		// A3 watches the backlog after the edit idle, so keep sampling past it.
		watchUntil := res.lastWrite.Add(deferredRetirementEditIdle + 10*time.Second)
		for time.Now().Before(bound) {
			backlog, err := l.RetirementBacklog(context.Background())
			if err == nil && res.drainedAt.IsZero() && backlog.Generations <= paceSlack && backlog.Retiring == 0 {
				res.drainedAt = time.Now()
			}
			if !res.drainedAt.IsZero() && time.Now().After(watchUntil) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if retire {
		res.leaked = paceLeakedLayers(t, f, c, l)
	}
	res.mEnd, res.mEndAt = viewmetrics.Read(), time.Now()
	cancel()
	workers.Wait()
	smu.Lock()
	res.samples = append([]paceSample(nil), samples...)
	smu.Unlock()
	res.bursts = logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	for _, id := range res.extra {
		if _, found, err := f.catalog.GetViewGeneration(context.Background(), id); err != nil || found {
			res.extraLeft++
		}
	}
	cycles.mu.Lock()
	for _, out := range cycles.cycles[mark:] {
		if out.Err != nil {
			res.cycleErrs++
		}
	}
	cycles.mu.Unlock()
	return res
}

func p95(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (95*len(sorted)+99)/100 - 1
	return sorted[max(index, 0)]
}

func counterDeltaKey(before, after viewmetrics.Snapshot, key string) int64 {
	return after.Counters[key] - before.Counters[key]
}

// Deferred retirement keeps pace with a sustained linked-worktree edit stream
// and drains at idle, without costing the stream's reads their latency.
//
// Under the race detector the instrumented SQLite builds and deletes tens of
// times slower: most require_fresh reads miss their 10 s answer in both arms,
// so A7's latency measures nothing there and is skipped; A2's resume bound is
// widened to paceResumeDrainRace, and everything else runs as it does
// without the detector.
func TestDeferredRetirementKeepsPaceWithSustainedLinkedWorktreeEdits(t *testing.T) {
	if testing.Short() {
		t.Skip("native file watcher; minutes of wall clock")
	}
	var twin, run paceResult
	t.Run("retirement_off", func(t *testing.T) { twin = paceRun(t, false) })
	t.Run("retirement_on", func(t *testing.T) { run = paceRun(t, true) })
	if t.Failed() {
		return
	}

	owedSum := func(m0, m1 viewmetrics.Snapshot) int64 {
		var n int64
		for _, why := range []string{viewmetrics.OwedTorn, viewmetrics.OwedFailed, viewmetrics.OwedReleased, viewmetrics.OwedDiscovered} {
			n += counterDeltaKey(m0, m1, viewmetrics.GenerationRetireOwedTotal+"{why="+why+"}")
		}
		return n
	}
	owed := owedSum(run.m0, run.mEnd)
	retired := counterDeltaKey(run.m0, run.mEnd, viewmetrics.GenerationRetiredTotal+"{owner="+viewmetrics.OwnerCheckout+"}")
	errored := counterDeltaKey(run.m0, run.mEnd, viewmetrics.GenerationRetireRefusedTotal+"{reason="+viewmetrics.RefusedError+"}")
	maxRetiring := 0
	for _, s := range run.samples {
		maxRetiring = max(maxRetiring, s.retiring)
	}
	var final RetirementBacklog
	if n := len(run.samples); n > 0 {
		final = run.samples[n-1].backlog
	}
	t.Logf("stream %s: owed=%d retired=%d (yielded=%d preempted=%d error=%d) quanta=%d rows=%d; final backlog %+v; max retiring %d; cycle errors %d (twin %d)",
		run.lastWrite.Sub(run.started).Round(time.Second), owed, retired,
		counterDeltaKey(run.m0, run.mEnd, viewmetrics.GenerationRetireRefusedTotal+"{reason="+viewmetrics.RetireYielded+"}"),
		counterDeltaKey(run.m0, run.mEnd, viewmetrics.GenerationRetireRefusedTotal+"{reason="+viewmetrics.RetirePreempted+"}"),
		errored, counterDeltaKey(run.m0, run.mEnd, viewmetrics.RetirementQuantaTotal),
		counterDeltaKey(run.m0, run.mEnd, viewmetrics.RetirementRowsDeletedTotal), final, maxRetiring, run.cycleErrs, twin.cycleErrs)
	t.Logf("owed during the stream by why: torn=%d failed=%d released=%d discovered=%d; %d bursts",
		counterDeltaKey(run.m0, run.mEdit, viewmetrics.GenerationRetireOwedTotal+"{why=torn}"),
		counterDeltaKey(run.m0, run.mEdit, viewmetrics.GenerationRetireOwedTotal+"{why=failed}"),
		counterDeltaKey(run.m0, run.mEdit, viewmetrics.GenerationRetireOwedTotal+"{why=released}"),
		counterDeltaKey(run.m0, run.mEdit, viewmetrics.GenerationRetireOwedTotal+"{why=discovered}"), len(run.bursts))

	// A1: a pass that stands down over known, young debt does not scan. Only
	// the first sighting of new debt scans before it stands down — once per
	// debt episode: the clock restarts whenever the eligible debt drains.
	t.Logf("A1: %d passes, %d stand-downs (%d after a first sighting), %d discovery scans during the stream",
		run.passes, run.downs, run.sightings, run.scans)
	if run.scans > run.passes-run.downs+run.sightings+1 {
		t.Errorf("A1: %d discovery scans for %d passes of which %d stood down, %d after a first sighting",
			run.scans, run.passes, run.downs, run.sightings)
	}
	// A2: everything owed is retired, up to the slack, within the bound —
	// as counted (each generation is counted owed once) and as left in the
	// backlog, parked work included. The seeded failed generations are owed
	// to the lifecycle, counted owed and retired like any other, and must all
	// go.
	if retired < owed-paceSlack {
		t.Errorf("A2: retired %d of %d owed generations (slack %d)", retired, owed, paceSlack)
	}
	if run.extraLeft != 0 {
		t.Errorf("A2: %d of the %d seeded failed generations were not retired", run.extraLeft, len(run.extra))
	}
	if run.drainedAt.IsZero() {
		t.Errorf("A2: the backlog did not drain to %d generations within %s of the last write: %+v",
			paceSlack, 2*(deferredRetirementEditIdle+paceStarvation+paceDrain), final)
	} else {
		t.Logf("A2: drained %s after the last write", run.drainedAt.Sub(run.lastWrite).Round(time.Millisecond))
	}
	// Once retirement resumes after the stream, back-to-back bursts clear
	// what it left and the seeded debt within paceResumeDrain; one burst per
	// one-second pass does not.
	resumed, cleared := paceResumeWindow(run)
	if resumed.IsZero() {
		t.Errorf("A2: no burst ran after the seeded debt was owed")
	} else {
		t.Logf("A2: retirement resumed %s after the seeding and cleared the backlog in %s",
			resumed.Sub(run.extraSeeded).Round(time.Millisecond), cleared.Sub(resumed).Round(time.Millisecond))
		resumeBound := paceResumeDrain
		if indexerRaceDetectorOn {
			resumeBound = paceResumeDrainRace
		}
		if cleared.Sub(resumed) > resumeBound {
			t.Errorf("A2: the backlog took %s to clear once retirement resumed, want at most %s",
				cleared.Sub(resumed).Round(time.Millisecond), resumeBound)
		}
	}
	// A2: no working-tree layer is left behind that nothing owes, routes or
	// caches. Discovery never offers a live checkout's ready layers, so such
	// a layer is never retired, and it pins its chain.
	if len(run.leaked) > 0 {
		t.Errorf("A2: ready working-tree layers neither routed, cached nor owed: %v", run.leaked)
	}
	// A3: after the edit idle the backlog is small and its size never grows.
	// The seeded debt is owed once, after the stream's last publication
	// (later than the edit idle under the race detector); the watch starts
	// after it.
	quiet := maxTime(run.lastWrite.Add(deferredRetirementEditIdle), run.extraSeeded)
	var prev int64 = -1
	for _, s := range run.samples {
		if s.at.Before(quiet) {
			continue
		}
		if prev >= 0 && s.backlog.BytesEstimate > prev {
			t.Errorf("A3: backlog bytes grew %d -> %d at +%s after the last write", prev, s.backlog.BytesEstimate, s.at.Sub(run.lastWrite).Round(time.Second))
		}
		prev = s.backlog.BytesEstimate
	}
	if final.Generations > paceSlack {
		t.Errorf("A3: %d generations still owed at the end (%d parked, slack %d)", final.Generations, final.Parked, paceSlack)
	}
	// A4: generations are finished, not fenced in bulk.
	t.Logf("A4: at most %d generations retiring at once", maxRetiring)
	if maxRetiring > 5 {
		t.Errorf("A4: %d generations were retiring at once, want at most 5", maxRetiring)
	}
	// A5: from the first burst after the seeding until the backlog is
	// cleared (at most 10 s), with nobody waiting, bursts hold the lane at
	// least half of the time (each burst's hold clipped to the window).
	if resumed.IsZero() || cleared.Sub(resumed) < 2*time.Second {
		t.Errorf("A5: vacuous — %s of retirement after the seeding, want at least 2s to measure",
			cleared.Sub(resumed).Round(time.Millisecond))
	} else {
		end := minTime(cleared, resumed.Add(10*time.Second))
		duty := laneDuty(run.bursts, resumed, end)
		t.Logf("A5: duty %.2f over %s", duty, end.Sub(resumed).Round(time.Millisecond))
		if duty < 0.5 || duty > 1.01 {
			t.Errorf("A5: bursts held the lane %.0f%% of %s with work owed", 100*duty, end.Sub(resumed).Round(time.Millisecond))
		}
	}
	// A6: no retirement error, and no edit cycle failed that did not fail
	// without retirement.
	if errored != 0 {
		t.Errorf("A6: retire_refused{error} rose by %d", errored)
	}
	if run.cycleErrs > twin.cycleErrs {
		t.Errorf("A6: %d edit cycles failed with retirement, %d without", run.cycleErrs, twin.cycleErrs)
	}
	// A7: retirement does not cost the stream's reads their latency, and the
	// reads are answered.
	on, off := p95(run.tickets), p95(twin.tickets)
	t.Logf("A7: require_fresh p95 %s with retirement, %s without (%d / %d reads, %d / %d errors)",
		on, off, len(run.tickets), len(twin.tickets), run.ticketErrs, twin.ticketErrs)
	if indexerRaceDetectorOn {
		t.Logf("A7: skipped under the race detector")
		return
	}
	if run.ticketErrs != 0 || twin.ticketErrs != 0 {
		t.Errorf("A7: %d / %d require_fresh reads failed (retirement on / off)", run.ticketErrs, twin.ticketErrs)
	}
	// Twice the production 100 ms allowance: two separate runs' p95s differ by
	// their own noise as well.
	if on > off+200*time.Millisecond {
		t.Errorf("A7: require_fresh p95 %s with retirement against %s without", on, off)
	}
}

// paceResumeWindow is when retirement resumed after the stream and its seeded
// debt (the start of the first burst that ended after the seeding) and when
// the backlog was first seen cleared after that (no unparked generation owed,
// none retiring); zero when no burst ran after the seeding.
func paceResumeWindow(run paceResult) (resumed, cleared time.Time) {
	for _, entry := range run.bursts {
		start := entry.Time.Add(-entry.ContextMap()["lane_hold"].(time.Duration))
		if entry.Time.After(run.extraSeeded) && (resumed.IsZero() || start.Before(resumed)) {
			resumed = start
		}
	}
	if resumed.IsZero() {
		return resumed, resumed
	}
	resumed = maxTime(resumed, run.extraSeeded)
	cleared = run.mEndAt
	for _, s := range run.samples {
		if s.at.After(resumed) && s.backlog.Generations-s.backlog.Parked == 0 && s.retiring == 0 {
			cleared = s.at
			break
		}
	}
	return resumed, cleared
}

// laneDuty is the share of [from, to) the bursts held the lane, each burst's
// hold clipped to the window.
func laneDuty(bursts []observer.LoggedEntry, from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	var held time.Duration
	for _, entry := range bursts {
		end := entry.Time
		start := end.Add(-entry.ContextMap()["lane_hold"].(time.Duration))
		start, end = maxTime(start, from), minTime(end, to)
		if end.After(start) {
			held += end.Sub(start)
		}
	}
	return held.Seconds() / to.Sub(from).Seconds()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// paceLeakedLayers lists the checkout's ready working-tree layers that are not
// in the routed chain, not in a cached layer's chain and not owed.
func paceLeakedLayers(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, l *CheckoutLifecycle) []int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		States: []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady},
	})
	if err != nil {
		t.Fatalf("list ready generations: %v", err)
	}
	route, _, err := f.catalog.GetCheckoutRoute(ctx, f.checkoutID)
	if err != nil {
		t.Fatalf("read the route: %v", err)
	}
	c.mu.Lock()
	tops := []int64{route.DirtyGenerationID, c.routedDirty}
	for _, entry := range c.retainedDirty {
		tops = append(tops, entry.generationID)
	}
	c.mu.Unlock()
	kept := map[int64]struct{}{}
	for _, top := range tops {
		for _, id := range c.dirtyChainMembers(ctx, top) {
			kept[id] = struct{}{}
		}
	}
	owed := l.owedRetirementSet()
	var leaked []int64
	for _, row := range rows {
		if row.CheckoutID != f.checkoutID || row.GenerationKind != DirtyLayerGenerationKind {
			continue
		}
		_, keep := kept[row.GenerationID]
		_, owes := owed[row.GenerationID]
		if !keep && !owes {
			leaked = append(leaked, row.GenerationID)
		}
	}
	return leaked
}

// seedPaceDebt owes the lifecycle paceExtraGenerations failed generations of
// the checkout's own kind, each large enough to take several idle bursts.
func seedPaceDebt(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, l *CheckoutLifecycle) []int64 {
	t.Helper()
	generations, nodes := paceExtraGenerations, paceExtraNodes
	if indexerRaceDetectorOn {
		// The instrumented pure-Go SQLite deletes tens of times slower.
		nodes /= 40
	}
	c.mu.Lock()
	routed := c.routedDirty
	c.mu.Unlock()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), routed)
	if err != nil || !found {
		t.Fatalf("routed generation %d: found=%v err=%v", routed, found, err)
	}
	template := store_sqlite.PayloadGenerationRequest{
		OwnerKind: row.OwnerKind, GraphID: row.GraphID, CheckoutID: row.CheckoutID,
		GenerationKind: row.GenerationKind, ExtractorVersions: row.ExtractorVersions,
		ResolverVersion: row.ResolverVersion,
	}
	// A building generation no flight owns is an orphan to discovery: hold
	// the sweep off until the seeded ones are written and failed.
	l.retirementSweepMu.Lock()
	defer l.retirementSweepMu.Unlock()
	ids := make([]int64, 0, generations)
	for i := 0; i < generations; i++ {
		ids = append(ids, seedRetirementPayload(t, f.store, template, fmt.Sprintf("pace-debt-%d", i), nodes, 1<<20))
	}
	l.oweRetirement(ids...)
	return ids
}
