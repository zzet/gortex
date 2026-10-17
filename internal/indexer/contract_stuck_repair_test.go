package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// contractRepairTestPeriod is the repair period the in-process pins run
// with: short enough for a test, long against a fixture apply.
const contractRepairTestPeriod = 500 * time.Millisecond

// contractRepairFixture is the route/constant job fixture with the real
// scheduler and the stuck repair wired to it the way the daemon wires them.
type contractRepairFixture struct {
	*contractJobFixture
	c    *ContractAnalysisCoordinator
	r    *ContractStuckRepair
	logs *observer.ObservedLogs
	// published counts the Published events per repository.
	published sync.Map
}

func newContractRepairFixture(t *testing.T, adjust func(*contractRepairFixture, *ContractStuckRepair)) *contractRepairFixture {
	t.Helper()
	job := newRouteConstantFixture(t)
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	f := &contractRepairFixture{contractJobFixture: job, logs: logs}
	var repair atomic.Pointer[ContractStuckRepair]
	capture := job.options
	capture.SuspectStuck = func(repo string, paths []string) { repair.Load().Suspect(repo, paths) }
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: job.store, Leases: job.options.Materializer.Leases, Registry: job.options.Registry, Config: job.options.Config, Logger: logger,
		Capture: NewContractFollowupSourceCapture(capture), Inputs: NewContractPrimaryInputCapture(job.options.Materializer), Yield: job.options.Yield})
	require.NoError(t, err)
	f.c = c
	job.mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(ctx context.Context, repo, checkout string) {
		count, _ := f.published.LoadOrStore(repo, new(atomic.Int64))
		count.(*atomic.Int64).Add(1)
		job.announce()
		c.Published(ctx, repo, checkout)
	}})
	// The test seams are set before the repair starts.
	r, err := newContractStuckRepair(ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: job.options, Coordinator: c, Logger: logger, Period: contractRepairTestPeriod}, func(r *ContractStuckRepair) {
		if adjust != nil {
			adjust(f, r)
		}
	})
	require.NoError(t, err)
	repair.Store(r)
	f.r = r
	t.Cleanup(func() {
		r.Close()
		require.NoError(t, c.Close())
	})
	return f
}

func (f *contractRepairFixture) publishedCount(repo string) int64 {
	count, ok := f.published.Load(repo)
	if !ok {
		return 0
	}
	return count.(*atomic.Int64).Load()
}

// strict waits, the way the strict consumer does, until the attachment of
// the fixture's current inputs opens, within limit.
func (f *contractRepairFixture) strict(t *testing.T, limit time.Duration) map[string]*graph.Node {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	scheduleUntilAttached(t, ctx, f.c, f.options.Materializer, []string{"fixture"}, "fixture")
	return f.contracts()
}

// failBody runs one mutation of the fixture's repository lane whose body
// rewrites name and then fails: the file's receipt is staged, its window
// stays open and is marked orphaned.
func (f *contractRepairFixture) failBody(t *testing.T, name, text string) {
	t.Helper()
	m := newContractLaneMutation(t, f.contractJobFixture, "multi_indexer")
	_, apply := m.patch(t, name, text)
	// The body runs under the repository lane, as every primary mutation does.
	err := f.mi.withRepositoryMutationLanes(t.Context(), []string{"fixture"}, func() error {
		return m.run(t.Context(), func(func(*OutputSourceContent)) error {
			if err := apply(); err != nil {
				return err
			}
			return errors.New("injected body failure")
		})
	})
	require.ErrorContains(t, err, "injected body failure")
	require.NotEmpty(t, f.store.OrphanedContractCoreWindows("fixture"), "the failed body left its window open")
}

func repairCount(kind string) int64 {
	return contractCounter("views_contract_repair_total{kind=" + kind + "}")
}

// TestContractStuckReceiptRepairsAFailedBody: a body that
// failed on a file the route reads leaves its window open; a route edit of
// another file and one strict call later, the repair has reindexed the
// file, the job proved its reads and
// published, and the answer is complete and fresh.
func TestContractStuckReceiptRepairsAFailedBody(t *testing.T) {
	repairStuckReceipt(t, nil)
}

func repairStuckReceipt(t *testing.T, observe func(time.Duration)) {
	t.Helper()
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	consts := jobConsts + "// a failed apply\n"
	f.failBody(t, "consts.go", consts)
	var failedAt time.Time
	if observe != nil {
		failedAt = time.Now()
	}
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	f.save("routes.go", routes)
	got := f.strict(t, 20*time.Second)
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(got))
	require.Equal(t, repaired+1, repairCount("stuck_receipt"), "one forced reindex repaired the window")
	lines := f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()
	require.NotEmpty(t, lines)
	if observe != nil {
		observe(lines[0].Time.Sub(failedAt))
	}
	require.Empty(t, f.store.OpenContractCoreWindows("fixture"))
}

// saveQuiet saves a file through the real door, off the test goroutine.
func (f *contractJobFixture) saveQuiet(name, text string) error {
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	future := time.Now().Add(time.Duration(f.epoch.Load()+1) * time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		return err
	}
	_, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	return err
}

// TestContractStuckRepairUnderASteadyEventStream: the same failed body with a
// 1 Hz writer saving an unrelated file throughout: every save is accepted
// and announced, none closes the stuck window, and the repair still runs
// while the writer saves.
func TestContractStuckRepairUnderASteadyEventStream(t *testing.T) {
	repairUnderSteadyEvents(t, nil)
}

func repairUnderSteadyEvents(t *testing.T, observe func(time.Duration)) {
	t.Helper()
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	consts := jobConsts + "// a failed apply\n"
	f.failBody(t, "consts.go", consts)
	var failedAt time.Time
	if observe != nil {
		failedAt = time.Now()
	}
	stop := make(chan struct{})
	var written sync.WaitGroup
	var helper atomic.Value
	var saves atomic.Int64
	helper.Store(jobHelper)
	written.Add(1)
	go func() {
		defer written.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			text := jobHelper + "// tick " + time.Now().Format(time.RFC3339Nano) + "\n"
			if err := f.saveQuiet("helper.go", text); err != nil {
				t.Error(err)
				return
			}
			helper.Store(text)
			saves.Add(1)
		}
	}()
	var stopOnce sync.Once
	stopWriter := func() {
		stopOnce.Do(func() {
			close(stop)
			written.Wait()
		})
	}
	t.Cleanup(stopWriter)
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	f.save("routes.go", routes)
	require.Eventually(t, func() bool {
		return len(f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()) > 0
	}, 20*time.Second, 10*time.Millisecond, "the repair ran while the writer was saving")
	lines := f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()
	if observe != nil {
		observe(lines[0].Time.Sub(failedAt))
	}
	stopWriter()
	require.Positive(t, saves.Load(), "the writer saved throughout")
	require.Equal(t, repaired+1, repairCount("stuck_receipt"))
	got := f.strict(t, 20*time.Second)
	require.Contains(t, contractIDs(got), "http::POST::/edited")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": consts, "helper.go": helper.Load().(string)})), contractIDs(got))
}

// TestContractStuckRepairWhileAStuckJobHoldsTheSlot: the job blocked on the
// stuck window holds the only job slot; the repair runs on its own
// goroutine, so the same job proves its reads and publishes.
func TestContractStuckRepairWhileAStuckJobHoldsTheSlot(t *testing.T) {
	var armed, held atomic.Bool
	actor := NewContractActor("", []string{"fixture"})
	f := newContractRepairFixture(t, func(f *contractRepairFixture, r *ContractStuckRepair) {
		// Once armed, a repair runs only after the job blocked on the stuck
		// window holds the only job slot.
		r.beforeLane = func(string) {
			for deadline := time.Now().Add(20 * time.Second); armed.Load() && !held.Load() && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				held.Store(f.c.Status(actor).State == contractActorRunning.String())
			}
		}
	})
	f.strict(t, time.Minute)
	started := len(f.logs.FilterMessage("contract analysis job started").All())
	// A window left open over an accepted namespace: the route edit's job
	// admits the file in flight, runs and waits on it.
	f.stageInFlight("fixture/consts.go")
	armed.Store(true)
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	f.save("routes.go", routes)
	require.Eventually(t, func() bool {
		return f.c.Status(actor).State == contractActorRunning.String()
	}, 10*time.Second, time.Millisecond, "the route edit's job runs and blocks on the window")
	got := f.strict(t, 20*time.Second)
	require.True(t, held.Load(), "the repair ran while the job held the slot")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(got))
	require.Len(t, f.logs.FilterMessage("contract analysis job started").All(), started+1, "the job that held the slot published")
	ends := f.logs.FilterMessage("contract analysis job").All()
	require.Equal(t, "published", ends[len(ends)-1].ContextMap()["outcome"])
}

// TestContractStuckRepairRechecksUnderTheLane: a save that heals the stuck
// window between the repair's detection and its lane leaves nothing to
// repair: the pass logs the window healed and reindexes nothing.
func TestContractStuckRepairRechecksUnderTheLane(t *testing.T) {
	healed := make(chan struct{})
	allowLane := make(chan struct{})
	var allowOnce sync.Once
	defer allowOnce.Do(func() { close(allowLane) })
	var once sync.Once
	f := newContractRepairFixture(t, func(f *contractRepairFixture, r *ContractStuckRepair) {
		r.beforeLane = func(repo string) {
			// Also hold the initial scan pass: it could otherwise pass this
			// hook before failBody and acquire the lane after the failure.
			select {
			case <-allowLane:
			case <-r.ctx.Done():
				return
			}
			if len(f.store.OrphanedContractCoreWindows("fixture")) == 0 {
				return // Not the pass the failed body asked for.
			}
			once.Do(func() {
				if err := f.saveQuiet("consts.go", jobConsts+"// saved again\n"); err != nil {
					t.Error(err)
				}
				close(healed)
			})
		}
	})
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	f.failBody(t, "consts.go", jobConsts+"// a failed apply\n")
	// Observe the real watcher's detection before healing. The pass must
	// retain that detection across saveQuiet and recheck it under the lane.
	require.Eventually(t, func() bool {
		f.r.mu.Lock()
		defer f.r.mu.Unlock()
		state := f.r.repos["fixture"]
		return state != nil && state.detected["fixture/consts.go"]
	}, 20*time.Second, 10*time.Millisecond, "the watcher must detect the failed body's window")
	allowOnce.Do(func() { close(allowLane) })
	select {
	case <-healed:
	case <-time.After(20 * time.Second):
		t.Fatal("the repair never found the stuck window")
	}
	require.Eventually(t, func() bool {
		return len(f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("outcome", "healed")).All()) > 0
	}, 10*time.Second, 10*time.Millisecond)
	time.Sleep(2 * contractRepairTestPeriod)
	require.Equal(t, repaired, repairCount("stuck_receipt"), "a window healed before the lane is not reindexed")
}

// TestContractStuckRepairWaitsForABusyLane: while another mutation holds the
// repository's lane, past a whole repair period, the repair repairs nothing
// and the scheduler keeps dispatching; once the lane is released the repair,
// still asking for it, takes it at its next poll.
func TestContractStuckRepairWaitsForABusyLane(t *testing.T) {
	busyLaneRepair(t, nil, nil)
}

func TestContractStuckRepairBusyPassSchedulesAnotherPass(t *testing.T) {
	mi := &MultiIndexer{}
	release, ok := mi.TryRepositoryMutationLanes([]string{"fixture"})
	require.True(t, ok)
	defer release()
	core, logs := observer.New(zapcore.InfoLevel)
	r := &ContractStuckRepair{
		options: ContractStuckRepairOptions{MultiIndexer: mi, Logger: zap.New(core)},
		// Expire the pass budget so the first failed lane acquisition takes
		// the busy branch, with no poll or background watcher to wake it.
		period: -time.Nanosecond,
		ctx:    t.Context(),
	}
	state := &contractRepoRepair{repo: "fixture", wake: make(chan struct{}, 1)}
	r.pass(state)
	require.Len(t, logs.FilterMessage("contract stuck repair").FilterField(zap.String("outcome", "busy")).All(), 1)
	select {
	case <-state.wake:
	default:
		t.Fatal("a busy pass must schedule another pass without a watcher event")
	}
}

func TestContractRepairAdmissionHonorsTheBatchGate(t *testing.T) {
	mi := &MultiIndexer{}
	func() {
		mi.batchMutationGate.Lock()
		defer mi.batchMutationGate.Unlock()
		release, ok := mi.TryRepositoryMutationLanes([]string{"fixture"})
		require.False(t, ok, "repair admission waits until the batch transition ends")
		require.Nil(t, release)
	}()
	release, ok := mi.TryRepositoryMutationLanes([]string{"fixture"})
	require.True(t, ok)
	defer release()
}

// busyLaneRepair holds the fixture repository's lane over a stuck window for
// longer than a repair period, checking that the repair waits and the
// dispatcher does not, then releases it after the busy outcome is observed.
// Performance tests can observe dispatch and repair latency.
func busyLaneRepair(t *testing.T, dispatchTime, repairTime func(time.Duration)) {
	t.Helper()
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	f.failBody(t, "consts.go", jobConsts+"// a failed apply\n")
	unlock, ok := f.mi.TryRepositoryMutationLanes([]string{"fixture"})
	require.True(t, ok)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	defer release()
	held := time.Now()
	actor := NewContractActor("", []string{"fixture"})
	busy := func() bool {
		return len(f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("outcome", "busy")).All()) > 0
	}
	for !busy() {
		require.Less(t, time.Since(held), 20*time.Second, "a pass found the lane busy for its period")
		// A request is dispatched while the repair waits for the lane:
		// its inputs are pending, so the dispatcher parks it on their
		// acceptance at once.
		epoch := f.c.InputEpoch()
		inputs, err := NewContractPrimaryInputCapture(f.options.Materializer)(t.Context(), []string{"fixture"}, "fixture")
		require.NoError(t, err)
		var start time.Time
		if dispatchTime != nil {
			start = time.Now()
		}
		_, err = f.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}, Inputs: inputs, Epoch: epoch})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			state := f.c.Status(actor).State
			return state != contractActorQueued.String() && state != contractActorCapturing.String()
		}, 10*time.Second, time.Millisecond, "dispatch is not held by the repair")
		if dispatchTime != nil {
			dispatchTime(time.Since(start))
		}
		require.Equal(t, repaired, repairCount("stuck_receipt"), "no repair while the lane is held")
		time.Sleep(100 * time.Millisecond)
	}
	release()
	var released time.Time
	if repairTime != nil {
		released = time.Now()
	}
	require.Eventually(t, func() bool { return repairCount("stuck_receipt") == repaired+1 }, 10*time.Second, time.Millisecond)
	if repairTime != nil {
		repairTime(time.Since(released))
	}
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts + "// a failed apply\n", "helper.go": jobHelper})), contractIDs(f.strict(t, 20*time.Second)))
}

// TestContractStuckRepairBatchesAllStuckPathsInOneReindex: two stuck files
// of one repository are repaired by one forced reindex.
func TestContractStuckRepairBatchesAllStuckPathsInOneReindex(t *testing.T) {
	both := make(chan struct{})
	var callsMu sync.Mutex
	var calls [][]string
	f := newContractRepairFixture(t, func(_ *contractRepairFixture, r *ContractStuckRepair) {
		r.beforeLane = func(string) { <-both }
		r.reindex = func(ctx context.Context, repo string, paths []string) error {
			callsMu.Lock()
			calls = append(calls, append([]string(nil), paths...))
			callsMu.Unlock()
			return r.realReindex(ctx, repo, paths)
		}
	})
	repaired := repairCount("stuck_receipt")
	f.failBody(t, "consts.go", jobConsts+"// a failed apply\n")
	f.failBody(t, "helper.go", jobHelper+"// a failed apply\n")
	close(both)
	lines := func() []observer.LoggedEntry {
		return f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()
	}
	require.Eventually(t, func() bool { return len(f.store.OpenContractCoreWindows("fixture")) == 0 && len(lines()) > 0 }, 20*time.Second, 10*time.Millisecond)
	require.Equal(t, repaired+1, repairCount("stuck_receipt"), "one reindex for every stuck path")
	require.Len(t, lines(), 1)
	require.Equal(t, []any{"fixture/consts.go", "fixture/helper.go"}, lines()[0].ContextMap()["paths"])
	callsMu.Lock()
	defer callsMu.Unlock()
	require.Equal(t, [][]string{{"fixture/consts.go", "fixture/helper.go"}}, calls, "one actual reindex invocation receives the complete batch")
}

// failingReindex is a forced reindex that keeps failing the way the body
// that left the window did: it stages the paths' receipts and fails.
func failingReindex(r *ContractStuckRepair, failing *atomic.Bool, calls *atomic.Int64) func(context.Context, string, []string) error {
	return func(ctx context.Context, repo string, paths []string) error {
		calls.Add(1)
		if !failing.Load() {
			return r.realReindex(ctx, repo, paths)
		}
		idx, root, err := r.repoIndexer(repo)
		if err != nil {
			return err
		}
		return idx.withOutputGeneration(ctx, OutputEntryContractRepair, func() error {
			for _, path := range paths {
				if _, err := idx.incrementalPointWatcherPath(root, filepath.Join(root, filepath.FromSlash(path[len(repo)+1:]))); err != nil {
					return err
				}
			}
			return errors.New("injected persistent fault")
		})
	}
}

// TestContractStuckRepairStagesAnEnvelopeAfterThreeFailures: a path the
// forced reindex keeps failing on gets three reindexes, then a failure
// envelope: the job publishes equal to a cold build without the file, and a
// later good save replaces the envelope.
func TestContractStuckRepairStagesAnEnvelopeAfterThreeFailures(t *testing.T) {
	var failing atomic.Bool
	var calls atomic.Int64
	var evictWindowOpen atomic.Int64
	f := newContractRepairFixture(t, func(f *contractRepairFixture, r *ContractStuckRepair) {
		failing.Store(true)
		r.reindex = failingReindex(r, &failing, &calls)
		r.evict = func(repo, rel string) error {
			// The envelope is staged, its window open, before the evict.
			path := repo + "/" + rel
			row, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), repo, "", path)
			staged := err == nil && row != nil && !row.Accepted && slices.Contains(row.Scope.Causes, contractFileIndexFailed)
			if staged && f.store.ContractReceiptWriteStates(repo, "", []string{path})[path].Writing {
				evictWindowOpen.Add(1)
			}
			return r.realEvict(repo, rel)
		}
	})
	f.strict(t, time.Minute)
	repaired, enveloped := repairCount("stuck_receipt"), repairCount("stuck_envelope")
	f.failBody(t, "consts.go", jobConsts+"// a failed apply\n")
	require.Eventually(t, func() bool {
		return len(f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_envelope")).All()) > 0
	}, 30*time.Second, 10*time.Millisecond, "the envelope ran")
	envelopes := f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_envelope")).All()
	require.Equal(t, "repaired", envelopes[0].ContextMap()["outcome"], "%v", envelopes[0].ContextMap())
	require.Equal(t, enveloped+1, repairCount("stuck_envelope"))
	require.EqualValues(t, 3, calls.Load(), "three forced reindexes before the envelope")
	require.Equal(t, repaired+3, repairCount("stuck_receipt"))
	require.EqualValues(t, 1, evictWindowOpen.Load(), "the envelope is staged, its window open, when the file's core rows are evicted")
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.True(t, row.Accepted && contractReceiptIsFailureEnvelope(row), "the file is excluded by an accepted envelope")
	require.Empty(t, f.store.GetFileNodes("fixture/consts.go"), "its core rows are evicted")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "helper.go": jobHelper})), contractIDs(f.strict(t, 20*time.Second)))

	// A later save of the excluded file fails mid-body too, leaving its row
	// in flight over the envelope: three more failing reindexes, then one
	// more envelope, staged over the first, closes the window.
	f.failBody(t, "consts.go", jobConsts+"// a second failed apply\n")
	require.Eventually(t, func() bool {
		return len(f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_envelope")).All()) > 1
	}, 30*time.Second, 10*time.Millisecond, "the second envelope ran")
	envelopes = f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_envelope")).All()
	require.Equal(t, "repaired", envelopes[1].ContextMap()["outcome"], "%v", envelopes[1].ContextMap())
	require.EqualValues(t, 6, calls.Load())
	require.EqualValues(t, 2, evictWindowOpen.Load())
	require.Empty(t, f.store.OpenContractCoreWindows("fixture"), "the envelope over the envelope closed the window")
	row, _, err = f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.True(t, row.Accepted && contractReceiptIsFailureEnvelope(row))
	time.Sleep(2 * contractRepairTestPeriod)
	require.Equal(t, enveloped+2, repairCount("stuck_envelope"), "nothing is left to exclude")

	failing.Store(false)
	consts := "package fixture\nconst RoutePath = \"/after\"\n"
	f.save("consts.go", consts)
	row, _, err = f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
	require.NoError(t, err)
	require.True(t, row.Accepted && !contractReceiptIsFailureEnvelope(row), "a good save replaces the envelope")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(f.strict(t, 20*time.Second)))
}

// The stuck shapes that no failed body marks: each is found by its window's
// age (or a job's report) and repaired within one period.
func TestContractStuckRepairFailedStagingTransaction(t *testing.T) {
	stuckRepairPin(t, func(f *contractRepairFixture) {
		// The staging transaction opens the window before it begins and
		// fails on its CAS: the window stays open over the old row.
		ctx := t.Context()
		current, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
		require.NoError(t, err)
		wrong := current
		wrong.InputFingerprint = "not-the-current-state"
		next := current
		next.InputFingerprint = "staged"
		next.Accepted = false
		row := f.stagedReceipt("fixture/consts.go", "staged")
		require.Error(t, f.store.BeginContractInputMutationWithReceiptsContext(ctx, &wrong, next, nil, []graph.ContractBoundaryReceipt{row}))
	})
}

func TestContractStuckRepairColdDoorFlushThenFailure(t *testing.T) {
	stuckRepairPin(t, func(f *contractRepairFixture) {
		// A cold door flushed unaccepted receipts of its first files and
		// then failed: their windows are open and nothing marked them.
		f.stageInFlight("fixture/consts.go")
		f.stageInFlight("fixture/helper.go")
	})
}

func TestContractStuckRepairBaselineSetThenAcceptFailure(t *testing.T) {
	stuckRepairPin(t, func(f *contractRepairFixture) {
		// A baseline set its rows and its accept failed: the windows are
		// open and the namespace header is pending.
		ctx := t.Context()
		current, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
		require.NoError(t, err)
		pending := current
		pending.InputFingerprint = "baseline-pending"
		pending.Accepted = false
		require.NoError(t, f.store.BeginContractInputMutationContext(ctx, &current, pending, nil))
		f.stageInFlight("fixture/consts.go")
	})
}

func TestContractStuckRepairKeptWriteOverAnOpenWindow(t *testing.T) {
	stuckRepairPin(t, func(f *contractRepairFixture) {
		// A receipt write after the staging moves the seq past the one
		// that opened the window: a settle of the older seq is a no-op,
		// and the window stays open.
		f.stageInFlight("fixture/consts.go")
		current, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
		require.NoError(t, err)
		refused := *current
		refused.Fingerprint = "refused"
		require.ErrorIs(t, f.store.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{refused}), store_sqlite.ErrCatalogStaleGuard)
		state := f.store.ContractReceiptWriteStates("fixture", "", []string{"fixture/consts.go"})["fixture/consts.go"]
		require.True(t, state.Writing)
	})
}

// stuckRepairPin leaves a stuck state no failed body marked, then pins that
// the repair finds it by age and the next strict call is
// complete and equal to cold.
func stuckRepairPin(t *testing.T, stick func(*contractRepairFixture)) {
	stuckRepairMeasured(t, stick, nil)
}

func stuckRepairMeasured(t *testing.T, stick func(*contractRepairFixture), observe func(time.Duration)) {
	t.Helper()
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	stick(f)
	var stuckAt time.Time
	if observe != nil {
		stuckAt = time.Now()
	}
	require.NotEmpty(t, f.store.OpenContractCoreWindows("fixture"))
	require.Empty(t, f.store.OrphanedContractCoreWindows("fixture"), "nothing marked the window")
	require.Eventually(t, func() bool { return len(f.store.OpenContractCoreWindows("fixture")) == 0 }, 20*time.Second, 10*time.Millisecond)
	if observe != nil {
		observe(time.Since(stuckAt))
	}
	require.Equal(t, repaired+1, repairCount("stuck_receipt"))
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(f.strict(t, 20*time.Second)))
}

func TestContractStuckRepairRetainsRestartLeftRetries(t *testing.T) {
	job := newRouteConstantFixture(t)
	row := job.stagedReceipt("fixture/consts.go", "restart-left")
	require.NoError(t, job.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	job.store.ForgetContractReceiptWritesForTest("fixture", "", []string{row.FilePath})
	var attempts atomic.Int64
	envelopes := repairCount("stuck_envelope")
	r, err := newContractStuckRepair(ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: job.options, Period: contractRepairTestPeriod}, func(r *ContractStuckRepair) {
		r.reindex = func(context.Context, string, []string) error {
			attempts.Add(1)
			return errors.New("injected failure before staging")
		}
	})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	require.Eventually(t, func() bool {
		stored, _, err := job.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", row.FilePath)
		return err == nil && stored != nil && stored.Accepted && contractReceiptIsFailureEnvelope(stored)
	}, 20*time.Second, 10*time.Millisecond)
	require.EqualValues(t, contractRepairEnvelopeAfter, attempts.Load())
	require.Equal(t, envelopes+1, repairCount("stuck_envelope"))
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.repos["fixture"] != nil && !r.repos["fixture"].retry
	}, 10*time.Second, time.Millisecond)
}

// TestContractStuckRepairRestartLeftRow: an unaccepted row no mutation of
// this process wrote (an earlier process stopped mid-body) is found by the
// repair's start scan and reindexed.
func TestContractStuckRepairRestartLeftRow(t *testing.T) {
	job := newRouteConstantFixture(t)
	row := job.stagedReceipt("fixture/consts.go", "restart-left")
	require.NoError(t, job.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	// The process that staged the row stopped: this one never wrote it.
	job.store.ForgetContractReceiptWritesForTest("fixture", "", []string{"fixture/consts.go"})
	state := job.store.ContractReceiptWriteStates("fixture", "", []string{"fixture/consts.go"})["fixture/consts.go"]
	require.Zero(t, state.Seq, "no write of this process")
	require.False(t, state.Writing)
	repaired := repairCount("stuck_receipt")
	r, err := NewContractStuckRepair(ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: job.options, Period: contractRepairTestPeriod})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	// The repair is counted as it starts; its reindex accepts the receipt.
	require.Eventually(t, func() bool {
		stored, _, err := job.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/consts.go")
		return err == nil && stored != nil && stored.Accepted && repairCount("stuck_receipt") == repaired+1
	}, 20*time.Second, 10*time.Millisecond, "the reindex accepted the file's receipt")
}

// TestContractHeaderRepairWaitsForOpenWindows: a stuck window and a pending
// namespace header with an actor waiting on it. The first forced reindex
// fails and leaves the window open again, younger than a period; the header
// is not repaired while it is open. The second reindex closes it and leaves
// the header pending, and only then is the header repaired.
func TestContractHeaderRepairWaitsForOpenWindows(t *testing.T) {
	var failing atomic.Bool
	var calls atomic.Int64
	failing.Store(true)
	f := newContractRepairFixture(t, func(f *contractRepairFixture, r *ContractStuckRepair) {
		reindex := failingReindex(r, &failing, &calls)
		r.reindex = func(ctx context.Context, repo string, paths []string) error {
			if failing.Load() {
				// The first reindex fails; the next ones do not.
				defer failing.Store(false)
				return reindex(ctx, repo, paths)
			}
			if err := reindex(ctx, repo, paths); err != nil {
				return err
			}
			// The reindex's accept may have accepted the header: it is left
			// pending again, for the header repair.
			current, _, err := f.store.ContractInputStateContext(ctx, repo, "")
			if err != nil || !current.Accepted {
				return err
			}
			pending := current
			pending.InputFingerprint = "header-pending-again"
			pending.Accepted = false
			return f.store.BeginContractInputMutationContext(ctx, &current, pending, nil)
		}
	})
	f.strict(t, time.Minute)
	ctx := t.Context()
	current, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	pending := current
	pending.InputFingerprint = "header-pending"
	pending.Accepted = false
	require.NoError(t, f.store.BeginContractInputMutationContext(ctx, &current, pending, nil))
	consts := jobConsts + "// a failed apply\n"
	f.failBody(t, "consts.go", consts)
	// An actor waits on the pending inputs from now on.
	inputs, err := NewContractPrimaryInputCapture(f.options.Materializer)(ctx, []string{"fixture"}, "fixture")
	require.NoError(t, err)
	require.False(t, inputs.State.Accepted)
	scheduled, err := f.c.Schedule(ctx, ContractScheduleRequest{Actor: NewContractActor("", []string{"fixture"}), Key: graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}, Inputs: inputs, Epoch: f.c.InputEpoch()})
	require.NoError(t, err)
	require.Equal(t, ContractDeferred, scheduled.Result)
	require.Eventually(t, func() bool {
		state, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
		headers := f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "pending_header")).All()
		return err == nil && state.Accepted && len(f.store.OpenContractCoreWindows("fixture")) == 0 && len(headers) > 0
	}, 20*time.Second, 10*time.Millisecond, "the window is repaired and the header accepted")
	var failed, repaired, header time.Time
	for _, line := range f.logs.FilterMessage("contract stuck repair").All() {
		fields := line.ContextMap()
		switch {
		case fields["kind"] == "stuck_receipt" && fields["outcome"] == "failed" && failed.IsZero():
			failed = line.Time
		case fields["kind"] == "stuck_receipt" && fields["outcome"] == "repaired" && repaired.IsZero():
			repaired = line.Time
		case fields["kind"] == "pending_header" && header.IsZero():
			header = line.Time
		}
	}
	require.GreaterOrEqual(t, calls.Load(), int64(2))
	require.False(t, failed.IsZero(), "the first reindex failed and left the window open")
	require.False(t, repaired.IsZero(), "a later reindex closed the window")
	require.False(t, header.IsZero(), "the header left pending was repaired")
	require.False(t, header.Before(repaired), "the header is repaired only once no window is open")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(f.strict(t, 20*time.Second)))
}

func TestContractPendingHeaderRepairWithoutAnActor(t *testing.T) {
	job := newRouteConstantFixture(t)
	initial, err := job.job(nil, nil)
	require.NoError(t, err)
	require.True(t, initial.Published)
	job.saveHeld("consts.go", "package fixture\nconst RoutePath = \"/held\"\n")
	require.Empty(t, job.store.OpenContractCoreWindows("fixture"))
	state, found, err := job.store.ContractInputStateContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, state.Accepted)
	headers := repairCount("pending_header")
	r, err := NewContractStuckRepair(ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: job.options, Period: contractRepairTestPeriod})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	require.Eventually(t, func() bool {
		state, found, err := job.store.ContractInputStateContext(t.Context(), "fixture", "")
		return err == nil && found && state.Accepted
	}, 20*time.Second, 10*time.Millisecond)
	require.Equal(t, headers+1, repairCount("pending_header"))
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.repos["fixture"] != nil && !r.repos["fixture"].retry
	}, 10*time.Second, time.Millisecond)
}

func TestContractHeaderRepairRetriesFailedBaselinesWithoutAnActor(t *testing.T) {
	for _, beforeStart := range []bool{true, false} {
		t.Run(fmt.Sprintf("failure_before_start_%t", beforeStart), func(t *testing.T) {
			job := newRouteConstantFixture(t)
			job.saveHeld("consts.go", "package fixture\nconst RoutePath = \"/held\"\n")
			options := job.options
			var failed atomic.Bool
			options.Yield = func(ctx context.Context) error {
				if failed.CompareAndSwap(false, true) {
					return errors.New("injected baseline failure after its pending marker")
				}
				return ctx.Err()
			}
			if beforeStart {
				require.ErrorContains(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"), "injected baseline failure")
				require.False(t, job.mi.contractBaselineActive("fixture"))
			}
			headers := repairCount("pending_header")
			r, err := NewContractStuckRepair(ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: options, Period: contractRepairTestPeriod})
			require.NoError(t, err)
			t.Cleanup(r.Close)
			require.Eventually(t, func() bool {
				state, found, err := job.store.ContractInputStateContext(t.Context(), "fixture", "")
				return err == nil && found && state.Accepted
			}, 20*time.Second, 10*time.Millisecond)
			want := int64(2)
			if beforeStart {
				want = 1
			}
			require.Equal(t, headers+want, repairCount("pending_header"))
			require.False(t, job.mi.contractBaselineActive("fixture"))
		})
	}
}

func TestContractHeaderRepairWaitsForALiveBaseline(t *testing.T) {
	job := newRouteConstantFixture(t)
	job.saveHeld("consts.go", "package fixture\nconst RoutePath = \"/held\"\n")
	entered, proceed := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	t.Cleanup(release)
	options := job.options
	options.Yield = func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-proceed:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ended := make(chan error, 1)
	go func() { ended <- reconcilePrimaryContractBaseline(t.Context(), options, "fixture") }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("baseline did not reach its pending marker")
	}
	require.True(t, job.mi.contractBaselineActive("fixture"))
	current, found, err := job.store.ContractInputStateContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	state := &contractRepoRepair{repo: "fixture", pendingKey: current.InputVersion + "\x00" + current.InputFingerprint, pendingSince: time.Now().Add(-2 * contractRepairTestPeriod)}
	r := &ContractStuckRepair{options: ContractStuckRepairOptions{Store: job.store, MultiIndexer: job.mi, Baseline: job.options, Logger: zap.NewNop()}, period: contractRepairTestPeriod}
	headers := repairCount("pending_header")
	require.NoError(t, job.mi.withRepositoryMutationLanes(t.Context(), []string{"fixture"}, func() error {
		r.repairHeader(withHeldRepositoryLanes(t.Context(), []string{"fixture"}), state)
		return nil
	}))
	require.Equal(t, headers, repairCount("pending_header"))
	require.NotEmpty(t, state.pendingKey, "a failed live owner must leave a later retry possible")
	release()
	select {
	case err := <-ended:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("baseline did not finish")
	}
	require.False(t, job.mi.contractBaselineActive("fixture"))
}

func TestContractBaselineActivityRetainsOverlappingOwners(t *testing.T) {
	mi := &MultiIndexer{}
	first, second := mi.beginContractBaseline("fixture"), mi.beginContractBaseline("fixture")
	require.True(t, mi.contractBaselineActive("fixture"))
	first()
	require.True(t, mi.contractBaselineActive("fixture"))
	second()
	require.False(t, mi.contractBaselineActive("fixture"))
}

// TestContractPendingHeaderRepair: an accept refused after its windows
// closed leaves complete rows and a pending header that no save will accept;
// with an actor waiting on it for a period, the repair rebuilds the
// baseline, accepts the header and announces it once.
func TestContractPendingHeaderRepair(t *testing.T) {
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	headers := repairCount("pending_header")
	consts := "package fixture\nconst RoutePath = \"/held\"\n"
	f.saveHeld("consts.go", consts)
	require.Empty(t, f.store.OpenContractCoreWindows("fixture"), "the held accept closed its windows")
	state, _, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.False(t, state.Accepted, "the header is pending")
	before := f.publishedCount("fixture")
	got := f.strict(t, 20*time.Second)
	require.Equal(t, headers+1, repairCount("pending_header"))
	require.Equal(t, before+1, f.publishedCount("fixture"), "the repaired header is announced once")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(got))
}

// TestContractReceiptWindowsOpenOnlyUnderTheRepositoryLane: every primary
// core-write window opens under its repository's mutation lane, and every
// door but a failed body closes it before the door returns, whichever door
// opens it: the cold batch and the cold track, the incremental door, a
// deletion, the watcher's point patch and its explicit reindex, IndexFile,
// the reconcile lane, the baseline, the repair's reindex and its envelope.
// The repair relies on it: under the lane, every open window is stuck. The
// repair is held off its lane while the doors run, so the lane a window
// opens under is the door's own.
func TestContractReceiptWindowsOpenOnlyUnderTheRepositoryLane(t *testing.T) {
	var mi atomic.Pointer[MultiIndexer]
	var opened, unheld atomic.Int64
	restore := store_sqlite.SetContractCoreWindowOpenHookForTest(func(repo, checkout, path string) {
		owner := mi.Load()
		if owner == nil || checkout != "" {
			return
		}
		opened.Add(1)
		lane := owner.existingRepositoryMutationCoordinator(repo)
		if lane == nil || len(lane.lane) != 0 {
			unheld.Add(1)
			t.Errorf("a core-write window of %s opened without its repository lane", path)
		}
	})
	defer restore()
	doors := make(chan struct{})
	var opening sync.Once
	open := func() { opening.Do(func() { close(doors) }) }
	f := newContractRepairFixture(t, func(_ *contractRepairFixture, r *ContractStuckRepair) {
		r.beforeLane = func(string) { <-doors }
	})
	t.Cleanup(open)
	mi.Store(f.mi)
	closed := func(door string) {
		t.Helper()
		for _, window := range f.store.OpenContractCoreWindows("fixture") {
			require.NotEqual(t, "", window.Checkout, "%s left the window of %s open", door, window.Path)
		}
	}
	touches := 0
	touch := func(name, text string) string {
		path := filepath.Join(f.root, name)
		writeFile(t, path, text)
		touches++
		future := time.Now().Add(time.Duration(touches) * time.Hour)
		require.NoError(t, os.Chtimes(path, future, future))
		return path
	}
	// The cold batch of every tracked repository, and the cold track of one.
	_, err := f.mi.IndexAll()
	require.NoError(t, err)
	closed("IndexAll")
	_, err = f.mi.IndexRepo("fixture")
	require.NoError(t, err)
	closed("IndexRepo")
	f.save("consts.go", jobConsts+"// saved\n")
	closed("IncrementalReindexRepo")
	f.save("extra.go", "package fixture\nfunc Extra() {}\n")
	f.remove("extra.go")
	closed("a deletion")
	// The doors of the repository's own Indexer lane, which the watcher, the
	// git watcher, the poller and the edit tools reach.
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, Paths: []string{idx.rootPath}, DebounceMs: 50}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.patchGraph(touch("consts.go", jobConsts+"// patched\n"), ChangeModified))
	closed("the watcher's point patch")
	_, _, err = w.reindexExplicitMutationPaths([]string{touch("consts.go", jobConsts+"// reindexed\n")})
	require.NoError(t, err)
	closed("the watcher's explicit reindex")
	require.NoError(t, idx.IndexFile(touch("consts.go", jobConsts+"// indexed\n")))
	closed("IndexFile")
	_, err = idx.IncrementalReindexPaths(idx.rootPath, []string{touch("consts.go", jobConsts+"// reconciled\n")})
	require.NoError(t, err)
	closed("the reconcile lane")
	require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), f.options, "fixture"))
	closed("the baseline")
	open()
	f.failBody(t, "helper.go", jobHelper+"// a failed apply\n")
	require.Eventually(t, func() bool { return len(f.store.OpenContractCoreWindows("fixture")) == 0 }, 20*time.Second, 10*time.Millisecond, "the repair's reindex")
	// The repair's pass holds the lane a little past the close.
	var release func()
	require.Eventually(t, func() bool {
		var ok bool
		release, ok = f.mi.TryRepositoryMutationLanes([]string{"fixture"})
		return ok
	}, 20*time.Second, 10*time.Millisecond, "the repair released the lane")
	require.NoError(t, f.r.envelopePaths(withHeldRepositoryLanes(t.Context(), []string{"fixture"}), "fixture", []string{"fixture/helper.go"}))
	release()
	require.Positive(t, opened.Load())
	require.Zero(t, unheld.Load())
}

// TestContractOrphanMarkClearedByASaveIsNotRepaired: a save that accepts the
// file a failed body left in flight, within the orphan grace, closes its
// window and drops its mark: the repair finds nothing and counts nothing.
func TestContractOrphanMarkClearedByASaveIsNotRepaired(t *testing.T) {
	f := newContractRepairFixture(t, nil)
	repaired := repairCount("stuck_receipt")
	f.failBody(t, "consts.go", jobConsts+"// a failed apply\n")
	f.save("consts.go", jobConsts+"// saved again\n")
	require.Empty(t, f.store.OrphanedContractCoreWindows("fixture"))
	require.Empty(t, f.store.OpenContractCoreWindows("fixture"))
	time.Sleep(contractRepairOrphanGrace + 2*contractRepairTestPeriod)
	require.Equal(t, repaired, repairCount("stuck_receipt"), "an accepted save leaves nothing to repair")
}

// TestContractStuckRepairTreatsEveryOpenWindowUnderTheLaneAsStuck: no
// mutation is live while the repair holds the lane, so a window it finds
// open there is reindexed at once, however young: a suspect report of a
// window opened a moment ago is repaired long before a period.
func TestContractStuckRepairTreatsEveryOpenWindowUnderTheLaneAsStuck(t *testing.T) {
	const period = 20 * time.Second
	f := newContractRepairFixture(t, func(_ *contractRepairFixture, r *ContractStuckRepair) { r.period = period })
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	f.stageInFlight("fixture/consts.go")
	require.NotEmpty(t, f.store.OpenContractCoreWindows("fixture"))
	f.r.Suspect("fixture", []string{"fixture/consts.go"})
	require.Eventually(t, func() bool {
		return repairCount("stuck_receipt") == repaired+1 && len(f.store.OpenContractCoreWindows("fixture")) == 0
	}, period/2, 10*time.Millisecond, "the young window is reindexed under the lane")
}

// TestContractJobReportsAWindowBlockedOnOneStampForAPeriod: a job's read
// refused on an open core-write window whose stamp has not moved for a
// repair period reports the path to the repair once; a stamp that moved
// starts the period again, and a quiescent stamp is never reported.
func TestContractJobReportsAWindowBlockedOnOneStampForAPeriod(t *testing.T) {
	var reported [][]string
	c := &contractConvergence{snap: ContractFollowupSnapshot{Suspect: func(paths []string) { reported = append(reported, paths) }}}
	// backdate moves path's first sighting of its stamp a period back.
	backdate := func(path string) {
		sample := c.stuck[path]
		sample.since = sample.since.Add(-ContractRepairPeriod)
		c.stuck[path] = sample
	}
	open := ContractReceiptStamp{Seq: 3, Writing: true}
	c.block("fixture/a.go", open)
	require.Empty(t, reported, "a window just found open is not suspect")
	backdate("fixture/a.go")
	c.block("fixture/a.go", open)
	require.Equal(t, [][]string{{"fixture/a.go"}}, reported, "one unchanged stamp for a period is reported")
	c.block("fixture/a.go", open)
	require.Len(t, reported, 1, "it is reported once")
	moved := ContractReceiptStamp{Seq: 4, Writing: true}
	c.block("fixture/a.go", moved)
	c.block("fixture/a.go", moved)
	require.Len(t, reported, 1, "a moved stamp starts the period again")
	backdate("fixture/a.go")
	c.block("fixture/a.go", moved)
	require.Len(t, reported, 2)
	quiet := ContractReceiptStamp{Seq: 2, Accepted: true}
	c.block("fixture/b.go", quiet)
	require.NotContains(t, c.stuck, "fixture/b.go", "a quiescent stamp waits on no window")
	require.Len(t, reported, 2)
}

// TestContractStuckRepairForgetsAnUntrackedRepository: a report about a
// repository that is not tracked mints no lane, repairs nothing and leaves
// no repair state behind.
func TestContractStuckRepairForgetsAnUntrackedRepository(t *testing.T) {
	f := newContractRepairFixture(t, nil)
	repaired := repairCount("stuck_receipt")
	f.r.Suspect("gone", []string{"gone/a.go"})
	require.Eventually(t, func() bool {
		f.r.mu.Lock()
		defer f.r.mu.Unlock()
		return f.r.repos["gone"] == nil
	}, 10*time.Second, time.Millisecond, "the untracked repository's state is dropped")
	time.Sleep(2 * contractRepairTestPeriod)
	require.Nil(t, f.mi.existingRepositoryMutationCoordinator("gone"), "no lane is minted for it")
	require.Equal(t, repaired, repairCount("stuck_receipt"))
}

// TestContractUnsettledJobRerunsWhenTheRepairFindsNothing: a job that could
// not settle its reads twice, with the repair attached and nothing stuck,
// runs again on the backoff timer: the strict call completes with no repair
// and no Published to run it.
func TestContractUnsettledJobRerunsWhenTheRepairFindsNothing(t *testing.T) {
	f := newContractRepairFixture(t, nil)
	var unsettle, runs atomic.Int64
	execute := f.c.execute
	f.c.run = func(job *contractAnalysisJob) string {
		runs.Add(1)
		if unsettle.Add(-1) >= 0 {
			return viewmetrics.ContractJobSupersededReadUnsettled
		}
		return execute(job)
	}
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	// The save's Published may start the job before the strict call does.
	before := runs.Load()
	unsettle.Store(2)
	f.save("routes.go", routes)
	published := f.publishedCount("fixture")
	got := f.strict(t, 20*time.Second)
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(got))
	require.Equal(t, before+3, runs.Load(), "two unsettled jobs, then the one that published")
	require.Equal(t, published, f.publishedCount("fixture"), "no Published ran it again")
	require.Equal(t, repaired, repairCount("stuck_receipt"))
}

func TestContractRepairAdmissionFailureReleasesEarlierLanes(t *testing.T) {
	mi := &MultiIndexer{}
	held, ok := mi.TryRepositoryMutationLanes([]string{"b"})
	require.True(t, ok)
	var once sync.Once
	releaseHeld := func() { once.Do(held) }
	defer releaseHeld()
	release, ok := mi.TryRepositoryMutationLanes([]string{"b", "a", "a"})
	require.False(t, ok)
	require.Nil(t, release)
	// b remains occupied, so retry a alone to isolate rollback of the
	// successful first acquisition in sorted order.
	first, ok := mi.TryRepositoryMutationLanes([]string{"a"})
	require.True(t, ok, "a failed later lane must release the earlier lane")
	first()
	releaseHeld()
	require.True(t, mi.batchMutationGate.TryLock(), "failed admission must release every batch reader")
	mi.batchMutationGate.Unlock()
	release, ok = mi.TryRepositoryMutationLanes([]string{"a", "b"})
	require.True(t, ok)
	release()
}

func TestContractRepairOrphanCandidateStartsAtItsGraceBoundary(t *testing.T) {
	f := newRouteConstantFixture(t)
	const path = "fixture/consts.go"
	f.stageInFlight(path)
	windows := f.store.OpenContractCoreWindows("fixture")
	require.Len(t, windows, 1)
	f.store.OrphanContractCoreWrites("fixture", "", map[string]uint64{path: windows[0].OpenerSeq})
	windows = f.store.OrphanedContractCoreWindows("fixture")
	require.Len(t, windows, 1)
	mark := windows[0].MarkedAt
	require.False(t, mark.IsZero())
	r := &ContractStuckRepair{options: ContractStuckRepairOptions{Store: f.store}, period: time.Hour}
	boundary := mark.Add(contractRepairOrphanGrace)
	require.Less(t, boundary.Sub(windows[0].OpenedAt), r.period, "ordinary age eligibility must not mask orphan grace")
	require.Empty(t, r.stuckWindows("fixture", boundary.Add(-time.Nanosecond)))
	require.Equal(t, []string{path}, r.stuckWindows("fixture", boundary))
}
