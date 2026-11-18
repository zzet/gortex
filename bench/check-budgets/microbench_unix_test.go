//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMicroProcessIgnoringInterruptHasBoundedFailedExit(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var runErr error
	go func() {
		_, _, runErr = executeMicroProcess(ctx, binary, []string{"-test.run=^TestMicroProcessHelper$", "--", ready}, filepath.Dir(ready), append(os.Environ(), "PERF_MICRO_TEST_HELPER=1"))
		close(done)
	}()
	pid := 0
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			return
		default:
		}
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("microbenchmark child did not join after cleanup")
		}
	})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for pid == 0 {
		select {
		case <-done:
			t.Fatalf("helper exited early: %v", runErr)
		case <-deadline.C:
			t.Fatal("helper did not register Interrupt refusal")
		case <-ticker.C:
			data, err := os.ReadFile(ready)
			if err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
		}
	}
	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ignored Interrupt bypassed microbenchmark timeout")
	}
	var exit *exec.ExitError
	if runErr == nil || !errors.Is(runErr, context.Canceled) || !errors.As(runErr, &exit) {
		t.Fatalf("forced/late exit was not retained as failed evidence: %v", runErr)
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("unexpected cleanup bound: %s", elapsed)
	}
}

func TestMicroProcessHelper(t *testing.T) {
	if os.Getenv("PERF_MICRO_TEST_HELPER") != "1" {
		return
	}
	signal.Ignore(os.Interrupt)
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Minute)
	}
}
