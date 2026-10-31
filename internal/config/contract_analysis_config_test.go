package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContractAnalysisBlockLoadsFromWorkspaceConfig: the contract_analysis
// block is a known top-level key that reaches the repository config, and it
// stays out of IndexConfig, which is part of a contract cohort's identity.
func TestContractAnalysisBlockLoadsFromWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, ".gortex.yaml")
	content := `
contract_analysis:
  linked_policy: throttled
  linked_idle_window: 8s
  linked_throttle_interval: 45s
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	assert.Empty(t, UnknownWorkspaceKeys(path))

	cm.LoadWorkspaceConfig("my-repo", repoDir)
	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)
	assert.Equal(t, ContractAnalysisConfig{LinkedPolicy: "throttled", LinkedIdleWindow: "8s", LinkedThrottleInterval: "45s"}, cfg.ContractAnalysis)

	unset := cm.GetRepoConfig("other-repo")
	require.NotNil(t, unset)
	assert.Equal(t, ContractAnalysisConfig{}, unset.ContractAnalysis, "an absent block leaves the shipped policy to its consumer")
	assert.Equal(t, ContractAnalysisConfig{}, Default().ContractAnalysis)
}

// TestContractAnalysisBlockLoadsFromDaemonConfig: the daemon reads the block
// through Load, from the same file as its views block.
func TestContractAnalysisBlockLoadsFromDaemonConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("views:\n  retain_inactive: 24h\ncontract_analysis:\n  linked_policy: eager\n  linked_idle_window: 2s\n"), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "24h", cfg.Views.RetainInactive)
	assert.Equal(t, ContractAnalysisConfig{LinkedPolicy: "eager", LinkedIdleWindow: "2s"}, cfg.ContractAnalysis)
}
