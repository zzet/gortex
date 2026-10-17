package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func TestContractDependencyBaselineSchedulesTheMissingPhysicalNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, store := newSQLiteIndexer(t)
	leases := graphview.NewLeaseManager()
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}
	mi := NewMultiIndexer(store, first.registry, search.NewNull(), nil, zap.NewNop())
	mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	for _, repo := range []string{"a-certified", "b-missing", "c-missing", "d-missing"} {
		idx := first
		if repo != "a-certified" {
			idx = New(store, first.registry, first.config, zaptest.NewLogger(t))
			t.Cleanup(idx.Close)
		}
		idx.SetRepoPrefix(repo)
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nconst route = \"/dependency-route\"\nfunc register(router *Router) { router.GET(route, serve) }\nfunc serve() {}\n")
		backend, err := newPrimaryContractCoreStorageBackend(ctx, store, repo)
		require.NoError(t, err)
		idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
		require.NoError(t, err)
		result, err := idx.IndexCtx(ctx, root)
		require.NoError(t, err)
		require.Empty(t, result.FailedFiles)
		mi.repos[repo] = &RepoMetadata{RepoPrefix: repo, RootPath: root}
		mi.indexers[repo] = idx
		registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: repo, RootIdentity: root, Incarnation: repo}, nil)
		require.NoError(t, err)
		_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-"+repo)
		require.NoError(t, err)
	}
	var admission sync.Mutex
	var jobs sync.WaitGroup
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: first.registry, Config: first.config, Logger: zaptest.NewLogger(t), Yield: func(ctx context.Context) error { return ctx.Err() }, BaselineAdmissionMu: &admission, BaselineJobs: &jobs}
	// Certify only the output namespace. Its composed input still needs the
	// other physical namespace; retrying the output's baseline cannot help.
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, "a-certified"))
	for _, repo := range []string{"b-missing", "c-missing", "d-missing"} {
		prior, found, err := store.ContractInputStateContext(ctx, repo, "")
		require.NoError(t, err)
		require.True(t, found)
		pending := prior
		pending.InputFingerprint = repo + "-pending"
		pending.Accepted = false
		require.NoError(t, store.BeginContractInputMutationContext(ctx, &prior, pending, nil))
		baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, repo, "")
		require.NoError(t, err)
		require.Nil(t, baseline)
	}
	entered := make(chan struct{}, 2)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	options.Yield = func(ctx context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-resume:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: store, Leases: leases, Registry: first.registry, Config: first.config, Capture: NewContractFollowupSourceCapture(options), Inputs: NewContractPrimaryInputCapture(materializer), ReconcileBaseline: NewContractBaselineReconciler(options), Yield: options.Yield})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coordinator.Close()); jobs.Wait() })
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	repos := []string{"a-certified", "b-missing", "c-missing", "d-missing"}
	pending, err := NewContractPrimaryInputCapture(materializer)(ctx, repos, "a-certified")
	require.NoError(t, err)
	require.False(t, pending.State.Accepted)
	scheduled, err := coordinator.Schedule(ctx, ContractScheduleRequest{Actor: NewContractActor("", repos), Key: graph.ContractAttachmentKey{RepoPrefix: "a-certified", InputVersion: pending.State.InputVersion, InputFingerprint: pending.State.InputFingerprint}, Inputs: pending, Epoch: coordinator.InputEpoch()})
	require.NoError(t, err)
	require.Equal(t, ContractDeferred, scheduled.Result)
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("missing physical namespaces had no baseline producer", ctx.Err())
		}
	}
	// Both baseline slots are occupied by witnessed producers; the third
	// missing namespace could not start. Their completion announces itself,
	// the actor captures its inputs again, and the namespace still pending
	// gets its producer once capacity is available.
	resumeOnce.Do(func() { close(resume) })
	complete := scheduleUntilAttached(t, ctx, coordinator, materializer, repos, "a-certified")
	require.True(t, complete.State.Accepted)
	jobs.Wait()
	for _, repo := range repos[1:] {
		baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, repo, "")
		require.NoError(t, err)
		require.NotNil(t, baseline, repo)
		require.Equal(t, contractBoundaryReceiptVersion, baseline.Version)
	}
	analysis, err := materializer.OpenContractAnalysisForInputs(ctx, nil, complete)
	require.NoError(t, err)
	nodes, err := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
	require.NoError(t, err)
	var ids []string
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	require.Contains(t, ids, "http::GET::/dependency-route")
	require.NoError(t, analysis.Validate(ctx))
	analysis.Close()
}

func TestContractDependencyBaselineDoesNotInventAProducer(t *testing.T) {
	for _, mode := range []string{"refused", "canceled", "changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newContractWorkerFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prior, found, err := f.store.ContractInputStateContext(ctx, "repo-b", "")
			require.NoError(t, err)
			require.True(t, found)
			pending := prior
			pending.InputFingerprint = "pending-dependency"
			pending.Accepted = false
			require.NoError(t, f.store.BeginContractInputMutationContext(ctx, &prior, pending, nil))
			materializer := &graphview.Materializer{Store: f.store, Catalog: f.store.Catalog(), Leases: f.leases}
			a, err := materializer.CaptureContractInputs(ctx, nil, "repo-a", "")
			require.NoError(t, err)
			b, err := materializer.CaptureContractInputs(ctx, nil, "repo-b", "")
			require.NoError(t, err)
			input, err := graphview.ComposeSelectedContractInputs("repo-a", "", a, b, b)
			require.NoError(t, err)
			var calls atomic.Int64
			coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				t.Error("unaccepted input reached attachment capture")
				return ContractFollowupSnapshot{}, nil, errors.New("unexpected capture")
			}, ReconcileBaseline: func(ctx context.Context, view *graphview.RepoView, repo, checkout string) (bool, error) {
				calls.Add(1)
				if view != nil || repo != "repo-b" || checkout != "" {
					t.Errorf("baseline admission for view=%v repo=%q checkout=%q", view, repo, checkout)
				}
				if mode == "canceled" {
					// The request gives up; the worker's admission is not
					// the request's to cancel.
					cancel()
				}
				if mode == "changed" {
					if err := f.store.AcceptContractInputMutationContext(ctx, pending); err != nil {
						t.Error(err)
					}
				}
				return false, nil
			}, Yield: ContractAnalysisYield(nil)})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, coordinator.Close()) })
			actor := NewContractActor("", []string{"repo-a", "repo-b"})
			scheduled, err := coordinator.Schedule(ctx, ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a", InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}, Inputs: input, Epoch: coordinator.InputEpoch()})
			require.NoError(t, err)
			require.Equal(t, ContractDeferred, scheduled.Result)
			// No producer was admitted: the actor waits on the inputs'
			// acceptance, or on their recapture once they moved.
			require.Eventually(t, func() bool {
				return coordinator.Status(actor).State == contractActorDeferred.String() || coordinator.Status(actor).Result == ContractActorInputsStale
			}, 5*time.Second, time.Millisecond)
			require.EqualValues(t, 1, calls.Load(), "duplicate physical witnesses must not duplicate repair admission")
			wait, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer stop()
			status, err := coordinator.WaitActor(wait, actor, scheduled.ArrivalSeq)
			switch mode {
			case "refused", "canceled":
				require.ErrorIs(t, err, context.DeadlineExceeded, "no producer means no false progress: %+v", status)
				require.Equal(t, contractActorDeferred.String(), coordinator.Status(actor).State)
			case "changed":
				require.NoError(t, err)
				require.Equal(t, ContractActorInputsStale, status.Result, "the accepted inputs are captured again")
			}
		})
	}
}

func TestContractDependencyBaselineAdmitsWhileAPrimarySourceApplies(t *testing.T) {
	options, _, _ := contractBaselineAuthorityFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options.Context = ctx
	var admission sync.Mutex
	var jobs sync.WaitGroup
	options.BaselineAdmissionMu, options.BaselineJobs = &admission, &jobs
	reconcile := NewContractBaselineReconciler(options)
	admittedBaseline := make(chan bool, 1)
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: options.Store, Leases: options.Materializer.Leases, Registry: options.Registry, Config: options.Config, Capture: NewContractFollowupSourceCapture(options), ReconcileBaseline: func(ctx context.Context, view *graphview.RepoView, repo, checkout string) (bool, error) {
		started, err := reconcile(ctx, view, repo, checkout)
		select {
		case admittedBaseline <- started && err == nil:
		default:
		}
		return started, err
	}, Yield: options.Yield})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coordinator.Close()); jobs.Wait() })
	options.MultiIndexer.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	mutation, err := options.Materializer.Leases.AcquireBaseCorpusMutation(ctx, "fixture", options.MultiIndexer.repoRootPath("fixture"))
	require.NoError(t, err)
	t.Cleanup(mutation.Release)
	input, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.False(t, input.State.Accepted)
	// An applying raw source no longer refuses admission: refusing it kept a
	// cold store's consumers waiting on a producer that never started. The
	// job's census and writes serialize with core writes on the repository
	// lanes and fence on the pending input state instead.
	actor := NewContractActor("", []string{"fixture"})
	scheduled, err := coordinator.Schedule(ctx, ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}, Inputs: input, Epoch: coordinator.InputEpoch()})
	require.NoError(t, err)
	require.Equal(t, ContractDeferred, scheduled.Result)
	select {
	case started := <-admittedBaseline:
		require.True(t, started, "the baseline is admitted while a raw-source mutation applies")
	case <-ctx.Done():
		t.Fatal("no baseline admission")
	}
	require.NoError(t, mutation.Complete("accepted-after-apply"))
	mutation.Release()
	coordinator.Published(ctx, "fixture", "")
	status, err := coordinator.WaitActor(ctx, actor, scheduled.ArrivalSeq)
	require.NoError(t, err)
	require.Equal(t, ContractActorInputsStale, status.Result)
	jobs.Wait()
	input, err = options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, input.State.Accepted)
	reconciler := NewContractBaselineReconciler(options)
	admitted, err := reconciler(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.False(t, admitted, "already certified primary input has no baseline repair producer")
	admitted, err = reconciler(ctx, &graphview.RepoView{}, "fixture", "historical")
	require.NoError(t, err)
	require.False(t, admitted, "an immutable positive view cannot be repaired as mutable primary")
}
