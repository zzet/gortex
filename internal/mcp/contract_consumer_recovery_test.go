package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// This fixture owns an explicit close boundary so a reopen has no in-memory
// actor, cache, source receipt or materializer state from the publication.
func persistentConsumerFixture(t *testing.T, path string) (*contractConsumerFixture, func()) {
	t.Helper()
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	var once sync.Once
	closeStore := func() { once.Do(func() { require.NoError(t, store.Close()) }) }
	t.Cleanup(closeStore)
	f := &contractConsumerFixture{store: store, root: filepath.Dir(path), states: map[string]graph.ContractInputState{}}
	for _, repo := range []string{"repo", "b"} {
		state, found, err := store.ContractInputStateContext(t.Context(), repo, "")
		require.NoError(t, err)
		if !found {
			state = graph.ContractInputState{RepoPrefix: repo, InputVersion: "v", InputFingerprint: repo + "-original", Accepted: true}
			require.NoError(t, store.BeginContractInputMutationContext(t.Context(), nil, state, nil))
			require.NoError(t, store.AcceptContractInputMutationContext(t.Context(), state))
			path := repo + "/handler.go"
			store.AddBatch([]*graph.Node{{ID: path, Name: "handler.go", Kind: graph.KindFile, FilePath: path, RepoPrefix: repo}, {ID: path + "::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: path, RepoPrefix: repo, WorkspaceID: "ws", ProjectID: "project"}}, nil)
		}
		f.states[repo] = state
	}
	engine := query.NewEngine(store)
	f.srv = NewServer(engine, store, nil, nil, zap.NewNop(), nil)
	f.srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()})
	return f, closeStore
}

func absentRecoveryRuntime(t *testing.T, f *contractConsumerFixture) *contractRuntimeFake {
	t.Helper()
	c, err := indexer.NewContractAnalysisCoordinator(indexer.ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.srv.materializer.Leases, Registry: testRegistry(), Config: config.Default().Index, Yield: func(ctx context.Context) error { return ctx.Err() }, Capture: func(context.Context, *indexer.ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (indexer.ContractFollowupSnapshot, []indexer.ContractFollowupTarget, error) {
		t.Fatal("restart recovery started fresh analysis")
		return indexer.ContractFollowupSnapshot{}, nil, nil
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2), acquire: c.AcquireLastPublishedCohort}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	return fake
}

func TestContractRestartCurrentExactAndSingleMovedCohortNeedNoFreshAnalysis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	original, closeOriginal := persistentConsumerFixture(t, path)
	original.publish(t, nil, "", "/published")
	proof := capturedConsumerProof(t, original, 1)
	closeOriginal()
	f, _ := persistentConsumerFixture(t, path)
	fake := absentRecoveryRuntime(t, f)
	current, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{}, capabilityRequest{})
	require.Nil(t, refusal)
	contractAnalysisFromContext(current).close()
	require.Zero(t, fake.schedules.Load())
	moveConsumerInput(t, f, "after-restart")
	// Optional recovery has no producer wait. The fake only records interest;
	// its real empty coordinator supplies the typed absent-metadata boundary.
	ctx := prepareOptionalForTest(t, t.Context(), f, false)
	registry := f.srv.optionalContractRegistryForContext(ctx)
	require.NotNil(t, registry)
	require.NotEmpty(t, registry.ByID("http::GET::/published"))
	binding := contractAnalysisFromContext(ctx)
	require.True(t, binding.recovered)
	require.True(t, binding.served)
	require.NoError(t, binding.validate(ctx))
	for _, target := range proof.Targets {
		require.Equal(t, target.PayloadGeneration, binding.views[target.Key.RepoPrefix].Attachment.PayloadGeneration)
	}
	status := contractConsumerStatusFromContext(ctx)
	require.False(t, status.fresh)
	require.True(t, status.receiptsUnknown)
	facts := optionalContractFacts(status)
	require.Equal(t, false, facts["changed_files_known"])
	require.NotContains(t, facts, "changed_file_count")
	require.Equal(t, int64(1), fake.schedules.Load())
	require.Zero(t, fake.waits.Load())
	moveConsumerInput(t, f, "third-during-handler")
	require.NoError(t, binding.validate(ctx))
	binding.close()
	for _, target := range proof.Targets {
		require.False(t, f.srv.materializer.Leases.InUse(target.PayloadGeneration))
	}
}

func moveCompanionInput(t *testing.T, f *contractConsumerFixture, fingerprint string) {
	t.Helper()
	old := f.states["b"]
	next := old
	next.InputFingerprint = fingerprint
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, next, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), next))
	f.states["b"] = next
}

func TestContractRecoveryTwoMovedOrOtherCohortPublicationCannotMix(t *testing.T) {
	for _, kind := range []string{"two_moved", "published_only_other_cohort"} {
		t.Run(kind, func(t *testing.T) {
			f := newContractConsumerFixture(t)
			f.publish(t, nil, "", "/old")
			moveConsumerInput(t, f, "repo-new")
			if kind == "two_moved" {
				moveCompanionInput(t, f, "b-new")
			} else {
				// A different admitted cohort published only repo's current identity.
				// Discovery must not invent a matching two-repo previous vector.
				input, err := f.srv.materializer.CaptureContractInputs(t.Context(), nil, "repo", "")
				require.NoError(t, err)
				generation, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "ref_view", GraphID: "other-cohort", LayerID: "single", GenerationKind: "commit", TreeOID: "other", CreatedAt: 1})
				require.NoError(t, err)
				require.NoError(t, payload.SetProducerState(store_sqlite.ProducerCompleteness{Producer: string(graphview.CapContracts), State: store_sqlite.ProducerStateComplete}))
				header := graph.ContractAttachment{RepoPrefix: "repo", InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint, PayloadGeneration: generation}
				require.NoError(t, f.store.PublishContractAttachmentWithInputsContext(t.Context(), input.State, input.Witnesses, header, nil, 2))
				moveCompanionInput(t, f, "b-new")
			}
			absentRecoveryRuntime(t, f)
			binding, proof, err := f.srv.openContractPredecessorBinding(t.Context(), nil, "", []string{"b", "repo"}, indexer.NewContractActor("", []string{"b", "repo"}))
			require.Error(t, err)
			require.Nil(t, binding)
			require.Nil(t, proof)
		})
	}
}

func TestContractRecoveryExactTwinsAndKnownInvalidMetadataNeverFallThrough(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	moveConsumerInput(t, f, "new")
	fake := absentRecoveryRuntime(t, f)
	for _, policy := range []string{"exact", "explicit"} {
		fake.schedule = func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
			req.Source.Release()
			return indexer.ContractScheduleResult{}, &indexer.ContractQueueFullError{RetryAfter: time.Second}
		}
		freshness := requestFreshness{requireExact: policy == "exact"}
		want := capabilityRequest{}
		if policy == "explicit" {
			want.required = []graphview.CapabilityID{graphview.CapContracts}
		}
		_, refusal := prepareConsumerForTest(t.Context(), f, freshness, want)
		require.NotNil(t, refusal)
		require.True(t, refusal.IsError)
	}
	for _, invalid := range []error{graph.ErrContractProjectionIncomplete, graph.ErrContractProjectionStale, graph.ErrContractPredecessorRetired} {
		fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
			return nil, nil, invalid
		}
		f.srv.SetContractAnalysisRuntime(fake.runtime())
		before := viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal]
		binding, _, err := f.srv.openContractPredecessorBinding(t.Context(), nil, "", []string{"b", "repo"}, indexer.NewContractActor("", []string{"b", "repo"}))
		require.ErrorIs(t, err, invalid)
		require.Nil(t, binding, "invalid known metadata cannot use an otherwise recoverable previous key")
		expected := before
		if invalid == graph.ErrContractPredecessorRetired {
			expected++
		}
		require.Equal(t, expected, viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal])
	}
}

func TestContractRecoveryRetirementRefusesAndCountsOnce(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	proof := capturedConsumerProof(t, f, 1)
	moveConsumerInput(t, f, "new")
	absentRecoveryRuntime(t, f)
	require.NoError(t, f.store.Catalog().SetViewGenerationState(t.Context(), proof.Targets[1].PayloadGeneration, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady))
	before := viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal]
	binding, _, err := f.srv.openContractPredecessorBinding(t.Context(), nil, "", []string{"b", "repo"}, indexer.NewContractActor("", []string{"b", "repo"}))
	require.ErrorIs(t, err, graph.ErrContractPredecessorRetired)
	require.Nil(t, binding)
	require.Equal(t, before+1, viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal])
	for _, target := range proof.Targets {
		require.False(t, f.srv.materializer.Leases.InUse(target.PayloadGeneration))
	}
}

func TestContractRecoveryOptionalExactActualReadRefusesInsteadOfRecovering(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	moveConsumerInput(t, f, "new")
	absentRecoveryRuntime(t, f)
	ctx := prepareOptionalForTest(t, t.Context(), f, true)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	require.Nil(t, f.srv.optionalContractRegistryForContext(ctx))
	require.ErrorContains(t, binding.lazyErr, "required_capability_incomplete")
	require.False(t, binding.recovered)
}

// Keep the middleware's corrected checked-registry boundary on recovered views.
func TestContractRecoveryMiddlewareRejectsCheckedRegistryFailure(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	moveConsumerInput(t, f, "new")
	absentRecoveryRuntime(t, f)
	result, err := f.request(t.Context(), "get_architecture", map[string]any{"format": "json"}, func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		binding := contractAnalysisFromContext(ctx)
		require.NoError(t, binding.ensure(ctx))
		require.True(t, binding.recovered)
		view := binding.views["repo"]
		view.RegistryReader = failedContractAnalysisRegistry{Reader: view.RegistryReader, failure: graph.ErrContractProjectionLimit}
		require.Nil(t, f.srv.optionalContractRegistryForContext(ctx))
		require.NoError(t, view.ValidateServed(ctx))
		return mcplib.NewToolResultText(`{"must_not_escape":true}`), nil
	})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, viewResultText(t, result), graph.ErrContractProjectionLimit.Error())
}

func recoveryLinkedView(t *testing.T, f *contractConsumerFixture) *graphview.RepoView {
	t.Helper()
	graphID := indexer.GraphIDFor("repo")
	worktreeRoot := filepath.Join(f.root, "linked")
	require.NoError(t, os.MkdirAll(worktreeRoot, 0o755))
	seedViewCatalog(t, f.store, graphID, filepath.Join(f.root, "repo"), worktreeRoot)
	generation, handle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: graphID, CheckoutID: viewTestWorktree, LayerID: "recovery-core", GenerationKind: "commit", TreeOID: "recovery-tree", CreatedAt: 1000})
	require.NoError(t, err)
	state := graph.ContractInputState{RepoPrefix: "repo", CheckoutID: viewTestWorktree, InputVersion: "boundary-v1", InputFingerprint: "linked-stable", Accepted: true}
	require.NoError(t, handle.SetContractInputStateWithWorkContext(t.Context(), nil, state, nil))
	handle.AddBatch([]*graph.Node{{ID: "repo/handler.go", Name: "handler.go", Kind: graph.KindFile, FilePath: "repo/handler.go", RepoPrefix: "repo"}, {ID: "repo/handler.go::Handle", Name: "SelectedLinkedHandler", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}}, nil)
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/handler.go", Mode: store_sqlite.OwnershipReplace}}))
	require.NoError(t, f.store.PublishPayloadGeneration(t.Context(), generation, 2000))
	dirty, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: graphID, CheckoutID: viewTestWorktree, LayerID: "recovery-dirty", GenerationKind: "dirty", BaseGenerationID: generation, TreeOID: "recovery-tree", CreatedAt: 3000})
	require.NoError(t, err)
	require.NoError(t, payload.SetContractInputStateWithWorkContext(t.Context(), &state, state, nil))
	require.NoError(t, f.store.PublishPayloadGeneration(t.Context(), dirty, 4000))
	routeViewCheckout(t, f.store, graphID, generation, dirty, store_sqlite.RouteActive)
	selected, err := f.srv.materializer.MaterializeCheckout(t.Context(), viewTestWorktree)
	require.NoError(t, err)
	t.Cleanup(selected.Close)
	require.True(t, selected.ComposesBaseCorpus())
	return selected
}

func TestContractRecoveryLinkedAncestryOnePhysicalMoveAndTwoMoveRefusal(t *testing.T) {
	f := newContractConsumerFixture(t)
	selected := recoveryLinkedView(t, f)
	f.publish(t, selected, viewTestWorktree, "/linked-old")
	absentRecoveryRuntime(t, f)
	moveConsumerInput(t, f, "base-moved")
	actor := indexer.NewContractActor(viewTestWorktree, []string{"b", "repo"})
	binding, proof, err := f.srv.openContractPredecessorBinding(t.Context(), selected, viewTestWorktree, []string{"b", "repo"}, actor)
	require.NoError(t, err)
	require.Nil(t, proof, "recovered ancestry is not a receipt/seq publication record")
	require.True(t, binding.recovered)
	require.Equal(t, viewTestWorktree, binding.views["b"].Attachment.CheckoutID)
	reader, err := binding.analysisReader(t.Context())
	require.NoError(t, err)
	require.Equal(t, "SelectedLinkedHandler", reader.GetNode("repo/handler.go::Handle").Name, "current admitted linked composition must not primary-substitute")
	require.NotNil(t, reader.GetNode("http::GET::/linked-old"))
	require.NoError(t, binding.validate(t.Context()))
	binding.close()
	moveCompanionInput(t, f, "companion-moved")
	binding, _, err = f.srv.openContractPredecessorBinding(t.Context(), selected, viewTestWorktree, []string{"b", "repo"}, actor)
	require.Error(t, err)
	require.Nil(t, binding, "two physical contributors cannot compose one historical predecessor")
	binding, _, err = f.srv.openContractPredecessorBinding(t.Context(), selected, "other-checkout", []string{"b", "repo"}, indexer.NewContractActor("other-checkout", []string{"b", "repo"}))
	require.Error(t, err)
	require.Nil(t, binding)
}

func TestContractRecoveryKeepsAlreadyPublishedContributorDespiteOlderRider(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/first")
	moveCompanionInput(t, f, "b-current")
	f.publish(t, nil, "", "/second")
	// B's previous_* still points to first publication. Its CURRENT identity
	// already participates in second's complete cohort and must stay current.
	moveConsumerInput(t, f, "repo-pending")
	absentRecoveryRuntime(t, f)
	binding, _, err := f.srv.openContractPredecessorBinding(t.Context(), nil, "", []string{"b", "repo"}, indexer.NewContractActor("", []string{"b", "repo"}))
	require.NoError(t, err)
	defer binding.close()
	registry, err := binding.loadRegistry(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, registry.ByID("http::GET::/second"))
	require.Empty(t, registry.ByID("http::GET::/first"))
}

func TestContractRecoveryCancellationDoesNotCountRetirement(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	moveConsumerInput(t, f, "new")
	absentRecoveryRuntime(t, f)
	before := viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	binding, _, err := f.srv.openContractPredecessorBinding(ctx, nil, "", []string{"b", "repo"}, indexer.NewContractActor("", []string{"b", "repo"}))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, binding)
	require.Equal(t, before, viewmetrics.Read().Counters[viewmetrics.ContractAnalysisPredecessorRetiredTotal])
}

func TestContractRecoveryStrictBoundUsesUnknownHistoryWithoutFreshSeqClaim(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/old")
	moveConsumerInput(t, f, "new")
	fake := absentRecoveryRuntime(t, f)
	fake.waitActor = func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
		return indexer.ContractActorStatus{}, context.DeadlineExceeded
	}
	ctx, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{}, capabilityRequest{})
	require.Nil(t, refusal)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	require.True(t, binding.recovered)
	status := contractConsumerStatusFromContext(ctx)
	require.False(t, status.fresh)
	require.True(t, status.receiptsUnknown)
	require.Equal(t, graphview.StateBuilding, status.state)
	require.Equal(t, int64(1), fake.schedules.Load())
	require.Equal(t, int64(1), fake.waits.Load())
	result := decorateContractConsumerResult(ctx, mcplib.NewToolResultText(`{"served":true}`))
	require.Equal(t, false, result.Meta.AdditionalFields["contract_analysis"].(map[string]any)["changed_files_known"])
}
