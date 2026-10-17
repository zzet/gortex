package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/review"
)

func prepareOptionalForTest(t *testing.T, ctx context.Context, f *contractConsumerFixture, exact bool) context.Context {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "explain_change_impact"
	req.Params.Arguments = map[string]any{}
	prepared, refusal := f.srv.prepareContractConsumer(ctx, req, requestFreshness{requireExact: exact}, capabilityRequest{})
	require.Nil(t, refusal)
	return prepared
}

func TestOptionalContractPreparationIsLazyEvenForExactCoreOnlyAnswer(t *testing.T) {
	for _, exact := range []bool{false, true} {
		f := newContractConsumerFixture(t)
		fake := installForbiddenContractRuntime(t, f.srv)
		ctx := prepareOptionalForTest(t, t.Context(), f, exact)
		binding := contractAnalysisFromContext(ctx)
		defer binding.close()
		require.False(t, contractConsumerStatusFromContext(ctx).consulted)
		result := decorateContractConsumerResult(ctx, mcplib.NewToolResultText(`{"core":"answer"}`))
		require.NotContains(t, viewResultText(t, result), "contract_analysis")
		if result.Meta != nil {
			require.NotContains(t, result.Meta.AdditionalFields, "contract_analysis")
		}
		require.Zero(t, fake.captures.Load())
		require.Zero(t, fake.schedules.Load())
		require.Zero(t, fake.waits.Load())
	}
}

func TestOptionalContractFirstCurrentReadBindsOnceWithoutScheduling(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/current")
	fake := installForbiddenContractRuntime(t, f.srv)
	ctx := prepareOptionalForTest(t, t.Context(), f, false)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	reg := f.srv.optionalContractRegistryForContext(ctx)
	require.NotNil(t, reg)
	require.NotEmpty(t, reg.ByID("http::GET::/current"))
	require.NotNil(t, f.srv.optionalContractRegistryForContext(ctx))
	reader, err := f.srv.contractReaderForContext(ctx)
	require.NoError(t, err)
	require.NotNil(t, reader.GetNode("http::GET::/current"))
	require.NoError(t, binding.validate(ctx))
	status := contractConsumerStatusFromContext(ctx)
	require.True(t, status.consulted)
	require.True(t, status.fresh)
	require.Equal(t, graphview.StateComplete, status.state)
	require.Equal(t, int64(1), fake.captures.Load())
	require.Zero(t, fake.schedules.Load())
	require.Zero(t, fake.waits.Load())
}

func TestOptionalContractStaleAndMissingReadScheduleOnceNeverWait(t *testing.T) {
	for _, state := range []string{"stale", "absent", "queue_full", "wrong_winner"} {
		t.Run(state, func(t *testing.T) {
			f := newContractConsumerFixture(t)
			f.publish(t, nil, "", "/served")
			proof := capturedConsumerProof(t, f, 1)
			moveConsumerInput(t, f, "pending")
			fake := &contractRuntimeFake{t: t}
			fake.schedule = func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
				req.Source.Release()
				require.NotNil(t, req.Inputs)
				if state == "queue_full" {
					return indexer.ContractScheduleResult{}, &indexer.ContractQueueFullError{RetryAfter: 3 * time.Second}
				}
				return indexer.ContractScheduleResult{Result: indexer.ContractScheduled, Actor: req.Actor, ArrivalSeq: 2}, nil
			}
			acquired, released := 0, 0
			fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
				acquired++
				if state == "absent" {
					return nil, nil, graph.ErrContractProjectionIncomplete
				}
				if state == "wrong_winner" {
					copy := *proof
					copy.Targets = append([]indexer.ContractPublishedTarget(nil), proof.Targets...)
					copy.Targets[1].PayloadGeneration++
					return &copy, func() { released++ }, nil
				}
				return proof, func() { released++ }, nil
			}
			runtime := fake.runtime()
			runtime.WaitCapacity = func(context.Context, uint64) error { t.Fatal("optional contract read waited for capacity"); return nil }
			runtime.WaitInputs = func(context.Context, uint64) error { t.Fatal("optional contract read waited for inputs"); return nil }
			f.srv.SetContractAnalysisRuntime(runtime)
			ctx := prepareOptionalForTest(t, t.Context(), f, false)
			binding := contractAnalysisFromContext(ctx)
			reg := f.srv.optionalContractRegistryForContext(ctx)
			status := contractConsumerStatusFromContext(ctx)
			if state == "absent" || state == "wrong_winner" {
				require.Nil(t, reg)
				require.Equal(t, graphview.StateIncomplete, status.state)
				require.Equal(t, "no_attachment", status.reason)
			} else {
				require.NotNil(t, reg)
				require.NotEmpty(t, reg.ByID("http::GET::/served"))
				require.Equal(t, graphview.StateBuilding, status.state)
				for _, target := range proof.Targets {
					require.Equal(t, target.PayloadGeneration, binding.views[target.Key.RepoPrefix].Attachment.PayloadGeneration)
				}
				reader, err := f.srv.contractReaderForContext(ctx)
				require.NoError(t, err)
				require.NotNil(t, reader.GetNode("http::GET::/served"))
				require.NoError(t, binding.validate(ctx))
			}
			_ = f.srv.optionalContractRegistryForContext(ctx)
			require.False(t, status.fresh)
			require.Equal(t, 1, acquired)
			require.Equal(t, int64(1), fake.schedules.Load())
			require.Zero(t, fake.waits.Load())
			if state == "queue_full" {
				require.Equal(t, 3*time.Second, status.retryAfter)
			}
			binding.close()
			binding.close()
			if state != "absent" {
				require.Equal(t, 1, released)
			}
		})
	}
}

func TestOptionalExactActualContractReadRefusesStaleAndAbsent(t *testing.T) {
	for _, predecessor := range []bool{false, true} {
		f := newContractConsumerFixture(t)
		if predecessor {
			f.publish(t, nil, "", "/old")
			moveConsumerInput(t, f, "pending")
		}
		fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2)}
		fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
			t.Fatal("exact optional request borrowed predecessor")
			return nil, nil, errors.New("forbidden")
		}
		f.srv.SetContractAnalysisRuntime(fake.runtime())
		ctx := prepareOptionalForTest(t, t.Context(), f, true)
		binding := contractAnalysisFromContext(ctx)
		defer binding.close()
		require.Nil(t, f.srv.optionalContractRegistryForContext(ctx))
		require.ErrorContains(t, binding.lazyErr, "required_capability_incomplete")
		require.ErrorContains(t, binding.validate(ctx), "retry after")
		require.Equal(t, int64(1), fake.schedules.Load())
		require.Zero(t, fake.waits.Load())
	}
}

func TestOptionalContractParentCancellationHasNoCaptureFallbackOrSchedule(t *testing.T) {
	f := newContractConsumerFixture(t)
	fake := installForbiddenContractRuntime(t, f.srv)
	ctx, cancel := context.WithCancel(t.Context())
	prepared := prepareOptionalForTest(t, ctx, f, false)
	cancel()
	binding := contractAnalysisFromContext(prepared)
	defer binding.close()
	require.Nil(t, f.srv.optionalContractRegistryForContext(prepared))
	require.ErrorIs(t, binding.lazyErr, context.Canceled)
	require.Zero(t, fake.captures.Load())
	require.Zero(t, fake.schedules.Load())
}

func TestOptionalReviewGatesPreserveRealBlockAndWarnOnMissingAuthority(t *testing.T) {
	for _, state := range []graphview.CapabilityState{graphview.StateComplete, graphview.StateBuilding, graphview.StateIncomplete} {
		status := &contractConsumerStatus{mode: contractConsumerOptional, consulted: true, state: state, fresh: state == graphview.StateComplete}
		for _, severity := range []string{"pass", "warn", "block"} {
			impact := &contractImpact{}
			expected := prReviewPass
			if severity == "warn" {
				impact.Warning = 1
				expected = prReviewWarn
			}
			if severity == "block" {
				impact.Breaking = 1
				expected = prReviewBlock
			}
			if severity == "pass" && state != graphview.StateComplete {
				expected = prReviewWarn
			}
			gate := optionalContractReviewGate(status, impact)
			require.Equal(t, expected, gate.Status)
		}
		for _, original := range []review.Verdict{review.VerdictApprove, review.VerdictReview, review.VerdictBlock} {
			expected := original
			if original == review.VerdictApprove && state != graphview.StateComplete {
				expected = review.VerdictReview
			}
			require.Equal(t, expected, optionalContractReviewVerdict(original, status))
		}
	}
}

func TestOptionalConsumersUseServedShapeAndApiDrift(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/served")
	proof := capturedConsumerProof(t, f, 1)
	moveConsumerInput(t, f, "pending")
	// Current core type has different fields; contract shapes must remain from
	// the immutable served attachment, not borrowed from newer core rows.
	f.store.AddNode(&graph.Node{ID: "repo/types.go::Response", Name: "Response", Kind: graph.KindType, FilePath: "repo/types.go", RepoPrefix: "repo", Meta: map[string]any{"shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "wrong_current", Type: "int"}}}}})
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2)}
	fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
		return proof, func() {}, nil
	}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	ctx := prepareOptionalForTest(t, t.Context(), f, false)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	impact := f.srv.computeContractImpactContext(ctx, []string{"repo/types.go::Response"})
	require.NotNil(t, impact)
	shape := binding.shapeLookup(ctx)("repo/types.go::Response")
	require.NotNil(t, shape)
	require.Equal(t, "fresh", shape.Fields[0].Name)
	// API lens reads attachment-owned provides/consumes edges and marks only
	// contract-derived drift stale; the ordinary current caller graph stays core.
	node := f.store.GetNode("repo/handler.go::Handle")
	require.NotNil(t, node)
	reasons, _ := f.srv.apiDriftReasons(ctx, &prediction{nodes: []*graph.Node{node}})
	found := false
	for _, reason := range reasons {
		if reason.Family == "contract_drift" {
			found = true
			require.True(t, reason.Stale)
		}
	}
	require.True(t, found)
	reg := f.srv.optionalContractRegistryForContext(ctx) // wiki's shared source
	require.NotEmpty(t, reg.ByID("http::GET::/served"))
	require.Equal(t, int64(1), fake.schedules.Load())
}

func TestOptionalContractMiddlewareExactActualReadAndCoreOnlyTwins(t *testing.T) {
	for _, consulted := range []bool{false, true} {
		t.Run(map[bool]string{false: "core_only", true: "actual_contract_read"}[consulted], func(t *testing.T) {
			f := newContractConsumerFixture(t)
			fake := &contractRuntimeFake{t: t, schedule: scheduledAt(1)}
			f.srv.SetContractAnalysisRuntime(fake.runtime())
			reached := false
			result, err := f.request(t.Context(), "get_architecture", map[string]any{"require_exact": true, "format": "json"}, func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				reached = true
				if consulted {
					require.Nil(t, f.srv.optionalContractRegistryForContext(ctx))
				}
				return mcplib.NewToolResultText(`{"core":"still_available"}`), nil
			})
			require.NoError(t, err)
			require.True(t, reached)
			require.NotNil(t, result)
			if consulted {
				require.True(t, result.IsError)
				require.Contains(t, viewResultText(t, result), "required_capability_incomplete")
				require.Contains(t, viewResultText(t, result), "retry after")
				require.Equal(t, int64(1), fake.schedules.Load())
			} else {
				require.False(t, result.IsError)
				require.NotContains(t, viewResultText(t, result), "contract_analysis")
				require.Zero(t, fake.captures.Load())
				require.Zero(t, fake.schedules.Load())
			}
			require.Zero(t, fake.waits.Load())
		})
	}
}

func TestOptionalContractMiddlewareDoesNotServeClosedPayload(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/current")
	installForbiddenContractRuntime(t, f.srv)
	result, err := f.request(t.Context(), "get_architecture", map[string]any{"format": "json"}, func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		require.NotNil(t, f.srv.optionalContractRegistryForContext(ctx))
		binding := contractAnalysisFromContext(ctx)
		binding.views["repo"].Close()
		return mcplib.NewToolResultText(`{"contracts":"must_be_refused"}`), nil
	})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.NotContains(t, viewResultText(t, result), "must_be_refused")
}

func TestOptionalImpactWithNoChangedSymbolsNeverConsultsContracts(t *testing.T) {
	f := newContractConsumerFixture(t)
	fake := installForbiddenContractRuntime(t, f.srv)
	ctx := prepareOptionalForTest(t, t.Context(), f, false)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	require.Nil(t, f.srv.computeContractImpactContext(ctx, nil))
	require.False(t, contractConsumerStatusFromContext(ctx).consulted)
	require.Zero(t, fake.captures.Load())
	require.Zero(t, fake.schedules.Load())
}

func TestOptionalContractMiddlewareRefusesCheckedRegistryFailureWithValidCustody(t *testing.T) {
	for _, policy := range []string{"default_current", "exact_current", "default_served"} {
		for _, failure := range []error{graph.ErrContractProjectionIncomplete, graph.ErrContractProjectionLimit} {
			t.Run(policy+"/"+failure.Error(), func(t *testing.T) {
				f := newContractConsumerFixture(t)
				f.publish(t, nil, "", "/attachment")
				proof := capturedConsumerProof(t, f, 1)
				if policy == "default_served" {
					moveConsumerInput(t, f, "pending")
				}
				fake := &contractRuntimeFake{t: t, schedule: scheduledAt(2)}
				acquired, released := 0, 0
				fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
					require.Equal(t, "default_served", policy, "a current binding never borrows predecessor")
					acquired++
					return proof, func() { released++ }, nil
				}
				f.srv.SetContractAnalysisRuntime(fake.runtime())
				args := map[string]any{"format": "json"}
				if policy == "exact_current" {
					args["require_exact"] = true
				}
				reached := false
				result, err := f.request(t.Context(), "get_architecture", args, func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					reached = true
					binding := contractAnalysisFromContext(ctx)
					require.NoError(t, binding.ensure(ctx))
					require.NotEmpty(t, binding.views)
					require.NoError(t, binding.validate(ctx), "custody, revision and current input stamps are valid before the checked read")
					selected := binding.views["repo"]
					selected.RegistryReader = failedContractAnalysisRegistry{Reader: selected.RegistryReader, failure: failure}
					require.Nil(t, f.srv.optionalContractRegistryForContext(ctx), "the optional helper omits a failed registry")
					require.ErrorIs(t, binding.readError(), failure, "the omitted checked result must remain sticky")
					// Physical leases and stamps still pass. Only the checked projection
					// error should cause the middleware refusal, not a closed-payload proxy.
					if policy == "exact_current" {
						require.NoError(t, selected.Validate(ctx))
					} else {
						require.NoError(t, selected.ValidateServed(ctx))
					}
					require.ErrorIs(t, binding.validate(ctx), failure)
					return mcplib.NewToolResultText(`{"core":"must_not_escape_checked_registry_failure"}`), nil
				})
				require.NoError(t, err)
				require.True(t, reached)
				require.NotNil(t, result)
				require.True(t, result.IsError)
				require.Contains(t, viewResultText(t, result), failure.Error())
				require.NotContains(t, viewResultText(t, result), "must_not_escape_checked_registry_failure")
				require.Zero(t, fake.waits.Load())
				if policy == "default_served" {
					require.Equal(t, int64(1), fake.schedules.Load())
					require.Equal(t, 1, acquired)
					require.Equal(t, 1, released)
				} else {
					require.Zero(t, fake.schedules.Load())
					require.Zero(t, acquired)
				}
			})
		}
	}
}

func TestOptionalContractMiddlewareAbsentAttachmentStillAllowsDefaultCoreAnswer(t *testing.T) {
	f := newContractConsumerFixture(t)
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(1)}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	result, err := f.request(t.Context(), "get_architecture", map[string]any{"format": "json"}, func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		binding := contractAnalysisFromContext(ctx)
		require.Nil(t, f.srv.optionalContractRegistryForContext(ctx))
		require.Empty(t, binding.views)
		require.Nil(t, binding.readError(), "no opened payload means absence, not a checked projection failure")
		return mcplib.NewToolResultText(`{"core":"still_available_without_attachment"}`), nil
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.Contains(t, viewResultText(t, result), "still_available_without_attachment")
	require.Contains(t, viewResultText(t, result), "no_attachment")
	require.Equal(t, int64(1), fake.schedules.Load())
	require.Zero(t, fake.waits.Load())
}
