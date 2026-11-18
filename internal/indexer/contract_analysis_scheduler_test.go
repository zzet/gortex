package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// schedulerProbe drives a coordinator whose captures and job bodies are
// fakes: a capture names one target per actor and key, a job records its
// start, waits on its key's gate and ends with the outcome the test chose.
type schedulerProbe struct {
	t         *testing.T
	f         *contractWorkerFixture
	c         *ContractAnalysisCoordinator
	mu        sync.Mutex
	starts    []string
	triggers  []string
	startedAt []time.Time
	ends      []time.Time
	gates     map[string]chan struct{}
	running   map[string]int
	// maxActor is the most jobs one actor ran at once, maxAll across all.
	maxActor, all, maxAll int
	jobs                  map[string]int
	// outcome chooses each job's end by key and the key's job count.
	outcome func(key string, n int) string
	// captureErr, when set, refuses a key's capture.
	captureErr func(key string, n int) error
	captures   map[string]int
	releases   atomic.Int64
	// inputs is what a primary actor's recapture finds, by repo.
	inputs map[string]string
}

func newSchedulerProbe(t *testing.T, adjust func(*ContractAnalysisCoordinatorOptions)) *schedulerProbe {
	t.Helper()
	p := &schedulerProbe{t: t, f: newContractWorkerFixture(t), gates: make(map[string]chan struct{}), running: make(map[string]int), jobs: make(map[string]int), captures: make(map[string]int), inputs: make(map[string]string)}
	options := ContractAnalysisCoordinatorOptions{Store: p.f.store, Leases: p.f.leases, Registry: p.f.registry, Config: p.f.cfg, Capture: p.capture, Yield: func(ctx context.Context) error { return ctx.Err() }}
	if adjust != nil {
		adjust(&options)
	}
	c, err := NewContractAnalysisCoordinator(options)
	require.NoError(t, err)
	c.run = p.run
	p.c = c
	t.Cleanup(func() {
		p.mu.Lock()
		for key, gate := range p.gates {
			select {
			case <-gate:
			default:
				close(gate)
			}
			delete(p.gates, key)
		}
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, c.CloseContext(ctx))
	})
	return p
}

// schedulerInputs is a cohort's accepted inputs at key.
func schedulerInputs(repos []string, key string) *graphview.SelectedContractInputs {
	witnesses := make([]graph.ContractInputWitness, 0, len(repos))
	for _, repo := range repos {
		witnesses = append(witnesses, graph.ContractInputWitness{State: graph.ContractInputState{RepoPrefix: repo, InputVersion: "scheduler-v", InputFingerprint: key, Accepted: true}, Found: true})
	}
	return &graphview.SelectedContractInputs{State: graph.ContractInputState{RepoPrefix: repos[0], InputVersion: "scheduler-v", InputFingerprint: key, Accepted: true}, Witnesses: witnesses}
}

func (p *schedulerProbe) capture(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
	defer source.Release()
	label := repo + "/" + checkout + "@" + inputs.State.InputFingerprint
	p.mu.Lock()
	p.captures[label]++
	n, refuse := p.captures[label], p.captureErr
	p.mu.Unlock()
	if refuse != nil {
		if err := refuse(label, n); err != nil {
			return ContractFollowupSnapshot{}, nil, err
		}
	}
	key := graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout, InputVersion: "scheduler-v", InputFingerprint: label}
	snapshot := ContractFollowupSnapshot{Key: key, Core: p.f.store, Inputs: inputs.Witnesses, Release: func() { p.releases.Add(1) }}
	return snapshot, []ContractFollowupTarget{{Key: key, Catalog: p.f.store}}, ctx.Err()
}

func (p *schedulerProbe) gate(label string) chan struct{} {
	gate := p.gates[label]
	if gate == nil {
		gate = make(chan struct{})
		close(gate)
		p.gates[label] = gate
	}
	return gate
}

// hold makes the jobs at label wait until release(label).
func (p *schedulerProbe) hold(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gates[label] = make(chan struct{})
}

func (p *schedulerProbe) release(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gate := p.gates[label]; gate != nil {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}
}

func (p *schedulerProbe) run(job *contractAnalysisJob) string {
	label := job.snapshot.Key.InputFingerprint
	// The actor is the job's checkout and cohort, whichever repository
	// the request selected.
	actor := job.snapshot.Key.CheckoutID + "|"
	for _, witness := range job.snapshot.Inputs {
		actor += witness.State.RepoPrefix + ","
	}
	defer job.snapshot.Release()
	p.mu.Lock()
	p.starts = append(p.starts, label)
	p.triggers = append(p.triggers, job.trigger)
	p.startedAt = append(p.startedAt, time.Now())
	p.running[actor]++
	p.maxActor = max(p.maxActor, p.running[actor])
	p.all++
	p.maxAll = max(p.maxAll, p.all)
	p.jobs[label]++
	n := p.jobs[label]
	gate := p.gate(label)
	outcome := p.outcome
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running[actor]--
		p.all--
		p.ends = append(p.ends, time.Now())
		p.mu.Unlock()
	}()
	select {
	case <-gate:
	case <-p.c.ctx.Done():
		return viewmetrics.ContractJobPreempted
	}
	if outcome == nil {
		return viewmetrics.ContractJobPublished
	}
	return outcome(label, n)
}

func (p *schedulerProbe) started() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...)
}

func (p *schedulerProbe) waitStarts(n int) []string {
	p.t.Helper()
	require.Eventually(p.t, func() bool { return len(p.started()) >= n }, 10*time.Second, time.Millisecond, "starts: %v", p.started())
	return p.started()
}

func (p *schedulerProbe) schedule(checkout string, repos []string, selected, key string) ContractScheduleResult {
	p.t.Helper()
	result, err := p.c.Schedule(p.t.Context(), ContractScheduleRequest{Actor: NewContractActor(checkout, repos), Key: graph.ContractAttachmentKey{RepoPrefix: selected, CheckoutID: checkout}, Inputs: schedulerInputs(repos, key), Epoch: p.c.InputEpoch()})
	require.NoError(p.t, err)
	return result
}

func (p *schedulerProbe) wait(result ContractScheduleResult) ContractActorStatus {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.t.Context(), 30*time.Second)
	defer cancel()
	status, err := p.c.WaitActor(ctx, result.Actor, result.ArrivalSeq)
	require.NoError(p.t, err)
	return status
}

// TestContractScheduleDoesNoStoreOrCaptureWork: a request records interest
// and returns while another actor's capture is blocked; no capture runs on
// the request's goroutine.
func TestContractScheduleDoesNoStoreOrCaptureWork(t *testing.T) {
	scheduleWithCaptureBlocked(t, nil)
}

func scheduleWithCaptureBlocked(t *testing.T, record func(time.Duration)) {
	t.Helper()
	blocked := make(chan struct{})
	entered := make(chan struct{}, 64)
	p := newSchedulerProbe(t, nil)
	p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		entered <- struct{}{}
		select {
		case <-blocked:
		case <-ctx.Done():
		}
		return p.capture(ctx, source, inputs, repo, checkout)
	}
	defer close(blocked)
	scheduled := make(chan error, 1)
	go func() {
		_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("", []string{"a"}), Key: graph.ContractAttachmentKey{RepoPrefix: "a"}, Inputs: schedulerInputs([]string{"a"}, "k0")})
		scheduled <- err
	}()
	select {
	case err := <-scheduled:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("a request waited on its own capture")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first capture never started")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			var start time.Time
			if record != nil {
				start = time.Now()
			}
			_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("", []string{fmt.Sprintf("b%d", i%4)}), Key: graph.ContractAttachmentKey{RepoPrefix: "b"}, Inputs: schedulerInputs([]string{fmt.Sprintf("b%d", i%4)}, fmt.Sprintf("k%d", i))})
			if record != nil {
				record(time.Since(start))
			}
			if err != nil && !errors.Is(err, ErrContractQueueFull) {
				t.Error(err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a request waited on a blocked capture")
	}
}

// TestContractSchedulerRoundRobinAcrossActors: three actors of one home ring
// and one slot start in the order they were queued; an actor that asks for
// a rerun while it runs goes to the back.
func TestContractSchedulerRoundRobinAcrossActors(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	a, b, c := []string{"r", "x1"}, []string{"r", "x2"}, []string{"r", "x3"}
	p.hold("r/@a1")
	p.schedule("", a, "r", "a1")
	p.waitStarts(1)
	p.schedule("", a, "r", "a2")
	p.schedule("", b, "r", "b1")
	p.schedule("", c, "r", "c1")
	p.release("r/@a1")
	require.Equal(t, []string{"r/@a1", "r/@b1", "r/@c1", "r/@a2"}, p.waitStarts(4))
}

// TestContractSchedulerSharesTheJobSlotFairlyAcrossRepos: one global slot,
// granted in the order the dispatchers asked: a repository with many queued
// actors cannot starve another.
func TestContractSchedulerSharesTheJobSlotFairlyAcrossRepos(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("x/@x1")
	p.schedule("", []string{"x", "x1"}, "x", "x1")
	p.waitStarts(1)
	p.schedule("", []string{"x", "x2"}, "x", "x2")
	p.schedule("", []string{"x", "x3"}, "x", "x3")
	p.schedule("", []string{"x", "x4"}, "x", "x4")
	p.schedule("", []string{"y", "y1"}, "y", "y1")
	// Both dispatchers ask for the slot before it frees.
	require.Eventually(t, func() bool {
		p.c.slots.mu.Lock()
		defer p.c.slots.mu.Unlock()
		return len(p.c.slots.waiters) == 2
	}, 10*time.Second, time.Millisecond)
	p.release("x/@x1")
	starts := p.waitStarts(5)
	index := -1
	for i, label := range starts {
		if label == "y/@y1" {
			index = i
		}
	}
	require.LessOrEqual(t, index, 2, "the other repository's actor runs within one job of the slot freeing: %v", starts)
	require.Equal(t, 1, p.maxAll, "one global job slot")
}

// TestContractSchedulerRunsAtMostOneJobPerActor: with spare slots, an actor
// reached through two selected repositories and asked for new keys while it
// runs never runs two jobs at once.
func TestContractSchedulerRunsAtMostOneJobPerActor(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) { options.JobSlots = 4 })
	cohort := []string{"r1", "r2"}
	p.hold("r1/@k1")
	first := p.schedule("", cohort, "r1", "k1")
	p.waitStarts(1)
	second := p.schedule("", cohort, "r2", "k2")
	require.Equal(t, first.Actor, second.Actor, "the selected repository is not part of the actor")
	require.Equal(t, ContractRunning, second.Result)
	third := p.schedule("", cohort, "r2", "k3")
	require.Equal(t, ContractRunning, third.Result)
	time.Sleep(100 * time.Millisecond)
	require.Len(t, p.started(), 1, "no second job starts while the first runs")
	p.release("r1/@k1")
	require.Equal(t, ContractActorPublished, p.wait(third).Result)
	require.Equal(t, 1, p.maxActor, "at most one job per actor")
	require.Equal(t, []string{"r1/@k1", "r2/@k3"}, p.started())
}

// TestContractSchedulerCoalescesInterestAtAnUnchangedKey: requests at the
// running job's key join it; none reruns it.
func TestContractSchedulerCoalescesInterestAtAnUnchangedKey(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("r/@k")
	first := p.schedule("", []string{"r"}, "r", "k")
	p.waitStarts(1)
	for range 10 {
		again := p.schedule("", []string{"r"}, "r", "k")
		require.Equal(t, ContractRunning, again.Result)
		require.Equal(t, first.ArrivalSeq, again.ArrivalSeq)
	}
	p.release("r/@k")
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, []string{"r/@k"}, p.started(), "no rerun at an unchanged key")
}

// TestContractSchedulerPublicationBeforeTheWaitResolvesIt: a job that
// publishes between Schedule and WaitActor resolves the wait at once; the
// promise is a level, not an edge.
func TestContractSchedulerPublicationBeforeTheWaitResolvesIt(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	scheduled := p.schedule("", []string{"r"}, "r", "k")
	require.Eventually(t, func() bool {
		return len(p.started()) == 1 && p.c.Status(scheduled.Actor).State == contractActorIdle.String()
	}, 10*time.Second, time.Millisecond)
	require.Equal(t, ContractActorPublished, p.wait(scheduled).Result)
}

// TestContractSchedulerRerunsOnceAtTheNewestKey: keys that arrive while a
// job runs rerun it once, at the newest; each replaced key's source is
// released exactly once.
func TestContractSchedulerRerunsOnceAtTheNewestKey(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	cohort := []string{"r"}
	p.hold("r/wt@k1")
	request := func(key string, released *atomic.Int64) ContractScheduleResult {
		source := &ContractCaptureSource{onRelease: func() { released.Add(1) }}
		result, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("wt", cohort), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: "wt"}, Inputs: schedulerInputs(cohort, key), Source: source, Epoch: p.c.InputEpoch()})
		require.NoError(t, err)
		return result
	}
	var first, second, third atomic.Int64
	request("k1", &first)
	p.waitStarts(1)
	request("k2", &second)
	newest := request("k3", &third)
	p.release("r/wt@k1")
	require.Equal(t, ContractActorPublished, p.wait(newest).Result)
	require.Equal(t, []string{"r/wt@k1", "r/wt@k3"}, p.started(), "one rerun, at the newest key")
	require.EqualValues(t, 1, second.Load(), "the replaced key's source is released once")
	require.EqualValues(t, 1, first.Load())
	require.EqualValues(t, 1, third.Load())
}

// TestContractSchedulerOlderVectorDoesNotRegressWant: a request whose inputs
// were captured before a Published-driven recapture cannot replace the
// newer inputs.
func TestContractSchedulerOlderVectorDoesNotRegressWant(t *testing.T) {
	var p *schedulerProbe
	p = newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return schedulerInputs(repos, p.inputs[repo]), nil
		}
	})
	cohort := []string{"r"}
	stale := p.c.InputEpoch()
	first := p.schedule("", cohort, "r", "k1")
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	p.hold("r/@k2")
	p.mu.Lock()
	p.inputs["r"] = "k2"
	p.mu.Unlock()
	p.c.Published(t.Context(), "r", "")
	p.waitStarts(2)
	older, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: first.Actor, Key: graph.ContractAttachmentKey{RepoPrefix: "r"}, Inputs: schedulerInputs(cohort, "k1"), Epoch: stale})
	require.NoError(t, err)
	require.Greater(t, older.ArrivalSeq, first.ArrivalSeq, "the older request waits on the newer inputs")
	p.release("r/@k2")
	require.Equal(t, ContractActorPublished, p.wait(older).Result)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, []string{"r/@k1", "r/@k2"}, p.started(), "the older vector never reruns")
}

// TestContractSchedulerNoopIsNotAJob: inputs already published answer at
// once, with no job counted, both from the actor's own last publication and,
// after a restart with no such memory, from the stored attachments.
func TestContractSchedulerNoopIsNotAJob(t *testing.T) {
	jobs := func() int64 {
		var total int64
		for _, outcome := range []string{viewmetrics.ContractJobPublished, viewmetrics.ContractJobFailed, viewmetrics.ContractJobPanic} {
			total += contractCounter("views_contract_analysis_jobs_total{mode=cold,outcome=" + outcome + "}")
		}
		return total
	}
	p := newSchedulerProbe(t, nil)
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	before := jobs()
	again := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractNoop, again.Result)
	require.Equal(t, ContractActorPublished, p.wait(again).Result)
	require.Equal(t, before, jobs())

	// A restarted scheduler has no last publication: the attachments the
	// store holds answer for it, before any capture.
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "noop-a")
	b := f.request(t, "repo-b", "noop-b")
	state, err := graph.ComposeContractInputState("repo-a", "", f.inputs)
	require.NoError(t, err)
	inputs := &graphview.SelectedContractInputs{State: state, Witnesses: f.inputs}
	var captures, noopNow atomic.Int64
	noopBase := time.Now()
	options := ContractAnalysisCoordinatorOptions{Now: func() time.Time { return noopBase.Add(time.Duration(noopNow.Load())) }, Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Yield: func(ctx context.Context) error { return ctx.Err() },
		Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
			captures.Add(1)
			snapshot := a.Snapshot
			snapshot.Release = func() {}
			snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
				return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
			}
			return snapshot, []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: f.store}, {Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: f.store}}, nil
		}}
	cohort := []string{"repo-a", "repo-b"}
	publisher, err := NewContractAnalysisCoordinator(options)
	require.NoError(t, err)
	_, status := scheduleAndWait(t, t.Context(), publisher, cohort, "repo-a", inputs)
	require.Equal(t, ContractActorPublished, status.Result)
	require.NoError(t, publisher.Close())
	require.EqualValues(t, 1, captures.Load())
	restarted, err := NewContractAnalysisCoordinator(options)
	require.NoError(t, err)
	defer restarted.Close()
	before = jobs()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	scheduled, status := scheduleAndWait(t, ctx, restarted, cohort, "repo-a", inputs)
	require.Equal(t, ContractScheduled, scheduled.Result)
	require.Equal(t, ContractActorPublished, status.Result, "an attachment-backed noop satisfies publication waiters")
	require.Equal(t, ContractActorNoop, restarted.Status(scheduled.Actor).Result, "the restarted actor actually took endNoop from stored attachments")
	noopNow.Store(int64(contractInterestTTL + time.Second))
	renewed, expired := scheduleAndWait(t, ctx, restarted, cohort, "repo-a", inputs)
	require.Equal(t, scheduled.ArrivalSeq, renewed.ArrivalSeq)
	require.Equal(t, ContractNoop, renewed.Result)
	require.Equal(t, ContractActorPublished, expired.Result)
	require.Equal(t, ContractActorNoop, restarted.Status(scheduled.Actor).Result)
	require.EqualValues(t, 1, captures.Load(), "a noop captures nothing")
	require.Equal(t, before, jobs(), "a noop is not a job")
}

// TestContractSchedulerPublishedStartsAnInterestedPrimaryActor: while a
// request's interest lasts, a cohort Published captures a primary actor's
// inputs again and starts its job; the Published-driven job does not extend
// the interest, and once it ran out a Published starts nothing.
func TestContractSchedulerPublishedStartsAnInterestedPrimaryActor(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	var p *schedulerProbe
	p = newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = func() time.Time { return base.Add(time.Duration(now.Load())) }
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return schedulerInputs(repos, p.inputs[repo]), nil
		}
	})
	cohort := []string{"r", "s"}
	first := p.schedule("", cohort, "r", "k1")
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	now.Store(int64(30 * time.Second))
	p.mu.Lock()
	p.inputs["r"] = "k2"
	p.mu.Unlock()
	p.c.Published(t.Context(), "s", "")
	starts := p.waitStarts(2)
	require.Equal(t, "r/@k2", starts[1])
	p.mu.Lock()
	require.Equal(t, contractJobTriggerPublished, p.triggers[1], "the accept queued the job")
	p.mu.Unlock()
	require.Eventually(t, func() bool { return p.c.Status(first.Actor).State == contractActorIdle.String() }, 10*time.Second, time.Millisecond)
	now.Store(int64(61 * time.Second))
	p.mu.Lock()
	p.inputs["r"] = "k3"
	p.mu.Unlock()
	p.c.Published(t.Context(), "r", "")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, p.started(), 2, "an expired interest starts nothing")
}

func TestContractSchedulerRenewsExpiredBaselineInterest(t *testing.T) {
	var now, admissions atomic.Int64
	base := time.Now()
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = func() time.Time { return base.Add(time.Duration(now.Load())) }
		options.ReconcileBaseline = func(context.Context, *graphview.RepoView, string, string) (bool, error) {
			admissions.Add(1)
			return true, nil
		}
	})
	req := ContractScheduleRequest{Actor: NewContractActor("", []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r"}}
	first, err := p.c.Schedule(t.Context(), req)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return admissions.Load() == 1 && p.c.Status(first.Actor).State == contractActorDeferred.String()
	}, 10*time.Second, time.Millisecond)
	now.Store(int64(contractInterestTTL + time.Second))
	renewed, err := p.c.Schedule(t.Context(), req)
	require.NoError(t, err)
	require.Greater(t, renewed.ArrivalSeq, first.ArrivalSeq)
	require.Eventually(t, func() bool {
		return admissions.Load() == 2 && p.c.Status(first.Actor).State == contractActorDeferred.String()
	}, 10*time.Second, time.Millisecond)
	later := p.schedule("", []string{"r"}, "r", "accepted")
	require.Equal(t, ContractActorPublished, p.wait(later).Result)
}

// A primary stale capture leaves its promise unresolved while awaiting an
// input event. Expiring that wait must still permit a fresh equal-key RPC.
func TestContractSchedulerRenewsExpiredStaleCaptureInterest(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = func() time.Time { return base.Add(time.Duration(now.Load())) }
		options.Inputs = func(_ context.Context, repos []string, _ string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	p.captureErr = func(_ string, n int) error {
		if n == 1 {
			return graph.ErrContractProjectionStale
		}
		return nil
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Eventually(t, func() bool {
		p.c.mu.Lock()
		defer p.c.mu.Unlock()
		a := p.c.actors[first.Actor]
		return a.state == contractActorIdle && a.retryOnInputs && a.resolved < first.ArrivalSeq
	}, 10*time.Second, time.Millisecond, "the actual stale capture must suspend its unresolved promise")
	now.Store(int64(contractInterestTTL + time.Second))
	renewed := p.schedule("", []string{"r"}, "r", "k")
	require.Greater(t, renewed.ArrivalSeq, first.ArrivalSeq)
	require.Equal(t, ContractActorPublished, p.wait(renewed).Result)
	require.Len(t, p.started(), 1, "only the renewed capture starts a job")
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Equal(t, 2, p.captures["r/@k"], "one stale capture and one renewed capture")
}

// Expiration only renews suspended work, not terminal equal-key results.
func TestContractSchedulerExpiredTerminalInterestStaysResolved(t *testing.T) {
	for _, outcome := range []string{viewmetrics.ContractJobFailed, viewmetrics.ContractJobPublished} {
		t.Run(outcome, func(t *testing.T) {
			var now atomic.Int64
			base := time.Now()
			p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
				options.Now = func() time.Time { return base.Add(time.Duration(now.Load())) }
			})
			p.outcome = func(string, int) string { return outcome }
			first := p.schedule("", []string{"r"}, "r", "k")
			result := p.wait(first).Result
			expected := ContractActorFailed
			if outcome == viewmetrics.ContractJobPublished {
				expected = ContractActorPublished
			}
			require.Equal(t, expected, result)
			now.Store(int64(contractInterestTTL + time.Second))
			again := p.schedule("", []string{"r"}, "r", "k")
			require.Equal(t, first.ArrivalSeq, again.ArrivalSeq)
			if outcome == viewmetrics.ContractJobPublished {
				require.Equal(t, ContractNoop, again.Result, "a published key remains an immediate noop after expiry")
			}
			require.Equal(t, result, p.wait(again).Result)
			require.Len(t, p.started(), 1)
		})
	}
}

func TestContractSchedulerRenewsExpiredBackoffInterest(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = func() time.Time { return base.Add(time.Duration(now.Load())) }
	})
	p.outcome = func(_ string, n int) string {
		if n <= 2 {
			return viewmetrics.ContractJobSupersededPublishRefused
		}
		return viewmetrics.ContractJobPublished
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Eventually(t, func() bool { return p.c.Status(first.Actor).State == contractActorBackoff.String() }, 10*time.Second, time.Millisecond)
	now.Store(int64(contractInterestTTL + time.Second))
	renewed := p.schedule("", []string{"r"}, "r", "k")
	require.Greater(t, renewed.ArrivalSeq, first.ArrivalSeq)
	require.Equal(t, ContractActorPublished, p.wait(renewed).Result)
	require.Len(t, p.started(), 3)
}

// TestContractSchedulerRequeuesOnceThenBacksOff: a job that could not settle
// its reads, or publish, is queued again once at the back, then retried on
// a timer.
func TestContractSchedulerRequeuesOnceThenBacksOff(t *testing.T) {
	for _, outcome := range []string{viewmetrics.ContractJobSupersededReadUnsettled, viewmetrics.ContractJobSupersededPublishRefused} {
		t.Run(outcome, func(t *testing.T) {
			p := newSchedulerProbe(t, nil)
			p.outcome = func(_ string, n int) string {
				if n <= 2 {
					return outcome
				}
				return viewmetrics.ContractJobPublished
			}
			requeued := contractCounter("views_contract_analysis_requeued_total")
			first := p.schedule("", []string{"r"}, "r", "k")
			require.Equal(t, ContractActorPublished, p.wait(first).Result)
			require.Len(t, p.started(), 3)
			require.Equal(t, requeued+1, contractCounter("views_contract_analysis_requeued_total"), "one requeue")
			p.mu.Lock()
			defer p.mu.Unlock()
			require.Equal(t, []string{contractJobTriggerInterest, contractJobTriggerRequeue, contractJobTriggerTimer}, p.triggers)
			require.GreaterOrEqual(t, p.startedAt[2].Sub(p.ends[1]), contractBackoffBase, "the retry after the requeue waits for its timer")
		})
	}
}

// TestContractSchedulerDoesNotRerunAFailedJobOnATimer: a failed job reruns
// on a key change only, not on a timer, a Published or a request at its key.
func TestContractSchedulerDoesNotRerunAFailedJobOnATimer(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	p.outcome = func(string, int) string { return viewmetrics.ContractJobFailed }
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorFailed, p.wait(first).Result)
	p.c.Published(t.Context(), "r", "")
	time.Sleep(2500 * time.Millisecond)
	require.Len(t, p.started(), 1, "no timed or Published rerun of a failed job")
	again := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, first.ArrivalSeq, again.ArrivalSeq)
	require.Equal(t, ContractActorFailed, p.wait(again).Result)
	p.mu.Lock()
	p.outcome = nil
	p.mu.Unlock()
	moved := p.schedule("", []string{"r"}, "r", "k2")
	require.Equal(t, ContractActorPublished, p.wait(moved).Result)
	require.Len(t, p.started(), 2)
}

// TestContractSchedulerDefersRequeueToAPendingRepair: once its requeue is
// spent, a job that could not settle its reads waits for a pending repair's
// accept instead of a timer.
func TestContractSchedulerDefersRequeueToAPendingRepair(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	var asked atomic.Int64
	p.c.repairPending = func([]string) bool { asked.Add(1); return true }
	p.outcome = func(_ string, n int) string {
		if n <= 2 {
			return viewmetrics.ContractJobSupersededReadUnsettled
		}
		return viewmetrics.ContractJobPublished
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	p.waitStarts(2)
	time.Sleep(2500 * time.Millisecond)
	require.Len(t, p.started(), 2, "no timer while a repair is pending")
	require.Positive(t, asked.Load())
	p.c.Published(t.Context(), "r", "")
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	require.Len(t, p.started(), 3)
}

// TestContractSchedulerBacksOffWhenARepairPassRepairsNothing: an actor that
// deferred its unsettled requeue to a due repair is backed off on the timer
// once a pass that started after the deferral ends without a Published; a
// pass that started before it does not count.
func TestContractSchedulerBacksOffWhenARepairPassRepairsNothing(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	p.c.repairPending = func([]string) bool { return true }
	p.outcome = func(_ string, n int) string {
		if n <= 2 {
			return viewmetrics.ContractJobSupersededReadUnsettled
		}
		return viewmetrics.ContractJobPublished
	}
	// A pass already running when the actor defers.
	p.c.repairPassStarted("r")
	first := p.schedule("", []string{"r"}, "r", "k")
	p.waitStarts(2)
	require.Eventually(t, func() bool { return p.c.Status(first.Actor).State == contractActorIdle.String() }, 10*time.Second, time.Millisecond)
	p.c.repairPassEnded("r")
	time.Sleep(1500 * time.Millisecond)
	require.Len(t, p.started(), 2, "a pass that started before the deferral does not back the actor off")
	p.c.repairPassStarted("r")
	p.c.repairPassEnded("r")
	require.Equal(t, contractActorBackoff.String(), p.c.Status(first.Actor).State, "a pass that repaired nothing backs the actor off")
	require.Equal(t, ContractActorPublished, p.wait(first).Result, "the timer runs it again without a Published")
	require.Len(t, p.started(), 3)
}

// TestContractSchedulerDoesNotDeferToTheScanItsOwnEndAsks: the scan an
// unsettled end asks the repair for is not a repair already due; with none
// due, the actor backs off on the timer at once.
func TestContractSchedulerDoesNotDeferToTheScanItsOwnEndAsks(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	// The repair is pending exactly while a scan it was asked for is.
	var scans atomic.Int64
	p.c.unsettled = func([]string) { scans.Add(1) }
	p.c.repairPending = func([]string) bool { return scans.Load() > 1 }
	p.outcome = func(_ string, n int) string {
		if n <= 2 {
			return viewmetrics.ContractJobSupersededReadUnsettled
		}
		return viewmetrics.ContractJobPublished
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorPublished, p.wait(first).Result, "the timer runs it again")
	require.Len(t, p.started(), 3)
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Equal(t, []string{contractJobTriggerInterest, contractJobTriggerRequeue, contractJobTriggerTimer}, p.triggers)
}

// TestContractSchedulerStallsAfterThreeNonPublishingEnds: three consecutive
// jobs that did not publish flag the actor stalled and resolve its waiters;
// the next Published schedules it again.
func TestContractSchedulerStallsAfterThreeNonPublishingEnds(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	p.outcome = func(_ string, n int) string {
		if n <= 3 {
			return viewmetrics.ContractJobSupersededKeyMoved
		}
		return viewmetrics.ContractJobPublished
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	status := p.wait(first)
	require.Equal(t, ContractActorStalled, status.Result)
	require.True(t, p.c.Status(first.Actor).Stalled)
	require.Len(t, p.started(), 3)
	p.c.Published(t.Context(), "r", "")
	require.Eventually(t, func() bool { return len(p.started()) == 4 && !p.c.Status(first.Actor).Stalled }, 10*time.Second, time.Millisecond)
}

// TestContractSchedulerStallResolvesOnlyTheEndedJob: a stalled actor's next
// job that ends without publishing resolves its own promise stalled, never
// the newer key a request asked for while it ran: that key's job still runs
// and its waiter gets what it publishes.
func TestContractSchedulerStallResolvesOnlyTheEndedJob(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.outcome = func(key string, _ int) string {
		if key == "r/@k3" {
			return viewmetrics.ContractJobPublished
		}
		return viewmetrics.ContractJobSupersededKeyMoved
	}
	first := p.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorStalled, p.wait(first).Result)
	require.Len(t, p.started(), 3)
	p.hold("r/@k2")
	ended := p.schedule("", []string{"r"}, "r", "k2")
	p.waitStarts(4)
	newer := p.schedule("", []string{"r"}, "r", "k3")
	require.Equal(t, ContractRunning, newer.Result)
	p.hold("r/@k3")
	p.release("r/@k2")
	p.waitStarts(5)
	status := p.c.Status(newer.Actor)
	require.Equal(t, ended.ArrivalSeq, status.Seq, "the stall resolved the job that ended")
	require.Equal(t, ContractActorStalled, status.Result)
	p.release("r/@k3")
	require.Equal(t, ContractActorPublished, p.wait(newer).Result, "the newer key's job ran and published")
	require.Equal(t, []string{"r/@k", "r/@k", "r/@k", "r/@k2", "r/@k3"}, p.started())
}

func TestContractSchedulerRecoversFromBaselineAdmissionPanic(t *testing.T) {
	var panicked atomic.Bool
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.ReconcileBaseline = func(context.Context, *graphview.RepoView, string, string) (bool, error) {
			if panicked.CompareAndSwap(false, true) {
				panic("injected baseline admission panic")
			}
			return true, nil
		}
	})
	first, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("", []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r"}})
	require.NoError(t, err)
	require.Equal(t, ContractActorPanic, p.wait(first).Result)
	later := p.schedule("", []string{"r"}, "r", "accepted")
	require.Equal(t, ContractActorPublished, p.wait(later).Result)
}

func TestContractSchedulerAdmissionPanicPreservesNewerInterest(t *testing.T) {
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.ReconcileBaseline = func(ctx context.Context, _ *graphview.RepoView, _, _ string) (bool, error) {
			close(entered)
			select {
			case <-proceed:
				panic("injected pending baseline admission panic")
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
	})
	t.Cleanup(release)
	current := p.f.inputs[0].State
	pending := current
	pending.Accepted, pending.InputFingerprint = false, "pending"
	require.NoError(t, p.f.store.BeginContractInputMutationContext(t.Context(), &current, pending, nil))
	inputs, err := (&graphview.Materializer{Store: p.f.store}).CaptureContractInputs(t.Context(), nil, "repo-a", "")
	require.NoError(t, err)
	var released atomic.Int64
	first, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("", []string{"repo-a"}), Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a"}, Inputs: inputs, Source: &ContractCaptureSource{onRelease: func() { released.Add(1) }}})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("baseline admission did not start")
	}
	p.hold("repo-a/@accepted")
	later := p.schedule("", []string{"repo-a"}, "repo-a", "accepted")
	release()
	require.Eventually(t, func() bool { return p.c.Status(first.Actor).Result == ContractActorPanic }, 10*time.Second, time.Millisecond)
	require.Equal(t, ContractActorPanic, p.wait(first).Result)
	require.EqualValues(t, 1, released.Load())
	p.release("repo-a/@accepted")
	require.Equal(t, ContractActorPublished, p.wait(later).Result)
}

// TestContractSchedulerRecoversFromAJobPanic: a panicking job body is
// counted, releases its snapshot once, serves its waiter and is
// retried once; the actor stays schedulable and Close returns.
func TestContractSchedulerRecoversFromAJobPanic(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	run := p.run
	var panicked atomic.Bool
	p.c.run = func(job *contractAnalysisJob) string {
		if panicked.CompareAndSwap(false, true) {
			panic("injected job panic")
		}
		return run(job)
	}
	recoverPanicPin(t, p, "", nil, nil)
}

// TestContractSchedulerRecoversFromACapturePanic is the same for a capture,
// which also releases the request's source once.
func TestContractSchedulerRecoversFromACapturePanic(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	var panicked atomic.Bool
	p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		if panicked.CompareAndSwap(false, true) {
			panic("injected capture panic")
		}
		return p.capture(ctx, source, inputs, repo, checkout)
	}
	var released atomic.Int64
	recoverPanicPin(t, p, "wt", &ContractCaptureSource{onRelease: func() { released.Add(1) }}, nil)
	require.EqualValues(t, 1, released.Load(), "the panicking capture's source is released once")
}

// recoverPanicPin schedules an actor whose first job panics and pins what
// the recovery leaves: the panic counted, its waiter served, every
// captured snapshot released exactly once, the actor schedulable, and Close.
func recoverPanicPin(t *testing.T, p *schedulerProbe, checkout string, source *ContractCaptureSource, observeServed func()) {
	t.Helper()
	panics := contractCounter("views_contract_analysis_jobs_total{mode=cold,outcome=panic}")
	first, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor(checkout, []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: checkout}, Inputs: schedulerInputs([]string{"r"}, "k1"), Source: source})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	status, err := p.c.WaitActor(ctx, first.Actor, first.ArrivalSeq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPanic, status.Result)
	if observeServed != nil {
		observeServed()
	}
	require.Equal(t, panics+1, contractCounter("views_contract_analysis_jobs_total{mode=cold,outcome=panic}"))
	if checkout == "" {
		// The one retry publishes the same key.
		require.Eventually(t, func() bool {
			s, _ := p.c.WaitActor(ctx, first.Actor, first.ArrivalSeq)
			return s.Result == ContractActorPublished
		}, 10*time.Second, 10*time.Millisecond)
	}
	later, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: first.Actor, Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: checkout}, Inputs: schedulerInputs([]string{"r"}, "k2"), Source: &ContractCaptureSource{}})
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, p.wait(later).Result, "the actor stays schedulable")
	p.mu.Lock()
	captured := 0
	for _, n := range p.captures {
		captured += n
	}
	p.mu.Unlock()
	require.EqualValues(t, captured, p.releases.Load(), "every captured snapshot is released exactly once")
	closed := make(chan error, 1)
	go func() { closed <- p.c.CloseContext(t.Context()) }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after a panic")
	}
}

// TestContractScheduleQueueFullIsTypedAndRetryable: a new actor arriving at
// capacity gets a typed retryable refusal with a retry hint and its source
// released once; a known actor, idle or with work, is never refused;
// capacity frees wake WaitCapacity.
func TestContractScheduleQueueFullIsTypedAndRetryable(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) { options.QueueCapacity = 2 })
	idle := p.schedule("", []string{"d"}, "d", "d1")
	require.Equal(t, ContractActorPublished, p.wait(idle).Result)
	require.Eventually(t, func() bool { return p.c.Status(idle.Actor).State == contractActorIdle.String() }, 10*time.Second, time.Millisecond)
	p.hold("a/@a1")
	running := p.schedule("", []string{"a"}, "a", "a1")
	p.waitStarts(2)
	p.schedule("", []string{"b"}, "b", "b1")
	epoch := p.c.CapacityEpoch()
	var released atomic.Int64
	source := &ContractCaptureSource{onRelease: func() { released.Add(1) }}
	_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("wt", []string{"c"}), Key: graph.ContractAttachmentKey{RepoPrefix: "c", CheckoutID: "wt"}, Inputs: schedulerInputs([]string{"c"}, "c1"), Source: source})
	require.ErrorIs(t, err, ErrContractQueueFull)
	var full *ContractQueueFullError
	require.ErrorAs(t, err, &full)
	require.GreaterOrEqual(t, full.RetryAfter, time.Second)
	require.LessOrEqual(t, full.RetryAfter, 30*time.Second)
	require.EqualValues(t, 1, released.Load(), "the refused request's source is released once")
	known := p.schedule("", []string{"a"}, "a", "a2")
	require.Equal(t, ContractRunning, known.Result, "an actor with work is never refused")
	again := p.schedule("", []string{"d"}, "d", "d2")
	require.Equal(t, ContractScheduled, again.Result, "an idle known actor is never refused")
	require.Equal(t, ContractActorKeyMoved, p.wait(running).Result, "the newer key replaced the running one")
	woke := make(chan error, 1)
	go func() { woke <- p.c.WaitCapacity(t.Context(), epoch) }()
	select {
	case <-woke:
		t.Fatal("capacity woke before any actor left its work")
	case <-time.After(50 * time.Millisecond):
	}
	p.release("a/@a1")
	require.Equal(t, ContractActorPublished, p.wait(known).Result)
	require.Greater(t, p.c.CapacityEpoch(), epoch, "leaving active work advances capacity epoch")
	select {
	case err := <-woke:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("WaitCapacity did not wake")
	}
}

// TestContractSchedulerCloseReleasesQueuedSourcesOnceAndJoinsJobs: Close
// releases a queued actor's source once and returns after the running job.
func TestContractSchedulerCloseReleasesQueuedSourcesOnceAndJoinsJobs(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("r/@k")
	p.schedule("", []string{"r"}, "r", "k")
	p.waitStarts(1)
	var released atomic.Int64
	source := &ContractCaptureSource{onRelease: func() { released.Add(1) }}
	_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("wt", []string{"r", "z"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: "wt"}, Inputs: schedulerInputs([]string{"r", "z"}, "w"), Source: source})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, p.c.CloseContext(ctx), "close joins the canceled job and the dispatchers")
	require.EqualValues(t, 1, released.Load(), "the queued source is released once")
	require.NoError(t, p.c.Close())
	require.EqualValues(t, 1, released.Load())
	_, err = p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("", []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r"}, Inputs: schedulerInputs([]string{"r"}, "k2")})
	require.ErrorIs(t, err, ErrContractSchedulerClosed)
}

// TestContractCaptureStaleRetriesOnTheNextEventOnly: a capture refused stale
// waits for the next Published of the cohort; it never captures again in a
// loop.
func TestContractCaptureStaleRetriesOnTheNextEventOnly(t *testing.T) {
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Inputs = func(_ context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
			return schedulerInputs(repos, "k"), nil
		}
	})
	p.captureErr = func(string, int) error { return graph.ErrContractProjectionStale }
	stale := contractCounter("views_contract_analysis_captures_total{result=stale}")
	captures := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.captures["r/@k"]
	}
	p.schedule("", []string{"r"}, "r", "k")
	require.Eventually(t, func() bool { return captures() == 1 }, 10*time.Second, time.Millisecond)
	const events = 3
	for i := range events {
		time.Sleep(200 * time.Millisecond)
		require.Equal(t, i+1, captures(), "no capture without an event")
		p.c.Published(t.Context(), "r", "")
		require.Eventually(t, func() bool { return captures() == i+2 }, 10*time.Second, time.Millisecond, "status %+v", p.c.Status(NewContractActor("", []string{"r"})))
	}
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, events+1, captures(), "one capture per event")
	require.Equal(t, stale+events+1, contractCounter("views_contract_analysis_captures_total{result=stale}"))
}

func TestContractSchedulerRegistryEvictsOnlyTheLeastRecentIdleActor(t *testing.T) {
	require.Equal(t, 64, contractSchedulerMaxActors)
	// The registry helper runs under the coordinator mutex. Supplying its
	// ring directly isolates bookkeeping without starting dispatch workers.
	ring := &contractRing{repo: "r", wake: make(chan struct{}, 1)}
	c := &ContractAnalysisCoordinator{actors: make(map[ContractActor]*contractActor), rings: map[string]*contractRing{"r": ring}}
	base := time.Now()
	var active, oldestIdle ContractActor
	for i := 0; i < contractSchedulerMaxActors; i++ {
		id := NewContractActor(fmt.Sprintf("checkout-%d", i), []string{"r"})
		a := c.registerLocked(id, base.Add(time.Duration(i)*time.Second))
		switch i {
		case 0:
			active, a.state = id, contractActorRunning
		case 1:
			oldestIdle = id
		}
	}
	// Refresh the earliest idle actor before overflowing: FIFO would evict
	// it, whereas LRU must preserve it and remove the next oldest idle.
	refreshed := oldestIdle
	c.actors[refreshed].lastUsed = base.Add(2 * time.Hour)
	oldestIdle = NewContractActor("checkout-2", []string{"r"})
	old := c.actors[oldestIdle]
	changed := old.changed
	newID := NewContractActor("new", []string{"r"})
	c.registerLocked(newID, base.Add(time.Hour))
	require.Len(t, c.actors, contractSchedulerMaxActors)
	require.Contains(t, c.actors, active, "even the oldest active actor retains custody")
	require.Contains(t, c.actors, refreshed, "recent use protects an older idle actor from FIFO eviction")
	require.NotContains(t, c.actors, oldestIdle)
	require.Contains(t, c.actors, newID)
	require.Len(t, ring.actors, contractSchedulerMaxActors)
	for _, a := range ring.actors {
		require.NotSame(t, old, a, "eviction removes the old dispatch entry")
	}
	select {
	case <-changed:
	default:
		t.Fatal("eviction must notify waiters on the removed actor")
	}
}

func TestContractSchedulerContinuesPrimaryFillAfterOtherActor(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("a/@k")
	p.hold("b/@k")
	resumed := make(chan struct{})
	resume := make(chan struct{})
	var resumedOnce sync.Once
	defer resumedOnce.Do(func() { close(resume) })
	p.c.run = func(job *contractAnalysisJob) string {
		if job.snapshot.Key.InputFingerprint == "a/@k" {
			p.mu.Lock()
			n := p.jobs["a/@k"]
			p.mu.Unlock()
			if n == 1 {
				close(resumed)
				select {
				case <-resume:
				case <-p.c.ctx.Done():
				}
			}
		}
		return p.run(job)
	}
	p.outcome = func(key string, n int) string {
		if key == "a/@k" && n == 1 {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	a := p.schedule("", []string{"a"}, "a", "k")
	p.waitStarts(1)
	b := p.schedule("", []string{"b"}, "b", "k")
	require.Eventually(t, func() bool {
		p.c.mu.Lock()
		actor := p.c.actors[a.Actor]
		p.c.mu.Unlock()
		return p.c.otherActorWaiting(actor)
	}, 10*time.Second, time.Millisecond)
	// Slot fairness starts when B actually asks for the slot. A queued actor
	// alone does not prove its dispatcher has reached that boundary.
	require.Eventually(t, func() bool {
		p.c.slots.mu.Lock()
		defer p.c.slots.mu.Unlock()
		return len(p.c.slots.waiters) == 1
	}, 10*time.Second, time.Millisecond, "B must be waiting for A's slot")
	p.release("a/@k")
	require.Equal(t, []string{"a/@k", "b/@k"}, p.waitStarts(2))
	p.c.mu.Lock()
	actor := p.c.actors[a.Actor]
	require.Less(t, actor.resolved, a.ArrivalSeq, "continuation does not resolve the attachment waiter")
	require.Zero(t, actor.failures)
	require.Zero(t, actor.stalled.cycles)
	require.Empty(t, actor.lastPublished.key)
	p.c.mu.Unlock()
	require.GreaterOrEqual(t, p.releases.Load(), int64(1), "first snapshot released before another actor runs")
	p.release("b/@k")
	p.wait(b)
	select {
	case <-resumed:
	case <-time.After(10 * time.Second):
		t.Fatal("primary fill was not recaptured")
	}
	p.mu.Lock()
	require.Equal(t, 2, p.captures["a/@k"])
	p.mu.Unlock()
	resumedOnce.Do(func() { close(resume) })
	p.wait(a)
}

func TestContractSchedulerLinkedJobCannotContinueWithoutFreshSource(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	var released atomic.Int64
	observed := make(chan bool, 1)
	p.c.run = func(job *contractAnalysisJob) string {
		observed <- job.otherActorWaiting == nil
		return p.run(job)
	}
	result, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("wt", []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: "wt"}, Inputs: schedulerInputs([]string{"r"}, "k"), Source: &ContractCaptureSource{onRelease: func() { released.Add(1) }}})
	require.NoError(t, err)
	p.wait(result)
	require.True(t, <-observed, "linked jobs keep their complete single-capture execution path")
	require.EqualValues(t, 1, released.Load(), "the prepared source is consumed exactly once")
}

func TestContractSchedulerLostFillCheckpointRunsToCompletion(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	callbacks := make(chan bool, 2)
	loss := make(chan ContractFillCheckpointStatus, 1)
	p.c.run = func(job *contractAnalysisJob) string {
		callbacks <- job.otherActorWaiting != nil
		if job.otherActorWaiting == nil {
			loss <- p.c.Status(NewContractActor("", []string{"a"})).FillCheckpoint
		}
		return p.run(job)
	}
	p.outcome = func(_ string, n int) string {
		if n == 1 {
			// This fake retains no checkpoint, like a rejected LRU admission.
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	a := p.schedule("", []string{"a"}, "a", "k")
	p.wait(a)
	require.True(t, <-callbacks)
	require.False(t, <-callbacks, "missing partial state must not repeatedly refill the same prefix")
	require.Equal(t, []string{"a/@k", "a/@k"}, p.started())
	require.Equal(t, ContractFillCheckpointMissing, <-loss, "loss is explicitly observable; this fallback still needs bounded fair custody")
}

func TestContractSchedulerNewSequenceRetainsSuccessfulContinuation(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("a/@old")
	observed := make(chan bool, 1)
	p.c.run = func(job *contractAnalysisJob) string {
		if job.snapshot.Key.InputFingerprint == "a/@old" {
			unit := &contractUnit{path: "a/file.go"}
			job.delta.cache = &contractDeltaCache{identity: job.delta.identity, units: map[string]*contractUnit{unit.path: unit}, continuation: &contractFillContinuation{identity: job.delta.identity, units: []contractFillUnitProof{{unit: unit}}}}
		} else {
			observed <- job.delta.cache != nil && job.delta.cache.continuation != nil
		}
		return p.run(job)
	}
	p.outcome = func(key string, _ int) string {
		if key == "a/@old" {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	p.schedule("", []string{"a"}, "a", "old")
	p.waitStarts(1)
	newer := p.schedule("", []string{"a"}, "a", "new")
	p.release("a/@old")
	p.wait(newer)
	require.True(t, <-observed, "a new actor sequence alone does not erase an already proven obligation anchor")
}
