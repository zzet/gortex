package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Gate-only tests cannot prove the real maintenance producers use the timed
// admission. These owned completion controls perform actual final checkpoints;
// their existing completion allowance is not a production latency claim.
func TestWALReclaimRealFinalWritersRecordHistograms(t *testing.T) {
	for _, kind := range []string{"pressure", "short", "last_resort"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			before := s.writeMu.reclaimHold.snapshot()
			s.writeMu.Lock()
			s.writeMu.Unlock()
			require.Equal(t, before.Count(), s.writeMu.reclaimHold.snapshot().Count(), "an ordinary writer must not be attributed to reclaim")

			res := &walReclaimResult{}
			require.NoError(t, runFinalReclaimCompletionControl(t.Context(), kind, s, db, res))
			after := s.writeMu.reclaimHold.snapshot()
			require.Greater(t, after.Count(), before.Count(), "a real reclaim writer hold was missing from the distribution")
			require.GreaterOrEqual(t, after.Total, before.Total)
			require.Equal(t, after, s.WALReclaimStats().WriterHold, "the public counter snapshot must expose actual producer holds")
			require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"), "completion control must really reset its private WAL")
		})
	}
}
