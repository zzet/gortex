package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetRepoConfig_IndexWorkersEnv: per-repository configs honour
// GORTEX_INDEX_WORKERS, and a repository's own index.workers wins over it.
func TestGetRepoConfig_IndexWorkersEnv(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		workspace string // "" means no .gortex.yaml
		want      int
	}{
		{name: "env without workspace file", env: "4", want: 4},
		{name: "env with partial workspace file", env: "4", workspace: "index:\n  exclude:\n    - \"dist/**\"\n", want: 4},
		{name: "workspace file wins over env", env: "4", workspace: "index:\n  workers: 3\n", want: 3},
		{name: "unset env keeps NumCPU", env: "", workspace: "index:\n  exclude:\n    - \"dist/**\"\n", want: runtime.NumCPU()},
		{name: "non-numeric env ignored", env: "four", want: runtime.NumCPU()},
		{name: "zero env ignored", env: "0", want: runtime.NumCPU()},
		{name: "negative env ignored", env: "-2", want: runtime.NumCPU()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(indexWorkersEnv, tc.env)
			cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
			require.NoError(t, err)

			repoDir := t.TempDir()
			if tc.workspace != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(tc.workspace), 0o644))
			}
			cm.LoadWorkspaceConfig("my-repo", repoDir)

			cfg := cm.GetRepoConfig("my-repo")
			require.NotNil(t, cfg)
			assert.Equal(t, tc.want, cfg.Index.Workers)
		})
	}
}
