package mcp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestHealthTerminalCloseOwnsInitialReplayAndRefusesLateStart(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	fake := &fakeSpecificSender{}
	var calls atomic.Int32
	b := newHealthBroadcaster(fake, func() map[string]any {
		calls.Add(1)
		close(entered)
		<-release
		return map[string]any{"state": "last"}
	}, zap.NewNop())
	s := &Server{healthBroadcaster: b}
	replayDone := make(chan struct{})
	go func() { defer close(replayDone); b.subscribe("initial") }()
	t.Cleanup(func() { unlock(); s.DrainBackground(); <-replayDone })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial replay never entered snapshot")
	}
	ctx, backgroundDone, admitted := s.admitBackground()
	require.True(t, admitted)
	var doneOnce sync.Once
	finishBackground := func() { doneOnce.Do(backgroundDone) }
	t.Cleanup(finishBackground)
	s.BeginBackgroundClose() // Must cancel every owner without joining replay.
	require.Error(t, ctx.Err())
	finishBackground()
	require.False(t, b.subscribe("late"))
	b.start()
	b.setInterval(time.Second)
	b.setSnapshotFn(func() map[string]any { calls.Add(1); return nil })
	b.publishOnce()
	joined := make(chan struct{})
	go func() { defer close(joined); s.DrainBackground() }()
	select {
	case <-joined:
		t.Fatal("drain returned while admitted health replay still ran")
	default:
	}
	unlock()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("health replay did not actually join")
	}
	<-replayDone
	require.Equal(t, int32(1), calls.Load())
	b.mu.Lock()
	remaining := b.stopTicker
	b.mu.Unlock()
	require.Nil(t, remaining)
}

func TestHealthTerminalCloseJoinsDetachedTickerBeforeCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	b := newHealthBroadcaster(&fakeSpecificSender{}, func() map[string]any {
		calls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		return map[string]any{"x": 1}
	}, zap.NewNop())
	b.interval = time.Millisecond // One local timer trigger, not a latency claim.
	b.start()
	stopped := make(chan struct{})
	t.Cleanup(func() { unlock(); b.close() })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("ticker did not enter snapshot")
	}
	go func() { defer close(stopped); b.stop() }()
	require.Eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.stopTicker == nil }, 3*time.Second, time.Millisecond)
	b.start() // Normal reconfiguration may overlap a canceled prior callback.
	b.beginClose()
	joined := make(chan struct{})
	go func() { defer close(joined); b.close() }()
	select {
	case <-joined:
		t.Fatal("terminal close forgot a detached ticker owner")
	default:
	}
	unlock()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal health close did not join")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary stop did not join its exact ticker")
	}
	before := calls.Load()
	b.start()
	b.publishOnce()
	require.False(t, b.subscribe("late"))
	require.Equal(t, before, calls.Load())
}
