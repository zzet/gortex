//go:build !gortex_edit_gate_seed

package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEditGateProductionBuildCannotEnableClosedSeed(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "closed-read-gate")
	s, err := openPristine(t, filepath.Join(t.TempDir(), "production.sqlite"))
	require.NoError(t, err)
	defer s.Close()
	before := s.WALReclaimStats()
	release := s.AnnounceWrite()
	defer release()
	require.Equal(t, before.PauseCount, s.WALReclaimStats().PauseCount, "runtime environment cannot activate the test-only closed-gate policy")
}
