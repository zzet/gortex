package mcp

import (
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

func TestForegroundMCPToolCallActivityNested(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("foreground activity unexpectedly active at test start")
	}

	beginMCPToolCall()
	beginMCPToolCall()
	if !foregroundMCPToolCallActiveForTest() {
		t.Fatal("nested calls were not reported active")
	}
	endMCPToolCall(zap.NewNop(), "activity_test")
	if !foregroundMCPToolCallActiveForTest() {
		t.Fatal("ending one nested call cleared the remaining call")
	}
	endMCPToolCall(zap.NewNop(), "activity_test")
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("balanced calls remained active")
	}
}

func TestForegroundMCPToolCallActivityConcurrent(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("foreground activity unexpectedly active at test start")
	}

	const workers = 32
	started := make(chan struct{}, workers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			beginMCPToolCall()
			defer endMCPToolCall(zap.NewNop(), "activity_test")
			started <- struct{}{}
			<-release
		}()
	}
	for range workers {
		<-started
	}
	if !foregroundMCPToolCallActiveForTest() {
		t.Fatal("concurrent calls were not reported active")
	}
	close(release)
	wg.Wait()
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("concurrent calls remained active after every worker exited")
	}
}

func TestForegroundMCPToolCallActivityHeldUntilWorkerExit(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("foreground activity unexpectedly active at test start")
	}

	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		beginMCPToolCall()
		close(workerStarted)
		<-releaseWorker
		endMCPToolCall(zap.NewNop(), "activity_test")
		close(workerDone)
	}()
	<-workerStarted

	if !foregroundMCPToolCallActiveForTest() {
		t.Fatal("activity cleared while the worker remained blocked")
	}

	close(releaseWorker)
	<-workerDone
	if foregroundMCPToolCallActiveForTest() {
		t.Fatal("activity remained after the worker and end hook completed")
	}
}

func foregroundMCPToolCallActiveForTest() bool {
	return runtimeactivity.Current().ByKind["mcp"] > 0
}
