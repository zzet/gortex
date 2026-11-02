//go:build gortex_edit_gate_seed

package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEditGateClosedSeedUsesRealAdmissionAndPauseCounter(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "closed-read-gate")
	s, err := openPristine(t, filepath.Join(t.TempDir(), "seed.sqlite"))
	require.NoError(t, err)
	defer s.Close()
	before := s.WALReclaimStats()
	release := s.AnnounceWrite()
	defer release()
	require.True(t, s.editIntentActive(), "the forced reclaim belongs to an actual admitted write announcement")
	after := s.WALReclaimStats()
	require.Greater(t, after.PauseCount, before.PauseCount, "test-only policy must actually close read admission, not synthesize a report value")
}
