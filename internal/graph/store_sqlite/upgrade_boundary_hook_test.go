package store_sqlite

import (
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

// A checkpoint is a synchronous callback in the migration stack, not a
// best-effort observation of a RuntimeState value that may already be stale.
func TestUpgradeHookHoldsActualMigrationStack(t *testing.T) {
	path := seedOlderStore(t, 40, 50)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	type outcome struct {
		store *Store
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		store, err := Open(path, WithRebuild(), withDiskAvail(fixedDiskAvail(1<<50)),
			withUpgradeHook(func(point upgradePoint, version int) {
				if point == upgradeInStep && version == 41 {
					close(entered)
					<-release
				}
			}))
		done <- outcome{store, err}
	}()
	select {
	case <-entered:
	case result := <-done:
		if result.store != nil {
			require.NoError(t, result.store.Close())
		}
		t.Fatalf("migration advanced without the actual checkpoint: %v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("no actual migration checkpoint entered")
	}
	select {
	case result := <-done:
		if result.store != nil {
			require.NoError(t, result.store.Close())
		}
		t.Fatal("Open advanced while its checkpoint callback was held")
	default:
	}
	unblock()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.NoError(t, result.store.Close())
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not resume after checkpoint release")
	}
}
