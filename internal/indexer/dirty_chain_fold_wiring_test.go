package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// foldWiringEdits are the working-tree edits of tree A the stepped-fold tests
// publish, in order: a body change, a removed function, a deleted file, a
// renamed file, an added file, and further body changes. Each returns the
// label it is reported under.
var foldWiringEdits = []func(t *testing.T, f *coordinatorFixture){
	func(t *testing.T, f *coordinatorFixture) {
		builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	},
	func(t *testing.T, f *coordinatorFixture) { // a removal: Run no longer calls Compute
		builderWriteFile(t, f.worktree, "caller.go", "package fixture\n\nfunc Run() {\n}\n")
	},
	func(t *testing.T, f *coordinatorFixture) { // a file deletion
		if err := os.Remove(filepath.Join(f.worktree, "gone.go")); err != nil {
			t.Fatal(err)
		}
	},
	func(t *testing.T, f *coordinatorFixture) { // a rename
		if err := os.Rename(filepath.Join(f.worktree, "oldname.go"), filepath.Join(f.worktree, "newname.go")); err != nil {
			t.Fatal(err)
		}
	},
	func(t *testing.T, f *coordinatorFixture) { // an added file
		builderWriteFile(t, f.worktree, "added.go", "package fixture\n\nfunc Added() {\n\tHelper()\n}\n")
	},
	func(t *testing.T, f *coordinatorFixture) {
		builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\tHelper()\n}\n")
	},
	func(t *testing.T, f *coordinatorFixture) {
		builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	},
}

// startSteppedFold runs one compaction of out's chain in the background and
// holds it before its first step until the returned proceed is called. It
// returns once the fold is begun (the folding chain is published).
func startSteppedFold(t *testing.T, c *CheckoutCoordinator, out CheckoutCycle) (proceed func(), result <-chan DirtyChainCompaction) {
	t.Helper()
	begun := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	c.compaction.mu.Lock()
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step == 0 {
			close(begun)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()
	done := make(chan DirtyChainCompaction, 1)
	go func() { done <- c.compactDirtyChain(context.Background(), out) }()
	select {
	case <-begun:
	case report := <-done:
		t.Fatalf("the fold ended before its first step: %+v", report)
	case <-time.After(30 * time.Second):
		t.Fatal("the fold did not begin")
	}
	return func() { once.Do(func() { close(gate) }) }, done
}

// 1. A stepped fold runs while edits publish above it, lands, and re-bases
// the lowest layer above it; the served view equals a clean index of the
// working tree (renames, removals and a deleted file included).
func TestSteppedFoldLandsUnderEditsAndRebasesTheLayerAbove(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	if trigger.DirtyChainDepth != 4 || !trigger.CompactionScheduled {
		t.Fatalf("the fourth edit: depth %d scheduled %t, want depth 4 and a compaction", trigger.DirtyChainDepth, trigger.CompactionScheduled)
	}
	folded := c.dirtyChainMembers(context.Background(), trigger.DirtyGenerationID)
	slices.Reverse(folded)
	proceed, result := startSteppedFold(t, c, trigger)
	var above []int64
	for i := 4; i < 6; i++ {
		out := mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("edit %d while the fold runs built direct (%s)", i+1, out.DirtyChainReason)
		}
		above = append(above, out.DirtyGenerationID)
	}
	proceed()
	report := <-result
	if report.Landing != foldLandRebase || report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the fold landed %q (%s, err %v), want a re-base", report.Landing, report.Outcome, report.Err)
	}
	lowest, found := f.generation(above[0])
	if !found || lowest.BaseGenerationID != report.GenerationID {
		t.Fatalf("the lowest layer above stands on %d, want the fold %d", lowest.BaseGenerationID, report.GenerationID)
	}
	chain := c.dirtyChainMembers(context.Background(), f.route().DirtyGenerationID)
	if want := []int64{above[1], above[0], report.GenerationID}; !slices.Equal(chain, want) {
		t.Fatalf("the routed chain is %v, want %v (folded %v)", chain, want, folded)
	}
	chainAssertFlat(t, f, "stepped-fold-rebase")
	// The next edit chains over the re-based chain.
	if out := mcpEdit(t, l, f, func() { foldWiringEdits[6](t, f) }); out.DirtyParentGenerationID != above[1] || out.DirtyChainDepth != 4 {
		t.Fatalf("the edit after the landing: parent %d depth %d, want %d at depth 4", out.DirtyParentGenerationID, out.DirtyChainDepth, above[1])
	}
	chainAssertFlat(t, f, "stepped-fold-after")
}

// 2. A burst of 12 edits at a 5 s pace, a search after each, with the fold
// running in the background: no edit meets the chain bound, and a writer that
// arrives while a step holds the write gate waits no longer than a step's
// yield (measured 1.2-1.3 ms; the bound asserted is the store test's limit).
func TestSteppedFoldBurstNeverExhaustsTheChainAndNoEditWaitsOnAStep(t *testing.T) {
	pace := 5 * time.Second
	if testing.Short() {
		pace = 200 * time.Millisecond
	}
	f, c, l := mcpChainFixture(t, builderTreeA(), true)
	var probeMu sync.Mutex
	var probes []time.Duration
	c.compaction.mu.Lock()
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step == 0 {
			// Hold the fold open across the next edits, so they publish above
			// a running fold (on this small fixture the steps themselves take
			// milliseconds).
			select {
			case <-time.After(pace * 3 / 2):
			case <-ctx.Done():
			}
		}
		// A writer that arrives right as the next step begins.
		go func() {
			time.Sleep(time.Millisecond)
			started := time.Now()
			_, _, _ = f.store.LoadActiveAnalysisHeader(0)
			probeMu.Lock()
			probes = append(probes, time.Since(started))
			probeMu.Unlock()
		}()
	}
	c.compaction.mu.Unlock()
	above := 0
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		out := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
		if out.DirtyChainReason == dirtyChainFallbackChainDepthExhausted {
			t.Fatalf("edit %d met the chain bound (depth %d)", i+1, out.DirtyChainDepth)
		}
		if out.DirtyParentGenerationID > 0 && len(c.foldingChain()) > 0 {
			above++
		}
		view := chainMaterialize(t, f) // the search
		_ = view.Reader.FindNodesByName("Helper")
		view.Close()
		time.Sleep(pace)
	}
	if err := c.waitDirtyChainCompactions(ctx); err != nil {
		t.Fatal(err)
	}
	stats := c.DirtyChainCompactionStats()
	if stats.Flipped == 0 {
		t.Fatalf("no stepped fold landed during the burst: %+v", stats)
	}
	if above == 0 {
		t.Fatal("no edit published above a running fold")
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	worst := time.Duration(0)
	for _, p := range probes {
		worst = max(worst, p)
	}
	t.Logf("burst: %d folds landed, %d edits above a running fold, %d gate probes during steps, worst wait %s", stats.Flipped, above, len(probes), worst)
	if worst > 20*time.Millisecond {
		t.Fatalf("a writer waited %s for the gate while the fold stepped", worst)
	}
}

// 3. A fold canceled half way leaves its target owed to the sweep (failed, or
// building and unowned after a crash), the store's fold lease is free, and
// the next fold of the same chain starts clean and lands.
func TestSteppedFoldCanceledHalfWayIsSweptAndTheNextFoldStartsClean(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.compaction.stepHook = func(context.Context, int) { cancel() }
	report := c.compactDirtyChain(ctx, trigger)
	if !report.Canceled {
		t.Fatalf("the canceled fold: %+v", report)
	}
	if len(c.foldingChain()) != 0 {
		t.Fatal("the canceled fold is still published as folding")
	}
	var abandoned []store_sqlite.ViewGeneration
	for _, row := range chainDirtyGenerations(t, f) {
		if row.LayerID == dirtyLayerID(f.checkoutID) && row.State != store_sqlite.ViewGenerationReady &&
			row.State != store_sqlite.ViewGenerationSuperseded {
			abandoned = append(abandoned, row)
		}
	}
	if len(abandoned) != 1 || abandoned[0].State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("the canceled fold left %+v, want one failed generation", abandoned)
	}
	if f.store.PayloadBuildFlightActive(abandoned[0].GenerationID) {
		t.Fatal("the canceled fold still owns its target")
	}
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now().Add(2*time.Minute))
	lifecycle.leases = f.leases
	owed, err := lifecycle.discoverDeferredRetirements(context.Background(), nil, nil)
	if err != nil || !slices.Contains(owed, abandoned[0].GenerationID) {
		t.Fatalf("the sweep does not owe the canceled fold's target %d: %v %v", abandoned[0].GenerationID, owed, err)
	}
	// The crash shape: a fold that stepped and was never published or marked
	// (the process ended) leaves its target building and unowned.
	folded := c.dirtyChainMembers(context.Background(), trigger.DirtyGenerationID)
	slices.Reverse(folded)
	head, _ := f.generation(trigger.DirtyGenerationID)
	to, _, err := f.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: head.OwnerKind, GraphID: head.GraphID, LayerID: head.LayerID, CheckoutID: head.CheckoutID,
		GenerationKind: head.GenerationKind, BaseGenerationID: trigger.CommitGenerationID, TreeOID: head.TreeOID,
		LowerViewFingerprint: "crash", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fold, err := storeChainFoldBackend{store: f.store}.BeginChainFold(context.Background(), folded, to, "crash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fold.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = fold.Release(context.Background()) // what the process's end releases
	owed, err = lifecycle.discoverDeferredRetirements(context.Background(), nil, nil)
	if err != nil || !slices.Contains(owed, to) {
		t.Fatalf("the recovery does not sweep the crashed fold's building target %d: %v %v", to, owed, err)
	}
	// The next fold of the same chain.
	c.compaction.stepHook = nil
	if again := c.compactDirtyChain(context.Background(), trigger); again.Outcome != dirtyChainCompactionFlipped || again.Landing != foldLandFlip {
		t.Fatalf("the next fold: %s landing %q err %v", again.Outcome, again.Landing, again.Err)
	}
	chainAssertFlat(t, f, "stepped-fold-after-cancel")
}

// 5. A fold over a large chain keeps its own WAL mark (1 GiB), which does not
// follow the reclaim threshold: with the threshold scaled to 1 MiB (override,
// named), the sweep's and the pressure mark shrink but the fold steps on, is
// refused nothing, commits no step over its mark, and lands. The fold's
// refusal over its own mark is the store's test
// (TestChainFoldStepRespectsItsWALMark). Scale: 4 edits of 50 files each (the
// accumulated fixture's 200 units, one package per file).
func TestSteppedFoldOverALargeChainObeysTheWALMark(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "1")
	const perEdit = accumulatedDirtyUnits / 4
	f, c, l := mcpChainFixture(t, accumulatedDirtyTree(accumulatedDirtyIndependent), false)
	var trigger CheckoutCycle
	for e := 0; e < 4; e++ {
		trigger = mcpEdit(t, l, f, func() {
			for u := e * perEdit; u < (e+1)*perEdit; u++ {
				accumulatedDirtyWriteUnit(t, f.worktree, accumulatedDirtyIndependent, u, true, false)
			}
		})
	}
	if trigger.DirtyChainDepth != 4 {
		t.Fatalf("depth %d after four edits (%s)", trigger.DirtyChainDepth, trigger.DirtyChainReason)
	}
	const mark = 1 << 30
	probe := &walMarkProbe{inner: storeChainFoldBackend{store: f.store}, store: f.store, mark: mark}
	c.compaction.backend = probe
	report := c.compactDirtyChain(context.Background(), trigger)
	t.Logf("fold over 4x%d files: %s landing %q, %d steps, %d refused over the mark, worst log at a committed step %d bytes",
		perEdit, report.Outcome, report.Landing, probe.steps, probe.refused, probe.worstCommitted)
	if report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the fold did not land: %s %v", report.Outcome, report.Err)
	}
	if probe.overCommitted > 0 {
		t.Fatalf("%d steps committed with the log over the mark", probe.overCommitted)
	}
	if probe.refused > 0 {
		t.Fatalf("%d steps refused over a mark with the log under the fold's own mark", probe.refused)
	}
	chainAssertFlat(t, f, "stepped-fold-wal-mark")
}

// walMarkProbe records the log size at the start of each step of the real
// fold, and whether the step committed or was refused over the mark.
type walMarkProbe struct {
	inner chainFoldBackend
	store *store_sqlite.Store
	mark  int64

	steps, refused, overCommitted int
	worstCommitted                int64
}

func (p *walMarkProbe) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	fold, err := p.inner.BeginChainFold(ctx, chain, to, owner)
	if err != nil {
		return nil, err
	}
	return &walMarkProbeFold{chainFoldSteps: fold, p: p}, nil
}

func (p *walMarkProbe) RebaseViewGeneration(ctx context.Context, generationID, fromBase, toBase int64) error {
	return p.inner.RebaseViewGeneration(ctx, generationID, fromBase, toBase)
}

func (p *walMarkProbe) StepRetryable(err error) bool { return p.inner.StepRetryable(err) }

type walMarkProbeFold struct {
	chainFoldSteps
	p *walMarkProbe
}

func (f *walMarkProbeFold) Step(ctx context.Context) (bool, error) {
	mark := f.p.store.WALWriteMark()
	logBytes := int64(0)
	if mark.Valid {
		logBytes = int64(mark.MxFrame) * int64(mark.PageSize+24)
	}
	done, err := f.chainFoldSteps.Step(ctx)
	switch {
	case errors.Is(err, store_sqlite.ErrChainFoldWALMark):
		f.p.refused++
	case err == nil:
		f.p.steps++
		f.p.worstCommitted = max(f.p.worstCommitted, logBytes)
		if logBytes > f.p.mark {
			f.p.overCommitted++
		}
	}
	return done, err
}

// 4. The per-stack key holds across a real stepped fold, its landing by
// re-base (the store's RebaseViewGeneration) and a flip to the fold: every
// edit keys its caches as the edits before the fold did, and the edit over
// the re-based chain serves rows they kept. The fold is the store's
// ChainFold stepped to its end, with the chain's manifest, published.
func TestSteppedFoldKeepsTheStackKeyAcrossItsLanding(t *testing.T) {
	resetChainOverlayCaches()
	t.Cleanup(resetChainOverlayCaches)
	store := builderOpenStore(t, "stepped-fold-key")
	repoDir := accumulatedDirtyRepo(t, accumulatedDirtyIndependent, store)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	ctx := context.Background()
	const commit = int64(1) << 40
	keyedBuild := func(label string) *EditDeltaReport {
		t.Helper()
		req := h.request()
		req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{commit}}
		if len(h.chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, h.chain)
			if why != "" {
				t.Fatalf("%s: the chain's manifest: %s", label, why)
			}
			parent := h.chain[len(h.chain)-1]
			req.Base = commitLayerBase{Reader: dirtyChainComposed(t, store, h.chain), corpus: store,
				stack: append([]int64{commit}, h.chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(h.chain)
		}
		recordLastEditDelta(nil)
		id, report, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(h.chain) > 0 && report.ParentGenerationID != h.chain[len(h.chain)-1] {
			t.Fatalf("%s: built direct (%q), want over the chain", label, report.ChainFallbackReason)
		}
		delta := LastEditDeltaReport()
		if delta == nil || delta.StackCacheKey == "" {
			t.Fatalf("%s: no keyed edit delta", label)
		}
		h.chain = append(h.chain, id)
		return delta
	}
	edit := func(unit int) { accumulatedDirtyWriteUnit(t, repoDir, accumulatedDirtyIndependent, unit, true, false) }
	parity := func(label string) {
		t.Helper()
		if result := assertCleanIndexParityChain(t, store, h.chain, repoDir, label, false); !result.NodesEqual || !result.EdgesEqual {
			t.Fatalf("%s: the chain does not compose to a clean index: %v", label, result.Diffs)
		}
	}

	edit(0)
	key := keyedBuild("c1").StackCacheKey
	edit(1)
	keyedBuild("c2")
	edit(2)
	keyedBuild("c3")
	lower := slices.Clone(h.chain)

	// The real fold of c1..c3, begun before the edits above it.
	top, _, err := store.Catalog().GetViewGeneration(ctx, lower[len(lower)-1])
	if err != nil {
		t.Fatal(err)
	}
	to, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: top.OwnerKind, GraphID: top.GraphID, LayerID: top.LayerID, CheckoutID: top.CheckoutID,
		GenerationKind: top.GenerationKind, TreeOID: top.TreeOID, LowerViewFingerprint: top.LowerViewFingerprint,
		ConfigHash: top.ConfigHash, ExtractorVersions: top.ExtractorVersions, ResolverVersion: top.ResolverVersion,
		DependencyRevision: top.DependencyRevision, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := storeChainFoldBackend{store: store}
	fold, err := backend.BeginChainFold(ctx, lower, to, "test")
	if err != nil {
		t.Fatal(err)
	}
	// u1, u2 above the lower chain while the fold runs.
	edit(3)
	if u1 := keyedBuild("u1"); u1.StackCacheKey != key {
		t.Fatal("u1 moved the key")
	}
	edit(4)
	if u2 := keyedBuild("u2"); u2.StackCacheKey != key {
		t.Fatal("u2 moved the key")
	}
	if _, _, err := runChainFoldSteps(ctx, fold, backend.StepRetryable, nil); err != nil {
		t.Fatalf("the fold's steps: %v", err)
	}
	manifest, why := loadDirtyChainManifest(ctx, store, lower)
	if why != "" {
		t.Fatalf("the lower chain's manifest: %s", why)
	}
	entries := make([]store_sqlite.InputManifestEntry, 0, len(manifest.entries))
	for _, e := range manifest.entries {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b store_sqlite.InputManifestEntry) int { return strings.Compare(a.FilePath, b.FilePath) })
	if err := handle.WriteInputManifest(ctx, store_sqlite.InputManifestMeta{
		ManifestVersion: store_sqlite.InputManifestVersion, IsFull: true, EntryCount: len(entries), PolicyDigest: manifest.policy,
	}, entries); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPayloadGeneration(ctx, to, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := fold.Release(ctx); err != nil {
		t.Fatal(err)
	}
	parity("before-landing")

	// The landing: the store re-bases u1 from c3 onto the fold.
	u1 := h.chain[len(lower)]
	if err := backend.RebaseViewGeneration(ctx, u1, lower[len(lower)-1], to); err != nil {
		t.Fatalf("rebase u1: %v", err)
	}
	if row, _, _ := store.Catalog().GetViewGeneration(ctx, u1); row.BaseGenerationID != to {
		t.Fatalf("u1 stands on %d after the re-base, want the fold %d", row.BaseGenerationID, to)
	}
	h.chain = append([]int64{to}, h.chain[len(lower):]...)
	parity("rebased")
	edit(5)
	hitsBefore := chainOverlayCacheHits(editDeltaBaseCache(key))
	if over := keyedBuild("over-the-rebased-chain"); over.StackCacheKey != key {
		t.Fatal("the edit over the re-based chain moved the key")
	}
	if hits := chainOverlayCacheHits(editDeltaBaseCache(key)) - hitsBefore; hits == 0 {
		t.Fatal("the edit over the re-based chain served nothing the edits before the landing kept")
	}
	parity("after-landing")

	// The flip: the next edit stands on the fold alone.
	h.chain = []int64{to}
	edit(6)
	if flipped := keyedBuild("over-the-flipped-fold"); flipped.StackCacheKey != key {
		t.Fatal("the edit over the flipped fold moved the key")
	}
}

// mcpChainFixture is a running chained coordinator over tree, with the
// lifecycle an MCP edit goes through (BeginCheckoutMutation). Its loop is
// parked (every cycle is driven by an edit or by hand: overrides Debounce and
// debounceDemand); the compactor keeps the daemon's settings. With background
// off (an override: compaction closed), the
// compactions an edit schedules are refused, so a test runs its folds by hand.
func mcpChainFixture(t *testing.T, tree map[string]string, background bool) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixtureWithTree(t, tree)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	if !background {
		c.compaction.mu.Lock()
		c.compaction.closed = true
		c.compaction.mu.Unlock()
	}
	if out := c.reconcile(context.Background()); out.Err != nil {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	return f, c, l
}

// mcpEdit is one edit the way an agent's MCP edit makes it: a checkout
// mutation lease (which withdraws the routed working-tree top), the write,
// and the lease's synchronous republish.
func mcpEdit(t *testing.T, l *CheckoutLifecycle, f *coordinatorFixture, write func()) CheckoutCycle {
	t.Helper()
	ctx := context.Background()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer m.Close()
	if err := m.Prepare(ctx); err != nil {
		t.Fatalf("prepare the edit: %v", err)
	}
	write()
	out, err := m.Refresh(ctx)
	if errors.Is(err, ErrCheckoutMutationPending) {
		// A large edit is built in batches: the lease published the first,
		// the checkout's next cycles build the rest.
		c := l.coordinators[f.checkoutID]
		for i := 0; i < 1000 && (out.Rescheduled || out.DirtyBatchRemaining > 0); i++ {
			out = c.reconcile(ctx)
			err = out.Err
		}
	}
	if err != nil {
		t.Fatalf("republish the edit: %v (%+v)", err, out)
	}
	return out
}

// A fold scheduled by an edit at the soft depth survives the next edit's
// republish and the checkout's next cycle: it steps to its end and lands
// (a re-base under the edit published above it).
func TestScheduledSteppedFoldSurvivesTheNextEditAndCycle(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), true)
	begun := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	c.compaction.mu.Lock()
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step == 0 {
			once.Do(func() { close(begun) })
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	if !trigger.CompactionScheduled {
		t.Fatalf("the fourth edit scheduled no fold: %+v", trigger)
	}
	select {
	case <-begun:
	case <-time.After(30 * time.Second):
		t.Fatal("the scheduled fold did not begin")
	}
	above := mcpEdit(t, l, f, func() { foldWiringEdits[4](t, f) })
	c.cycle(context.Background()) // the checkout's own next cycle
	close(gate)
	if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := c.DirtyChainCompactionStats()
	if stats.Canceled != 0 || stats.Flipped == 0 {
		t.Fatalf("the scheduled fold: %+v, want it landed and never canceled", stats)
	}
	if row, _ := f.generation(above.DirtyGenerationID); row.BaseGenerationID == trigger.DirtyGenerationID {
		t.Fatalf("the edit above the fold still stands on %d: the fold did not re-base it", row.BaseGenerationID)
	}
	chainAssertFlat(t, f, "scheduled-fold-survives")
}

// An MCP edit at the chain bound withdraws the routed top, so its preferred
// parent is the chain at the bound: that chain is folded and the edit chains
// on the fold, instead of building its whole dirty set direct.
func TestMCPEditAtTheBoundFoldsThePreferredChain(t *testing.T) {
	f, _, l := mcpChainFixture(t, builderTreeA(), false)
	var out CheckoutCycle
	for i := 0; i < maxDirtyChainDepth; i++ {
		out = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	if out.DirtyChainDepth != maxDirtyChainDepth {
		t.Fatalf("depth %d after %d edits (%s)", out.DirtyChainDepth, maxDirtyChainDepth, out.DirtyChainReason)
	}
	at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxDirtyChainDepth) })
	if at.DirtyParentGenerationID == 0 || !at.DirtyParentPreferred || at.DirtyChainDepth != 2 {
		t.Fatalf("the edit at the bound: parent %d preferred %t depth %d reason %q, want a child of the fold at depth 2",
			at.DirtyParentGenerationID, at.DirtyParentPreferred, at.DirtyChainDepth, at.DirtyChainReason)
	}
	// The fold was verified against the chain it replaced before it was
	// published (verifyFlattenedChain). A clean-index comparison is not made
	// here: on this fixture a chained edit at depth 6 or more already differs
	// from a clean index without any fold (clone signatures and two edges;
	// reported to the delta core's owner).
}
