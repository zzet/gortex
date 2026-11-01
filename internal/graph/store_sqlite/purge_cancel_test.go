package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPurgeRepoCancellationReleasesQueuedWriter(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "gone")
	s.writeMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			s.writeMu.Unlock()
			locked = false
		}
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if purger, ok := any(s).(interface {
			PurgeRepoContext(context.Context, string) error
		}); ok {
			done <- purger.PurgeRepoContext(ctx, "gone")
		} else {
			done <- s.PurgeRepo("gone")
		}
	}()
	// Observe actual writer admission rather than canceling before the call.
	deadline := time.Now().Add(time.Second)
	for s.writeMu.waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Positive(t, s.writeMu.waiting())
	cancel()
	var result error
	var joined bool
	select {
	case result = <-done:
		joined = true
	case <-time.After(200 * time.Millisecond):
	}
	// Cleanup always joins actual work before Close, even on the expected RED.
	unlock()
	if !joined {
		select {
		case result = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("tiny purge failed to join after releasing its owned gate")
		}
	}
	require.True(t, joined, "canceled purge remained queued until an unrelated writer released the gate")
	require.ErrorIs(t, result, context.Canceled)
	require.Equal(t, 1, countByPrefix(t, s.db, "nodes", "gone"), "canceled writer admission must preserve the orphan row")
}
