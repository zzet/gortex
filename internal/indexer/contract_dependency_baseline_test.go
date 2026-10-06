package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
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
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: store, Leases: leases, Registry: first.registry, Config: first.config, Capture: NewContractFollowupCapture(options), ReconcileBaseline: NewContractBaselineReconciler(options), Yield: options.Yield})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coordinator.Close()); jobs.Wait() })
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	capture := func() *graphview.SelectedContractInputs {
		var dependencies []*graphview.SelectedContractInputs
		for _, repo := range []string{"a-certified", "b-missing", "c-missing", "d-missing"} {
			input, err := materializer.CaptureContractInputs(ctx, nil, repo, "")
			require.NoError(t, err)
			dependencies = append(dependencies, input)
		}
		require.True(t, dependencies[0].State.Accepted)
		input, err := graphview.ComposeSelectedContractInputs("a-certified", "", dependencies...)
		require.NoError(t, err)
		return input
	}
	pending := capture()
	require.False(t, pending.State.Accepted)
	admitted, err := coordinator.Request(ctx, nil, pending, "a-certified", "")
	require.NoError(t, err)
	require.True(t, admitted)
	key := graph.ContractAttachmentKey{RepoPrefix: "a-certified", InputVersion: pending.State.InputVersion, InputFingerprint: pending.State.InputFingerprint}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("missing physical namespaces had no baseline producer", ctx.Err())
		}
	}
	// Both slots are occupied by witnessed producers. Register a separate
	// missing dependency before releasing them; completion must wake it after
	// capacity is available, even though this request cannot start a third job.
	saturated, err := coordinator.Request(ctx, nil, nil, "d-missing", "")
	require.NoError(t, err)
	require.True(t, saturated)
	resumeOnce.Do(func() { close(resume) })
	require.NoError(t, coordinator.WaitChange(ctx, graph.ContractAttachmentKey{RepoPrefix: "d-missing"}))
	require.NoError(t, coordinator.WaitChange(ctx, key), "the missing dependency must have an actual producer")
	var complete *graphview.SelectedContractInputs
	for {
		complete = capture()
		if complete.State.Accepted {
			break
		}
		admitted, err = coordinator.Request(ctx, nil, complete, "a-certified", "")
		if errors.Is(err, graph.ErrContractProjectionStale) {
			continue
		}
		require.NoError(t, err)
		require.True(t, admitted)
		key.InputVersion, key.InputFingerprint = complete.State.InputVersion, complete.State.InputFingerprint
		require.NoError(t, coordinator.WaitChange(ctx, key))
	}
	jobs.Wait()
	baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, "b-missing", "")
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.Equal(t, contractBoundaryReceiptVersion, baseline.Version)
	admitted, err = coordinator.Request(ctx, nil, complete, "a-certified", "")
	require.NoError(t, err)
	require.True(t, admitted)
	key.InputVersion, key.InputFingerprint = complete.State.InputVersion, complete.State.InputFingerprint
	for {
		analysis, err := materializer.OpenContractAnalysisForInputs(ctx, nil, complete)
		if err == nil {
			nodes, readErr := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
			require.NoError(t, readErr)
			var ids []string
			for _, node := range nodes {
				ids = append(ids, node.ID)
			}
			require.Contains(t, ids, "http::GET::/dependency-route")
			require.NoError(t, analysis.Validate(ctx))
			analysis.Close()
			break
		}
		require.Equal(t, graphview.CodeRequiredCapabilityIncomplete, graphview.CodeOf(err))
		require.NoError(t, coordinator.WaitChange(ctx, key))
	}
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
			calls := 0
			coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				t.Error("unaccepted input reached attachment capture")
				return ContractFollowupSnapshot{}, nil, errors.New("unexpected capture")
			}, ReconcileBaseline: func(ctx context.Context, view *graphview.RepoView, repo, checkout string) (bool, error) {
				calls++
				require.Nil(t, view)
				require.Equal(t, "repo-b", repo)
				require.Empty(t, checkout)
				if mode == "canceled" {
					cancel()
					return false, ctx.Err()
				}
				if mode == "changed" {
					require.NoError(t, f.store.AcceptContractInputMutationContext(ctx, pending))
				}
				return false, nil
			}, Yield: ContractAnalysisYield(nil)})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, coordinator.Close()) })
			admitted, err := coordinator.Request(ctx, nil, input, "repo-a", "")
			require.False(t, admitted)
			require.Equal(t, 1, calls, "duplicate physical witnesses must not duplicate repair admission")
			switch mode {
			case "refused":
				require.NoError(t, err)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			case "changed":
				require.ErrorIs(t, err, graph.ErrContractProjectionStale)
			}
			coordinator.mu.Lock()
			require.Empty(t, coordinator.baselines, "no false progress-only waiter may survive refusal")
			coordinator.mu.Unlock()
		})
	}
}

func TestContractDependencyBaselineRecapturesAnApplyingPrimarySource(t *testing.T) {
	options, _, _ := contractBaselineAuthorityFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options.Context = ctx
	var admission sync.Mutex
	var jobs sync.WaitGroup
	options.BaselineAdmissionMu, options.BaselineJobs = &admission, &jobs
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: options.Store, Leases: options.Materializer.Leases, Registry: options.Registry, Config: options.Config, Capture: NewContractFollowupCapture(options), ReconcileBaseline: NewContractBaselineReconciler(options), Yield: options.Yield})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coordinator.Close()); jobs.Wait() })
	options.MultiIndexer.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	mutation, err := options.Materializer.Leases.AcquireBaseCorpusMutation(ctx, "fixture", options.MultiIndexer.repoRootPath("fixture"))
	require.NoError(t, err)
	t.Cleanup(mutation.Release)
	input, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.False(t, input.State.Accepted)
	admitted, err := coordinator.Request(ctx, nil, input, "fixture", "")
	require.False(t, admitted, "applying authority does not pretend a baseline worker was admitted")
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "the existing bounded recapture path must preserve primary source publication")
	require.NoError(t, mutation.Complete("accepted-after-apply"))
	mutation.Release()
	coordinator.Published(ctx, "fixture", "")
	// This is the same original request context and deadline, with actual core
	// publication now complete. It can admit the previously refused repair.
	input, err = options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	admitted, err = coordinator.Request(ctx, nil, input, "fixture", "")
	require.NoError(t, err)
	require.True(t, admitted)
	require.NoError(t, coordinator.WaitChange(ctx, graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}))
	jobs.Wait()
	input, err = options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, input.State.Accepted)
	reconciler := NewContractBaselineReconciler(options)
	admitted, err = reconciler(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.False(t, admitted, "already certified primary input has no baseline repair producer")
	admitted, err = reconciler(ctx, &graphview.RepoView{}, "fixture", "historical")
	require.NoError(t, err)
	require.False(t, admitted, "an immutable positive view cannot be repaired as mutable primary")
}
