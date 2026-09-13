package indexer

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The committed-base counter suite.
//
// `views_coordinator_cycle_total{adopted_commit}` is the process's only
// reuse-vs-rebuild evidence, and the committed-base machinery never touches it:
// the dedicated-base publisher, its live advancement trigger and its publisher
// drain emitted no counter at all, so "did this advance pay for a build or
// re-adopt one" was answerable only by reading the catalog afterwards.
//
// Every test here drives a REAL publication or advance and asserts on the
// delta of the process registry, because that is what the W8 measurement will
// read. The keys are spelled out rather than composed from the catalog
// constants on purpose: the flattened series key is the wire format the ledger
// cites, so a rename that keeps the code compiling still fails here.
const (
	metricClaimBuilt          = "views_dedicated_base_claim_total{outcome=built}"
	metricClaimCoalesced      = "views_dedicated_base_claim_total{outcome=coalesced}"
	metricClaimReused         = "views_dedicated_base_claim_total{outcome=reused}"
	metricPublishRoot         = "views_dedicated_base_publish_total{shape=root}"
	metricPublishDelta        = "views_dedicated_base_publish_total{shape=delta}"
	metricClosureTruncated    = "views_dedicated_base_closure_truncated_total"
	metricPublicationDone     = "views_dedicated_base_publication_total{outcome=published}"
	metricPublicationCoalesce = "views_dedicated_base_publication_total{outcome=coalesced}"
	metricPublicationReadopt  = "views_dedicated_base_publication_total{outcome=readopted}"
	metricPublicationSkipped  = "views_dedicated_base_publication_total{outcome=skipped}"
	metricPublicationFailed   = "views_dedicated_base_publication_total{outcome=failed}"
	metricAdvanceDispatched   = "views_dedicated_base_advance_total{outcome=dispatched}"
	metricAdvanceRepeat       = "views_dedicated_base_advance_total{outcome=repeat}"
	metricAdvanceRefused      = "views_dedicated_base_advance_total{outcome=refused}"
	metricDrainImmediate      = "views_dedicated_base_drain_total{outcome=immediate}"
	metricDrainWaited         = "views_dedicated_base_drain_total{outcome=waited}"
	metricRecomposition       = "views_dependent_recomposition_total"
)

// viewCounterReading is a flattened snapshot of the process registry.
//
// Deltas rather than a Reset: the registry is process-wide and this package's
// suite shares it, so resetting would clobber a neighbour's reading while
// a delta is correct whatever else has been counted.
type viewCounterReading map[string]int64

func readViewCounters() viewCounterReading {
	return viewCounterReading(viewmetrics.Read().Flat())
}

// since returns how much one series moved between two readings.
func (after viewCounterReading) since(before viewCounterReading, key string) int64 {
	return after[key] - before[key]
}

// requireDelta asserts one series' movement, naming the series in the failure
// so a wrong count reads as "this counter", not "this number". The optional
// trailing strings are the caller's own explanation of what the count means.
func requireDelta(t *testing.T, before, after viewCounterReading, key string, want int64, why ...string) {
	t.Helper()
	got := after.since(before, key)
	msg := ""
	if len(why) > 0 {
		msg = ": " + why[0]
	}
	require.Equal(t, want, got, "series %s moved by %d, want %d%s", key, got, want, msg)
}

// The publisher's completion callback settles AFTER the join, and these two
// helpers are what closes that window.
//
// InitialBasePublisher.record (dedicated_base_startup.go:447-452) appends the
// outcome, bumps `attempted` and wakes every parked Wait, and only THEN runs
// the request's own done callback — the field's own doc says so: "done, when
// set, is called with the outcome after the worker records it"
// (dedicated_base_startup.go:117-119). The advance trigger's record IS that
// callback (dedicated_base_advance_trigger.go:261-263), so the advance row and
// the accepted-commit memo both land AFTER InitialBasePublisher.Wait has
// already returned. A test that joins on Wait and then reads Advances() — or
// observes the same commit again expecting a repeat — is reading state that is
// still in flight on the worker goroutine. Under load (a loaded chunk, a busy
// machine, -race) the worker can be descheduled between the two, which is the
// observed flake: "the production dispatch recorded no advance".
//
// settlePublisher closes it with a BARRIER, not a timer.
// InitialBasePublisher.run is strictly serial by design ("Serial on purpose",
// dedicated_base_startup.go:416-420): it pops one request, publishes it,
// records it and runs its done callback to completion before it pops the next.
// Queueing a second request through the same production door (enqueueAdvance,
// which appends FIFO) and blocking on ITS callback is therefore a happens-after
// for the first request's callback — no sleep, no poll, and no assumption about
// how long a publication takes. TestThePublisherSettlesOneRequestFullyBeforeTheNext
// pins the serialization the barrier rests on, so the day the worker stops
// being serial this stops being a barrier loudly rather than silently.
//
// The barrier's only counter footprint is one
// views_dedicated_base_publication_total{outcome=skipped}: its prefix names no
// dedicated graph, so publish takes the "no ready dedicated graph" early return
// after a single catalog read. No assertion in this file reads a skipped delta
// across a barrier.

// metricsBarrierPrefix names a repository that deliberately does not exist, and
// the sequence keeps each barrier's prefix unique so one barrier can never
// coalesce with another in the publisher's pending map (enqueueLocked replaces
// by prefix).
const metricsBarrierPrefix = "gortex-metrics-barrier-"

var metricsBarrierSeq atomic.Int64

// settlePublisher joins every queued publication AND every completion callback
// the worker has run for one, so everything the trigger records is visible when
// it returns.
func (f *advanceFixture) settlePublisher(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The publisher's own accounting first: every queued publication attempted.
	require.NoError(t, f.publisher.Wait(ctx))
	// Then the barrier, which cannot be reached until the callback of every
	// request queued before it has returned.
	settled := make(chan struct{})
	prefix := metricsBarrierPrefix + strconv.FormatInt(metricsBarrierSeq.Add(1), 10)
	require.True(t, f.publisher.enqueueAdvance(basePublishRequest{
		prefix: prefix,
		done:   func(InitialBasePublication) { close(settled) },
	}), "the publisher refused the barrier request %s", prefix)
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatalf("the publisher never settled the barrier request %s: %v", prefix, ctx.Err())
	}
}

// dispatchAndSettle drives ONE observed HEAD movement through the trigger's
// production entry point and returns only once the trigger has RECORDED it.
//
// It is advanceFixture.dispatchAndWait plus the barrier. dispatchAndWait joins
// InitialBasePublisher.Wait and reads Advances() immediately, which is the race
// described above; this file needs the stronger join because two of its tests
// read state the trigger writes in its callback — the advance itself, and the
// accepted-commit memo a repeat observation is classified against.
func (f *advanceFixture) dispatchAndSettle(t *testing.T, root, commitOID string) DedicatedBaseAdvance {
	t.Helper()
	before := len(f.trigger.Advances())
	f.trigger.HeadChanged(f.prefix, root, commitOID)
	f.settlePublisher(t)
	advances := f.trigger.Advances()
	require.Greater(t, len(advances), before,
		"the production dispatch recorded no advance for %s", shortCommit(commitOID))
	return advances[len(advances)-1]
}

// TestColdCommittedBasePublicationCountsARootBuild is the cold half: the first
// committed base a repository ever gets is a self-contained root that paid for
// a physical pass, and all three axes have to say so.
//
// Revert-red: delete the recordDedicatedBaseAdoption call from ensureObserved
// (dedicated_base_runtime.go) and the publish/claim assertions fail; delete the
// deferred count from InitialBasePublisher.publish and the publication
// assertion fails.
func TestColdCommittedBasePublicationCountsARootBuild(t *testing.T) {
	before := readViewCounters()
	f := newAdvanceFixture(t, "metrics-cold")
	require.Len(t, f.generations(t), 1, "the fixture published more than one base")
	after := readViewCounters()

	requireDelta(t, before, after, metricPublishRoot, 1)
	requireDelta(t, before, after, metricPublishDelta, 0)
	requireDelta(t, before, after, metricClaimBuilt, 1)
	requireDelta(t, before, after, metricClaimReused, 0)
	requireDelta(t, before, after, metricClaimCoalesced, 0)
	requireDelta(t, before, after, metricPublicationDone, 1)
	requireDelta(t, before, after, metricClosureTruncated, 0,
		"a cold full root reported a truncated affected-by closure")
}

// TestAnObservedCommitCountsADeltaPublishAndTellsTheDependents is the live
// half and the one the W8 measurement reads: a HEAD movement the running
// daemon observed publishes a DELTA over the base it already had, and raises
// exactly one dependent-recomposition signal for it.
//
// It runs through GitWatcher.reconcile — the production entry point — so it
// fails if the dispatch is removed from finalizeReconcile even though the
// trigger itself still counts.
//
// Revert-red: drop the DependentRecompositionTotal count beside
// invalidateDependencyCohortsForPrefix and the fan-out assertion fails; drop
// the shape branch in recordDedicatedBaseAdoption and the delta/root pair
// inverts.
func TestAnObservedCommitCountsADeltaPublishAndTellsTheDependents(t *testing.T) {
	f := newAdvanceFixture(t, "metrics-advance")
	require.Len(t, f.generations(t), 1)

	before := readViewCounters()
	f.commit(t, "advanced.go", "package a\n\nfunc Advanced() {}\n", "advance for metrics")
	f.reconcileAndWait(t)
	require.Len(t, f.generations(t), 2, "the observed commit published no new generation")
	after := readViewCounters()

	requireDelta(t, before, after, metricPublishDelta, 1)
	requireDelta(t, before, after, metricPublishRoot, 0,
		"an advance re-rooted instead of extending the chain")
	requireDelta(t, before, after, metricClaimBuilt, 1)
	requireDelta(t, before, after, metricPublicationDone, 1)
	requireDelta(t, before, after, metricAdvanceDispatched, 1)
	requireDelta(t, before, after, metricAdvanceRepeat, 0)
	requireDelta(t, before, after, metricRecomposition, 1)
}

// TestASameTreeCommitCountsAReusedClaim is the reuse case the ratio exists
// for: an amend with no content change names a tree the active base already
// describes, so adoption replays it, nothing is published, and the claim is
// counted as reused rather than built.
//
// Revert-red: remove the AlreadyAdopted arm from recordDedicatedBaseAdoption
// and the replay is counted as a built root — which is exactly the reading
// that would make a zero-write warm path look like a full re-index.
func TestASameTreeCommitCountsAReusedClaim(t *testing.T) {
	f := newAdvanceFixture(t, "metrics-sametree")
	base := f.generations(t)
	require.Len(t, base, 1)

	// An amend with no content change: a new commit object over the same tree.
	runGit(t, f.root, "commit", "-q", "--amend", "-m", "amended for metrics")
	amended := gitHead(t, f.root)
	require.Equal(t, base[0].TreeOID, gitTree(t, f.root, amended),
		"the amend was supposed to leave the tree alone")

	before := readViewCounters()
	advance := f.dispatchAndSettle(t, f.root, amended)
	require.NoError(t, advance.Err)
	require.False(t, advance.Published, "a same-tree commit published a new generation")
	after := readViewCounters()

	requireDelta(t, before, after, metricClaimReused, 1)
	requireDelta(t, before, after, metricClaimBuilt, 0)
	requireDelta(t, before, after, metricPublishRoot, 0)
	requireDelta(t, before, after, metricPublishDelta, 0,
		"a re-adoption was counted as a published generation")
	requireDelta(t, before, after, metricPublicationReadopt, 1)
	requireDelta(t, before, after, metricPublicationDone, 0)
	requireDelta(t, before, after, metricAdvanceDispatched, 1)
	requireDelta(t, before, after, metricRecomposition, 1)
	require.Len(t, f.generations(t), 1, "a same-tree commit allocated a generation")
}

// TestARepeatedObservationCountsARepeatAndNothingElse pins the memo's counter.
//
// The same commit observed twice — a watcher restart, two observers seeing one
// transition — must cost nothing the second time, and the counter has to say
// that it was a repeat rather than silently recording nothing: a daemon whose
// advances all turn into repeats is a different fault from one that never
// observes anything.
//
// Revert-red: delete the repeat/refused count from HeadChanged's early return
// and the repeat assertion fails.
func TestARepeatedObservationCountsARepeatAndNothingElse(t *testing.T) {
	f := newAdvanceFixture(t, "metrics-repeat")
	sha := f.commit(t, "repeat.go", "package a\n\nfunc Repeat() {}\n", "repeat for metrics")

	first := f.dispatchAndSettle(t, f.root, sha)
	require.NoError(t, first.Err)
	require.True(t, first.Published)

	before := readViewCounters()
	f.trigger.HeadChanged(f.prefix, f.root, sha)
	after := readViewCounters()

	requireDelta(t, before, after, metricAdvanceRepeat, 1)
	requireDelta(t, before, after, metricAdvanceDispatched, 0,
		"a repeat of the commit that already landed was dispatched again")
	requireDelta(t, before, after, metricRecomposition, 0,
		"a repeat told the dependents to recompose for a movement that did not happen")
	requireDelta(t, before, after, metricPublicationDone, 0)
}

// TestAnObservationAfterAdvancementStoppedCountsARefusal is the other drop:
// an observation that arrives after the publisher's admission closed. It is
// counted separately from a repeat because the two mean opposite things — a
// repeat is the memo working, a refusal is a daemon whose committed base has
// silently stopped advancing.
//
// Revert-red: count the stopped arm as a repeat (or not at all) and this fails.
func TestAnObservationAfterAdvancementStoppedCountsARefusal(t *testing.T) {
	f := newAdvanceFixture(t, "metrics-refused")
	sha := f.commit(t, "refused.go", "package a\n\nfunc Refused() {}\n", "refused for metrics")

	f.publisher.Close()

	before := readViewCounters()
	f.trigger.HeadChanged(f.prefix, f.root, sha)
	after := readViewCounters()

	requireDelta(t, before, after, metricAdvanceRefused, 1)
	requireDelta(t, before, after, metricAdvanceDispatched, 0)
	requireDelta(t, before, after, metricRecomposition, 0)
}

// TestSkippingARepositoryWithNothingToPublishIsCounted pins the skip arm of
// the publication series through the publisher's own door: a prefix with no
// ready dedicated graph is an outcome, not an absence, and a series that only
// counted successes would make a daemon that publishes nothing indistinguishable
// from one that was never asked to.
//
// Revert-red: move the deferred count in InitialBasePublisher.publish to after
// the early returns and this fails.
func TestSkippingARepositoryWithNothingToPublishIsCounted(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	installStartupPublisherRuntime(t, f)
	publisher := startupPublisher(t, f)

	before := readViewCounters()
	out := publisher.PublishRepo(context.Background(), "no-such-repo")
	require.NotEmpty(t, out.Skipped, "an unknown prefix was not skipped")
	require.NoError(t, out.Err)
	after := readViewCounters()

	requireDelta(t, before, after, metricPublicationSkipped, 1)
	requireDelta(t, before, after, metricPublicationDone, 0)
	requireDelta(t, before, after, metricPublicationFailed, 0)
}

// TestClosingPublisherAdmissionCountsWhetherShutdownWaited drives the drain
// counter through the production shutdown door — the method
// CheckoutLifecycle.Close reaches via stopRepositoryPublishers
// (repository_admission.go:71-78) — in both of its states.
//
// The waited case is the one worth having: an admitted publication holds
// teardown open for the length of a physical index, and nothing else in the
// process records that it happened.
//
// Revert-red: delete the select in DedicatedBaseRuntime.CloseDedicatedBaseAdmission
// and both assertions fail.
func TestClosingPublisherAdmissionCountsWhetherShutdownWaited(t *testing.T) {
	t.Run("immediate", func(t *testing.T) {
		f := newLifecycleFixture(t)
		t.Cleanup(f.close)
		runtime := installStartupPublisherRuntime(t, f)

		before := readViewCounters()
		drained := runtime.CloseDedicatedBaseAdmission()
		after := readViewCounters()

		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("closing admission with no admitted actor did not drain")
		}
		requireDelta(t, before, after, metricDrainImmediate, 1)
		requireDelta(t, before, after, metricDrainWaited, 0)
	})

	t.Run("waited", func(t *testing.T) {
		f := newLifecycleFixture(t)
		t.Cleanup(f.close)
		runtime := installStartupPublisherRuntime(t, f)

		// One admitted actor, exactly as a running publication holds one.
		owner := store_sqlite.DedicatedBaseOwner{CheckoutID: "chk-metrics", Incarnation: "inc-1"}
		release, _, err := runtime.dedicatedBaseRuntime.admitOwnerState(
			context.Background(), "graph-metrics", owner, nil)
		require.NoError(t, err)
		// Registered AFTER the fixture's own cleanup so it runs BEFORE it
		// (t.Cleanup is LIFO), and the release is sync.Once-guarded so the
		// explicit call below is not a double release. Without this a failing
		// assertion below aborts the test with the actor still admitted, and
		// the fixture's close joins a drain that can never complete — the
		// whole package then dies on the panic timeout instead of reporting a
		// failed counter.
		t.Cleanup(release)

		before := readViewCounters()
		drained := runtime.CloseDedicatedBaseAdmission()
		after := readViewCounters()

		requireDelta(t, before, after, metricDrainWaited, 1)
		requireDelta(t, before, after, metricDrainImmediate, 0)
		select {
		case <-drained:
			t.Fatal("the drain completed while an actor was still admitted")
		default:
		}

		release()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("releasing the last admitted actor did not complete the drain")
		}
	})
}

// TestATruncatedClosureIsCountedOnAPublish is the one arm the end-to-end
// fixtures cannot reach: the affected-by closure hitting its cap makes the
// published generation knowingly incomplete, and there is no cheap way to
// build a real committed tree that overflows the cap inside a unit suite.
//
// It is driven against the classifier directly for that reason, and it is a
// real guard rather than a tautology: the truncation count sits behind the
// same not-a-replay gate as the shape count, so the two ways to lose it — a
// replay counting a truncation it did not have, and a real truncation counting
// nothing — are both pinned here.
func TestATruncatedClosureIsCountedOnAPublish(t *testing.T) {
	t.Run("a truncated publish is counted", func(t *testing.T) {
		before := readViewCounters()
		recordDedicatedBaseAdoption(dedicatedBaseResult{
			Claim:    store_sqlite.DedicatedBaseBuildClaim{BaseGenerationID: 7},
			Report:   BuildReport{ClosureTruncated: true, ClosureCap: 200},
			Adoption: store_sqlite.DedicatedBaseAdoption{GenerationID: 8},
		})
		after := readViewCounters()

		requireDelta(t, before, after, metricClosureTruncated, 1)
		requireDelta(t, before, after, metricPublishDelta, 1)
		requireDelta(t, before, after, metricClaimBuilt, 1)
	})

	t.Run("a replay counts no truncation", func(t *testing.T) {
		before := readViewCounters()
		recordDedicatedBaseAdoption(dedicatedBaseResult{
			Report:   BuildReport{ClosureTruncated: true, Coalesced: true},
			Adoption: store_sqlite.DedicatedBaseAdoption{GenerationID: 8, AlreadyAdopted: true},
		})
		after := readViewCounters()

		requireDelta(t, before, after, metricClosureTruncated, 0,
			"a re-adoption reported a truncated closure it never computed")
		requireDelta(t, before, after, metricClaimReused, 1)
		requireDelta(t, before, after, metricClaimCoalesced, 0,
			"a replay was counted twice on the cost axis")
	})

	t.Run("a coalesced build is counted once", func(t *testing.T) {
		before := readViewCounters()
		recordDedicatedBaseAdoption(dedicatedBaseResult{
			Report:   BuildReport{Coalesced: true},
			Adoption: store_sqlite.DedicatedBaseAdoption{GenerationID: 9},
		})
		after := readViewCounters()

		requireDelta(t, before, after, metricClaimCoalesced, 1)
		requireDelta(t, before, after, metricClaimBuilt, 0)
		requireDelta(t, before, after, metricPublishRoot, 1,
			"a coalesced publication published no generation")
	})
}

// TestPublicationOutcomeNamesOneFactPerPublication pins the classifier's
// precedence. The five outcomes ride on one label, so two of them being true
// at once — a coalesced build that was also a re-adoption, a skip that also
// carried an error — must resolve to the single stronger fact rather than to
// whichever branch happens to come first.
func TestPublicationOutcomeNamesOneFactPerPublication(t *testing.T) {
	cases := []struct {
		name string
		out  InitialBasePublication
		want string
	}{
		{"a failure outranks everything", InitialBasePublication{
			Err: context.DeadlineExceeded, Skipped: "publisher stopped", AlreadyAdopted: true,
		}, viewmetrics.PublicationFailed},
		{"a skip outranks a reuse fact", InitialBasePublication{
			Skipped: "owner has no committed tree", Coalesced: true,
		}, viewmetrics.PublicationSkipped},
		{"a re-adoption outranks a coalesced build", InitialBasePublication{
			AlreadyAdopted: true, Coalesced: true,
		}, viewmetrics.PublicationReadopted},
		{"a coalesced build outranks a plain publication", InitialBasePublication{
			Coalesced: true,
		}, viewmetrics.PublicationCoalesced},
		{"everything else published", InitialBasePublication{
			GenerationID: 4,
		}, viewmetrics.PublicationPublished},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, publicationOutcome(tc.out))
		})
	}
}

// TestThePublisherSettlesOneRequestFullyBeforeTheNext pins the property
// settlePublisher's barrier rests on: InitialBasePublisher.run is serial, and
// "serial" reaches all the way through the request's completion callback, not
// only through the publication.
//
// It matters beyond this file. The callback is where the advance trigger writes
// its advance row and its accepted-commit memo (record,
// dedicated_base_advance_trigger.go:290-312), and the publisher's own join —
// Wait — is already satisfied by then (record bumps `attempted` and wakes every
// parked Wait BEFORE it calls done). A test that wants the trigger's state has
// nothing else to join on. If the worker ever publishes two requests
// concurrently, or runs a completion callback off the worker goroutine, the
// barrier silently stops being a barrier and the flake this test exists to
// prevent comes back in a form nothing reports.
//
// Both requests name a repository that does not exist, so each one is a single
// catalog read and a skip: what is being pinned is the ORDER the worker settles
// them in, not what it publishes.
func TestThePublisherSettlesOneRequestFullyBeforeTheNext(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	installStartupPublisherRuntime(t, f)
	publisher := startupPublisher(t, f)

	// The first request's callback parks until the test releases it. Released
	// through a t.Cleanup registered AFTER the fixture's own (t.Cleanup is
	// LIFO, so it runs BEFORE publisher.Close and f.close) and guarded by a
	// sync.Once, so a failing assertion below cannot leave the worker parked
	// inside a callback while teardown waits for it.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)

	firstRunning := make(chan struct{})
	require.True(t, publisher.enqueueAdvance(basePublishRequest{
		prefix: metricsBarrierPrefix + "serial-first",
		done: func(InitialBasePublication) {
			close(firstRunning)
			<-release
		},
	}), "the publisher refused the first request")

	select {
	case <-firstRunning:
	case <-time.After(30 * time.Second):
		t.Fatal("the publisher never ran the first request's completion callback")
	}

	secondSettled := make(chan struct{})
	require.True(t, publisher.enqueueAdvance(basePublishRequest{
		prefix: metricsBarrierPrefix + "serial-second",
		done:   func(InitialBasePublication) { close(secondSettled) },
	}), "the publisher refused the second request")

	// The negative half. The first callback is parked indefinitely — it is
	// released only by this goroutine — so this window can only ever
	// under-report a broken serialization, never fail a sound one: there is no
	// schedule in which a serial worker settles the second request here.
	select {
	case <-secondSettled:
		t.Fatal("a later request settled while an earlier request's completion callback " +
			"was still running: the publisher is no longer serial, and settlePublisher's " +
			"barrier is no longer a barrier")
	case <-time.After(250 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-secondSettled:
	case <-time.After(30 * time.Second):
		t.Fatal("the barrier request never settled after the earlier callback returned")
	}
}
