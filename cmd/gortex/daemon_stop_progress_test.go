package main

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// fakeClosingDaemon is a daemon process whose store close reports progress
// through the stop path's seams.
type fakeClosingDaemon struct {
	mu       sync.Mutex
	progress store_sqlite.CloseProgress
	exited   atomic.Bool
	killed   atomic.Bool
}

func (f *fakeClosingDaemon) install(t *testing.T, pid int) {
	t.Helper()
	prevAlive, prevKill, prevProgress := daemonProcessAlive, daemonKillProcess, daemonCloseProgress
	prevBound, prevPeriod := daemonCloseNoProgressBound, daemonDrainProgressPeriod
	daemonProcessAlive = func(int) bool { return !f.exited.Load() && !f.killed.Load() }
	daemonKillProcess = func(int) error { f.killed.Store(true); return nil }
	daemonCloseProgress = func() (store_sqlite.CloseProgress, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.progress, true
	}
	daemonDrainProgressPeriod = 100 * time.Millisecond
	t.Cleanup(func() {
		daemonProcessAlive, daemonKillProcess, daemonCloseProgress = prevAlive, prevKill, prevProgress
		daemonCloseNoProgressBound, daemonDrainProgressPeriod = prevBound, prevPeriod
	})
	f.progress = store_sqlite.CloseProgress{PID: pid, Phase: "draining", StartedUnixNano: time.Now().UnixNano(), PendingFrames: 5_000_000}
}

func (f *fakeClosingDaemon) advance() {
	f.mu.Lock()
	f.progress.LastProgressUnixNano = time.Now().UnixNano()
	f.progress.FramesCopiedEstimate += 1000
	f.mu.Unlock()
}

// A close that keeps making progress, slowly, well past the stop's grace:
// the stop waits for it to finish instead of killing it.
func TestDaemonStopWaitsForACloseThatMakesProgress(t *testing.T) {
	const pid = 4242
	f := &fakeClosingDaemon{}
	f.install(t, pid)
	daemonCloseNoProgressBound = 400 * time.Millisecond
	stop := make(chan struct{})
	go func() { // progress every 150 ms for 1.5 s, then the close finishes
		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			f.advance()
		}
		f.exited.Store(true)
	}()
	defer close(stop)
	var out bytes.Buffer
	started := time.Now()
	waitForDaemonExitWithin(&out, pid, 200*time.Millisecond, true)
	t.Logf("waited %s:\n%s", time.Since(started).Round(time.Millisecond), out.String())
	if f.killed.Load() {
		t.Fatal("the stop killed a close that was making progress")
	}
	if !f.exited.Load() {
		t.Fatal("the stop returned before the daemon exited")
	}
	if !strings.Contains(out.String(), "close still making progress") {
		t.Fatalf("the decision to keep waiting was not logged:\n%s", out.String())
	}
}

// A close that stops making progress is given up on after the no-progress
// bound, not after an open-ended wait.
func TestDaemonStopGivesUpOnAStalledCloseAfterTheNoProgressBound(t *testing.T) {
	const pid = 4243
	f := &fakeClosingDaemon{}
	f.install(t, pid)
	daemonCloseNoProgressBound = 400 * time.Millisecond
	f.advance() // one sign of life, then nothing
	var out bytes.Buffer
	started := time.Now()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		waitForDaemonExitWithin(&out, pid, 200*time.Millisecond, true)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		f.exited.Store(true) // release the wait so the test can end
		<-returned
		t.Fatalf("the stop was still waiting on a stalled close after 5 s (bound 400ms)")
	}
	waited := time.Since(started)
	t.Logf("waited %s:\n%s", waited.Round(time.Millisecond), out.String())
	if !f.killed.Load() {
		t.Fatal("the stop never gave up on a stalled close")
	}
	if waited > 3*time.Second {
		t.Fatalf("the stop waited %s on a close with no progress (bound 400ms)", waited)
	}
	if !strings.Contains(out.String(), "close made no progress for") {
		t.Fatalf("the give-up decision was not logged:\n%s", out.String())
	}
}
