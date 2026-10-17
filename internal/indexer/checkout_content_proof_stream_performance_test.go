//go:build performance

package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The sustained edit stream tests pin the content-pinned fence's liveness
// under load: whether a stream's reads are answered while it runs depends on
// the runner building the whole rotation well inside the stream, which CI's
// runners under the race detector cannot (one to five seconds a build), so
// they run in the tagged performance flow. The fence's mechanism is pinned in
// the default suite (checkout_content_proof_test.go).

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
	requireTrustedChangeStamps(t, c.sampler)
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
