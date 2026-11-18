package indexer

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func priorityBatchRequest(t *testing.T, f *contractWorkerFixture) (ContractFollowupBatchRequest, ContractFollowupRequest, ContractFollowupRequest) {
	t.Helper()
	a := f.request(t, "repo-a", "priority-a")
	b := f.request(t, "repo-b", "priority-b")
	// Deliberately put the priority target second in the captured vector.
	return ContractFollowupBatchRequest{Snapshot: a.Snapshot, Targets: []ContractFollowupTarget{
		{Key: b.Snapshot.Key, Work: b.Snapshot.Work, Payload: b.Payload, Catalog: b.Catalog},
		{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Payload: a.Payload, Catalog: a.Catalog},
	}, RebuildReason: a.RebuildReason, Registry: a.Registry, Config: a.Config, Leases: a.Leases,
		Yield: a.Yield, ScratchParent: t.TempDir(), PriorityRepos: []string{"repo-a"}}, a, b
}

func TestContractPriorityPublicationCommitsBeforeCompanionAndCompletesProof(t *testing.T) {
	f := newContractWorkerFixture(t)
	req, a, b := priorityBatchRequest(t, f)
	first := make(chan ContractFollowupTargetResult, 2)
	resume := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	req.TargetPublished = func(result ContractFollowupTargetResult) { first <- result }
	req.BackgroundCurrent = func(ctx context.Context) error {
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	type outcome struct {
		report ContractFollowupBatchReport
		err    error
	}
	done := make(chan outcome, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer func() {
		cancel()
		once.Do(func() { close(resume) })
		<-finished
	}()
	go func() {
		defer close(finished)
		report, err := RunContractFollowupBatch(ctx, req)
		done <- outcome{report, err}
	}()
	select {
	case result := <-first:
		require.Equal(t, a.Snapshot.Key, result.Key)
		require.True(t, result.Report.Published)
		require.NoError(t, result.Err)
	case <-ctx.Done():
		t.Fatal("priority target never committed")
	}
	header, err := f.store.GetContractAttachmentContext(ctx, a.Snapshot.Key)
	require.NoError(t, err)
	require.NotNil(t, header)
	require.Equal(t, a.Payload.ViewGeneration(), header.PayloadGeneration)
	absent, err := f.store.GetContractAttachmentContext(ctx, b.Snapshot.Key)
	require.NoError(t, err)
	require.Nil(t, absent, "companion is still behind the explicit barrier")
	pending, err := f.store.PendingContractWorkForScopeContext(ctx, "repo-a", "")
	require.NoError(t, err)
	require.Empty(t, pending, "first commit acknowledges the actual immutable work")
	once.Do(func() { close(resume) })
	var got outcome
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("companion did not finish")
	}
	require.NoError(t, got.err)
	require.True(t, contractPublishedCohortComplete(got.report.Publication), "final plural validation accepts the first target's durable acknowledgments")
	require.Equal(t, []string{"repo-b", "repo-a"}, []string{got.report.Targets[0].Key.RepoPrefix, got.report.Targets[1].Key.RepoPrefix})
	for _, target := range got.report.Targets {
		require.True(t, target.Report.Published)
		require.NoError(t, target.Err)
	}
	require.Equal(t, 1, f.releases)
	for _, target := range []ContractFollowupRequest{a, b} {
		require.False(t, f.leases.InUse(target.Payload.ViewGeneration()))
		require.NotNil(t, target.Payload.GetNode("http::GET::/v1/items"))
	}
	entries, err := os.ReadDir(req.ScratchParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestContractPriorityPublicationPreservesWinnerOnCompanionFailure(t *testing.T) {
	for _, mode := range []string{"superseded", "cancel", "input-cas"} {
		t.Run(mode, func(t *testing.T) {
			f := newContractWorkerFixture(t)
			req, a, b := priorityBatchRequest(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls int
			req.BackgroundCurrent = func(context.Context) error {
				calls++
				switch mode {
				case "superseded":
					return errContractJobKeyMoved
				case "cancel":
					cancel()
					return ctx.Err()
				case "input-cas":
					if calls == 1 {
						old := f.inputs[1].State
						next := old
						next.InputFingerprint = "new-companion-input"
						return f.store.BeginContractInputMutationContext(context.Background(), &old, next, nil)
					}
				}
				return nil
			}
			report, err := RunContractFollowupBatch(ctx, req)
			require.Error(t, err)
			require.Positive(t, calls)
			require.Nil(t, report.Publication, "partial success cannot mint whole-cohort proof")
			require.True(t, report.Targets[1].Report.Published)
			require.NoError(t, report.Targets[1].Err)
			require.False(t, report.Targets[0].Report.Published)
			require.Error(t, report.Targets[0].Err)
			header, err := f.store.GetContractAttachmentContext(context.Background(), a.Snapshot.Key)
			require.NoError(t, err)
			require.NotNil(t, header)
			require.Equal(t, a.Payload.ViewGeneration(), header.PayloadGeneration)
			absent, err := f.store.GetContractAttachmentContext(context.Background(), b.Snapshot.Key)
			require.NoError(t, err)
			require.Nil(t, absent)
			require.Equal(t, 1, f.releases)
			for _, target := range []ContractFollowupRequest{a, b} {
				require.False(t, f.leases.InUse(target.Payload.ViewGeneration()))
			}
			entries, err := os.ReadDir(req.ScratchParent)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestContractCoordinatorWaitTargetReopensBeforeWholeActorCompletes(t *testing.T) {
	f := newContractWorkerFixture(t)
	_, a, b := priorityBatchRequest(t, f)
	first := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	capture := func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		snapshot := a.Snapshot
		// The coordinator reserves its own payloads; this capture transfers
		// source authority independently of the fixture's unused reservations.
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
		}
		return snapshot, []ContractFollowupTarget{
			{Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: b.Catalog},
			{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: a.Catalog},
		}, nil
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: a.Yield})
	require.NoError(t, err)
	defer func() { once.Do(func() { close(resume) }); require.NoError(t, c.Close()) }()
	execute := c.execute
	c.run = func(job *contractAnalysisJob) string {
		notify := job.targetPublished
		job.targetPublished = func(result ContractFollowupTargetResult) {
			notify(result)
			if result.Key.RepoPrefix == "repo-a" {
				close(first)
				select {
				case <-resume:
				case <-c.ctx.Done():
				}
			}
		}
		return execute(job)
	}
	// A global actor reached through B still prioritizes the actual edit in A.
	c.Published(t.Context(), "repo-a", "")
	actor := NewContractActor("", []string{"repo-a", "repo-b"})
	scheduled, err := c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: b.Snapshot.Key, Inputs: &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}, Epoch: c.InputEpoch()})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("edited repository did not publish")
	}
	status, err := c.WaitTarget(ctx, actor, scheduled.ArrivalSeq, a.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	_, release, err := c.AcquireLastPublishedCohort(ctx, actor)
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, ErrContractPublishedCohortAbsent)
	stopped, stop := context.WithCancel(ctx)
	stop()
	_, err = c.WaitActor(stopped, actor, scheduled.ArrivalSeq)
	require.ErrorIs(t, err, context.Canceled)
	_, err = c.WaitTarget(stopped, actor, scheduled.ArrivalSeq, b.Snapshot.Key)
	require.ErrorIs(t, err, context.Canceled)
	wrong := a.Snapshot.Key
	wrong.RepoPrefix = "foreign"
	_, err = c.WaitTarget(ctx, actor, scheduled.ArrivalSeq, wrong)
	require.ErrorIs(t, err, graph.ErrContractInputVector)
	once.Do(func() { close(resume) })
	status, err = c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	cohort, release, err := c.AcquireLastPublishedCohort(ctx, actor)
	require.NoError(t, err)
	defer release()
	require.True(t, contractPublishedCohortComplete(cohort))
}

func TestContractCoordinatorExplicitTargetPublishesBeforeOlderRepositoryHint(t *testing.T) {
	f := newContractWorkerFixture(t)
	_, a, b := priorityBatchRequest(t, f)
	capture := func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		snapshot := a.Snapshot
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
		}
		return snapshot, []ContractFollowupTarget{
			{Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: b.Catalog},
			{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: a.Catalog},
		}, nil
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: a.Yield})
	require.NoError(t, err)
	first := make(chan ContractFollowupTargetResult, 2)
	resume := make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(resume) }); require.NoError(t, c.Close()) }()
	execute := c.execute
	c.run = func(job *contractAnalysisJob) string {
		notify := job.targetPublished
		job.targetPublished = func(result ContractFollowupTargetResult) {
			notify(result)
			first <- result
			select {
			case <-resume:
			case <-c.ctx.Done():
			}
		}
		return execute(job)
	}
	// The last notification belongs to the companion. It remains a useful
	// fallback hint, but must not precede this explicitly requested target.
	c.Published(t.Context(), "repo-b", "")
	actor := NewContractActor("", []string{"repo-a", "repo-b"})
	scheduled, err := c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: a.Snapshot.Key, Inputs: &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}, Epoch: c.InputEpoch(), PriorityTarget: true})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	select {
	case target := <-first:
		require.Equal(t, a.Snapshot.Key, target.Key)
		require.True(t, target.Report.Published)
		require.NoError(t, target.Err)
	case <-ctx.Done():
		t.Fatal("requested target never committed")
	}
	header, err := f.store.GetContractAttachmentContext(ctx, a.Snapshot.Key)
	require.NoError(t, err)
	require.NotNil(t, header, "first notification follows a real durable target publication")
	absent, err := f.store.GetContractAttachmentContext(ctx, b.Snapshot.Key)
	require.NoError(t, err)
	require.Nil(t, absent, "the older companion hint remains behind the publication barrier")
	status, err := c.WaitTarget(ctx, actor, scheduled.ArrivalSeq, a.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	stopped, stop := context.WithCancel(ctx)
	stop()
	_, err = c.WaitActor(stopped, actor, scheduled.ArrivalSeq)
	require.ErrorIs(t, err, context.Canceled, "local readiness does not claim whole-cohort completion")
	once.Do(func() { close(resume) })
	status, err = c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	cohort, release, err := c.AcquireLastPublishedCohort(ctx, actor)
	require.NoError(t, err)
	defer release()
	require.True(t, contractPublishedCohortComplete(cohort), "all companion obligations remain in the final proof")
	header, err = f.store.GetContractAttachmentContext(ctx, b.Snapshot.Key)
	require.NoError(t, err)
	require.NotNil(t, header)
}

func TestContractSchedulerNewerRequestSupersedesWithoutStalling(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("repo-a/@first")
	p.hold("repo-a/@second")
	p.outcome = func(string, int) string { return viewmetrics.ContractJobSupersededKeyMoved }
	p.schedule("", []string{"repo-a"}, "repo-a", "first")
	p.waitStarts(1)
	second := p.schedule("", []string{"repo-a"}, "repo-a", "second")
	p.c.mu.Lock()
	a := p.c.actors[second.Actor]
	a.failures = contractStallEnds - 1
	p.c.mu.Unlock()
	p.release("repo-a/@first")
	p.waitStarts(2)
	p.c.mu.Lock()
	failures, stalls, state := a.failures, a.stalled.cycles, a.state
	p.c.mu.Unlock()
	require.Equal(t, contractStallEnds-1, failures)
	require.Zero(t, stalls)
	require.Equal(t, contractActorRunning, state)
	require.NoError(t, p.c.Close())
}

func TestContractCoordinatorPriorityWinnerSurvivesWakePanic(t *testing.T) {
	f := newContractWorkerFixture(t)
	_, a, b := priorityBatchRequest(t, f)
	capture := func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		snapshot := a.Snapshot
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
		}
		return snapshot, []ContractFollowupTarget{
			{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: a.Catalog},
			{Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: b.Catalog},
		}, nil
	}
	// Freeze the existing scheduler clock so its automatic panic retry cannot
	// race the assertions; no elapsed-time assumption establishes custody.
	now := time.Now()
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: a.Yield, Now: func() time.Time { return now }})
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	execute := c.execute
	c.run = func(job *contractAnalysisJob) string {
		job.targetPublished = func(ContractFollowupTargetResult) { panic("after durable priority commit") }
		return execute(job)
	}
	c.Published(t.Context(), "repo-a", "")
	actor := NewContractActor("", []string{"repo-a", "repo-b"})
	scheduled, err := c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: a.Snapshot.Key, Inputs: &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}, Epoch: c.InputEpoch()})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	status, err := c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPanic, status.Result)
	header, err := f.store.GetContractAttachmentContext(ctx, a.Snapshot.Key)
	require.NoError(t, err)
	require.NotNil(t, header)
	generation, found, err := f.store.Catalog().GetViewGeneration(ctx, header.PayloadGeneration)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationReady, generation.State)
	require.False(t, f.leases.InUse(header.PayloadGeneration))
	absent, err := f.store.GetContractAttachmentContext(ctx, b.Snapshot.Key)
	require.NoError(t, err)
	require.Nil(t, absent)
	require.Equal(t, 1, f.releases)
	_, release, err := c.AcquireLastPublishedCohort(ctx, actor)
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, ErrContractPublishedCohortAbsent)
}

func TestContractSchedulerPartialProgressResetsOnlyNoPublicationStalls(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-progress", true: "real-target-progress"}[progress], func(t *testing.T) {
			p := newSchedulerProbe(t, nil)
			type step struct {
				job  *contractAnalysisJob
				stop chan bool
			}
			steps := make(chan step, 1)
			capture := p.c.options.Capture
			p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				snapshot, targets, err := capture(ctx, source, inputs, repo, checkout)
				snapshot.inputsCurrent = func(context.Context) error { return graph.ErrContractProjectionStale }
				return snapshot, targets, err
			}
			p.c.run = func(job *contractAnalysisJob) string {
				defer job.snapshot.Release()
				// The scheduler probe simulates the worker's post-commit
				// callback; real durability/custody is covered above.
				if progress {
					job.targetPublished(ContractFollowupTargetResult{Report: ContractFollowupReport{PayloadGeneration: 1, Published: true}})
				}
				s := step{job: job, stop: make(chan bool, 1)}
				select {
				case steps <- s:
				case <-p.c.ctx.Done():
					return viewmetrics.ContractJobPreempted
				}
				select {
				case quiet := <-s.stop:
					if quiet {
						return viewmetrics.ContractJobPublished
					}
					return contractJobOutcome(job.backgroundCurrent(p.c.ctx))
				case <-p.c.ctx.Done():
					return viewmetrics.ContractJobPreempted
				}
			}
			scheduled := p.schedule("", []string{"repo-a", "repo-b"}, "repo-a", "same-actor")
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			for range contractStallEnds {
				select {
				case s := <-steps:
					// No new request/sequence: accepted input movement alone
					// cooperatively stops this job's background companions.
					p.c.Published(ctx, "repo-a", "")
					s.stop <- false
				case <-ctx.Done():
					t.Fatal("scheduler stopped making progress")
				}
			}
			if progress {
				select {
				case s := <-steps:
					s.stop <- true
				case <-ctx.Done():
					t.Fatal("quiet inputs never converged after partial publications")
				}
			}
			status, err := p.c.WaitActor(ctx, scheduled.Actor, scheduled.ArrivalSeq)
			require.NoError(t, err)
			if progress {
				require.Equal(t, ContractActorPublished, status.Result)
			} else {
				require.Equal(t, ContractActorStalled, status.Result, "no-publication loops still retain the existing stall detector")
			}
		})
	}
}

func TestContractSchedulerPriorityHintsAndRelevantInputFences(t *testing.T) {
	t.Run("coalesced-hint-keeps-source-anchor", func(t *testing.T) {
		p := newSchedulerProbe(t, nil)
		p.hold("repo-a/@same")
		repos := []string{"repo-a", "repo-b"}
		inputs := schedulerInputs(repos, "same")
		actor := NewContractActor("", repos)
		first, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a"}, Inputs: inputs})
		require.NoError(t, err)
		p.waitStarts(1)
		second, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-b"}, Inputs: schedulerInputs(repos, "same"), PriorityTarget: true})
		require.NoError(t, err)
		require.Equal(t, first.ArrivalSeq, second.ArrivalSeq)
		p.c.mu.Lock()
		want := p.c.actors[actor].want
		p.c.mu.Unlock()
		require.Equal(t, "repo-a", want.repo, "coalescing must not change the retained source's capture anchor")
		require.Same(t, inputs, want.inputs)
		require.Equal(t, "repo-b", want.priorityRepo)
		require.NoError(t, p.c.Close())
	})
	for _, mode := range []string{"cold-global", "explicit", "edited-global", "edited-and-explicit"} {
		t.Run(mode, func(t *testing.T) {
			p := newSchedulerProbe(t, nil)
			jobs := make(chan *contractAnalysisJob, 1)
			resume := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(resume) })
			var validations atomic.Int64
			capture := p.c.options.Capture
			p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				snapshot, targets, err := capture(ctx, source, inputs, repo, checkout)
				snapshot.inputsCurrent = func(context.Context) error { validations.Add(1); return nil }
				return snapshot, targets, err
			}
			p.c.run = func(job *contractAnalysisJob) string {
				defer job.snapshot.Release()
				select {
				case jobs <- job:
				case <-p.c.ctx.Done():
				}
				select {
				case <-resume:
				case <-p.c.ctx.Done():
				}
				return viewmetrics.ContractJobPublished
			}
			if mode == "edited-global" || mode == "edited-and-explicit" {
				p.c.Published(t.Context(), "repo-a", "")
			}
			repos := []string{"repo-a", "repo-b"}
			actor := NewContractActor("", repos)
			scheduled, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-b"}, Inputs: schedulerInputs(repos, "first"), Epoch: p.c.InputEpoch(), PriorityTarget: mode == "explicit" || mode == "edited-and-explicit"})
			require.NoError(t, err)
			var job *contractAnalysisJob
			select {
			case job = <-jobs:
			case <-time.After(time.Minute):
				t.Fatal("scheduler never dispatched")
			}
			switch mode {
			case "cold-global":
				require.Empty(t, job.priorityRepos)
			case "explicit":
				require.Equal(t, []string{"repo-b"}, job.priorityRepos)
			case "edited-global":
				require.Equal(t, []string{"repo-a"}, job.priorityRepos)
			case "edited-and-explicit":
				require.Equal(t, []string{"repo-b", "repo-a"}, job.priorityRepos)
			}
			for range 3 {
				require.NoError(t, job.backgroundCurrent(t.Context()))
			}
			p.c.Published(t.Context(), "outside-cohort", "")
			require.NoError(t, job.backgroundCurrent(t.Context()))
			require.Zero(t, validations.Load(), "unrelated epochs and unchanged chunks do not reread all inputs")
			p.c.Published(t.Context(), "repo-a", "")
			require.NoError(t, job.backgroundCurrent(t.Context()))
			require.EqualValues(t, 1, validations.Load())
			require.NoError(t, job.backgroundCurrent(t.Context()))
			require.EqualValues(t, 1, validations.Load())
			_, err = p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-b"}, Inputs: schedulerInputs(repos, "newer"), Epoch: p.c.InputEpoch()})
			require.NoError(t, err)
			require.ErrorIs(t, job.backgroundCurrent(t.Context()), errContractJobKeyMoved)
			require.EqualValues(t, 1, validations.Load(), "newer actor sequence refuses before another full validation")
			require.Positive(t, scheduled.ArrivalSeq)
			require.NoError(t, p.c.Close())
			once.Do(func() { close(resume) })
		})
	}
}
