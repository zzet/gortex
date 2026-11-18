package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestContractMutationReportsBuildingWithoutHidingUnaffectedRepo(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/healthy")
	proof := capturedConsumerProof(t, f, 1)
	old := f.states["repo"]
	pending := old
	pending.InputFingerprint, pending.Accepted = "synthetic-in-flight", false
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, pending, nil))
	fake := &contractRuntimeFake{t: t}
	fake.schedule = func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
		req.Source.Release()
		return indexer.ContractScheduleResult{}, &indexer.ContractQueueFullError{RetryAfter: time.Second}
	}
	fake.acquire = func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error) {
		return proof, func() {}, nil
	}
	f.srv.SetContractAnalysisRuntime(fake.runtime())
	result, err := f.request(t.Context(), "contracts", map[string]any{"action": "list", "all_repos": true}, f.srv.handleGetContracts)
	body := requireContractBody(t, result, err)
	var payload struct {
		ByRepo   map[string]json.RawMessage `json:"by_repo"`
		Analysis struct {
			State string `json:"state"`
			Fresh bool   `json:"fresh"`
		} `json:"contract_analysis"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Contains(t, string(payload.ByRepo["b"]), "/healthy")
	require.Equal(t, "building", payload.Analysis.State)
	require.False(t, payload.Analysis.Fresh)
}
