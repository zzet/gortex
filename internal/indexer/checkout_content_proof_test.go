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
	// publishWithin is how long a test waits for a publication after its
	// edit: twice what the parent's save took to publish on this runner (its
	// quiet window, admission and build — the calibration), and never less
	// than 5 s.
	publishWithin time.Duration
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
	x.publishWithin = max(5*time.Second, 2*cycles.published(t, f, c, saved, 60*time.Second))
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

// requireTrustedChangeStamps skips a test that pins the content-pinned
// fence's liveness (a build published as its sample while the tree moves on)
// when the checkout's filesystem gives no change stamps the sampler trusts —
// Windows, and any filesystem outside darwin's APFS and linux's
// ext4/xfs/btrfs/tmpfs. There the fence keeps main's full-sample decision
// (build_content_proof.go, residual (d)), which
// TestASaveAfterTheParseWithoutChangeStampsTearsAndPublishesTheSave pins on
// every platform. Any other unconfirmed verdict fails the test: the fixture
// should have been provable.
func requireTrustedChangeStamps(t *testing.T, sampler *gitstate.DirtySampler) {
	t.Helper()
	if !trustedChangeStamps(t, sampler) {
		t.Skipf("%s: the content proof needs trusted change stamps (APFS; ext4, xfs, btrfs or tmpfs), and this checkout keeps the full-sample fence", gitstate.ReadSetNoChangeStamps)
	}
}

// trustedChangeStamps reports whether the checkout's filesystem gives change
// stamps the sampler trusts; any unconfirmed verdict other than the
// platform's fails the test.
func trustedChangeStamps(t *testing.T, sampler *gitstate.DirtySampler) bool {
	t.Helper()
	sample, err := sampler.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := sampler.ConfirmReadSet(t.Context(), sample, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Reason == gitstate.ReadSetNoChangeStamps {
		return false
	}
	if !proof.Confirmed {
		t.Fatalf("fixture cannot prove its read set: %s", proof.Reason)
	}
	return true
}

// since returns the cycles recorded after the fixture's mark.
func (x *contentProofFixture) since() []CheckoutCycle {
	x.cycles.mu.Lock()
	defer x.cycles.mu.Unlock()
	return append([]CheckoutCycle(nil), x.cycles.cycles[x.mark:]...)
}

// firstBuilt is the first cycle after the mark that routed a working-tree
// generation it built. A cycle completes its tickets before it is recorded,
// so it waits a little for the record (publishWithin).
func (x *contentProofFixture) firstBuilt(t *testing.T) CheckoutCycle {
	t.Helper()
	deadline := time.Now().Add(x.publishWithin)
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
	requireTrustedChangeStamps(t, x.c.sampler)
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
	took := x.cycles.published(t, x.f, x.c, savedAt, x.publishWithin)
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

// On a filesystem without trusted change stamps — Windows, and anything
// outside APFS and ext4/xfs/btrfs/tmpfs — no read can be proven by its stamp,
// and the fence keeps main's full-sample decision (build_content_proof.go,
// residual (d)). The same save after the parse then tears the build instead
// of publishing it as its sample: no generation is labelled with a sample the
// working copy left, the bytes the build parsed confirm nothing, the ticket
// is answered by a generation holding the save, and once that is routed a
// require_fresh read is fresh. The sampler is told its stamps are untrusted
// (DistrustChangeStampsForTest), the verdict Windows gives every checkout.
func TestASaveAfterTheParseWithoutChangeStampsTearsAndPublishesTheSave(t *testing.T) {
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
	x.c.sampler.DistrustChangeStampsForTest()
	// The gate the proof-dependent tests skip on sees the platform's verdict
	// here, whatever the platform.
	if trustedChangeStamps(t, x.c.sampler) {
		t.Fatalf("the distrusted sampler's read set was confirmed, want %q", gitstate.ReadSetNoChangeStamps)
	}
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
	if first.Err != nil || first.DirtyOutpaced {
		t.Fatalf("the first build after the save = %+v, want the outpaced attempt torn and the saved state built", first)
	}
	if torn := x.tornBuilds(edited); len(torn) == 0 {
		t.Fatal("the save after the parse tore no build: the fence confirmed a sample it cannot prove")
	}
	if readSetConfirmations.contentProven.Load() != proven {
		t.Fatal("the fence confirmed a build by its parsed bytes without change stamps")
	}
	for _, out := range x.since() {
		if out.DirtyGenerationID <= 0 {
			continue
		}
		if row, found := x.f.generation(out.DirtyGenerationID); found && row.LowerViewFingerprint == sample.Fingerprint {
			t.Fatalf("generation %d was published under the sample the save left (%q)", out.DirtyGenerationID, sample.Fingerprint)
		}
	}
	if !x.node(int64(result.AppliedGeneration), "helper.go::Moved") {
		t.Fatalf("the ticket was answered by generation %d, which does not hold the save", result.AppliedGeneration)
	}
	x.cycles.published(t, x.f, x.c, savedAt, x.publishWithin)
	if proof, err := x.lifecycle.ProveCheckoutFresh(t.Context(), x.f.checkoutID, x.f.worktree); err != nil || !proof.Fresh {
		t.Fatalf("a require_fresh read after the saved state was routed = %+v (%v), want fresh", proof, err)
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
	requireTrustedChangeStamps(t, x.c.sampler)
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
	requireTrustedChangeStamps(t, x.c.sampler)
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

// The sustained edit stream tests are in checkout_content_proof_stream_performance_test.go (performance tag): their liveness is load-dependent, and runners under -race cannot build in under a second.

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
