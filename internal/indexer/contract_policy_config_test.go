package indexer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func contractPolicyAcceptedRows(t *testing.T, settings config.IndexConfig) (string, contractBoundaryReceipt, string) {
	t.Helper()
	idx := newTestIndexer(graph.New())
	idx.config = settings
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	source := []byte("export async function Call() { await apiGet(\"/items\"); }\n")
	parsed, err := languages.NewTypeScriptExtractor().Extract("fixture/api.ts", source)
	require.NoError(t, err)
	if parsed.Tree != nil {
		defer parsed.Tree.Release()
	}
	stampExtractionGraphFingerprint(parsed)
	idx.stampContractDependencyInputs("fixture/api.ts", "typescript", source, parsed)
	rows, err := json.Marshal(parsed.Nodes)
	require.NoError(t, err)
	receipt, err := idx.collectContractBoundaryReceipt(context.Background(), "fixture/api.ts", "typescript", source, parsed)
	require.NoError(t, err)
	workerPolicy, err := contractFollowupPolicy(idx, "typescript")
	require.NoError(t, err)
	require.Equal(t, receipt.Policy, workerPolicy, "accepted collector and detached worker must use the same policy")
	return string(rows), receipt, workerPolicy
}

func TestContractPolicyConstructionSettingsPreserveAcceptedRows(t *testing.T) {
	baseline := config.Default().Index
	rows, receipt, policy := contractPolicyAcceptedRows(t, baseline)
	configured := baseline
	off := false
	configured.DirtyChain = &config.DirtyChainConfig{ResolverEvidenceScope: &off}
	otherRows, otherReceipt, otherPolicy := contractPolicyAcceptedRows(t, configured)
	require.Equal(t, rows, otherRows, "evidence-scoping strategy must not change durable file metadata")
	require.Equal(t, receipt, otherReceipt)
	require.Equal(t, policy, otherPolicy)

	// This is a semantic setting: recognize the real wrapped client call.
	configured.HTTPClientAliases = []string{"apiGet"}
	semanticRows, semanticReceipt, semanticPolicy := contractPolicyAcceptedRows(t, configured)
	require.NotEqual(t, rows, semanticRows)
	require.NotEqual(t, receipt.Policy, semanticReceipt.Policy)
	require.NotEqual(t, receipt.Records.Semantic, semanticReceipt.Records.Semantic)
	require.NotEqual(t, policy, semanticPolicy)
}
