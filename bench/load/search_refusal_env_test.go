package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMain dispatches this existing-owned-child probe before CLI flag parsing.
// It reports only these controlled scalars, never the process environment.
func runSearchRefusalEnvProbe() int {
	err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"evidence": os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE"),
		"foreign":  os.Getenv("GORTEX_REFUSAL_TEST_LEAK"),
		"owned":    os.Getenv("GORTEX_REFUSAL_TEST_OWNED"),
		"tools":    os.Getenv("GORTEX_TOOLS"),
	})
	if err != nil {
		return 1
	}
	return 0
}

func TestSearchRefusalEvidenceEnvBridgeReachesOnlySelectedDaemon(t *testing.T) {
	for _, tc := range []struct {
		name          string
		flag          string
		search        bool
		editGateModes bool
		scenario      string
		ownedOverride bool
		want          string
	}{
		{name: "default_off", search: true},
		{name: "explicit_search", flag: "1", search: true, want: "1"},
		{name: "invalid_flag", flag: "true", search: true},
		{name: "generic_profile", flag: "1"},
		{name: "preserved_owned_override", flag: "1", search: true, ownedOverride: true, want: "1"},
		{name: "explicit_edit_gate", flag: "1", editGateModes: true, want: "1"},
		{name: "edit_gate_off", editGateModes: true},
		{name: "edit_gate_invalid_flag", flag: "true", editGateModes: true},
		{name: "other_scenario", flag: "1", search: true, editGateModes: true, scenario: "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOAD_DAEMON_FIXTURE", "")
			t.Setenv("LOAD_SEARCH_REFUSAL_ENV_PROBE", "selected-env-observation")
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", tc.flag)
			t.Setenv("GORTEX_REFUSAL_TEST_LEAK", "must-not-reach-daemon")
			root := t.TempDir()
			binary, err := os.Executable()
			require.NoError(t, err)
			p := &privateRun{Root: root, Binary: binary, Config: filepath.Join(root, "config.yaml"), Store: filepath.Join(root, "store.db")}
			p.Env = privateEnv(root, filepath.Join(root, "d.sock"), filepath.Join(root, "gitconfig"))
			baseEnv := append([]string(nil), p.Env...)
			if tc.ownedOverride {
				p.daemonEnv = append(append([]string(nil), p.Env...), "GORTEX_REFUSAL_TEST_OWNED=preserved")
			}
			priorOverride := p.daemonEnv
			scenario := tc.scenario
			if scenario == "" {
				scenario = editProbeScenario
			}
			require.NoError(t, p.startWithScenarioRefusalEvidence(scenario, runOptions{SearchReadbackAcceptance: tc.search, EditGateModes: tc.editGateModes}))
			wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			select {
			case <-p.done:
			case <-wait.Done():
				_, _, _ = p.stop(wait)
				t.Fatal("owned environment probe did not finish")
			}
			exitErr := p.exitErr
			_, forced, stopErr := p.stop(wait)
			require.NoError(t, exitErr)
			require.False(t, forced)
			require.ErrorContains(t, stopErr, "private daemon exited before requested shutdown")
			require.Nil(t, p.process, "existing lifecycle reaped the owned child and closed its logs")
			require.Equal(t, baseEnv, p.Env, "Git/private environment must remain unchanged")
			require.Equal(t, priorOverride, p.daemonEnv, "one selected start cannot contaminate a later generic start")
			data, err := os.ReadFile(filepath.Join(root, "daemon-1.stdout.log"))
			require.NoError(t, err)
			var observed map[string]string
			require.NoError(t, json.Unmarshal(data, &observed))
			require.Equal(t, tc.want, observed["evidence"])
			require.Empty(t, observed["foreign"], "generic GORTEX isolation must not be weakened")
			require.Equal(t, "facade-v1", observed["tools"])
			if tc.ownedOverride {
				require.Equal(t, "preserved", observed["owned"])
			} else {
				require.Empty(t, observed["owned"])
			}
			t.Logf("scenario=%s selected_search=%v edit_gate=%v explicit_flag=%q real_child_observed=%v reaped=true forced=false", scenario, tc.search, tc.editGateModes, tc.flag, observed)
		})
	}
}
