package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/platform"
)

func TestRuntimeStateStampsActualOwnerAndKeepsLegacyJSONReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	identity, err := platform.CaptureProcessIdentity(os.Getpid())
	require.NoError(t, err)
	token, err := identity.Token()
	require.NoError(t, err)
	require.NoError(t, WriteRuntimeState(RuntimeState{PID: 1, ProcessBirthToken: "caller-supplied", BackendPath: "owned.sqlite"}))
	state, ok := ReadRuntimeState()
	require.True(t, ok)
	require.Equal(t, os.Getpid(), state.PID)
	require.Equal(t, token, state.ProcessBirthToken)
	// An exact old-schema record has no stamp. It stays readable rather
	// than being silently promoted to positive native provenance.
	old, err := json.Marshal(map[string]any{"pid": os.Getpid(), "backend_path": "legacy.sqlite"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, old, 0o600))
	state, ok = ReadRuntimeState()
	require.True(t, ok)
	require.Empty(t, state.ProcessBirthToken)
}
