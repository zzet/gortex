package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// policyFixture is a set of tracked repositories, each indexed with an
// accepted namespace header over a current baseline, whose capture options
// own a baseline job runtime.
type policyFixture struct {
	repos     []string
	logs      *observer.ObservedLogs
	manager   *config.ConfigManager
	options   ContractFollowupCaptureOptions
	jobs      *sync.WaitGroup
	published atomic.Int64
	// announcedMu guards announced, the repository of each Published in
	// order.
	announcedMu sync.Mutex
	announced   []string
}

func newPolicyFixture(t *testing.T, repos ...string) *policyFixture {
	t.Helper()
	ctx := t.Context()
	_, store := newSQLiteIndexer(t)
	f := &policyFixture{repos: repos, jobs: &sync.WaitGroup{}}
	cfg := &config.GlobalConfig{}
	for i, repo := range repos {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "route.go"), fmt.Sprintf("package server\nfunc route(r Router) { r.GET(\"/%d\", users) }\nfunc users() {}\n", i))
		writeFile(t, filepath.Join(root, "admin.go"), fmt.Sprintf("package server\nfunc admin(r Router) { r.POST(\"/%d/admin\", users) }\n", i))
		cfg.Repos = append(cfg.Repos, config.RepoEntry{Name: repo, Path: root})
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg.SetConfigPath(path)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(path)
	require.NoError(t, err)
	f.manager = manager
	registry := newTestRegistry()
	mi := NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(_ context.Context, repo, _ string) {
		f.announcedMu.Lock()
		f.announced = append(f.announced, repo)
		f.announcedMu.Unlock()
		f.published.Add(1)
	}})
	_, err = mi.IndexAll()
	require.NoError(t, err)
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()}
	core, logs := observer.New(zap.InfoLevel)
	f.logs = logs
	f.options = ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: registry, Config: config.Default().Index, Logger: zap.New(core), BaselineAdmissionMu: &sync.Mutex{}, BaselineJobs: f.jobs, Yield: func(ctx context.Context) error { return ctx.Err() }}
	for _, repo := range repos {
		require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, repo))
		require.True(t, f.header(t, repo).Accepted)
	}
	f.published.Store(0)
	f.announcedMu.Lock()
	f.announced = nil
	f.announcedMu.Unlock()
	return f
}

// announcedFor returns the repository of each Published so far, in order.
func (f *policyFixture) announcedFor() []string {
	f.announcedMu.Lock()
	defer f.announcedMu.Unlock()
	return append([]string(nil), f.announced...)
}

// excludeIn adds an exclude pattern that matches no file to each repository
// entry, as an operator's edit of config.yaml followed by a reload does: no
// file changes, but every receipt of those repositories now carries an
// earlier extraction policy.
func (f *policyFixture) excludeIn(t *testing.T, repos ...string) {
	t.Helper()
	f.excludeWith(t, "generated-never-present/**", repos...)
}

func (f *policyFixture) excludeWith(t *testing.T, pattern string, repos ...string) {
	t.Helper()
	for i := range f.manager.Global().Repos {
		for _, repo := range repos {
			if f.manager.Global().Repos[i].Name == repo {
				f.manager.Global().Repos[i].Exclude = []string{pattern}
			}
		}
	}
	require.NoError(t, f.manager.Global().Save())
	require.NoError(t, f.manager.Reload())
}

func (f *policyFixture) header(t *testing.T, repo string) graph.ContractInputState {
	t.Helper()
	state, found, err := f.options.Store.AtGeneration(0).ContractInputStateContext(t.Context(), repo, "")
	require.NoError(t, err)
	require.True(t, found)
	return state
}

// rebuilds counts the policy rebuilds admitted so far.
func (f *policyFixture) rebuilds() int {
	return f.logs.FilterMessageSnippet("earlier extraction policy").Len()
}

// holdSlots makes the next two policy jobs wait at their first yield until
// the returned release runs, so both job slots stay taken.
func (f *policyFixture) holdSlots(t *testing.T) (ContractFollowupCaptureOptions, func(), func()) {
	t.Helper()
	gate := make(chan struct{})
	var yields atomic.Int64
	options := f.options
	options.Yield = func(ctx context.Context) error {
		if yields.Add(1) <= 2 {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
		return ctx.Err()
	}
	held := func() {
		require.Eventually(t, func() bool { return yields.Load() >= 2 }, 10*time.Second, 5*time.Millisecond, "both slots are held")
	}
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return options, held, release
}

// capture runs the strict capture over every repository, as the primary
// actor's request does.
func (f *policyFixture) capture(t *testing.T, options ContractFollowupCaptureOptions) error {
	t.Helper()
	inputs := make([]*graphview.SelectedContractInputs, 0, len(f.repos))
	for _, repo := range f.repos {
		input, err := options.Materializer.CaptureContractInputs(t.Context(), nil, repo, "")
		require.NoError(t, err)
		inputs = append(inputs, input)
	}
	selected, err := graphview.ComposeSelectedContractInputs(f.repos[0], "", inputs...)
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), nil, selected, f.repos[0], "")
	if err == nil {
		snapshot.Release()
	}
	return err
}

// TestContractCaptureReportsEveryRepositoryWithAnEarlierPolicy: an exclude
// edit leaves every receipt of the edited repositories under an earlier
// extraction policy. The capture still refuses, but it reports each such
// repository once, all of them in one pass, and none it can admit.
func TestContractCaptureReportsEveryRepositoryWithAnEarlierPolicy(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	var reported []string
	options := f.options
	options.OutdatedPolicy = func(repo string) { reported = append(reported, repo) }
	require.NoError(t, f.capture(t, options))
	require.Empty(t, reported, "current receipts are not reported")

	f.excludeIn(t, "repo-a", "repo-c")
	require.ErrorIs(t, f.capture(t, f.options), graph.ErrContractProjectionStale, "without a reporter the refusal is unchanged")
	require.ErrorIs(t, f.capture(t, options), graph.ErrContractProjectionStale)
	require.Equal(t, []string{"repo-a", "repo-c"}, reported, "each drifted repository once, though both of its files drifted")
}

// TestContractPolicyRebuildConvergesAfterAnExcludeEdit is the regression: an
// exclude edit used to strand every capture of the cohort as stale until each
// file of the edited repositories happened to be saved again, because the
// baseline reconcile left an accepted header over a current baseline alone.
// The capture's report now admits a rebuild of each such namespace, queued
// past the two job slots, and the next capture succeeds.
func TestContractPolicyRebuildConvergesAfterAnExcludeEdit(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	before := make(map[string]graph.ContractInputState)
	for _, repo := range f.repos {
		before[repo] = f.header(t, repo)
	}
	f.excludeIn(t, f.repos...)
	jobOptions, held, release := f.holdSlots(t)
	reconcilers := NewContractBaselineReconcilers(jobOptions)
	options := f.options
	options.OutdatedPolicy = reconcilers.PolicyChanged

	// The reconcile alone cannot help: header accepted, baseline current.
	for _, repo := range f.repos {
		require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), f.options, repo))
	}
	require.ErrorIs(t, f.capture(t, f.options), graph.ErrContractProjectionStale)

	require.ErrorIs(t, f.capture(t, options), graph.ErrContractProjectionStale)
	held()
	require.Equal(t, 2, f.rebuilds(), "the third repository waits for a slot")
	release()
	f.jobs.Wait()
	require.Equal(t, 3, f.rebuilds(), "the queue drained without another report")
	require.NoError(t, f.capture(t, options), "every namespace was rebuilt under the current policy")
	for _, repo := range f.repos {
		after := f.header(t, repo)
		require.True(t, after.Accepted, repo)
		require.NotEqual(t, before[repo].InputFingerprint, after.InputFingerprint, "%s: the rebuild is a new accepted input", repo)
	}
	require.Equal(t, int64(len(f.repos)), f.published.Load(), "each rebuild wakes the consumers once")

	// A repeated report within the retry window starts nothing.
	for _, repo := range f.repos {
		reconcilers.PolicyChanged(repo)
	}
	f.jobs.Wait()
	require.Equal(t, 3, f.rebuilds())
	require.Equal(t, int64(len(f.repos)), f.published.Load())
}

// TestContractPolicyQueueSurvivesADroppedEntry: a queued repository whose
// header goes pending before its turn is dropped on admission; the freed
// slot passes on down the queue instead of stranding the rest.
func TestContractPolicyQueueSurvivesADroppedEntry(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c", "repo-d", "repo-e")
	f.excludeIn(t, f.repos...)
	options, held, release := f.holdSlots(t)
	reconcilers := NewContractBaselineReconcilers(options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-b")
	held()
	for _, repo := range []string{"repo-c", "repo-d", "repo-e"} {
		reconcilers.PolicyChanged(repo)
	}
	// A save of repo-c and repo-d is in progress when their turn comes.
	store := f.options.Store.AtGeneration(0)
	for _, repo := range []string{"repo-c", "repo-d"} {
		current := f.header(t, repo)
		pending := current
		pending.Accepted, pending.InputFingerprint = false, "save-in-progress"
		require.NoError(t, store.BeginContractInputMutationContext(t.Context(), &current, pending, nil))
	}
	before := f.header(t, "repo-e")
	release()
	f.jobs.Wait()
	require.Equal(t, 3, f.rebuilds(), "repo-a, repo-b, then repo-e")
	require.NotEqual(t, before.InputFingerprint, f.header(t, "repo-e").InputFingerprint)
	require.True(t, f.header(t, "repo-e").Accepted)
}

// TestContractPolicyRebuildLeavesACurrentNamespaceAlone: a report about a
// namespace whose receipts are all current rewrites nothing, wakes no one,
// and is not repeated over the same header.
func TestContractPolicyRebuildLeavesACurrentNamespaceAlone(t *testing.T) {
	f := newPolicyFixture(t, "repo-a")
	before := f.header(t, "repo-a")
	rebuilt, err := rebuildPrimaryContractBaseline(t.Context(), f.options, "repo-a", true)
	require.NoError(t, err)
	require.False(t, rebuilt)
	require.Equal(t, before, f.header(t, "repo-a"))

	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	f.jobs.Wait()
	require.Equal(t, 1, f.rebuilds())
	reconcilers.PolicyChanged("repo-a")
	f.jobs.Wait()
	require.Equal(t, 1, f.rebuilds(), "a repeat within the retry window starts no job")
	require.Equal(t, before, f.header(t, "repo-a"))
	require.Zero(t, f.published.Load())

	// Over the same header, an exclude edit is a drift the rebuild finds.
	f.excludeIn(t, "repo-a")
	rebuilt, err = rebuildPrimaryContractBaseline(t.Context(), f.options, "repo-a", true)
	require.NoError(t, err)
	require.True(t, rebuilt)
	require.NotEqual(t, before.InputFingerprint, f.header(t, "repo-a").InputFingerprint)
}

// TestContractPolicyRetryWindowDoublesWhileReportsReturn pins the damping
// of a repository whose receipts keep returning to an earlier policy.
func TestContractPolicyRetryWindowDoublesWhileReportsReturn(t *testing.T) {
	for streak, want := range map[int]time.Duration{0: time.Minute, 1: 2 * time.Minute, 4: 16 * time.Minute, 5: 30 * time.Minute, 40: 30 * time.Minute} {
		require.Equal(t, want, contractBaselinePolicyRun{streak: streak}.window(), "streak %d", streak)
	}
}

// shortPolicyRetry shortens the policy rebuild retry window for one test.
func shortPolicyRetry(t *testing.T, window time.Duration) {
	t.Helper()
	previous := contractBaselinePolicyRetry
	contractBaselinePolicyRetry = window
	t.Cleanup(func() { contractBaselinePolicyRetry = previous })
}

// TestContractPolicyReportHeldByItsWindowIsAdmittedLater: a second edit soon
// after a rebuild is reported inside the window. The report is held, not
// dropped: once the window passes the namespace is rebuilt without another
// report, so a consumer waiting on the refused capture is woken.
func TestContractPolicyReportHeldByItsWindowIsAdmittedLater(t *testing.T) {
	shortPolicyRetry(t, time.Second)
	f := newPolicyFixture(t, "repo-a")
	reconcilers := NewContractBaselineReconcilers(f.options)
	options := f.options
	options.OutdatedPolicy = reconcilers.PolicyChanged
	f.excludeWith(t, "first-edit/**", "repo-a")
	require.ErrorIs(t, f.capture(t, options), graph.ErrContractProjectionStale)
	f.jobs.Wait()
	require.NoError(t, f.capture(t, f.options))
	require.Equal(t, 1, f.rebuilds())

	f.excludeWith(t, "second-edit/**", "repo-a")
	require.ErrorIs(t, f.capture(t, options), graph.ErrContractProjectionStale)
	require.Equal(t, 1, f.rebuilds(), "held by the window")
	require.Eventually(t, func() bool { return f.capture(t, f.options) == nil }, 10*time.Second, 20*time.Millisecond, "rebuilt once the window passed")
	require.Equal(t, 2, f.rebuilds())
	f.jobs.Wait()
}

// TestContractPolicyWindowIsMeasuredFromTheRebuildsEnd: a rebuild that runs
// longer than twice the window still counts the next close report as a
// repeat, so a drift that keeps coming back is damped however long each
// rebuild takes.
func TestContractPolicyWindowIsMeasuredFromTheRebuildsEnd(t *testing.T) {
	shortPolicyRetry(t, 500*time.Millisecond)
	f := newPolicyFixture(t, "repo-a")
	jobOptions, _, release := f.holdSlots(t)
	reconcilers := NewContractBaselineReconcilers(jobOptions)
	f.excludeWith(t, "first-edit/**", "repo-a")
	reconcilers.PolicyChanged("repo-a")
	time.Sleep(2 * time.Second) // the rebuild outlasts twice the window
	release()
	f.jobs.Wait()

	f.excludeWith(t, "second-edit/**", "repo-a")
	reconcilers.PolicyChanged("repo-a")
	require.Eventually(t, func() bool { return f.rebuilds() == 2 }, 10*time.Second, 10*time.Millisecond)
	second := f.logs.FilterMessageSnippet("earlier extraction policy").All()[1].ContextMap()
	require.EqualValues(t, 1, second["repeats"], "the report followed the rebuild's end within twice its window")
	require.Eventually(t, func() bool { return f.capture(t, f.options) == nil }, 10*time.Second, 20*time.Millisecond)
	f.jobs.Wait()
}

// holdLanes holds the repository lanes of repos until the returned release
// runs, so a policy job of one of them waits at its census.
func (f *policyFixture) holdLanes(t *testing.T, repos ...string) func() {
	t.Helper()
	locked, gate := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.options.MultiIndexer.withRepositoryMutationLanes(t.Context(), repos, func() error {
			close(locked)
			<-gate
			return nil
		})
	}()
	<-locked
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(gate)
			require.NoError(t, <-done)
		})
	}
	t.Cleanup(release)
	return release
}

// admissionsDone waits out an admission that has logged its rebuild: it
// holds BaselineAdmissionMu until its job is added to BaselineJobs, so a
// Wait that follows covers that job.
func (f *policyFixture) admissionsDone() {
	f.options.BaselineAdmissionMu.Lock()
	defer f.options.BaselineAdmissionMu.Unlock()
}

// outdateBaseline gives repo's namespace a baseline of an earlier version,
// which Reconcile admits, and returns its current one.
func (f *policyFixture) outdateBaseline(t *testing.T, repo string) graph.ContractBoundaryReceiptBaseline {
	t.Helper()
	baseline, err := f.options.Store.ContractBoundaryReceiptBaselineContext(t.Context(), repo, "")
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.NoError(t, f.options.Store.SetContractBoundaryReceiptBaselineContext(t.Context(), graph.ContractBoundaryReceiptBaseline{RepoPrefix: repo, Version: "outdated", Fingerprint: "outdated"}))
	return *baseline
}

// baselineCurrent reports whether repo's namespace baseline is of the
// current version.
func (f *policyFixture) baselineCurrent(t *testing.T, repo string) bool {
	t.Helper()
	baseline, err := f.options.Store.ContractBoundaryReceiptBaselineContext(t.Context(), repo, "")
	require.NoError(t, err)
	return baseline != nil && baseline.Version == contractBoundaryReceiptVersion
}

// TestContractPolicyJobsWakeAReconcileTheyTurnedAway: a Reconcile request
// that finds both slots taken reports a baseline admitted, so its actor parks
// until a Published of a repository in its cohort. Policy jobs that rebuild
// nothing announce none, and a Published of their own repositories need not
// reach that actor: the request waits for a slot instead, and its own job
// rebuilds the baseline and announces for its repository.
func TestContractPolicyJobsWakeAReconcileTheyTurnedAway(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	f.outdateBaseline(t, "repo-c")
	release := f.holdLanes(t, "repo-a", "repo-b")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-b")
	admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-c", "")
	require.NoError(t, err)
	require.True(t, admitted, "the consumer is told to wait on a baseline")
	require.False(t, f.baselineCurrent(t, "repo-c"))
	release()
	f.jobs.Wait()
	require.Equal(t, 2, f.rebuilds())
	require.Equal(t, []string{"repo-c"}, f.announcedFor(), "the request turned away is woken once, by its own repository")
	require.True(t, f.baselineCurrent(t, "repo-c"), "the request's own job rebuilt its baseline")

	// Woken, its retry finds the baseline current.
	admitted, err = reconcilers.Reconcile(t.Context(), nil, "repo-c", "")
	require.NoError(t, err)
	require.False(t, admitted)
	f.jobs.Wait()
	require.Equal(t, []string{"repo-c"}, f.announcedFor())
}

// TestContractPolicyTurnedAwayReconcileIsCoalesced: a Reconcile request
// turned away again before a slot frees waits as the one already turned
// away, so the namespace is rebuilt and announced once.
func TestContractPolicyTurnedAwayReconcileIsCoalesced(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	f.outdateBaseline(t, "repo-c")
	releaseA := f.holdLanes(t, "repo-a")
	releaseB := f.holdLanes(t, "repo-b")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-b")
	for range 2 {
		admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-c", "")
		require.NoError(t, err)
		require.True(t, admitted)
	}
	releaseA()
	// repo-c's job announces last, once it no longer runs: a second request
	// left waiting would now start another job.
	require.Eventually(t, func() bool { return f.published.Load() >= 1 }, 10*time.Second, 10*time.Millisecond, "repo-a's slot goes to repo-c")
	releaseB()
	f.jobs.Wait()
	require.Equal(t, []string{"repo-c"}, f.announcedFor(), "repo-b's slot finds no second request")
}

// TestContractPolicyTurnedAwayReconcileEndsWithItsOwner: once the owner's
// context has ended, a freed slot starts no job for a request turned away.
func TestContractPolicyTurnedAwayReconcileEndsWithItsOwner(t *testing.T) {
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	f.outdateBaseline(t, "repo-c")
	release := f.holdLanes(t, "repo-a", "repo-b")
	ctx, cancel := context.WithCancel(t.Context())
	options := f.options
	options.Context = ctx
	reconcilers := NewContractBaselineReconcilers(options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-b")
	admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-c", "")
	require.NoError(t, err)
	require.True(t, admitted)
	cancel()
	release()
	f.jobs.Wait()
	require.NotContains(t, f.announcedFor(), "repo-c")
	require.False(t, f.baselineCurrent(t, "repo-c"))
}

// TestContractPolicyJobAnnouncesToACoalescedReconcile: a Reconcile request
// that coalesced onto a policy job waits on its Published, even when the job
// then finds every receipt current.
func TestContractPolicyJobAnnouncesToACoalescedReconcile(t *testing.T) {
	f := newPolicyFixture(t, "repo-a")
	release := f.holdLanes(t, "repo-a")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	// The request reads a baseline a moment before it turns current again.
	current := f.outdateBaseline(t, "repo-a")
	admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-a", "")
	require.NoError(t, err)
	require.True(t, admitted)
	require.NoError(t, f.options.Store.SetContractBoundaryReceiptBaselineContext(t.Context(), current))
	release()
	f.jobs.Wait()
	require.Equal(t, 1, f.rebuilds())
	require.EqualValues(t, 1, f.published.Load())
}

// TestContractPolicyReportCoalescedOntoACurrentRebuildIsAskedAgain: a report
// that arrived while a policy job ran saw a drift that job's census may have
// missed; when the job finds nothing, the report is admitted again once the
// window has passed.
func TestContractPolicyReportCoalescedOntoACurrentRebuildIsAskedAgain(t *testing.T) {
	shortPolicyRetry(t, 500*time.Millisecond)
	f := newPolicyFixture(t, "repo-a")
	release := f.holdLanes(t, "repo-a")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-a")
	require.Equal(t, 1, f.rebuilds(), "the second report coalesced")
	release()
	require.Eventually(t, func() bool { return f.rebuilds() == 2 }, 10*time.Second, 10*time.Millisecond, "admitted again after the window")
	f.admissionsDone()
	f.jobs.Wait()
	require.Zero(t, f.published.Load())
}

// TestContractPolicyReportIsAskedAgainWhenTheJobAnnouncesForAReconcile: a
// policy job that finds nothing announces for a Reconcile request coalesced
// onto it, but that announcement is not an answer to a policy report
// coalesced onto it as well: the report is still admitted again once the
// window has passed, with no capture asking.
func TestContractPolicyReportIsAskedAgainWhenTheJobAnnouncesForAReconcile(t *testing.T) {
	shortPolicyRetry(t, 500*time.Millisecond)
	f := newPolicyFixture(t, "repo-a")
	release := f.holdLanes(t, "repo-a")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-a")
	require.Equal(t, 1, f.rebuilds(), "the second report coalesced")
	current := f.outdateBaseline(t, "repo-a")
	admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-a", "")
	require.NoError(t, err)
	require.True(t, admitted)
	require.NoError(t, f.options.Store.SetContractBoundaryReceiptBaselineContext(t.Context(), current))
	release()
	require.Eventually(t, func() bool { return f.published.Load() == 1 }, 10*time.Second, 10*time.Millisecond, "the job announces for the coalesced Reconcile")
	require.Eventually(t, func() bool { return f.rebuilds() == 2 }, 10*time.Second, 10*time.Millisecond, "the coalesced report is admitted again after the window")
	f.admissionsDone()
	f.jobs.Wait()
	require.Equal(t, []string{"repo-a"}, f.announcedFor())
}

// TestContractPolicyReportIsAskedAgainWhenTheSlotGoesToAReconcile: the job
// that frees a slot hands it to a Reconcile request it turned away, whose job
// announces for its own repository. A report coalesced onto the freeing job,
// which found nothing, is still admitted again once the window has passed,
// with no capture asking.
func TestContractPolicyReportIsAskedAgainWhenTheSlotGoesToAReconcile(t *testing.T) {
	shortPolicyRetry(t, 500*time.Millisecond)
	f := newPolicyFixture(t, "repo-a", "repo-b", "repo-c")
	f.outdateBaseline(t, "repo-c")
	releaseA := f.holdLanes(t, "repo-a")
	releaseB := f.holdLanes(t, "repo-b")
	reconcilers := NewContractBaselineReconcilers(f.options)
	reconcilers.PolicyChanged("repo-a")
	reconcilers.PolicyChanged("repo-b")
	reconcilers.PolicyChanged("repo-a")
	require.Equal(t, 2, f.rebuilds(), "the third report coalesced")
	admitted, err := reconcilers.Reconcile(t.Context(), nil, "repo-c", "")
	require.NoError(t, err)
	require.True(t, admitted, "the consumer is told to wait on a baseline")
	releaseA()
	require.Eventually(t, func() bool { return f.published.Load() == 1 }, 10*time.Second, 10*time.Millisecond, "repo-a's slot goes to the request it turned away")
	require.Eventually(t, func() bool { return f.rebuilds() == 3 }, 10*time.Second, 10*time.Millisecond, "the coalesced report is admitted again after the window")
	f.admissionsDone()
	releaseB()
	f.jobs.Wait()
	require.Equal(t, []string{"repo-c"}, f.announcedFor())
}
