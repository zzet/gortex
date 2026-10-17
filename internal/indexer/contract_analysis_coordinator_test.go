package indexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// scheduleAndWait schedules inputs for the actor of repos and waits on the
// promise of the seq it answered.
func scheduleAndWait(t *testing.T, ctx context.Context, c *ContractAnalysisCoordinator, repos []string, repo string, inputs *graphview.SelectedContractInputs) (ContractScheduleResult, ContractActorStatus) {
	t.Helper()
	key := graph.ContractAttachmentKey{RepoPrefix: repo}
	if inputs != nil {
		key.InputVersion, key.InputFingerprint = inputs.State.InputVersion, inputs.State.InputFingerprint
	}
	scheduled, err := c.Schedule(ctx, ContractScheduleRequest{Actor: NewContractActor("", repos), Key: key, Inputs: inputs, Epoch: c.InputEpoch()})
	require.NoError(t, err)
	status, err := c.WaitActor(ctx, scheduled.Actor, scheduled.ArrivalSeq)
	require.NoError(t, err)
	return scheduled, status
}

// scheduleUntilAttached waits for the attachment of a primary cohort's
// current inputs the way the strict consumer does: it captures the inputs,
// schedules them and waits on the actor's promise, capturing again only once
// the promise resolved. It returns the inputs whose attachment opened.
func scheduleUntilAttached(t *testing.T, ctx context.Context, c *ContractAnalysisCoordinator, m *graphview.Materializer, repos []string, repo string) *graphview.SelectedContractInputs {
	t.Helper()
	actor := NewContractActor("", repos)
	for {
		require.NoError(t, ctx.Err(), "the attachment never opened")
		epoch := c.InputEpoch()
		inputs, err := NewContractPrimaryInputCapture(m)(ctx, repos, repo)
		key := graph.ContractAttachmentKey{RepoPrefix: repo}
		if err != nil {
			require.Equal(t, graphview.CodeRequiredCapabilityIncomplete, graphview.CodeOf(err), "%v", err)
			inputs = nil
		} else {
			analysis, err := m.OpenContractAnalysisForInputs(ctx, nil, inputs)
			if err == nil {
				analysis.Close()
				return inputs
			}
			if errors.Is(err, graph.ErrContractProjectionStale) {
				require.NoError(t, waitInputsFor(ctx, c, epoch, time.Second))
				continue
			}
			require.Equal(t, graphview.CodeRequiredCapabilityIncomplete, graphview.CodeOf(err), "%v", err)
			key.InputVersion, key.InputFingerprint = inputs.State.InputVersion, inputs.State.InputFingerprint
		}
		scheduled, err := c.Schedule(ctx, ContractScheduleRequest{Actor: actor, Key: key, Inputs: inputs, Epoch: epoch})
		require.NoError(t, err)
		status, err := c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
		require.NoError(t, err)
		switch status.Result {
		case ContractActorFailed, ContractActorPanic, ContractActorStalled:
			t.Fatalf("the actor's job ended %s", status.Result)
		case ContractActorInputsStale:
			require.NoError(t, waitInputsFor(ctx, c, epoch, time.Second))
		}
	}
}

// waitInputsFor waits for the input epoch to pass since, at most limit.
func waitInputsFor(ctx context.Context, c *ContractAnalysisCoordinator, since uint64, limit time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := c.WaitInputs(wait, since); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func TestContractAnalysisCoordinatorCoalescesOwnedCohortAndPublishesBothTargets(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "unused-coordinator-a")
	b := f.request(t, "repo-b", "unused-coordinator-b")
	var released, captures atomic.Int64
	var reads atomic.Int64
	var coreReads atomic.Int64
	var readMu sync.Mutex
	uniqueReads := make(map[string]bool)
	seedReads := make(map[string]int)
	started := make(chan struct{})
	resume := make(chan struct{})
	var entered atomic.Bool
	capture := func(ctx context.Context, _ *ContractCaptureSource, _ *graphview.SelectedContractInputs, _, _ string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		captures.Add(1)
		snapshot := a.Snapshot
		snapshot.Release = func() { released.Add(1) }
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			coreReads.Add(1)
			return a.Snapshot.ReadCoreFile(ctx, file)
		}
		// Match a real primary capture's checked seed queries. A nil Seeds
		// callback deliberately asks delta enrichment to reload every live
		// file, which would count fallback full projections as cohort replay.
		snapshot.Seeds = func(ctx context.Context, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
			readMu.Lock()
			seedReads[repo]++
			readMu.Unlock()
			edges, err := f.store.ContractSeedEdgesContext(ctx, repo)
			if err != nil {
				return nil, nil, false, err
			}
			nodes, err := f.store.ContractSeedNodesContext(ctx, repo)
			return nodes, edges, err == nil, err
		}
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			reads.Add(1)
			readMu.Lock()
			uniqueReads[file.Path] = true
			readMu.Unlock()
			if entered.CompareAndSwap(false, true) {
				close(started)
				select {
				case <-resume:
				case <-ctx.Done():
					return ContractAcceptedSource{}, ctx.Err()
				}
			}
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, nil
		}
		return snapshot, []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: f.store}, {Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: f.store}}, nil
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: ContractAnalysisYield(func() bool { return true })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	inputs := &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}
	actor := NewContractActor("", []string{"repo-a", "repo-b"})
	first, err := c.Schedule(context.Background(), ContractScheduleRequest{Actor: actor, Key: a.Snapshot.Key, Inputs: inputs})
	if err != nil || first.Result != ContractScheduled {
		t.Fatalf("schedule=%+v err=%v", first, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	// The cohort reached through its other repository is the same actor:
	// the request joins the running job.
	second, err := c.Schedule(context.Background(), ContractScheduleRequest{Actor: NewContractActor("", []string{"repo-b", "repo-a"}), Key: b.Snapshot.Key, Inputs: inputs})
	if err != nil || second.Result != ContractRunning || second.ArrivalSeq != first.ArrivalSeq {
		t.Fatalf("coalesce=%+v err=%v", second, err)
	}
	close(resume)
	// A starvation guard, not a latency bound: a worker the sustained
	// foreground demand starved never publishes. Both targets publish in
	// about a second on a Windows runner, once in 10 s while its disk took
	// 7.8 s for one WAL checkpoint.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	status, err := c.WaitActor(ctx, actor, first.ArrivalSeq)
	if err != nil || status.Result != ContractActorPublished {
		t.Fatalf("wait=%+v err=%v", status, err)
	}
	for _, key := range []graph.ContractAttachmentKey{a.Snapshot.Key, b.Snapshot.Key} {
		header, err := f.store.GetContractAttachmentContext(ctx, key)
		if err != nil || header == nil {
			t.Fatalf("attachment %v: %v %v", key, header, err)
		}
	}
	cohort, releaseCohort, readErr := c.AcquireLastPublishedCohort(ctx, actor)
	require.NoError(t, readErr)
	require.Equal(t, first.ArrivalSeq, cohort.Seq)
	require.Equal(t, f.inputs, cohort.Witnesses)
	require.Len(t, cohort.Stamps, len(f.files))
	require.Len(t, cohort.Targets, 2)
	for _, target := range cohort.Targets {
		header, readErr := f.store.GetContractAttachmentContext(ctx, target.Key)
		require.NoError(t, readErr)
		require.Equal(t, header.PayloadGeneration, target.PayloadGeneration)
		require.True(t, f.leases.InUse(target.PayloadGeneration))
	}
	cohort.Witnesses[0].State.InputFingerprint = "caller mutation"
	cohort.Targets[0].Key.InputFingerprint = "caller target mutation"
	cohort.ExpectedTargets[0].InputFingerprint = "caller expected mutation"
	clear(cohort.Stamps)
	releaseCohort()
	again, releaseAgain, readErr := c.AcquireLastPublishedCohort(ctx, actor)
	require.NoError(t, readErr)
	require.Equal(t, f.inputs, again.Witnesses)
	require.Len(t, again.Targets, 2)
	require.Equal(t, contractPublishedExpectedTargets(f.inputs, ""), again.ExpectedTargets)
	require.NotEqual(t, "caller target mutation", again.Targets[0].Key.InputFingerprint)
	require.Len(t, again.Stamps, len(f.files))
	releaseAgain()
	if err := c.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	readMu.Lock()
	uniqueCount := len(uniqueReads)
	seedsA, seedsB := seedReads["repo-a"], seedReads["repo-b"]
	readMu.Unlock()
	require.Equal(t, 1, seedsA, "enrichment reads the actual primary seed projection once for the cohort")
	require.Equal(t, 1, seedsB, "both repositories participate in the shared enrichment once")
	// Lazy body/signature enrichment may legitimately reread an accepted file;
	// full extraction projections must occur exactly once per cohort file;
	// node-only enrichment reads do not call ReadCoreFile.
	if captures.Load() != 1 || uniqueCount != len(f.files) || coreReads.Load() != int64(len(f.files)) || reads.Load() < int64(uniqueCount) || released.Load() != 1 || f.leases.Held() != 0 {
		t.Fatalf("captures=%d shared sources raw=%d unique=%d full core projections=%d releases=%d leases=%d", captures.Load(), reads.Load(), uniqueCount, coreReads.Load(), released.Load(), f.leases.Held())
	}
	// Sustained foreground demand stayed true for every scheduling checkpoint,
	// yet the real shared worker published both targets under this same deadline.
}

func TestContractAnalysisCoordinatorBaselineRaceAndCancellation(t *testing.T) {
	f := newContractWorkerFixture(t)
	var c *ContractAnalysisCoordinator
	var baselineCalls atomic.Int64
	var err error
	c, err = NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		t.Error("unknown baseline captured")
		return ContractFollowupSnapshot{}, nil, errors.New("unsupported")
	}, ReconcileBaseline: func(ctx context.Context, _ *graphview.RepoView, repo, checkout string) (bool, error) {
		if baselineCalls.Add(1) == 1 {
			// The baseline publishes before the actor waits on it.
			c.Published(ctx, repo, checkout)
		}
		return true, nil
	}, Yield: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scheduled, status := scheduleAndWait(t, ctx, c, []string{"repo-a"}, "repo-a", nil)
	if scheduled.Result != ContractDeferred || baselineCalls.Load() != 1 || status.Result != ContractActorInputsStale {
		t.Fatalf("baseline=%+v status=%+v calls=%d", scheduled, status, baselineCalls.Load())
	}
	second, cancelSecond := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelSecond()
	again, err := c.Schedule(second, ContractScheduleRequest{Actor: scheduled.Actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a"}, Epoch: c.InputEpoch()})
	if err != nil || again.ArrivalSeq <= scheduled.ArrivalSeq {
		t.Fatalf("second baseline admission=%+v %v", again, err)
	}
	if _, err := c.WaitActor(second, again.Actor, again.ArrivalSeq); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second baseline reused the resolved promise: %v", err)
	}
	require.Eventually(t, func() bool { return baselineCalls.Load() == 2 }, 5*time.Second, time.Millisecond)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := c.Schedule(canceled, ContractScheduleRequest{Actor: scheduled.Actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission err=%v", err)
	}
}

func TestContractAnalysisCoordinatorCloseJoinsAdmissionRelease(t *testing.T) {
	f := newContractWorkerFixture(t)
	releasing := make(chan struct{})
	resume := make(chan struct{})
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		return ContractFollowupSnapshot{Release: func() { close(releasing); <-resume }}, []ContractFollowupTarget{{Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a", InputVersion: "v", InputFingerprint: "f"}, Catalog: f.store}}, nil
	}, Yield: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	actor := NewContractActor("", []string{"repo-a"})
	scheduled, err := c.Schedule(context.Background(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a"}, Inputs: &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan ContractActorStatus, 1)
	go func() {
		status, _ := c.WaitActor(context.Background(), actor, scheduled.ArrivalSeq)
		waited <- status
	}()
	select {
	case <-releasing:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close certified a job still owning its release: %v", err)
	}
	close(resume)
	if status := <-waited; status.Result == ContractActorPublished {
		t.Fatal("incomplete handoff published")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContractAnalysisCoordinatorPendingAcceptanceWaitsWithoutCapture(t *testing.T) {
	f := newContractWorkerFixture(t)
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		t.Error("pending input was captured for analysis")
		return ContractFollowupSnapshot{}, nil, errors.New("pending")
	}, Yield: ContractAnalysisYield(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inputs := &graphview.SelectedContractInputs{State: graph.ContractInputState{RepoPrefix: "repo-a", InputVersion: "pending-v", InputFingerprint: "pending-f"}}
	actor := NewContractActor("", []string{"repo-a"})
	scheduled, err := c.Schedule(ctx, ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a", InputVersion: "pending-v", InputFingerprint: "pending-f"}, Inputs: inputs})
	if err != nil || scheduled.Result != ContractDeferred {
		t.Fatalf("pending observation=%+v %v", scheduled, err)
	}
	c.Published(ctx, "repo-a", "") // Acceptance races the actor's deferral.
	status, err := c.WaitActor(ctx, actor, scheduled.ArrivalSeq)
	if err != nil || status.Result != ContractActorInputsStale {
		t.Fatalf("acceptance did not resolve the pending promise: %+v %v", status, err)
	}
}
