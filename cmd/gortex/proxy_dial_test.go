package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// withFastDialRetry shortens the retry cadence so tests don't sleep on the
// production window, restoring the originals on cleanup.
func withFastDialRetry(t *testing.T, window time.Duration) {
	t.Helper()
	origDial, origWin, origInt := dialDaemon, proxyDialRetryWindow, proxyDialRetryInterval
	proxyDialRetryWindow = window
	proxyDialRetryInterval = time.Millisecond
	t.Cleanup(func() {
		dialDaemon = origDial
		proxyDialRetryWindow = origWin
		proxyDialRetryInterval = origInt
	})
}

func TestDialDaemonWithRetry_SuccessFirstTry(t *testing.T) {
	withFastDialRetry(t, time.Second)
	var calls int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		atomic.AddInt32(&calls, 1)
		return &daemon.Client{}, nil
	}
	client, recoverable, err := dialDaemonWithRetry(context.Background(), daemon.Handshake{})
	if client == nil || recoverable || err != nil {
		t.Fatalf("want (client, false, nil), got (%v, %v, %v)", client, recoverable, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want 1 dial, got %d", got)
	}
}

func TestDialDaemonWithRetry_RetriesThenConnects(t *testing.T) {
	withFastDialRetry(t, 5*time.Second)
	var calls int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		// Fail the first two attempts as "unavailable" (socket up but accept()
		// starved by warmup), then connect — the exact race the retry exists
		// to ride out.
		if atomic.AddInt32(&calls, 1) < 3 {
			return nil, fmt.Errorf("%w: connection refused", daemon.ErrDaemonUnavailable)
		}
		return &daemon.Client{}, nil
	}
	client, recoverable, err := dialDaemonWithRetry(context.Background(), daemon.Handshake{})
	if client == nil || recoverable || err != nil {
		t.Fatalf("want (client, false, nil), got (%v, %v, %v)", client, recoverable, err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("want 3 dials, got %d", got)
	}
}

func TestDialDaemonWithRetry_WindowExpiry(t *testing.T) {
	// A tiny window so the loop concedes almost immediately.
	withFastDialRetry(t, 5*time.Millisecond)
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		return nil, fmt.Errorf("%w: connection refused", daemon.ErrDaemonUnavailable)
	}
	client, recoverable, err := dialDaemonWithRetry(context.Background(), daemon.Handshake{})
	if client != nil || !recoverable {
		t.Fatalf("want (nil, true, err), got (%v, %v, %v)", client, recoverable, err)
	}
	if !errors.Is(err, daemon.ErrDaemonUnavailable) {
		t.Fatalf("want ErrDaemonUnavailable, got %v", err)
	}
}

func TestDialDaemonWithRetry_ProtocolMismatchConcedesImmediately(t *testing.T) {
	withFastDialRetry(t, 5*time.Second)
	var calls int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("%w: stale", daemon.ErrProtocolVersionMismatch)
	}
	client, recoverable, err := dialDaemonWithRetry(context.Background(), daemon.Handshake{})
	if client != nil || recoverable || !errors.Is(err, daemon.ErrProtocolVersionMismatch) {
		t.Fatalf("want (nil, false, mismatch), got (%v, %v, %v)", client, recoverable, err)
	}
	// A mismatch never resolves by waiting — must not retry.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want 1 dial (no retry on mismatch), got %d", got)
	}
}

func TestDialDaemonWithRetry_NonRecoverableSurfaced(t *testing.T) {
	withFastDialRetry(t, 5*time.Second)
	var calls int32
	boom := errors.New("permission denied")
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, boom
	}
	client, recoverable, err := dialDaemonWithRetry(context.Background(), daemon.Handshake{})
	if client != nil || recoverable || !errors.Is(err, boom) {
		t.Fatalf("want (nil, false, boom), got (%v, %v, %v)", client, recoverable, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want 1 dial (no retry on a real error), got %d", got)
	}
}

func TestDialDaemonWithRetry_ContextCancelSurfaced(t *testing.T) {
	withFastDialRetry(t, 10*time.Second)
	var calls int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("%w: connection refused", daemon.ErrDaemonUnavailable)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, recoverable, err := dialDaemonWithRetry(ctx, daemon.Handshake{})
	if client != nil || recoverable || !errors.Is(err, context.Canceled) {
		t.Fatalf("want (nil, false, context.Canceled), got (%v, %v, %v)", client, recoverable, err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("canceled context attempted %d daemon dial(s), want 0", got)
	}
}
