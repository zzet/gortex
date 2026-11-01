//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type queryMicroChild struct {
	done           chan struct{}
	cancel         context.CancelFunc
	pid            int
	stdout, stderr []byte
	err            error
}

func startQueryMicroChild(t *testing.T, mode string) *queryMicroChild {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, queryMicroDiagnosticsKey{}, true)
	child := &queryMicroChild{done: make(chan struct{}), cancel: cancel}
	go func() {
		child.stdout, child.stderr, child.err = executeMicroProcess(ctx, binary,
			[]string{"-test.run=^TestQueryMicroCaptureHelper$", "--", ready}, filepath.Dir(ready),
			append(os.Environ(), "PERF_QUERY_CAPTURE_HELPER="+mode, "GODEBUG=asyncpreemptoff=0,gctrace=0", "GOTRACEBACK=none"))
		close(child.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-child.done:
			return
		case <-time.After(15 * time.Second):
		}
		if child.pid > 0 {
			_ = syscall.Kill(child.pid, syscall.SIGKILL)
		}
		select {
		case <-child.done:
		case <-time.After(5 * time.Second):
			t.Error("query diagnostic child was not joined")
		}
	})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for child.pid == 0 {
		select {
		case <-child.done:
			t.Fatalf("query helper exited before ready: %v\n%s", child.err, child.stderr)
		case <-deadline.C:
			t.Fatal("query helper did not become ready")
		case <-ticker.C:
			data, err := os.ReadFile(ready)
			if err == nil {
				child.pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
		}
	}
	return child
}

func joinQueryMicroChild(t *testing.T, child *queryMicroChild) {
	t.Helper()
	select {
	case <-child.done:
	case <-time.After(15 * time.Second):
		t.Fatal("query child exceeded bounded capture cleanup")
	}
	if !errors.Is(child.err, context.Canceled) {
		t.Fatalf("expired query was not failed evidence: %v", child.err)
	}
	if err := syscall.Kill(child.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("query child remains after join: %v", err)
	}
}

func TestQueryMicroTimeoutRetainsGoStackAndGC(t *testing.T) {
	child := startQueryMicroChild(t, "stack")
	child.cancel()
	joinQueryMicroChild(t, child)
	if !strings.Contains(string(child.stderr), "SIGQUIT") || !strings.Contains(string(child.stderr), "goroutine ") {
		t.Fatalf("query timeout lost Go stack: %s", child.stderr)
	}
	if !strings.Contains(string(child.stderr), "gc ") {
		t.Fatalf("query timeout lost GC evidence: %s", child.stderr)
	}
}

func TestQueryMicroLateZeroExitStillFails(t *testing.T) {
	child := startQueryMicroChild(t, "zero")
	child.cancel()
	joinQueryMicroChild(t, child)
	if !strings.Contains(string(child.stdout), "quit handled; zero exit") {
		t.Fatalf("owned child did not receive timeout stack request: %s", child.stdout)
	}
}

func TestQueryMicroIgnoringQuitHasBoundedFailedExit(t *testing.T) {
	child := startQueryMicroChild(t, "ignore")
	started := time.Now()
	child.cancel()
	joinQueryMicroChild(t, child)
	if elapsed := time.Since(started); elapsed < 9*time.Second || elapsed > 15*time.Second {
		t.Fatalf("query capture grace changed or was unbounded: %s", elapsed)
	}
}

func TestQueryMicroSuccessPreservesDebugSettings(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, queryMicroDiagnosticsKey{}, true)
	stdout, stderr, err := executeMicroProcess(ctx, binary, []string{"-test.run=^TestQueryMicroCaptureHelper$"}, t.TempDir(),
		append(os.Environ(), "PERF_QUERY_CAPTURE_HELPER=success", "GODEBUG=asyncpreemptoff=0,gctrace=0", "GOTRACEBACK=none"))
	if err != nil {
		t.Fatalf("normal query failed: %v\n%s", err, stderr)
	}
	if !strings.Contains(string(stdout), "asyncpreemptoff=0,gctrace=1|all") || !strings.Contains(string(stderr), "gc ") {
		t.Fatalf("query diagnostics lost existing debug settings or GC evidence: %s\n%s", stdout, stderr)
	}
}

func TestQueryMicroCaptureHelper(t *testing.T) {
	mode := os.Getenv("PERF_QUERY_CAPTURE_HELPER")
	if mode == "" {
		return
	}
	signal.Ignore(os.Interrupt)
	if mode == "success" {
		runtime.GC()
		fmt.Println(os.Getenv("GODEBUG") + "|" + os.Getenv("GOTRACEBACK"))
		os.Exit(0)
	}
	if mode == "ignore" {
		signal.Ignore(syscall.SIGQUIT)
	}
	if mode == "zero" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGQUIT)
		go func() {
			<-signals
			fmt.Println("quit handled; zero exit")
			os.Exit(0)
		}()
	}
	runtime.GC()
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Minute)
	}
}
