package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// statusResult carries a Status answer back from the goroutine that runs it,
// so a blocked call fails the test instead of hanging it.
type statusResult struct {
	st  daemon.StatusResponse
	err error
}

// Status computes its slow half lock-free and then queues on the controller
// mutex, which a track holds for the length of an initial index — minutes on a
// large workspace. Every `gortex daemon status` during that window died on the
// control budget with a generic timeout, so the one call that would have told
// the user a track was running was the call the track took out.
//
// Status must answer inside the caller's budget while the mutex is held: the
// live half is already lock-free, and the mutex-guarded aggregate is served
// from the last pass that computed one, marked so the renderer can say so.
func TestStatusAnswersWhileTheControllerMutexIsHeld(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	c.ready.Store(true)
	c.toolSurface = func() (string, string, int) { return "core", "defer", 7 }

	// One uncontended pass, so there is a last-good aggregate to serve.
	warm, err := c.Status(context.Background())
	require.NoError(t, err)
	require.False(t, warm.AggregateBusy)
	require.Equal(t, "core", warm.ToolPreset)

	c.mu.Lock() // stand in for a track / reload / enrichment in flight
	defer c.mu.Unlock()
	// Mutating the aggregate's inputs under the mutex proves the busy answer
	// is the stored snapshot rather than a fresh read: a pass that recomputed
	// them here would report "changed". Writing under c.mu is race-free
	// precisely because the only reader takes it.
	c.toolSurface = func() (string, string, int) { return "changed", "defer", 9 }

	budget := 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	started := time.Now()
	done := make(chan statusResult, 1)
	go func() {
		st, err := c.Status(ctx)
		done <- statusResult{st, err}
	}()

	select {
	case got := <-done:
		elapsed := time.Since(started)
		require.NoError(t, got.err, "a busy daemon must still answer status")
		assert.Less(t, elapsed, budget, "Status must answer inside the caller's budget, not on top of it")
		assert.True(t, got.st.AggregateBusy, "the response must mark the aggregate as a snapshot")
		assert.NotZero(t, got.st.AggregateCachedUnix, "a served snapshot must say when it was taken")
		assert.Equal(t, "core", got.st.ToolPreset, "the aggregate must come from the last successful pass")
		assert.Equal(t, 7, got.st.LearnedTools)
		assert.True(t, got.st.Ready, "the lock-free live half must stay live")
	case <-time.After(5 * time.Second):
		t.Fatal("Status blocked behind the controller mutex — a busy daemon cannot report its own status")
	}
}

// The first status of a daemon whose initial track is still running has no
// snapshot to fall back on. It must degrade — an empty aggregate plus the busy
// marker — never fail: an error here reads as "the daemon is broken" when the
// truth is "the daemon is working on it". The tracked-repo registry is read
// lock-free, so the configured rows still render.
func TestStatusBeforeAnySuccessfulPassDegradesInsteadOfFailing(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")

	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan statusResult, 1)
	go func() {
		st, err := c.Status(ctx)
		done <- statusResult{st, err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err, "a daemon that has never completed a status pass must still answer")
		assert.True(t, got.st.AggregateBusy)
		assert.Zero(t, got.st.AggregateCachedUnix,
			"no pass has completed, so there is no snapshot age to claim")
		assert.Empty(t, got.st.Workspaces, "the mutex-guarded rollup is genuinely unknown")
		require.Len(t, got.st.TrackedRepos, 1, "the config registry is lock-free and still answers")
		assert.True(t, got.st.TrackedRepos[0].Unloaded)
	case <-time.After(5 * time.Second):
		t.Fatal("Status blocked behind the controller mutex on its very first pass")
	}
}

// The daemon abandons a control handler the instant its budget expires
// (Server.handleControlBounded), so an answer that arrives exactly on the
// deadline is a timeout with extra steps. The wait for the controller mutex
// keeps a reserve of the budget back to assemble and write the answer in.
func TestStatusAnswersWithBudgetLeftToSpare(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")

	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*statusLockReserve)
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)

	done := make(chan statusResult, 1)
	go func() {
		st, err := c.Status(ctx)
		done <- statusResult{st, err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.True(t, got.st.AggregateBusy)
		assert.Positive(t, time.Until(deadline),
			"the answer must arrive before the budget expires, not on it — by then the daemon has already abandoned the handler")
	case <-time.After(5 * time.Second):
		t.Fatal("Status blocked behind the controller mutex")
	}
}

// The uncontended pass must be exactly what it was: a freshly computed
// aggregate, no marker, and the mutex handed back. A marker that leaked onto a
// healthy status would teach every reader to distrust a correct table.
func TestStatusWithoutContentionCarriesNoBusyMarker(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	c.toolSurface = func() (string, string, int) { return "core", "defer", 7 }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := c.Status(ctx)
	require.NoError(t, err)
	assert.False(t, st.AggregateBusy, "an uncontended pass computed the aggregate itself")
	assert.Zero(t, st.AggregateCachedUnix)
	assert.Equal(t, "core", st.ToolPreset)
	require.Len(t, st.TrackedRepos, 1)
	assert.Equal(t, "/work/alpha", st.TrackedRepos[0].Path)

	require.True(t, c.mu.TryLock(), "Status must not leave the controller mutex held")
	c.mu.Unlock()
}

// StatusExact keeps its contract: the caller paid for measured numbers, and a
// cached table is not that. It waits for the mutex within the caller's budget
// and reports the expiry rather than silently serving the snapshot the routine
// pass would have served.
func TestStatusExactWaitsForTheControllerMutex(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	warm, err := c.Status(context.Background())
	require.NoError(t, err)
	require.False(t, warm.AggregateBusy, "precondition: a snapshot exists to be wrongly served")

	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan statusResult, 1)
	go func() {
		st, err := c.StatusExact(ctx)
		done <- statusResult{st, err}
	}()

	select {
	case got := <-done:
		require.Error(t, got.err, "exact status must not fall back to the cached aggregate")
		assert.ErrorIs(t, got.err, context.DeadlineExceeded)
		assert.False(t, got.st.AggregateBusy)
	case <-time.After(5 * time.Second):
		t.Fatal("StatusExact never returned — its wait must still honour the caller's budget")
	}
}
