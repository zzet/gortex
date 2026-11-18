package mcp

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

// A tool call that arrives while the idle heap release is running is admitted
// at once: the release runs outside the activity tracker's exclusive gate.
// The stand-in release takes 200 ms; the call may wait a few milliseconds at
// most (the release's decision, never the release).
func TestToolCallIsNotDelayedByARunningIdleHeapRelease(t *testing.T) {
	waitForMemoryReleaseSchedulerIdle(t)
	t.Setenv("GORTEX_DAEMON_MEMRELEASE_MIN_MB", "0")
	t.Setenv("GORTEX_DAEMON_MEMRELEASE_COOLDOWN", "0")

	started := make(chan struct{})
	previous := freeOSMemory
	freeOSMemory = func() {
		close(started)
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { freeOSMemory = previous })

	released := make(chan bool, 1)
	go func() {
		done, _ := releaseIdleMCPHeap(nil, "gate-test")
		released <- done
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle release never started")
	}
	begin := time.Now()
	beginMCPToolCall()
	waited := time.Since(begin)
	runtimeactivity.End("mcp") // not endMCPToolCall: that would schedule another release
	if waited > 20*time.Millisecond {
		t.Fatalf("a tool call waited %v for a running idle heap release", waited)
	}
	select {
	case done := <-released:
		if !done {
			t.Fatal("the release did not report done")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the release never finished")
	}
}
