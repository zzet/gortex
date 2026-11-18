package store_sqlite

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseCheckpointPolicyKeepsExplicitAndLegacyOptIns(t *testing.T) {
	for _, tc := range []struct {
		name, mode, legacy, want string
		max                      time.Duration
		warn                     bool
	}{
		{name: "default", want: "skip"},
		{name: "full", mode: "full", want: "full"},
		{name: "skip", mode: "skip", want: "skip"},
		{name: "legacy-cap", legacy: "30", want: "full", max: 30 * time.Second},
		{name: "legacy-zero-unlimited", legacy: "0", want: "full"},
		{name: "legacy-clamped", legacy: "100000", want: "full", max: 24 * time.Hour},
		{name: "skip-overrides-cap", mode: "skip", legacy: "5", want: "skip", max: 5 * time.Second},
		{name: "full-keeps-cap", mode: "full", legacy: "5", want: "full", max: 5 * time.Second},
		{name: "negative-not-opt-in", legacy: "-1", want: "skip", warn: true},
		{name: "invalid-not-opt-in", legacy: "bad", want: "skip", warn: true},
		{name: "invalid-mode", mode: "other", want: "skip", warn: true},
		{name: "invalid-mode-valid-legacy", mode: "other", legacy: "5", want: "full", max: 5 * time.Second, warn: true},
		{name: "full-invalid-cap", mode: "full", legacy: "bad", want: "full", warn: true},
		{name: "whitespace-case", mode: " FULL ", legacy: " 5 ", want: "full", max: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", tc.mode)
			t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", tc.legacy)
			policy := resolveCloseCheckpointPolicy()
			require.Equal(t, tc.want, policy.mode())
			require.Equal(t, tc.max, policy.max)
			require.Equal(t, tc.warn, policy.warning != "")
			if tc.want == "skip" || tc.max == 0 {
				require.Zero(t, policy.deadline(10))
			} else {
				require.Equal(t, min(tc.max, closeCheckpointEstimate(10)), policy.deadline(10))
			}
		})
	}
}

func TestCloseDefaultSkipsCheckpointAndRecoversCommittedWAL(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "active-coordinated-bulk"}[bulk], func(t *testing.T) {
			s, path := openTempStore(t)
			s.stopCheckpointLoop()
			s.stopMaintenanceLane()
			require.NoError(t, s.CheckpointWAL())
			var events []bulkFinalizeEvent
			s.bulkFinalizeObserver = func(e bulkFinalizeEvent) { events = append(events, e) }
			if bulk {
				require.True(t, s.BeginCoordinatedBulkLoad())
				require.True(t, s.bulkIndexesDeferred, "fixture did not require the mandatory seal")
			}
			nodes, edges := bulkFixture(1025, 3000)
			require.NoError(t, s.AddBatchChecked(nodes, edges))
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			wal, err := os.ReadFile(path + "-wal")
			require.NoError(t, err)
			require.Greater(t, len(wal), 32)
			estimate, pending := s.CloseCheckpointEstimate()
			require.Zero(t, estimate)
			require.GreaterOrEqual(t, pending, int64(0), "skip erased the real backlog telemetry")
			require.NoError(t, s.Close())
			require.Nil(t, s.bulkConn)
			require.False(t, s.bulkIndexesDeferred)
			for _, e := range events {
				require.False(t, e.Stage == "checkpoint" && e.Name == "wal_truncate", "default close scheduled the bulk final TRUNCATE")
			}
			_, progress := ReadCloseProgress(path)
			require.False(t, progress, "skip was mislabeled as a completed drain")
			_, rateErr := os.Stat(closeRatePath(path))
			require.True(t, os.IsNotExist(rateErr), "skip learned a synthetic drain rate")
			if !bulk {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, before, after, "ordinary default close copied WAL into main")
				after, err = os.ReadFile(path + "-wal")
				require.NoError(t, err)
				require.Equal(t, wal, after)
			}
			reopened, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			require.Equal(t, len(nodes), reopened.NodeCount())
			require.Equal(t, len(edges), reopened.EdgeCount())
			for _, node := range nodes {
				require.NotNil(t, reopened.GetNode(node.ID))
			}
			if bulk {
				indexes := indexNames(t, reopened.db)
				for _, idx := range bulkDroppableIndexes {
					require.True(t, indexes[idx.name], "close skipped mandatory index %s", idx.name)
				}
			}
		})
	}
}

func TestCloseCheckpointResolvesPolicyOnceBeforeBulkSeal(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "0")
	s, path := openTempStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	require.True(t, s.BeginCoordinatedBulkLoad())
	nodes, edges := bulkFixture(3, 2)
	require.NoError(t, s.AddBatchChecked(nodes, edges))
	changed, truncated := false, false
	s.bulkFinalizeObserver = func(e bulkFinalizeEvent) {
		if e.Stage == "index" && !changed {
			changed = true
			require.NoError(t, os.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "skip"))
		}
		if e.Stage == "checkpoint" && e.Name == "wal_truncate" {
			truncated = true
		}
	}
	require.NoError(t, s.Close())
	require.True(t, changed, "fixture did not cross the actual mandatory seal")
	require.True(t, truncated, "mid-close environment change replaced the captured full policy")
	require.Zero(t, walFileSize(path+"-wal"))
}
