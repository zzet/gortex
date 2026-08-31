package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

// TestShutdownDoesNotQueueBehindTheControllerMutex is the #370 invariant.
//
// The controller mutex is coarse: Track holds it across the whole indexing run,
// and Reload and every enricher hold it for their duration too — minutes on a
// large workspace. Shutdown used to take that same mutex just to read the
// teardown hook, which meant `gortex daemon stop` queued behind exactly the
// long-running work the user was trying to stop. Stopping has to be the one
// thing that still works when the daemon is at its least healthy.
func TestShutdownDoesNotQueueBehindTheControllerMutex(t *testing.T) {
	c := &realController{}

	// Stand in for an in-flight track / reload / enrichment.
	c.mu.Lock()
	defer c.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- c.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked while the controller mutex was held — a busy daemon cannot be stopped")
	}
}

// TestWatcherIsReachableWhileTheControllerMutexIsHeld pins the property the
// teardown hook depends on directly: the watcher pointer must be readable
// without the coarse mutex. Moving it back onto mu — the shape this replaced —
// turns this red.
func TestWatcherIsReachableWhileTheControllerMutexIsHeld(t *testing.T) {
	c := &realController{}
	c.AttachWatcher(nil)

	c.mu.Lock()
	defer c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.StopWatcher()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading the watcher required the controller mutex")
	}
}

// TestStatusHonoursACancelledCaller: Status is the aggregate that sits ahead of
// `daemon stop`, every `gortex call`, and the agent hooks. Once the caller's
// budget has expired, finishing a whole-store scan only burns reads nobody
// reads — and, worse, goes on to queue for the contended mutex.
func TestStatusHonoursACancelledCaller(t *testing.T) {
	c := &realController{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Status(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestDaemonProbeIndeterminate pins the three-state routing decision. Folding
// "the daemon is busy" into "the daemon does not own this repo" is what made a
// slow-but-healthy daemon tell users `gortex track <path>` — advice that is
// both false (the repo IS tracked) and actively harmful (track is the unbounded
// operation holding the lock in the first place).
func TestDaemonProbeIndeterminate(t *testing.T) {
	ok := daemon.ControlResponse{OK: true}

	assert.True(t, daemonProbeIndeterminate(
		fmt.Errorf("wrapped: %w", daemon.ErrDaemonUnresponsive), daemon.ControlResponse{}),
		"a client-side deadline means busy, not absent")
	assert.True(t, daemonProbeIndeterminate(nil,
		daemon.ControlResponse{ErrorCode: daemon.ErrTimeout}),
		"the daemon answering 'timeout' means busy too")

	assert.False(t, daemonProbeIndeterminate(nil, ok),
		"a real answer is never indeterminate")
	assert.False(t, daemonProbeIndeterminate(daemon.ErrDaemonUnavailable, daemon.ControlResponse{}),
		"an absent daemon is a real answer: there is nothing to ask")
	assert.False(t, daemonProbeIndeterminate(nil,
		daemon.ControlResponse{ErrorCode: daemon.ErrInternal}),
		"a genuine controller error is a real answer")
}
