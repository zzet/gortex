package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Keep the native scenario's eventual selector tied to the existing real
// migration matrix. Labels are roles; three roles may name fewer actual steps.
func TestUpgradePhaseSelectionUsesActualPendingSteps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored int
		steps  []int
	}{
		{"four_pending", 40, []int{41, 43, 44}},
		{"two_pending", 42, []int{43, 44}},
		{"one_pending", 43, []int{44}},
		{"current", currentSchemaVersion, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := upgradeKillPoints(tc.stored)
			var steps []int
			seen := make(map[upgradePoint]bool)
			for _, point := range points {
				if point.point == upgradeInStep {
					steps = append(steps, point.version)
					continue
				}
				require.False(t, seen[point.point], "non-step boundary duplicated")
				seen[point.point] = true
			}
			require.Equal(t, tc.steps, steps)
			if len(tc.steps) == 0 {
				require.Empty(t, points, "a current store cannot witness an upgrade boundary")
				return
			}
			require.Len(t, points, len(tc.steps)+3)
			require.True(t, seen[upgradeBackupCopied])
			require.True(t, seen[upgradeIndexRecreation])
			require.True(t, seen[upgradeBeforeStamp])
		})
	}
}
