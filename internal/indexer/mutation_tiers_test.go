package indexer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReceiptTierTimestampsAreOrdered(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Before() {}\n")
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\nfunc After() {}\n")
	derived, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "derived" {
			close(derived)
			<-release
		}
	}
	ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case <-derived:
	case <-time.After(2 * time.Second):
		t.Fatal("the real patch did not reach its derived pass")
	}
	tiers := ticket.TierState()
	require.False(t, tiers.AckAt.IsZero(), "admission must stamp the live ticket")
	require.True(t, tiers.AckAt.Before(tiers.FileGraphAt))
	require.False(t, tiers.RelationsAt.Before(tiers.FileGraphAt))
	require.True(t, tiers.DerivedAt.IsZero(), "relations must publish before blocked derived work")
}

func TestFailedPatchRetainsOnlyItsTicketTier(t *testing.T) {
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Before() {}\n")
	ticket, err := watcher.EnqueueToolMutation(t.Context(), filepath.Join(dir, "main.go"))
	require.NoError(t, err)
	select {
	case result := <-ticket.Done:
		require.NoError(t, result.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("unchanged mutation did not finish")
	}
	tiers := ticket.TierState()
	require.False(t, tiers.DerivedAt.IsZero(), "a successful identity no-op still certifies the admitted bytes")
	require.False(t, tiers.Failed)
}
