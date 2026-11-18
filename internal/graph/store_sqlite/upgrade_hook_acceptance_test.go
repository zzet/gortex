//go:build acceptance

package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptanceUpgradeHookPreservesAdmissionAndPerOpenIsolation(t *testing.T) {
	t.Run("refusal_before_hook", func(t *testing.T) {
		path := seedOlderStore(t, 40, 50)
		before := fileDigest(t, path)
		called := false
		store, err := Open(path, WithRebuild(), WithUpgradeBackup(), withDiskAvail(fixedDiskAvail(0)), WithAcceptanceUpgradeHook(func(string, int) { called = true }))
		require.Nil(t, store)
		require.ErrorIs(t, err, ErrInsufficientUpgradeSpace)
		require.False(t, called)
		require.Equal(t, before, fileDigest(t, path))
	})
	t.Run("only_selected_open", func(t *testing.T) {
		first, second := seedOlderStore(t, 40, 50), seedOlderStore(t, 40, 50)
		calls := 0
		store, err := Open(first, WithRebuild(), withDiskAvail(fixedDiskAvail(1<<50)), WithAcceptanceUpgradeHook(func(string, int) { calls++ }))
		require.NoError(t, err)
		require.NoError(t, store.Close())
		require.Positive(t, calls)
		before := calls
		store, err = Open(second, WithRebuild(), withDiskAvail(fixedDiskAvail(1<<50)))
		require.NoError(t, err)
		require.NoError(t, store.Close())
		require.Equal(t, before, calls, "a later default Open cannot inherit another invocation's callback")
	})
}
