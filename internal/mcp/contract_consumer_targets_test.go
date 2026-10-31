package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

func scopedContractList(repo string) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "contracts"
	req.Params.Arguments = map[string]any{"action": "list", "repo": repo, "format": "json", "require_exact": true}
	return req
}

func TestContractListTargetSelectionPreservesWholeCohortConsumers(t *testing.T) {
	for _, test := range []struct {
		name string
		req  mcplib.CallToolRequest
		want []string
	}{
		{"explicit", scopedContractList("repo"), []string{"repo"}},
		{"wildcard", scopedContractList("*"), nil},
		{"unadmitted", scopedContractList("other"), nil},
		{"default", scopedContractList(""), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, contractListTargetRepos(test.req, []string{"b", "repo"}))
		})
	}
	for _, action := range []string{"check", "validate", "bridge"} {
		req := scopedContractList("repo")
		req.Params.Arguments.(map[string]any)["action"] = action
		require.Nil(t, contractListTargetRepos(req, []string{"b", "repo"}))
	}
	req := scopedContractList("repo")
	req.Params.Arguments.(map[string]any)["all_repos"] = true
	require.Nil(t, contractListTargetRepos(req, []string{"b", "repo"}))
}

func TestContractScopedListOpensCurrentTargetBeforeCompanionPayload(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publishRowsForTargets(t, nil, "", "/priority", nil, nil, []string{"repo"})
	installForbiddenContractRuntime(t, f.srv)
	whole, pending, err := f.srv.openContractConsumerBinding(t.Context(), nil, "", []string{"b", "repo"})
	require.Error(t, err)
	require.Nil(t, whole)
	require.Equal(t, "b", pending.key.RepoPrefix)
	req := scopedContractList("repo")
	result, err := f.request(t.Context(), req.Params.Name, req.GetArguments(), f.srv.handleContracts)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "/priority")
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &object))
	status := object["contract_analysis"].(map[string]any)
	require.Equal(t, "complete", status["state"])
	require.Equal(t, true, status["fresh"])
	require.Equal(t, "repository", status["scope"])
	require.Equal(t, []any{"repo"}, status["repositories"])
}

func TestContractScopedListWaitsForExactTargetWithFullDependencyVector(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publishRowsForTargets(t, nil, "", "/before-companion-change", nil, nil, []string{"repo"})
	old := f.states["b"]
	next := old
	next.InputFingerprint = "b-moved"
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, next, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), next))
	f.states["b"] = next
	fake := &contractRuntimeFake{t: t}
	fake.schedule = func(ctx context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
		require.ElementsMatch(t, []string{"b", "repo"}, req.Actor.Repos())
		require.Equal(t, "repo", req.Key.RepoPrefix)
		require.True(t, req.PriorityTarget, "scoped lists request explicit target priority")
		require.True(t, req.Strict, "a waiting caller marks its request strict")
		require.Len(t, req.Inputs.Witnesses, 2, "target freshness retains the full dependency authority")
		return scheduledAt(7)(ctx, req)
	}
	runtime := fake.runtime()
	waits := 0
	runtime.WaitTarget = func(_ context.Context, actor indexer.ContractActor, seq uint64, key graph.ContractAttachmentKey) (indexer.ContractActorStatus, error) {
		waits++
		require.ElementsMatch(t, []string{"b", "repo"}, actor.Repos())
		require.Equal(t, uint64(7), seq)
		require.Equal(t, "repo", key.RepoPrefix)
		require.NotEmpty(t, key.InputFingerprint)
		f.publishRowsForTargets(t, nil, "", "/after-companion-change", nil, nil, []string{"repo"})
		return indexer.ContractActorStatus{Result: indexer.ContractActorPublished, Seq: seq}, nil
	}
	f.srv.SetContractAnalysisRuntime(runtime)
	req := scopedContractList("repo")
	result, err := f.request(t.Context(), req.Params.Name, req.GetArguments(), f.srv.handleContracts)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "/after-companion-change")
	require.NotContains(t, body, "/before-companion-change")
	require.Equal(t, 1, waits)
	require.Zero(t, fake.waits.Load(), "scoped target wait does not wait for actor completion")
	whole, pending, err := f.srv.openContractConsumerBinding(t.Context(), nil, "", []string{"b", "repo"})
	require.Error(t, err)
	require.Nil(t, whole)
	require.Equal(t, "b", pending.key.RepoPrefix, "the companion remains honestly incomplete")
}

func TestContractScopedListTargetHintCannotCertifyMissingPayload(t *testing.T) {
	f := newContractConsumerFixture(t)
	fake := &contractRuntimeFake{t: t, schedule: scheduledAt(1)}
	runtime := fake.runtime()
	waits := 0
	runtime.WaitTarget = func(_ context.Context, _ indexer.ContractActor, seq uint64, _ graph.ContractAttachmentKey) (indexer.ContractActorStatus, error) {
		waits++
		return indexer.ContractActorStatus{Result: indexer.ContractActorPublished, Seq: seq}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtime.WaitInputs = func(context.Context, uint64) error {
		cancel()
		return context.Canceled
	}
	f.srv.SetContractAnalysisRuntime(runtime)
	req := scopedContractList("repo")
	prepared, result := f.srv.prepareContractConsumer(ctx, req, requestFreshness{requireExact: true}, capabilityRequest{})
	require.Nil(t, contractAnalysisFromContext(prepared))
	require.NotNil(t, result)
	require.True(t, result.IsError, viewResultText(t, result))
	require.Equal(t, 1, waits)
	require.Zero(t, fake.waits.Load())
	require.EqualValues(t, 1, fake.schedules.Load())
}
