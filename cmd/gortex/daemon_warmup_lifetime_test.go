package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/indexer"
)

func TestDaemonWarmupShutdownCancelsBeforeJoinAndBackendClose(t *testing.T) {
	warmup := newDaemonWarmup(context.Background())
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { warmup.cancel(); unblock(); warmup.wait() })
	require.True(t, warmup.start(func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release // Producer cleanup must finish before the backend closes.
	}))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup producer did not enter")
	}
	controller := &realController{}
	logs, observed := observer.New(zap.InfoLevel)
	backendClosed := make(chan struct{})
	teardown := installDaemonTeardown(controller, func() {
		stopDaemonWarmup(warmup, controller, zap.New(logs))
	}, func() error {
		close(backendClosed)
		return nil
	})
	t.Cleanup(func() { unblock(); teardown() })
	controller.mu.Lock()
	defer controller.mu.Unlock()
	finished := make(chan error, 1)
	go func() { finished <- controller.Shutdown(context.Background()) }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel startup before joining it")
	}
	select {
	case <-backendClosed:
		t.Fatal("backend closed while startup still owned its producer")
	default:
	}
	unblock()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited on the coarse controller mutex")
	}
	select {
	case <-backendClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("joined startup did not release backend teardown")
	}
	completed := observed.FilterMessage("daemon: shutdown phase complete").All()
	require.Len(t, completed, 2)
	require.Equal(t, "warmup_join", completed[0].ContextMap()["phase"])
	require.Equal(t, "watcher_stop", completed[1].ContextMap()["phase"])
}

func TestDaemonWarmupCanceledBeforeStartDoesNotLaunch(t *testing.T) {
	warmup := newDaemonWarmup(context.Background())
	warmup.cancel()
	warmup.wait() // Early startup failure must not wait on an unstarted task.
	require.False(t, warmup.start(func(context.Context) { t.Error("startup launched after cancellation") }))
}

func TestDaemonWatcherStopWinsAnAdmittedPhysicalStart(t *testing.T) {
	// No repository or OS watcher is needed: the real MultiWatcher Start/Stop
	// state boundary is what must refuse a physical start after revocation.
	mw, err := indexer.NewMultiWatcher(nil, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mw.Stop()) })
	controller := &realController{}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	started := make(chan error, 1)
	go func() {
		started <- controller.startAndAttachWatcherWith(context.Background(), mw, func() error {
			close(entered)
			<-release
			return mw.Start()
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not reserve its physical start")
	}
	require.Nil(t, controller.watcher(), "an unstarted watcher must not be visible")
	controller.mu.Lock()
	defer controller.mu.Unlock()
	stopped := make(chan struct{})
	go func() { controller.StopWatcher(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop held a handoff/controller mutex while physical start was blocked")
	}
	unblock()
	select {
	case err := <-started:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("revoked watcher start did not finish")
	}
	require.Nil(t, controller.watcher())
	require.Error(t, mw.Start(), "the rejected watcher must already be stopped")
	controller.AttachWatcher(nil)
	late, err := indexer.NewMultiWatcher(nil, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, late.Stop()) })
	controller.AttachWatcher(late)
	require.Nil(t, controller.watcher(), "nil attach must not reopen a stopped handoff")
	require.Error(t, late.Start(), "late attach must clean up its unaccepted watcher")
}

func TestDaemonWatcherCanceledBeforeStartNeverInvokesStart(t *testing.T) {
	mw, err := indexer.NewMultiWatcher(nil, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mw.Stop()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	controller := &realController{}
	require.ErrorIs(t, controller.startAndAttachWatcherWith(ctx, mw, func() error {
		t.Error("physical start invoked with canceled startup context")
		return nil
	}), context.Canceled)
	require.Nil(t, controller.watcher())
	require.Error(t, mw.Start())
}
