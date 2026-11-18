package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func capturedConsumerProof(t *testing.T, f *contractConsumerFixture, seq uint64) *indexer.ContractPublishedCohort {
	t.Helper()
	var captures []*graphview.SelectedContractInputs
	for _, repo := range []string{"b", "repo"} {
		input, err := f.srv.materializer.CaptureContractInputs(t.Context(), nil, repo, "")
		require.NoError(t, err)
		captures = append(captures, input)
	}
	proof := &indexer.ContractPublishedCohort{Seq: seq}
	for _, input := range captures {
		proof.Witnesses = append(proof.Witnesses, input.Witnesses...)
	}
	for _, repo := range []string{"b", "repo"} {
		input, err := graphview.ComposeSelectedContractInputs(repo, "", captures...)
		require.NoError(t, err)
		key := graph.ContractAttachmentKey{RepoPrefix: repo, InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}
		header, err := f.store.GetContractAttachmentContext(t.Context(), key)
		require.NoError(t, err)
		require.NotNil(t, header)
		proof.ExpectedTargets = append(proof.ExpectedTargets, key)
		proof.Targets = append(proof.Targets, indexer.ContractPublishedTarget{Key: key, PayloadGeneration: header.PayloadGeneration})
	}
	return proof
}

func moveConsumerInput(t *testing.T, f *contractConsumerFixture, fingerprint string) {
	t.Helper()
	old := f.states["repo"]
	next := old
	next.InputFingerprint = fingerprint
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, next, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), next))
	f.states["repo"] = next
}

func prepareConsumerForTest(ctx context.Context, f *contractConsumerFixture, freshness requestFreshness, want capabilityRequest) (context.Context, *mcplib.CallToolResult) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "contracts"
	req.Params.Arguments = map[string]any{"action": "list"}
	return f.srv.prepareContractConsumer(ctx, req, freshness, want)
}

func TestContractConsumerQueueFullUsesOneExactHistoricalCohort(t *testing.T) {
	for _, policy := range []string{"default", "exact", "explicit", "require_complete", "unaccepted", "absent"} {
		t.Run(policy, func(t *testing.T) {
			f := newContractConsumerFixture(t)
			f.publish(t, nil, "", "/before")
			proof := capturedConsumerProof(t, f, 1)
			moveConsumerInput(t, f, "pending")
			if policy == "unaccepted" {
				old := f.states["repo"]
				pending := old
				pending.InputFingerprint, pending.Accepted = "pending-unaccepted", false
				require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, pending, nil))
			}
			fake := &contractRuntimeFake{t: t}
			fake.schedule = func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
				req.Source.Release()
				return indexer.ContractScheduleResult{}, &indexer.ContractQueueFullError{RetryAfter: time.Second}
			}
			releases, acquires := 0, 0
			fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
				acquires++
				if policy == "absent" {
					return nil, nil, graph.ErrContractProjectionIncomplete
				}
				return proof, func() { releases++ }, nil
			}
			f.srv.SetContractAnalysisRuntime(fake.runtime())
			freshness := requestFreshness{requireExact: policy == "exact"}
			want := capabilityRequest{requireComplete: policy == "require_complete"}
			if policy == "explicit" {
				want.required = []graphview.CapabilityID{graphview.CapContracts}
			}
			ctx, refusal := prepareConsumerForTest(t.Context(), f, freshness, want)
			require.Equal(t, int64(1), fake.schedules.Load())
			require.Zero(t, fake.waits.Load())
			if policy == "exact" || policy == "explicit" || policy == "absent" {
				require.NotNil(t, refusal)
				require.True(t, refusal.IsError)
				require.Contains(t, viewResultText(t, refusal), "required_capability_incomplete")
				if policy != "absent" {
					require.Zero(t, acquires)
				}
				return
			}
			require.Nil(t, refusal)
			binding := contractAnalysisFromContext(ctx)
			require.NotNil(t, binding)
			require.NoError(t, binding.validate(ctx))
			require.Equal(t, graphview.StateBuilding, contractConsumerStatusFromContext(ctx).state)
			require.False(t, contractConsumerStatusFromContext(ctx).fresh)
			for _, target := range proof.Targets {
				require.Equal(t, target.PayloadGeneration, binding.views[target.Key.RepoPrefix].Attachment.PayloadGeneration)
			}
			binding.close()
			binding.close()
			require.Equal(t, 1, releases)
		})
	}
}

func TestContractConsumerArrivalPublicationSurvivesThirdKeyMove(t *testing.T) {
	f := newContractConsumerFixture(t)
	moveConsumerInput(t, f, "arrival")
	var proof *indexer.ContractPublishedCohort
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(9)}
	fake.waitActor = func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
		f.publish(t, nil, "", "/arrival")
		proof = capturedConsumerProof(t, f, 9)
		moveConsumerInput(t, f, "third")
		return indexer.ContractActorStatus{Result: indexer.ContractActorPublished, Seq: 9}, nil
	}
	fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
		return proof, func() {}, nil
	}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	ctx, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{}, capabilityRequest{})
	require.Nil(t, refusal)
	binding := contractAnalysisFromContext(ctx)
	require.NotNil(t, binding)
	defer binding.close()
	require.Equal(t, graphview.StateComplete, contractConsumerStatusFromContext(ctx).state)
	require.True(t, contractConsumerStatusFromContext(ctx).fresh)
	moveConsumerInput(t, f, "fourth-during-handler")
	require.NoError(t, binding.validate(ctx))
	require.Equal(t, int64(1), fake.schedules.Load())
}

func TestContractConsumerCurrentPostHandlerValidationPolicyTwins(t *testing.T) {
	for _, policy := range []string{"default", "exact", "explicit"} {
		t.Run(policy, func(t *testing.T) {
			f := newContractConsumerFixture(t)
			f.publish(t, nil, "", "/current")
			fake := installForbiddenContractRuntime(t, f.srv)
			want := capabilityRequest{}
			if policy == "explicit" {
				want.required = []graphview.CapabilityID{graphview.CapContracts}
			}
			ctx, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{requireExact: policy == "exact"}, want)
			require.Nil(t, refusal)
			binding := contractAnalysisFromContext(ctx)
			defer binding.close()
			moveConsumerInput(t, f, "moved-during-handler")
			err := binding.validate(ctx)
			if policy == "default" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, graph.ErrContractProjectionStale)
			}
			require.Zero(t, fake.schedules.Load())
		})
	}
}

func TestContractConsumerBoundAndCancellationDoNotShareFallbackContext(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "cancel"}[canceled], func(t *testing.T) {
			f := newContractConsumerFixture(t)
			f.publish(t, nil, "", "/before")
			proof := capturedConsumerProof(t, f, 1)
			moveConsumerInput(t, f, "pending")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2)}
			fake.waitActor = func(wait context.Context, _ indexer.ContractActor, _ uint64) (indexer.ContractActorStatus, error) {
				if canceled {
					cancel()
					return indexer.ContractActorStatus{}, context.Canceled
				}
				// The wait seam reports its bound deterministically, without a timed sleep.
				return indexer.ContractActorStatus{}, context.DeadlineExceeded
			}
			acquires := 0
			fake.acquire = func(open context.Context, _ indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
				require.NoError(t, open.Err())
				acquires++
				return proof, func() {}, nil
			}
			f.srv.SetContractAnalysisRuntime(fake.runtime())
			bound, refusal := prepareConsumerForTest(ctx, f, requestFreshness{}, capabilityRequest{})
			if canceled {
				require.NotNil(t, refusal)
				require.Zero(t, acquires)
			} else {
				require.Nil(t, refusal)
				require.Equal(t, 1, acquires)
				binding := contractAnalysisFromContext(bound)
				defer binding.close()
				require.NoError(t, binding.validate(bound))
			}
		})
	}
}

func TestContractConsumerPredecessorRefusesWrongWinnerAndScope(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/before")
	proof := capturedConsumerProof(t, f, 1)
	releases := 0
	fake := &contractRuntimeFake{t: t}
	fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
		return proof, func() { releases++ }, nil
	}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	actor := indexer.NewContractActor("", []string{"b", "repo"})
	winner := proof.Targets[1].PayloadGeneration
	proof.Targets[1].PayloadGeneration++
	binding, _, err := f.srv.openPublishedContractConsumerBinding(t.Context(), nil, "", []string{"b", "repo"}, actor)
	require.Error(t, err)
	require.Nil(t, binding)
	require.Equal(t, 1, releases)
	require.False(t, f.srv.materializer.Leases.InUse(proof.Targets[0].PayloadGeneration))
	proof.Targets[1].PayloadGeneration = winner
	proof.Targets[1].Key.CheckoutID = "other-checkout"
	_, _, err = f.srv.openPublishedContractConsumerBinding(t.Context(), nil, "", []string{"b", "repo"}, actor)
	require.Error(t, err)
	require.Equal(t, 2, releases)
	require.False(t, errors.Is(err, context.Canceled))
}

func TestContractConsumerMinimalFreshnessNeverExceedsView(t *testing.T) {
	ctx := context.WithValue(t.Context(), contractConsumerContextKey{}, &contractConsumerStatus{state: graphview.StateComplete, fresh: true})
	result := mcplib.NewToolResultText(`{"freshness":{"fresh":false}}`)
	result = decorateContractConsumerResult(ctx, result)
	require.Equal(t, false, result.Meta.AdditionalFields["contract_analysis"].(map[string]any)["fresh"])
	require.Contains(t, viewResultText(t, result), `"fresh":false`)
}

func TestContractConsumerFailedActorCanServePredecessor(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/before")
	proof := capturedConsumerProof(t, f, 1)
	moveConsumerInput(t, f, "failed-arrival")
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2)}
	fake.waitActor = func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
		return indexer.ContractActorStatus{Result: indexer.ContractActorFailed, State: "idle", RetryAfter: time.Second}, nil
	}
	releases := 0
	fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
		return proof, func() { releases++ }, nil
	}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	ctx, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{}, capabilityRequest{})
	require.Nil(t, refusal)
	binding := contractAnalysisFromContext(ctx)
	require.NotNil(t, binding)
	require.NoError(t, binding.validate(ctx))
	require.False(t, contractConsumerStatusFromContext(ctx).fresh)
	binding.close()
	// The arrival check releases its older candidate, then fallback acquires
	// its own custody. Both paths must balance.
	require.Equal(t, 2, releases)
}

func TestContractConsumerZeroEpochSchedulesMissingOrUnacceptedInputsOnce(t *testing.T) {
	for _, state := range []string{"missing_baseline", "unaccepted", "missing_attachment"} {
		t.Run(state, func(t *testing.T) {
			f := newContractConsumerFixture(t)
			switch state {
			case "missing_baseline":
				// A real admitted repo has core rows but no input-state baseline yet.
				f.store.AddBatch([]*graph.Node{{ID: "cold/source.go", Name: "source.go", Kind: graph.KindFile, FilePath: "cold/source.go", RepoPrefix: "cold", WorkspaceID: "ws", ProjectID: "project"}}, nil)
			case "unaccepted":
				old := f.states["repo"]
				pending := old
				pending.InputFingerprint, pending.Accepted = "pending-at-zero", false
				require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, pending, nil))
			}
			fake := &contractRuntimeFake{t: t}
			require.Zero(t, fake.epoch.Load())
			fake.schedule = func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
				req.Source.Release()
				if state == "missing_baseline" {
					require.Nil(t, req.Inputs)
					require.Equal(t, "cold", req.Key.RepoPrefix)
				} else {
					require.NotNil(t, req.Inputs)
					require.Equal(t, state != "unaccepted", req.Inputs.State.Accepted)
				}
				require.Zero(t, req.Epoch)
				return indexer.ContractScheduleResult{Result: indexer.ContractScheduled, Actor: req.Actor, ArrivalSeq: 1}, nil
			}
			fake.waitActor = func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
				return indexer.ContractActorStatus{Result: indexer.ContractActorFailed, State: "idle"}, nil
			}
			runtime := fake.runtime()
			runtime.WaitInputs = func(context.Context, uint64) error {
				t.Fatal("epoch-zero initial miss waited for inputs instead of scheduling")
				return context.Canceled
			}
			f.srv.SetContractAnalysisRuntime(runtime)
			_, refusal := prepareConsumerForTest(t.Context(), f, requestFreshness{}, capabilityRequest{})
			require.NotNil(t, refusal)
			require.True(t, refusal.IsError)
			require.Equal(t, int64(1), fake.schedules.Load())
			require.Equal(t, int64(1), fake.waits.Load())
		})
	}
}

func TestContractConsumerZeroEpochAlreadyScheduledWaitsRatherThanReschedules(t *testing.T) {
	f := newContractConsumerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake := &contractRuntimeFake{t: t}
	fake.schedule = func(ctx context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
		if fake.schedules.Load() != 1 {
			t.Fatal("one unchanged epoch scheduled twice")
		}
		return scheduledAt(1)(ctx, req)
	}
	fake.waitActor = func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
		// Resolved promise without an opening attachment: wait for input movement.
		return indexer.ContractActorStatus{Result: indexer.ContractActorPublished, Seq: 1}, nil
	}
	runtime := fake.runtime()
	inputWaits := 0
	runtime.WaitInputs = func(context.Context, uint64) error {
		inputWaits++
		cancel()
		return context.Canceled
	}
	f.srv.SetContractAnalysisRuntime(runtime)
	_, refusal := prepareConsumerForTest(ctx, f, requestFreshness{}, capabilityRequest{})
	require.NotNil(t, refusal)
	require.Equal(t, 1, inputWaits)
	require.Equal(t, int64(1), fake.schedules.Load())
	require.Equal(t, int64(1), fake.waits.Load())
}
