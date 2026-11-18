package indexer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
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

func TestContractPolicyWorkerCountPreservesAcceptedRows(t *testing.T) {
	settings := config.Default().Index
	settings.Workers = 1
	rows, receipt, policy := contractPolicyAcceptedRows(t, settings)
	settings.Workers = 4
	otherRows, otherReceipt, otherPolicy := contractPolicyAcceptedRows(t, settings)
	require.Equal(t, rows, otherRows)
	require.Equal(t, receipt, otherReceipt, "worker count changes scheduling, not accepted contract facts")
	require.Equal(t, policy, otherPolicy)
}

func TestExecutionSettingsPreserveBothIdentities(t *testing.T) {
	settings := config.Default().Index
	_, receipt, policy := contractPolicyAcceptedRows(t, settings)
	_, generation, err := snapshotDedicatedBaseConfig(settings, "fixture", "", "")
	require.NoError(t, err)
	for _, workers := range []int{1, settings.Workers + 1} {
		t.Setenv("GORTEX_INDEX_WORKERS", strconv.Itoa(workers))
		manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
		require.NoError(t, err)
		configured := settings
		configured.Workers = manager.GetRepoConfig("fixture").Index.Workers
		require.Equal(t, workers, configured.Workers)
		_, otherReceipt, otherPolicy := contractPolicyAcceptedRows(t, configured)
		owned, otherGeneration, err := snapshotDedicatedBaseConfig(configured, "fixture", "", "")
		require.NoError(t, err)
		require.Equal(t, workers, owned.Workers, "snapshots must retain scheduling configuration")
		require.Equal(t, receipt, otherReceipt)
		require.Equal(t, policy, otherPolicy)
		require.Equal(t, generation, otherGeneration, "CPU defaults and environment overrides must not rekey generations")
	}
}
