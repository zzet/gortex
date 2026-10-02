package main

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// staleDaemonFixture fakes a live daemon whose version flips to the binary's
// once it has been stopped and respawned, and counts each lifecycle call.
type staleDaemonFixture struct {
	mu       sync.Mutex
	version  string
	stops    atomic.Int32
	spawns   atomic.Int32
	restarts atomic.Int32
}

func installStaleDaemonFixture(t *testing.T, daemonVer, binaryVer string) *staleDaemonFixture {
	t.Helper()
	isolateSpawnLock(t)
	t.Cleanup(restoreSeams)
	f := &staleDaemonFixture{version: daemonVer}
	isDaemonRunning = func() bool { return true }
	stopIntentActive = func() bool { return false }
	serviceActive = func() bool { return false }
	serviceRestart = func(io.Writer) error { f.restarts.Add(1); return nil }
	binaryVersion = func() string { return binaryVer }
	runningDaemonVersion = func() (string, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.version, f.version != ""
	}
	stopStaleDaemon = func() error {
		f.stops.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the window for racing callers
		return nil
	}
	spawnDaemon = func() error {
		f.spawns.Add(1)
		f.mu.Lock()
		f.version = binaryVer
		f.mu.Unlock()
		return nil
	}
	return f
}

func TestEnsureDaemon_ReplacesOlderDaemon(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.70.0", "v0.71.0")
	if d := ensureDaemonReady(true); d != daemonAutostarted {
		t.Fatalf("an older daemon must be replaced => daemonAutostarted, got %d", d)
	}
	if f.stops.Load() != 1 || f.spawns.Load() != 1 {
		t.Fatalf("want one stop + one spawn, got stops=%d spawns=%d", f.stops.Load(), f.spawns.Load())
	}
}

func TestEnsureDaemon_KeepsDaemonUnlessStrictlyOlder(t *testing.T) {
	cases := []struct {
		name, daemonVer, binaryVer string
	}{
		{"same version", "v0.71.0", "v0.71.0"},
		{"newer daemon is never downgraded", "v0.72.0", "v0.71.0"},
		{"dev binary", "v0.70.0", "v0.0.0-dev"},
		{"dev daemon", "v0.0.0-dev", "v0.71.0"},
		{"daemon reports no version", "", "v0.71.0"},
		{"unparseable daemon version", "garbage", "v0.71.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := installStaleDaemonFixture(t, tc.daemonVer, tc.binaryVer)
			if d := ensureDaemonReady(true); d != daemonReady {
				t.Fatalf("want daemonReady, got %d", d)
			}
			if f.stops.Load() != 0 || f.spawns.Load() != 0 {
				t.Fatalf("daemon must be left alone, got stops=%d spawns=%d", f.stops.Load(), f.spawns.Load())
			}
		})
	}
}

func TestEnsureDaemon_AutostartOffLeavesOlderDaemon(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.70.0", "v0.71.0")
	if d := ensureDaemonReady(false); d != daemonReady {
		t.Fatalf("want daemonReady, got %d", d)
	}
	if f.stops.Load() != 0 || f.spawns.Load() != 0 {
		t.Fatal("with autostart off the daemon must not be cycled")
	}
}

func TestEnsureDaemon_StaleReplaceIsSingleFlight(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.70.0", "v0.71.0")
	const K = 8
	var wg sync.WaitGroup
	results := make([]daemonDecision, K)
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = ensureDaemonReady(true) }(i)
	}
	wg.Wait()
	if f.stops.Load() != 1 || f.spawns.Load() != 1 {
		t.Fatalf("exactly one replacement across %d callers, got stops=%d spawns=%d", K, f.stops.Load(), f.spawns.Load())
	}
	for i, r := range results {
		if r == daemonUnavailable {
			t.Fatalf("caller %d should not be unavailable after a successful replacement", i)
		}
	}
}

func TestEnsureDaemon_StaleStopFailureKeepsOldDaemon(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.70.0", "v0.71.0")
	stopStaleDaemon = func() error { f.stops.Add(1); return errors.New("stop failed") }
	if d := ensureDaemonReady(true); d != daemonReady {
		t.Fatalf("a failed stop must fall back to the running daemon, got %d", d)
	}
	if f.spawns.Load() != 0 {
		t.Fatal("must not spawn a second daemon next to one that failed to stop")
	}
}

func TestEnsureDaemon_StaleSupervisedDaemonRestartsThroughSupervisor(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.70.0", "v0.71.0")
	serviceActive = func() bool { return true }
	if d := ensureDaemonReady(true); d != daemonReady {
		t.Fatalf("want daemonReady, got %d", d)
	}
	if f.restarts.Load() != 1 {
		t.Fatalf("a supervised daemon must be bounced via the supervisor, got %d restarts", f.restarts.Load())
	}
	if f.stops.Load() != 0 || f.spawns.Load() != 0 {
		t.Fatal("a supervised daemon must not be stopped/spawned by hand")
	}
}
