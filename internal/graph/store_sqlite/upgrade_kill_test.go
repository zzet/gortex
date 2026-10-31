package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpgradeKillPointSeamsFireInOrder(t *testing.T) {
	path := seedOlderStore(t, 40, 50)
	var points []string
	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), withDiskAvail(fixedDiskAvail(1<<50)),
		withUpgradeHook(func(point upgradePoint, version int) {
			points = append(points, fmt.Sprintf("%s@%d", point, version))
		}))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	want := []string{fmt.Sprintf("%s@40", upgradeBackupCopied)}
	for v := 41; v <= currentSchemaVersion; v++ {
		want = append(want, fmt.Sprintf("%s@%d", upgradeInStep, v))
	}
	want = append(want,
		fmt.Sprintf("%s@%d", upgradeIndexRecreation, currentSchemaVersion),
		fmt.Sprintf("%s@%d", upgradeBeforeStamp, currentSchemaVersion))
	require.Equal(t, want, points)

	// A current store crosses no boundary at all.
	points = nil
	s, err = Open(path, withUpgradeHook(func(point upgradePoint, version int) {
		points = append(points, fmt.Sprintf("%s@%d", point, version))
	}))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Empty(t, points)
}
