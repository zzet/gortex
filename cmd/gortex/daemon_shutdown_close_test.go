package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDaemonContractDrainTimeoutStillJoinsSameOwnerBeforeBackendClose(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	startupPublicationEnv(t)
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	runtime, err := installDaemonContractAnalysis(state, zap.NewNop())
	require.NoError(t, err)
	// A real admitted baseline handoff cannot disappear merely because the
	// diagnostic wait expires. No scheduler/source mock certifies its drain.
	runtime.baselineJobs.Add(1)
	var release sync.Once
	finish := func() { release.Do(runtime.baselineJobs.Done) }
	t.Cleanup(func() {
		finish()
		require.NoError(t, runtime.waitClosed())
		require.NoError(t, state.shared.Close())
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, runtime.Close(ctx), context.Canceled)
	originalDone := runtime.closeDone
	for range 5 {
		require.ErrorIs(t, runtime.Close(ctx), context.Canceled)
		require.Equal(t, originalDone, runtime.closeDone, "retry must wait on the same owned drain")
	}
	select {
	case <-originalDone:
		t.Fatal("close falsely certified the admitted handoff as drained")
	default:
	}
	core, logs := observer.New(zap.InfoLevel)
	backendErr := errors.New("backend close receipt")
	backendClosed := make(chan struct{})
	done := make(chan error, 1)
	teardown := newDaemonTeardown(nil, func() error {
		return closeDaemonOwners(ctx, runtime, func() error {
			close(backendClosed)
			return errors.Join(state.shared.Close(), backendErr)
		}, zap.New(core))
	})
	go func() { done <- teardown() }()
	require.Eventually(t, func() bool {
		return logs.FilterMessage("daemon: contract drain still pending; waiting for ownership").Len() == 1
	}, 5*time.Second, time.Millisecond)
	select {
	case <-backendClosed:
		t.Fatal("backend closed under a live baseline owner")
	default:
	}
	finish()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled, "diagnostic warning must survive successful eventual join")
		require.ErrorIs(t, err, backendErr, "actual backend failure must not be hidden by warning")
	case <-time.After(5 * time.Second):
		t.Fatal("joined contract drain stranded the backend")
	}
	require.Same(t, err, teardown(), "every caller must see the cached teardown outcome")
	require.NoError(t, runtime.Close(context.Background()), "deadline was a caller wait, not a permanent drain failure")
	require.Len(t, logs.FilterMessage("daemon: shutdown phase complete").All(), 2)
}

func TestDaemonOwnersWithoutContractRuntimeStillCloseBackend(t *testing.T) {
	var runtime *daemonContractAnalysis // production's explicit diagnostic opt-out
	backendErr := errors.New("backend close failed")
	calls := 0
	err := closeDaemonOwners(context.Background(), runtime, func() error {
		calls++
		return backendErr
	}, zap.NewNop())
	require.ErrorIs(t, err, backendErr)
	require.Equal(t, 1, calls)
}
