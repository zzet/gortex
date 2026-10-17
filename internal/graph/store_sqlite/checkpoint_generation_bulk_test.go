package store_sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type checkpointAttemptResult struct {
	complete bool
	retry    bool
	cause    error
}

type checkpointBeginResult struct {
	opened bool
	err    error
}

func waitCheckpointSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for checkpoint signal")
	}
}

func waitCheckpointAttemptResult(t *testing.T, result <-chan checkpointAttemptResult) checkpointAttemptResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for checkpoint attempt")
		return checkpointAttemptResult{}
	}
}

func waitCheckpointBeginResult(t *testing.T, result <-chan checkpointBeginResult) checkpointBeginResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for generation bulk begin")
		return checkpointBeginResult{}
	}
}

func TestGenerationBulkCheckpointSuppressionIsSharedAndOwned(t *testing.T) {
	store, _ := openTempStore(t)
	store.passiveCheckpointTimeout = 2 * time.Second

	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAttempt := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAttempt)
	attemptResult := make(chan checkpointAttemptResult, 1)
	go func() {
		var cause error
		complete, retry := store.runBackgroundCheckpointAttempt(func(ctx context.Context) (bool, bool) {
			close(entered)
			<-ctx.Done()
			cause = context.Cause(ctx)
			close(canceled)
			<-release
			return false, true
		})
		attemptResult <- checkpointAttemptResult{complete: complete, retry: retry, cause: cause}
	}()
	waitCheckpointSignal(t, entered)

	first := make(chan checkpointBeginResult, 1)
	go func() {
		opened, err := store.AtGeneration(101).BeginGenerationBulkLoad(101)
		first <- checkpointBeginResult{opened: opened, err: err}
	}()
	waitCheckpointSignal(t, canceled)

	opened, err := store.AtGeneration(102).BeginGenerationBulkLoad(102)
	require.False(t, opened)
	require.ErrorIs(t, err, ErrGenerationBulkCheckpointBusy)

	releaseAttempt()
	got := waitCheckpointBeginResult(t, first)
	require.NoError(t, got.err)
	require.True(t, got.opened)
	background := waitCheckpointAttemptResult(t, attemptResult)
	require.False(t, background.complete)
	require.True(t, background.retry)
	require.ErrorIs(t, background.cause, errWALCheckpointDeferredBulk)

	generation, active := store.InGenerationBulkLoad()
	require.True(t, active)
	require.Equal(t, int64(101), generation)

	opened, err = store.AtGeneration(103).BeginGenerationBulkLoad(103)
	require.NoError(t, err)
	require.False(t, opened)

	called := false
	complete, retry := store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) {
		called = true
		return true, false
	})
	require.False(t, complete)
	require.True(t, retry)
	require.False(t, called)

	// A stale End for another generation cannot take or release generation 101's lease.
	require.NoError(t, store.EndGenerationBulkLoadFor(102))
	generation, active = store.InGenerationBulkLoad()
	require.True(t, active)
	require.Equal(t, int64(101), generation)

	require.NoError(t, store.EndGenerationBulkLoadFor(101))
	called = false
	complete, retry = store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) {
		called = true
		return true, false
	})
	require.True(t, complete)
	require.False(t, retry)
	require.True(t, called)
}

func TestGenerationBulkCheckpointTimeoutDoesNotOpenOrWedge(t *testing.T) {
	store, _ := openTempStore(t)
	store.passiveCheckpointTimeout = 25 * time.Millisecond

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAttempt := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAttempt)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) {
			close(entered)
			<-release
			return false, true
		})
	}()
	waitCheckpointSignal(t, entered)

	started := time.Now()
	opened, err := store.BeginGenerationBulkLoad(201)
	require.False(t, opened)
	require.ErrorIs(t, err, ErrGenerationBulkCheckpointBusy)
	require.Less(t, time.Since(started), time.Second)
	_, active := store.InGenerationBulkLoad()
	require.False(t, active)

	releaseAttempt()
	waitCheckpointSignal(t, done)
	opened, err = store.BeginGenerationBulkLoad(201)
	require.NoError(t, err)
	require.True(t, opened)
	require.NoError(t, store.EndGenerationBulkLoad())
}

func TestGenerationBulkSetupFailureReleasesCheckpointLease(t *testing.T) {
	store, _ := openTempStore(t)
	generation := store.AtGeneration(211)
	require.NoError(t, generation.AddBatchChecked([]*graph.Node{{
		ID: "repo/file.go::AlreadyThere", Name: "AlreadyThere", Kind: graph.KindFunction,
		FilePath: "repo/file.go", RepoPrefix: "repo",
	}}, nil))

	opened, err := store.BeginGenerationBulkLoad(211)
	require.False(t, opened)
	require.ErrorIs(t, err, ErrGenerationBulkLoadPopulated)

	called := false
	complete, retry := store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) {
		called = true
		return true, false
	})
	require.True(t, complete)
	require.False(t, retry)
	require.True(t, called)
}

func TestBackgroundCheckpointAttemptHasNoAutonomousDeadline(t *testing.T) {
	store := &Store{storeCore: &storeCore{passiveCheckpointTimeout: 5 * time.Millisecond}}

	var cause error
	complete, retry := store.runBackgroundCheckpointAttempt(func(ctx context.Context) (bool, bool) {
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
			return false, true
		case <-timer.C:
			cause = context.Cause(ctx)
			return cause == nil, cause != nil
		}
	})
	require.True(t, complete)
	require.False(t, retry)
	require.NoError(t, cause)
}

func TestCheckpointLoopShutdownJoinsActiveAttempt(t *testing.T) {
	store := &Store{storeCore: &storeCore{
		passiveCheckpointTimeout: time.Second,
		stopCheckpoint:           make(chan struct{}),
		checkpointDone:           make(chan struct{}),
	}}
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var canceledOnce sync.Once
	var releaseOnce sync.Once
	releaseAttempt := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAttempt)

	go store.runCheckpointLoopWithAttemptAndCleanup(
		time.Nanosecond, time.Millisecond, time.Millisecond,
		func() bool {
			_, _ = store.runBackgroundCheckpointAttempt(func(ctx context.Context) (bool, bool) {
				enteredOnce.Do(func() { close(entered) })
				<-ctx.Done()
				canceledOnce.Do(func() { close(canceled) })
				<-release
				return false, true
			})
			return false
		}, nil,
	)
	waitCheckpointSignal(t, entered)
	stopped := make(chan struct{})
	go func() {
		store.stopCheckpointLoop()
		close(stopped)
	}()
	waitCheckpointSignal(t, canceled)
	select {
	case <-stopped:
		t.Fatal("checkpoint loop stopped before its active attempt returned")
	default:
	}
	releaseAttempt()
	waitCheckpointSignal(t, stopped)
}

func TestCloseReleasesGenerationBulkCheckpointLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	opened, err := store.BeginGenerationBulkLoad(301)
	require.NoError(t, err)
	require.True(t, opened)
	require.NoError(t, store.Close())

	store.backgroundCheckpoint.mu.Lock()
	lease := store.backgroundCheckpoint.generationLease
	store.backgroundCheckpoint.mu.Unlock()
	require.Zero(t, lease)
}

func TestCloseReleasesUnboundGenerationBulkCheckpointLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close-unbound.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	var closeOnce sync.Once
	var closeErr error
	closeStore := func() error {
		closeOnce.Do(func() { closeErr = store.Close() })
		return closeErr
	}
	t.Cleanup(func() { _ = closeStore() })

	lease, err := store.acquireGenerationBulkCheckpointLease()
	require.NoError(t, err)
	require.NotZero(t, lease)

	require.NoError(t, closeStore())
	require.False(t, store.bindGenerationBulkCheckpointLease(lease, 351))

	store.backgroundCheckpoint.mu.Lock()
	retainedLease := store.backgroundCheckpoint.generationLease
	boundGeneration := store.backgroundCheckpoint.boundGeneration
	store.backgroundCheckpoint.mu.Unlock()
	require.Zero(t, retainedLease)
	require.Zero(t, boundGeneration)
}

func TestGenerationBulkCheckpointStaleFinishCannotReleaseNewOwner(t *testing.T) {
	store, _ := openTempStore(t)
	opened, err := store.BeginGenerationBulkLoad(401)
	require.NoError(t, err)
	require.True(t, opened)
	require.NoError(t, store.EndGenerationBulkLoadFor(401))

	opened, err = store.BeginGenerationBulkLoad(402)
	require.NoError(t, err)
	require.True(t, opened)
	require.NoError(t, store.EndGenerationBulkLoadFor(401))
	generation, active := store.InGenerationBulkLoad()
	require.True(t, active)
	require.Equal(t, int64(402), generation)

	called := false
	complete, retry := store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) {
		called = true
		return true, false
	})
	require.False(t, complete)
	require.True(t, retry)
	require.False(t, called)

	// Legacy base/root compatibility still closes the current physical owner.
	require.NoError(t, store.EndGenerationBulkLoad())
	_, active = store.InGenerationBulkLoad()
	require.False(t, active)
}
