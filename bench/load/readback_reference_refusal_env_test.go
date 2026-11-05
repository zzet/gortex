//go:build acceptance

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

func TestReadbackProtocolRefusalEnvReachesOnlyDiagnosticDaemon(t *testing.T) {
	for _, tc := range []struct{ role, flag, want string }{
		{role: "protocol_preflight", flag: "1", want: "1"},
		{role: "protocol_preflight"},
		{role: "protocol_preflight", flag: "true"},
		{role: "under_edit", flag: "1"},
		{role: "no_edit", flag: "1"},
	} {
		t.Run(tc.role+"/"+tc.flag, func(t *testing.T) {
			t.Setenv("LOAD_DAEMON_FIXTURE", "")
			t.Setenv("LOAD_SEARCH_REFUSAL_ENV_PROBE", "selected-env-observation")
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", tc.flag)
			t.Setenv("GORTEX_REFUSAL_TEST_LEAK", "must-not-reach-daemon")
			root := t.TempDir()
			binary, err := os.Executable()
			require.NoError(t, err)
			p := &privateRun{Root: root, Binary: binary, Config: filepath.Join(root, "config.yaml"), Store: filepath.Join(root, "store.db")}
			p.Env = privateEnv(root, filepath.Join(root, "d.sock"), filepath.Join(root, "gitconfig"))
			originalEnv := append([]string(nil), p.Env...)
			p.daemonEnv = append(append([]string(nil), p.Env...), "GORTEX_REFUSAL_TEST_OWNED=preserved")
			originalOverride := append([]string(nil), p.daemonEnv...)
			require.NoError(t, p.startReadbackReferenceDaemon(tc.role))
			wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			select {
			case <-p.done:
			case <-wait.Done():
				_, _, _ = p.stop(wait)
				t.Fatal("owned diagnostic environment child did not finish")
			}
			exitErr := p.exitErr
			_, forced, stopErr := p.stop(wait)
			require.NoError(t, exitErr)
			require.False(t, forced)
			require.ErrorContains(t, stopErr, "private daemon exited before requested shutdown")
			require.Nil(t, p.process)
			require.Equal(t, originalEnv, p.Env)
			require.Equal(t, originalOverride, p.daemonEnv)
			data, err := os.ReadFile(filepath.Join(root, "daemon-1.stdout.log"))
			require.NoError(t, err)
			var observed map[string]string
			require.NoError(t, json.Unmarshal(data, &observed))
			t.Logf("role=%s offered_flag=%q actual_child_flag=%q reaped=true forced=false", tc.role, tc.flag, observed["evidence"])
			require.Equal(t, tc.want, observed["evidence"])
			require.Empty(t, observed["foreign"])
			require.Equal(t, "preserved", observed["owned"])
			require.Equal(t, "facade-v1", observed["tools"])
		})
	}
}
