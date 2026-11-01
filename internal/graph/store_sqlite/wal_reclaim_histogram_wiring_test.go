package store_sqlite

import (
	"encoding/json"
	"testing"
	"time"

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

func TestWALReclaimUrgentAttemptCountsOnlyAdmittedAttempts(t *testing.T) {
	for _, cell := range []struct {
		name      string
		threshold int64
		leased    bool
		want      int64
	}{
		{"ordinary", 1 << 30, false, 0},
		{"urgent", 1, false, 1},
		{"bulk_lease_refused", 1, true, 0},
	} {
		t.Run(cell.name, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			if cell.leased {
				lease, err := s.acquireGenerationBulkCheckpointLease()
				require.NoError(t, err)
				defer s.releaseGenerationBulkCheckpointLease(lease)
			}
			res := s.reclaimWALOnce(walReclaimConfig{thresholdBytes: cell.threshold, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}, db, s.dbPath+"-wal")
			if cell.leased {
				require.Equal(t, walReclaimSkipped, res.outcome)
				require.Zero(t, s.WALReclaimStats().Attempts)
			} else {
				require.Equal(t, walReclaimReset, res.outcome, "real owned checkpoint must complete")
				require.Equal(t, cell.want != 0, res.urgent, "the fixture must exercise the requested urgency")
				require.EqualValues(t, 1, s.WALReclaimStats().Attempts)
			}
			// Observe the public snapshot without requiring a not-yet-added Go
			// field: the pre-fix code must execute, not just fail to compile.
			data, err := json.Marshal(s.WALReclaimStats())
			require.NoError(t, err)
			var wire struct {
				Urgent *int64 `json:"UrgentAttempts"`
			}
			require.NoError(t, json.Unmarshal(data, &wire))
			require.NotNil(t, wire.Urgent, "urgent attempts are absent from the actual reclaim snapshot")
			require.Equal(t, cell.want, *wire.Urgent)
		})
	}
}
