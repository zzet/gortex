package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// contentProofFixture is the production shape the content-pinned prepublish
// fence is tested in: a real linked worktree and SQLite store, the daemon's
// contract-core runtime on the builder, an open build gate, a live loop, and
// a published working-tree generation (the parent) the next delta chains on.
// saver lands the saves the tests time against the per-file pass.
type contentProofFixture struct {
	f         *coordinatorFixture
	c         *CheckoutCoordinator
	saver     *savingExtractor
	cycles    *motionFixtureCycles
	logs      *observer.ObservedLogs
	lifecycle *CheckoutLifecycle
	parent    int64
	// mark is the number of cycles recorded before the test's edit.
	mark int
}

func startContentProofCoordinator(t *testing.T, saver func(worktree string, extractor *savingExtractor), onCycle func(CheckoutCycle)) *contentProofFixture {
	t.Helper()
	f := newCoordinatorFixture(t)
	registry := builderRegistry()
	goExtractor, ok := registry.GetByLanguage("go")
	if !ok {
		t.Fatal("no Go extractor registered")
	}
	extractor := &savingExtractor{Extractor: goExtractor, path: filepath.Join(f.worktree, "helper.go")}
	saver(f.worktree, extractor)
	registry.Register(extractor)
	core, logs := observer.New(zap.InfoLevel)
	builder := builderNewBuilder(f.store)
	builder.Registry = registry
	builder.Logger = zap.New(core)
	// The daemon installs the contract-core runtime on every builder.
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	gate := NewViewBuildGate()
	gate.Open()
	cycles := &motionFixtureCycles{}
	record := cycles.record
	if onCycle != nil {
		record = func(out CheckoutCycle) {
			onCycle(out)
			cycles.record(out)
		}
	}
	c := f.coordinator(t, CheckoutCoordinatorConfig{Builder: builder, Gate: gate, PollInterval: -1, cycleDone: record})
	x := &contentProofFixture{f: f, c: c, saver: extractor, cycles: cycles, logs: logs,
		lifecycle: &CheckoutLifecycle{catalog: f.catalog, store: f.store, leases: f.leases,
			coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}}
	c.Signal("initial")
	cycles.published(t, f, c, time.Time{}, 60*time.Second)

	// The parent: a published working-tree generation holding the files the
	// test edits. caller.go keeps the next delta smaller than the dirty set,
	// so it chains.
	saved := time.Now()
	builderWriteFile(t, f.worktree, "caller.go", "package fixture\n\nfunc Run() {\n\tCompute(Options{})\n\t_ = 1\n}\n")
	builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tHelper()\n\tIsland()\n}\n")
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 1\n}\n")
	c.Signal("save")
	cycles.published(t, f, c, saved, 60*time.Second)
	if x.parent = f.route().DirtyGenerationID; x.parent <= 0 {
		t.Fatal("the first save published no working-tree generation")
	}
	cycles.mu.Lock()
	x.mark = len(cycles.cycles)
	cycles.mu.Unlock()
	return x
}

// awaitTicket waits for a refresh ticket's answer within 10 s, twice the
// production require_fresh default wait.
func awaitTicket(t *testing.T, ticket *CheckoutRefreshTicket) MutationResult {
	t.Helper()
	select {
	case result := <-ticket.Ticket.Done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh ticket was not answered within 10s")
		return MutationResult{}
	}
}

// since returns the cycles recorded after the fixture's mark.
func (x *contentProofFixture) since() []CheckoutCycle {
	x.cycles.mu.Lock()
	defer x.cycles.mu.Unlock()
	return append([]CheckoutCycle(nil), x.cycles.cycles[x.mark:]...)
}

// firstBuilt is the first cycle after the mark that routed a working-tree
// generation it built. A cycle completes its tickets before it is recorded,
// so it waits a little for the record.
func (x *contentProofFixture) firstBuilt(t *testing.T) CheckoutCycle {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, out := range x.since() {
			if out.DirtyBuilt && out.DirtyGenerationID > 0 {
				return out
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no cycle built a working-tree generation after the edit")
			return CheckoutCycle{}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tornBuilds is the error of every working-tree build attempt logged after
// at that failed.
func (x *contentProofFixture) tornBuilds(after time.Time) []string {
	var out []string
	for _, entry := range x.logs.FilterMessage("indexer: working-tree build phases").All() {
		if !entry.Time.After(after) {
			continue
		}
		if message, _ := entry.ContextMap()["error"].(string); message != "" {
			out = append(out, message)
		}
	}
	return out
}

// node reports whether generation's own payload holds the node id.
func (x *contentProofFixture) node(generation int64, id string) bool {
	return x.f.store.AtGeneration(generation).GetNode(builderRepoPrefix+"/"+id) != nil
}

// A save of a file the per-file pass already parsed (its bytes still the
// build's sample) lands mid-build, between the read and the chunk's receipt
// check. The build is kept, not torn: the prepublish fence confirms the bytes
// it parsed against its sample and publishes the sample — the route advances
// to a generation labelled with the sample's fingerprint, describing
// helper.go as sampled — and the publication signals the next cycle itself,
// which publishes the saved state.
func TestASaveAfterTheParseIsPublishedAsTheBuildsSample(t *testing.T) {
	var (
		mu       sync.Mutex
		before   gitstate.DirtySnapshot
		beforeOK bool
		edited   time.Time
		x        *contentProofFixture
	)
	x = startContentProofCoordinator(t, func(_ string, e *savingExtractor) {
		e.trigger = "island.go"
		e.line = "\nfunc Moved() {}\n"
		e.onSave = func() {
			// The build's sample: the latest one, taken by its cycle.
			snap, _, ok := x.c.sampler.LatestSampleSince(edited)
			mu.Lock()
			before, beforeOK = snap, ok
			mu.Unlock()
		}
	}, nil)
	proven := readSetConfirmations.contentProven.Load()
	edited = time.Now()
	builderWriteFile(t, x.f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	builderWriteFile(t, x.f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
	x.saver.armed.Store(true)
	ticket, err := x.lifecycle.requestBoundCheckoutRefresh(t.Context(), x.f.checkoutID, x.f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	result := awaitTicket(t, ticket)
	if result.Err != nil {
		t.Fatalf("the refresh ticket failed: %v", result.Err)
	}
	savedAt, saveErr := x.saver.saved()
	if saveErr != nil || savedAt.IsZero() {
		t.Fatalf("the mid-pass save did not land: at=%v err=%v", savedAt, saveErr)
	}
	mu.Lock()
	sample, sampled := before, beforeOK
	mu.Unlock()
	if !sampled {
		t.Fatal("no build sample was taken before the save")
	}

	first := x.firstBuilt(t)
	if first.Err != nil || first.Rescheduled || !first.DirtyOutpaced {
		t.Fatalf("the build the save landed in = %+v, want it published as its sample", first)
	}
	if first.DirtyParentGenerationID != x.parent {
		t.Fatalf("the build stood on %d, want the chained parent %d", first.DirtyParentGenerationID, x.parent)
	}
	row, found := x.f.generation(first.DirtyGenerationID)
	if !found || row.LowerViewFingerprint != sample.Fingerprint {
		t.Fatalf("generation %d is labelled %q, want the build's sample %q", first.DirtyGenerationID, row.LowerViewFingerprint, sample.Fingerprint)
	}
	if !x.node(first.DirtyGenerationID, "helper.go::Helper") || x.node(first.DirtyGenerationID, "helper.go::Moved") {
		t.Fatal("the published generation does not describe helper.go as the build sampled it")
	}
	if result.AppliedGeneration != uint64(first.DirtyGenerationID) {
		t.Fatalf("the ticket admitted before the sample completed on %d, want the sample's publication %d", result.AppliedGeneration, first.DirtyGenerationID)
	}
	if torn := x.tornBuilds(edited); len(torn) != 0 {
		t.Fatalf("builds were torn by a save the build had parsed past: %v", torn)
	}
	if readSetConfirmations.contentProven.Load() == proven {
		t.Fatal("the fence did not confirm the build by its parsed bytes")
	}

	// No watcher, no poll and no further signal from the test: the
	// publication itself asked for the newer state.
	took := x.cycles.published(t, x.f, x.c, savedAt, 5*time.Second)
	t.Logf("the saved state was published %s after the save", took)
	if latest := x.f.route().DirtyGenerationID; latest == first.DirtyGenerationID || !x.node(latest, "helper.go::Moved") {
		t.Fatalf("the route names %d, want a later generation holding the save", latest)
	}
	for _, out := range x.since() {
		if out.Err != nil {
			t.Errorf("cycle failed: %v", out.Err)
		}
	}
}

// The build holds its own sample for its lifetime, and its cycle completes
// the refresh tickets its publication answers against that sample. Twenty
// freshness proofs of other states are taken while the build runs — more
// than the sampler's recent-sample cache holds — and then a file the build
// parsed is saved again: the fence still confirms the build by its parsed
// bytes and publishes its sample, and the ticket admitted before that sample
// is answered by that publication, not left for the next cycle.
func TestTwentyProofsDuringABuildDoNotTearItOrStrandItsTicket(t *testing.T) {
	var (
		x      *contentProofFixture
		proofs atomic.Int32
	)
	x = startContentProofCoordinator(t, func(worktree string, e *savingExtractor) {
		e.trigger = "island.go"
		e.line = "\nfunc Moved() {}\n"
		e.onSave = func() {
			for i := 1; i <= 20; i++ {
				if err := os.WriteFile(filepath.Join(worktree, "notes", "probe.txt"), fmt.Appendf(nil, "%d\n", i), 0o644); err != nil {
					t.Error(err)
					return
				}
				if _, err := x.c.sampler.Sample(context.Background()); err != nil {
					t.Error(err)
					return
				}
				proofs.Add(1)
			}
		}
	}, nil)
	// An untracked file the proofs create and rewrite after the build's
	// sample, which each proof finds changed and the build never reads. Its
	// directory exists before the edit, so the root does not move.
	if err := os.Mkdir(filepath.Join(x.f.worktree, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second / 20)
	edited := time.Now()
	builderWriteFile(t, x.f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	builderWriteFile(t, x.f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
	x.saver.armed.Store(true)
	ticket, err := x.lifecycle.requestBoundCheckoutRefresh(t.Context(), x.f.checkoutID, x.f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	result := awaitTicket(t, ticket)
	if result.Err != nil {
		t.Fatalf("the refresh ticket failed: %v", result.Err)
	}
	if got := proofs.Load(); got != 20 {
		t.Fatalf("%d proofs ran during the build, want 20", got)
	}
	if torn := x.tornBuilds(edited); len(torn) != 0 {
		t.Fatalf("builds were torn: %v", torn)
	}
	first := x.firstBuilt(t)
	if first.Err != nil || first.Rescheduled || !first.DirtyOutpaced {
		t.Fatalf("the build the proofs ran under (generation %d, outpaced %v, err %v) was not published as its sample",
			first.DirtyGenerationID, first.DirtyOutpaced, first.Err)
	}
	row, _ := x.f.generation(first.DirtyGenerationID)
	if _, _, cached := x.c.sampler.LatestSampleOf(row.LowerViewFingerprint, time.Time{}); cached {
		t.Fatal("the build's sample is still in the sampler's recent-sample cache: the test does not exercise eviction")
	}
	if result.AppliedGeneration != uint64(first.DirtyGenerationID) {
		t.Fatalf("the ticket admitted before the sample completed on %d, want the sample's publication %d", result.AppliedGeneration, first.DirtyGenerationID)
	}
}

// A save that lands on a file after the build's sample but before the pass
// reads it puts bytes other than the sample's into the payload. The pass
// refuses them at the file's read receipt (the content proof contradicts the
// sample), the attempt is torn there rather than at the fence, and the second
// attempt builds the saved state from a fresh sample (the stage-one
// behaviour).
func TestASaveBeforeTheParseTearsTheBuild(t *testing.T) {
	x := startContentProofCoordinator(t, func(_ string, e *savingExtractor) {
		e.early = "core.go"
	}, nil)
	edited := time.Now()
	builderWriteFile(t, x.f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tIsland()\n\tHelper()\n}\n")
	builderWriteFile(t, x.f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	builderWriteFile(t, x.f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
	x.saver.earlyArmed.Store(true)
	ticket, err := x.lifecycle.requestBoundCheckoutRefresh(t.Context(), x.f.checkoutID, x.f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitTicket(t, ticket); result.Err != nil {
		t.Fatalf("the refresh ticket failed: %v", result.Err)
	}
	if x.saver.earlyArmed.Load() {
		t.Fatal("the early save never landed")
	}
	torn := x.tornBuilds(edited)
	if len(torn) == 0 || !strings.Contains(torn[0], ErrDirtySnapshotChanged.Error()) ||
		!strings.Contains(torn[0], errFileVersionChanged.Error()) || !strings.Contains(torn[0], "helper.go") {
		t.Fatalf("torn attempts = %v, want the first attempt torn by the bytes the pass read for helper.go", torn)
	}
	first := x.firstBuilt(t)
	if first.Err != nil || first.DirtyOutpaced {
		t.Fatalf("the published cycle = %+v, want the second attempt's fresh sample", first)
	}
	snap, err := x.c.sampler.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := x.f.generation(x.f.route().DirtyGenerationID); row.LowerViewFingerprint != snap.Fingerprint {
		t.Fatalf("the route describes %q, want the saved state %q", row.LowerViewFingerprint, snap.Fingerprint)
	}
}

// A refresh ticket admitted after the build's sample was taken is not
// answered by the publication of that sample: it waits for the next cycle,
// which the publication signals at once, and is answered by the generation
// that holds the newer save. Until then a require_fresh read against the
// older generation is not fresh. The ticket admitted before the sample is
// answered by the sample's publication, although the late ticket's fence
// sampled the newer state after it.
func TestATicketAdmittedAfterTheSampleWaitsForTheNextPublication(t *testing.T) {
	var (
		x       *contentProofFixture
		late    atomic.Pointer[CheckoutRefreshTicket]
		lateErr atomic.Pointer[error]
		mu      sync.Mutex
		proofs  []CheckoutFreshProof
	)
	x = startContentProofCoordinator(t, func(_ string, e *savingExtractor) {
		e.trigger = "island.go"
		e.line = "\nfunc Moved() {}\n"
		e.onSave = func() {
			// The request arrives mid-build, after the build's sample.
			ticket, err := x.lifecycle.requestBoundCheckoutRefresh(context.Background(), x.f.checkoutID, x.f.worktree)
			if err != nil {
				lateErr.Store(&err)
				return
			}
			late.Store(ticket)
		}
	}, func(out CheckoutCycle) {
		if !out.DirtyOutpaced {
			return
		}
		// What a require_fresh read sees while the route names the sample.
		proof, err := x.lifecycle.ProveCheckoutFresh(context.Background(), x.f.checkoutID, x.f.worktree)
		if err != nil {
			t.Errorf("prove the route fresh: %v", err)
		}
		mu.Lock()
		proofs = append(proofs, proof)
		mu.Unlock()
	})
	builderWriteFile(t, x.f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
	builderWriteFile(t, x.f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
	x.saver.armed.Store(true)
	ticket, err := x.lifecycle.requestBoundCheckoutRefresh(t.Context(), x.f.checkoutID, x.f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	result := awaitTicket(t, ticket)
	if result.Err != nil {
		t.Fatalf("the ticket that started the build failed: %v", result.Err)
	}
	if err := lateErr.Load(); err != nil {
		t.Fatalf("admit the mid-build ticket: %v", *err)
	}
	lateTicket := late.Load()
	if lateTicket == nil {
		t.Fatal("the mid-build ticket was never admitted")
	}
	lateResult := awaitTicket(t, lateTicket)
	if lateResult.Err != nil {
		t.Fatalf("the mid-build ticket failed: %v", lateResult.Err)
	}

	first := x.firstBuilt(t)
	if !first.DirtyOutpaced {
		t.Fatalf("the build the request arrived in = %+v, want it published as its sample", first)
	}
	if lateResult.AppliedGeneration == uint64(first.DirtyGenerationID) {
		t.Fatalf("the ticket admitted after the sample was answered by that sample's publication %d", first.DirtyGenerationID)
	}
	// The late ticket's fence sampled the newer state, so the latest sample
	// no longer names the publication; the ticket admitted before the
	// build's sample is still answered by it, not left for the next cycle.
	if result.AppliedGeneration != uint64(first.DirtyGenerationID) {
		t.Fatalf("the ticket admitted before the sample was answered by %d, want the sample's publication %d", result.AppliedGeneration, first.DirtyGenerationID)
	}
	if !x.node(int64(lateResult.AppliedGeneration), "helper.go::Moved") {
		t.Fatalf("the ticket was answered by generation %d, which does not hold the save it arrived after", lateResult.AppliedGeneration)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(proofs) == 0 {
		t.Fatal("no require_fresh read was proven against the sample's publication")
	}
	for _, proof := range proofs {
		if proof.Fresh || proof.Reason != FreshProofSnapshotDiffers {
			t.Fatalf("a read against the sample's publication = %+v, want not fresh (the snapshot differs)", proof)
		}
	}
}

// streamFiles is how many files the sustained edit stream rewrites
// round-robin: the soak driver's rotating set. Each file is saved again
// every streamFiles*100 ms, about twice the longest build under -race, as
// the soak's saves come back to a file well after a build that parsed it.
const streamFiles = 20

// streamFreshWait is twice the production require_fresh default wait: the
// longest a stream's read may wait for its answer.
const streamFreshWait = 10 * time.Second

// streamTicket is one require_fresh read of the stream: when it arrived, how
// long it waited for its answer, how many tickets it took, and how it ended.
type streamTicket struct {
	admitted time.Time
	took     time.Duration
	attempts int
	err      error
}

// sustainedEditStream drives streamFiles files rewritten round-robin every
// 100 ms for 6 s, with a require_fresh read every second, on the production
// builder shape (contract-core runtime, open build gate, native watcher).
// enrich, when set, is the semantic manager the builder enriches through, as
// the daemon's does by default. It returns the cycles of the stream, when
// each was recorded, when the stream started and ended, how long after the
// last write the final state was published, and the require_fresh reads.
func sustainedEditStream(t *testing.T, enrich *semantic.Manager) (c *CheckoutCoordinator, stream []CheckoutCycle, at []time.Time, started, lastWrite time.Time, took time.Duration, tickets []streamTicket) {
	t.Helper()
	t.Setenv(checkoutWatchDisableEnv, "")
	f := newCoordinatorFixture(t)
	files := make([]string, streamFiles)
	for i := range files {
		files[i] = fmt.Sprintf("stream%02d.go", i)
		builderWriteFile(t, f.worktree, files[i], fmt.Sprintf("package fixture\n\nfunc Stream%d() {}\n", i))
	}
	builder := builderNewBuilder(f.store)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	if enrich != nil {
		builder.Semantic = enrich
	}
	gate := NewViewBuildGate()
	gate.Open()
	cycles := &motionFixtureCycles{}
	c = liveMotionCoordinator(t, f, cycles, CheckoutCoordinatorConfig{Builder: builder, Gate: gate})
	lifecycle := &CheckoutLifecycle{catalog: f.catalog, store: f.store, leases: f.leases,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	cycles.mu.Lock()
	mark := len(cycles.cycles)
	cycles.mu.Unlock()

	var (
		wg    sync.WaitGroup
		amu   sync.Mutex
		asked []streamTicket
	)
	// ask is one require_fresh read the way the MCP waiter makes it: a
	// freshness proof against a sample begun after the request arrived; when
	// the route is not current, a ticket admitted against the proof's sample,
	// and, whenever one is superseded (the tree moved under the snapshot it
	// was pinned to), a ticket bound at completion to any sample begun after
	// the arrival (RequestBoundCheckoutRefresh), until twice the production
	// wait.
	ask := func() {
		defer wg.Done()
		arrived := time.Now()
		deadline := arrived.Add(streamFreshWait)
		answer := func(attempts int, err error) {
			amu.Lock()
			asked = append(asked, streamTicket{admitted: arrived, took: time.Since(arrived), attempts: attempts, err: err})
			amu.Unlock()
		}
		ctx := WithFreshRequestArrival(context.Background(), arrived)
		proof, err := lifecycle.ProveCheckoutFresh(ctx, f.checkoutID, f.worktree)
		if err != nil || proof.Fresh {
			answer(0, err)
			return
		}
		for attempt := 1; ; attempt++ {
			var ticket *CheckoutRefreshTicket
			if attempt == 1 {
				ticket, err = lifecycle.RequestCheckoutRefreshAfterProof(ctx, f.checkoutID, f.worktree, proof)
			} else {
				ticket, err = lifecycle.RequestBoundCheckoutRefresh(ctx, f.checkoutID, f.worktree)
			}
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
				answer(attempt, err)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	started = time.Now()
	issued := 0
	for i := 0; time.Since(started) < 6*time.Second; i++ {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		// An in-place editor save, as the soak's driver makes: one appended
		// declaration per write.
		if err := appendToFile(filepath.Join(f.worktree, files[i%len(files)]), fmt.Sprintf("\nfunc Edit%d() {}\n", i)); err != nil {
			t.Fatal(err)
		}
		lastWrite = time.Now()
		if i%10 == 0 {
			issued++
			wg.Add(1)
			go ask()
		}
	}
	took = cycles.published(t, f, c, lastWrite, 30*time.Second)
	wg.Wait()

	cycles.mu.Lock()
	stream = append([]CheckoutCycle(nil), cycles.cycles[mark:]...)
	at = append([]time.Time(nil), cycles.at[mark:]...)
	cycles.mu.Unlock()
	amu.Lock()
	defer amu.Unlock()
	if len(asked) != issued || issued < 5 {
		t.Fatalf("%d of %d require_fresh reads answered, want one per second of the stream", len(asked), issued)
	}
	return c, stream, at, started, lastWrite, took, append([]streamTicket(nil), asked...)
}

// streamLiveness checks a stream's cycles: none failed, the route advanced at
// least three times after the stream's first publication and before its last
// write, and the final state was published within twice the quiet window plus
// the longest build after the last write. It checks the stream's
// require_fresh reads: each is answered without error within streamFreshWait,
// and one that arrived at least that settle bound before the last write is
// answered before the last write — by a publication during the stream, not
// by the final one. It returns the longest build.
func streamLiveness(t *testing.T, c *CheckoutCoordinator, stream []CheckoutCycle, at []time.Time, started, lastWrite time.Time, took time.Duration, tickets []streamTicket) time.Duration {
	t.Helper()
	published, outpaced, failed := 0, 0, 0
	var longest time.Duration
	for i, out := range stream {
		t.Logf("cycle +%s: built=%v reused=%v outpaced=%v rescheduled=%v by=%q held=%v yielded=%q generation=%d took=%s err=%v",
			at[i].Sub(started).Round(time.Millisecond), out.DirtyBuilt, out.DirtyReused, out.DirtyOutpaced, out.Rescheduled, out.rescheduledBy,
			out.Held, out.YieldedTo, out.DirtyGenerationID, at[i].Sub(out.cycleStarted).Round(time.Millisecond), out.Err)
		if out.Err != nil {
			failed++
			t.Errorf("cycle failed: %v", out.Err)
		}
		if !out.DirtyBuilt {
			continue
		}
		if !out.cycleStarted.IsZero() && at[i].Sub(out.cycleStarted) > longest {
			longest = at[i].Sub(out.cycleStarted)
		}
		if at[i].Before(lastWrite) {
			published++
		}
		if out.DirtyOutpaced {
			outpaced++
		}
	}
	// The stream's first publication is the build of its first save or two,
	// begun before the stream moved anything under it; liveness is what
	// follows it.
	during := max(published-1, 0)
	t.Logf("%d cycles over the stream: %d published before the last write, %d after the first (%d as an outpaced sample), longest build %s; final state published %s after the last write",
		len(stream), published, during, outpaced, longest, took)
	if failed != 0 {
		t.Fatalf("%d cycles failed under the stream", failed)
	}
	settle := 2 * (c.quiet + longest)
	early, starved := 0, 0
	for _, a := range tickets {
		arrived := a.admitted.Sub(started).Round(time.Millisecond)
		t.Logf("require_fresh read at +%s: answered after %s, %d tickets, err=%v", arrived, a.took.Round(time.Millisecond), a.attempts, a.err)
		if a.err != nil || a.took > streamFreshWait {
			starved++
			t.Errorf("the require_fresh read at +%s ended after %s with %v, want answered within %s", arrived, a.took, a.err, streamFreshWait)
			continue
		}
		// The first ticket is pinned to the proof's sample and a newer
		// publication may supersede it; the re-admission is bound at
		// completion, which no publication supersedes.
		if a.attempts > 2 {
			starved++
			t.Errorf("the require_fresh read at +%s took %d tickets, want at most 2 (one pinned, one bound at completion)", arrived, a.attempts)
			continue
		}
		if a.admitted.Before(lastWrite.Add(-settle)) {
			early++
			if !a.admitted.Add(a.took).Before(lastWrite) {
				starved++
				t.Errorf("the require_fresh read at +%s arrived more than %s before the last write but was answered only %s after it",
					arrived, settle, a.admitted.Add(a.took).Sub(lastWrite).Round(time.Millisecond))
			}
		}
	}
	if starved != 0 {
		t.Fatalf("%d of %d require_fresh reads were starved by the stream", starved, len(tickets))
	}
	if early == 0 {
		t.Fatalf("no require_fresh read arrived %s before the last write: the stream cannot show one answered during it", settle)
	}
	if during < 3 {
		t.Fatalf("%d publications while the stream ran after its first, want at least 3", during)
	}
	if bound := 2 * (c.quiet + longest); took > bound {
		t.Fatalf("the final state was published %s after the last write, want within %s", took, bound)
	}
	return longest
}

// A sustained edit stream — twenty files rewritten round-robin every 100 ms
// for 6 s, a require_fresh read every second, on the production builder shape
// with the native watcher — keeps publishing while the edits continue: no
// cycle fails, the route advances at least three times after the stream's
// first publication and before its last write, every read is answered
// within twice the require_fresh default wait (and, when it arrives early
// enough, while the stream still runs), and the final state is published
// within twice the quiet window plus the longest build after the last write.
func TestASustainedEditStreamKeepsPublishing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a native file watcher")
	}
	c, stream, at, started, lastWrite, took, tickets := sustainedEditStream(t, nil)
	streamLiveness(t, c, stream, at, started, lastWrite, took, tickets)
}

// slowEnrichProvider is the enrichment stand-in for the daemon's go/types
// stage: each pass takes as long as a small package's type check.
type slowEnrichProvider struct {
	*checkoutEnrichSpy
	took time.Duration
}

func (p *slowEnrichProvider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	time.Sleep(p.took)
	return p.checkoutEnrichSpy.Enrich(g, repoRoot)
}

// The same stream with an enrichment stage in every build, the daemon's
// default. The stage reads the working copy beyond what the content proof
// records, so the stream tears the enriched builds; the checkout then defers
// its enrichment and keeps publishing as without the stage, owing
// graph.semantic. Once the tree and the checkout are quiet, the follow-up
// enriches the owed paths, nothing is owed any more, and the deferral ends.
func TestASustainedEditStreamWithEnrichmentKeepsPublishing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a native file watcher")
	}
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	provider := &slowEnrichProvider{checkoutEnrichSpy: &checkoutEnrichSpy{}, took: 300 * time.Millisecond}
	mgr := semantic.NewManager(semantic.Config{
		Enabled:                  true,
		CheckoutLSPMaxWorkspaces: 4,
		Providers:                []semantic.ProviderConfig{{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true}},
	}, zap.NewNop())
	mgr.RegisterProvider(provider)
	t.Cleanup(func() { _ = mgr.Close() })
	c, stream, at, started, lastWrite, took, tickets := sustainedEditStream(t, mgr)
	longest := streamLiveness(t, c, stream, at, started, lastWrite, took, tickets)

	// The follow-up waits for the checkout and the tree to be quiet for its
	// window, then builds once; twice that is the bound.
	bound := 2 * (enrichmentFollowupQuiet + longest)
	deadline := time.Now().Add(bound)
	for {
		pending, err := c.SemanticPending(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(pending.Paths) == 0 && !c.enrichmentDeferredByMotion() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d paths still owed (deferred=%v) %s after the stream's final publication; follow-ups %+v",
				len(pending.Paths), c.enrichmentDeferredByMotion(), bound, c.EnrichmentFollowupStats())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stats := c.EnrichmentFollowupStats(); stats.Landed == 0 {
		t.Fatalf("nothing owed but no follow-up landed: %+v", stats)
	}
	t.Logf("follow-ups %+v; %d enrichment passes", c.EnrichmentFollowupStats(), len(provider.enriched()))
}

// A deletion, a rename or a HEAD move during a build is still refused by the
// fence: the parsed bytes prove nothing about a path that is gone or a HEAD
// that moved, so the payload of the state the build sampled is never
// published once the tree has left it that way.
func TestADeletionRenameOrHeadMoveDuringABuildStillTears(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(t *testing.T, root string)
	}{
		{"a parsed file deleted", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "island.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a parsed file renamed", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "helper.go"), filepath.Join(root, "renamed.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"HEAD moved", func(t *testing.T, root string) {
			builderGit(t, root, "add", "-A")
			builderGit(t, root, "commit", "-m", "moved under the build")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			builder := builderNewBuilder(f.store)
			builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
			var (
				armed   atomic.Bool
				sampled atomic.Pointer[gitstate.DirtySnapshot]
				c       *CheckoutCoordinator
				edited  time.Time
			)
			c = f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder, dirtyBarrier: func() {
				if !armed.CompareAndSwap(true, false) {
					return
				}
				// The first attempt's sample, then the move after its reads.
				if snap, _, ok := c.sampler.LatestSampleSince(edited); ok {
					sampled.Store(&snap)
				}
				tc.move(t, f.worktree)
			}})
			coordinatorReconcile(t, c)
			edited = time.Now()
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
			builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
			armed.Store(true)
			out := c.reconcile(context.Background())
			if armed.Load() {
				t.Fatal("the move never landed")
			}
			before := sampled.Load()
			if before == nil {
				t.Fatal("no build sample was taken before the move")
			}
			if out.DirtyOutpaced {
				t.Fatalf("cycle = %+v: the sample the move left was published", out)
			}
			if route := f.route(); route.DirtyGenerationID > 0 {
				if row, _ := f.generation(route.DirtyGenerationID); row.LowerViewFingerprint == before.Fingerprint {
					t.Fatalf("the route names generation %d of the sample the move left", route.DirtyGenerationID)
				}
			}
			t.Logf("cycle after the move: rescheduled=%v built=%v err=%v", out.Rescheduled, out.DirtyBuilt, out.Err)
		})
	}
}

// savingEnrichProvider is the enrichment stand-in that, armed, saves a file
// the build parsed while its pass runs: the stage reads the working copy after
// the parse.
type savingEnrichProvider struct {
	*checkoutEnrichSpy
	path  string
	armed atomic.Bool
}

func (p *savingEnrichProvider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	if p.armed.CompareAndSwap(true, false) {
		if err := appendToFile(p.path, "\nfunc Enriched() {}\n"); err != nil {
			return nil, err
		}
	}
	return p.checkoutEnrichSpy.Enrich(g, repoRoot)
}

// The enrichment stage loads the changed files' packages from disk after the
// per-file pass parsed them, beyond the reads the content proof records, so
// the fence confirms an enriched build by change stamps alone: a save of a
// file the build read, while the stage runs or after it (here, at the
// prepublish barrier), tears the build. The tear defers the checkout's
// enrichment: the second attempt builds the saved state without the stage,
// owing graph.semantic, and is published. The follow-up then enriches the
// owed paths and ends the deferral, and the next edit enriches in its own
// build again.
func TestASaveAroundTheEnrichmentStageTearsTheBuildAndDefersEnrichment(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	for _, tc := range []struct {
		name     string
		duringIt bool
	}{{"saved while the stage runs", true}, {"saved after the stage", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			provider := &savingEnrichProvider{checkoutEnrichSpy: &checkoutEnrichSpy{}, path: filepath.Join(f.worktree, "helper.go")}
			mgr := semantic.NewManager(semantic.Config{
				Enabled:                  true,
				CheckoutLSPMaxWorkspaces: 4,
				Providers:                []semantic.ProviderConfig{{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true}},
			}, zap.NewNop())
			mgr.RegisterProvider(provider)
			t.Cleanup(func() { _ = mgr.Close() })
			builder := builderNewBuilder(f.store)
			builder.Semantic = mgr
			var barrierArmed atomic.Bool
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder, dirtyBarrier: func() {
				if barrierArmed.CompareAndSwap(true, false) {
					if err := appendToFile(provider.path, "\nfunc Enriched() {}\n"); err != nil {
						t.Error(err)
					}
				}
			}})
			c.followup.quiet = -1
			coordinatorReconcile(t, c)
			passes := len(provider.enriched())
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 2\n}\n")
			if tc.duringIt {
				provider.armed.Store(true)
			} else {
				barrierArmed.Store(true)
			}
			out := coordinatorReconcile(t, c)
			if provider.armed.Load() || barrierArmed.Load() {
				t.Fatal("the save never landed")
			}
			snap, err := c.sampler.Sample(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			row, _ := f.generation(out.DirtyGenerationID)
			// Torn after its one pass, then the saved state built without one.
			if ran := len(provider.enriched()) - passes; out.DirtyOutpaced || row.LowerViewFingerprint != snap.Fingerprint || ran != 1 {
				t.Fatalf("cycle = %+v (fingerprint %q, now %q, %d passes): want the enriched build torn and the saved state built without the stage",
					out, row.LowerViewFingerprint, snap.Fingerprint, ran)
			}
			if !c.enrichmentDeferredByMotion() {
				t.Fatal("the tear did not defer the checkout's enrichment")
			}
			pending, err := c.SemanticPending(t.Context())
			if err != nil || len(pending.Paths) == 0 {
				t.Fatalf("nothing owed after a build without the stage (err=%v)", err)
			}

			if outcome := c.runEnrichmentFollowup(t.Context()); outcome != followupLanded {
				t.Fatalf("follow-up %s, want landed", outcome)
			}
			if pending, err := c.SemanticPending(t.Context()); err != nil || len(pending.Paths) > 0 || c.enrichmentDeferredByMotion() {
				t.Fatalf("after the follow-up: owed %v (err=%v), deferred=%v; want nothing owed and the deferral ended",
					pending.Paths, err, c.enrichmentDeferredByMotion())
			}
			passes = len(provider.enriched())
			builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t_ = 2\n}\n")
			if out := coordinatorReconcile(t, c); !out.DirtyBuilt || len(provider.enriched()) != passes+1 {
				t.Fatalf("the next edit = %+v with %d passes, want it enriched in its own build", out, len(provider.enriched())-passes)
			}
		})
	}
}

// A working-copy sample the tree moved under says nothing about a refresh
// ticket. A capture that hits one is superseded, so the require_fresh waiter
// asks again against the newer tree instead of ending the wait as an
// unavailable coordinator; a completion that hits one leaves the owed tickets
// waiting for the next cycle instead of failing them as a publication error.
func TestARefreshSampleTheTreeMovedUnderSupersedesOrDefersTheTicket(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	moved := fmt.Errorf("gitstate: fingerprint dirty content in %s: %w: %w", f.worktree, gitstate.ErrDirtyUnavailable,
		fmt.Errorf("dirty path %q changed while sampling: %w", "helper.go", gitstate.ErrDirtyMoved))
	var torn atomic.Int32
	c.refreshSample = func(ctx context.Context, since time.Time) (gitstate.DirtySnapshot, time.Time, error) {
		if torn.Add(-1) >= 0 {
			return gitstate.DirtySnapshot{}, time.Time{}, moved
		}
		return c.sampler.SampleSinceStarted(ctx, since)
	}

	torn.Store(1)
	if _, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("a capture the tree moved under = %v, want superseded", err)
	}

	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc MovedUnderTheTicket() {}\n")
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestCheckoutRefresh: %v", err)
	}
	torn.Store(1)
	c.cycle(t.Context())
	select {
	case result := <-ticket.Ticket.Done:
		t.Fatalf("a ticket whose completion sample the tree moved under was finished: %+v", result)
	default:
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket left waiting was not completed by the next cycle: %+v", result)
	}
}

// A ticket bound at completion for a require_fresh request is decided by any
// sample begun at or after the request arrived (WithFreshRequestArrival), the
// instant its promise is about — not only by one begun after its admission,
// which a build already in flight could never supply. Without an arrival its
// admission is that instant.
func TestABoundRefreshTicketIsDecidedByAnySampleSinceTheRequestArrived(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	admitted := func(ticket *CheckoutRefreshTicket) *checkoutRefreshRequest {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		return c.refreshWaiters[ticket.Ticket.Generation]
	}
	arrived := time.Now().Add(-time.Second)
	fresh, err := l.RequestBoundCheckoutRefresh(WithFreshRequestArrival(t.Context(), arrived), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestBoundCheckoutRefresh: %v", err)
	}
	if request := admitted(fresh); request == nil || !request.bindAtCompletion || !request.freshAfter.Equal(arrived) {
		t.Fatalf("the ticket admitted for a request that arrived at %s = %+v, want bound at completion from the arrival", arrived, request)
	}
	plain, err := l.RequestBoundCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestBoundCheckoutRefresh: %v", err)
	}
	if request := admitted(plain); request == nil || !request.freshAfter.Equal(request.admittedAt) {
		t.Fatalf("the ticket admitted with no arrival = %+v, want decided from its admission", request)
	}
	c.cycle(t.Context())
	for _, ticket := range []*CheckoutRefreshTicket{fresh, plain} {
		if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
			t.Fatalf("a bound ticket was not completed by the next cycle: %+v", result)
		}
	}
}

// A ticket's cycle whose own working-copy sample the tree moved under ends
// rescheduled, not failed: the sample answers nothing, so the owed ticket
// keeps waiting (a publication error would end the require_fresh wait), and
// the next cycle completes it.
func TestATicketCycleWhoseSampleTheTreeMovedUnderLeavesTheTicketWaiting(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	moved := fmt.Errorf("gitstate: fingerprint dirty content in %s: %w: %w", f.worktree, gitstate.ErrDirtyUnavailable,
		fmt.Errorf("dirty path %q changed while sampling: %w", "helper.go", gitstate.ErrDirtyMoved))
	// While moving is set, every sample the cycle takes finds the tree moving.
	var moving atomic.Bool
	var sampled atomic.Int32
	c.cycleSampleHook = func(context.Context) error {
		if moving.Load() {
			sampled.Add(1)
			return moved
		}
		return nil
	}
	var (
		mu       sync.Mutex
		outcomes []CheckoutCycle
	)
	c.cycleDone = func(out CheckoutCycle) {
		mu.Lock()
		outcomes = append(outcomes, out)
		mu.Unlock()
	}

	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc MovedUnderTheCycle() {}\n")
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestCheckoutRefresh: %v", err)
	}
	moving.Store(true)
	c.cycle(t.Context())
	moving.Store(false)
	if sampled.Load() == 0 {
		t.Fatal("the cycle never took its sample: the test does not exercise it")
	}
	mu.Lock()
	last := outcomes[len(outcomes)-1]
	mu.Unlock()
	if last.Err != nil || !last.Rescheduled {
		t.Fatalf("a ticket's cycle whose sample the tree moved under = %+v, want rescheduled without an error", last)
	}
	select {
	case result := <-ticket.Ticket.Done:
		t.Fatalf("a ticket whose cycle's sample the tree moved under was finished: %+v", result)
	default:
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket left waiting was not completed by the next cycle: %+v", result)
	}
}
