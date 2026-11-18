package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContractAnalysisBlockLoadsFromDaemonConfig: the daemon reads the
// contract_analysis block through Load, from the same file as its views
// block. That file may be the .gortex.yaml of the directory the daemon
// starts in, so the key is not reported as unknown there. The block stays
// out of IndexConfig, which is part of a contract cohort's identity.
func TestContractAnalysisBlockLoadsFromDaemonConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("views:\n  retain_inactive: 24h\ncontract_analysis:\n  linked_policy: eager\n  linked_idle_window: 2s\n"), 0o644))
	assert.Empty(t, UnknownWorkspaceKeys(path))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "24h", cfg.Views.RetainInactive)
	assert.Equal(t, ContractAnalysisConfig{LinkedPolicy: "eager", LinkedIdleWindow: "2s"}, cfg.ContractAnalysis)
	assert.Equal(t, ContractAnalysisConfig{}, Default().ContractAnalysis, "an absent block leaves the shipped policy to its consumer")
}
