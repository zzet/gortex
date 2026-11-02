//go:build acceptance

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
)

func TestIssue767GracefulStopRejectsForcedSuccessfulExit(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		wantError    bool
	}{
		{"graceful", "daemon stopped", nil, false},
		{"already_down", "daemon is not running", nil, false},
		{"draining_advisory", "draining 2 WAL frames, up to 15s before force-killing", nil, false},
		{"forced_exit_zero", "graceful shutdown timed out — force-killing", nil, true},
		{"stalled_close_exit_zero", "close made no progress for 1m — force-killing", nil, true},
		{"pid_fallback_exit_zero", "stopped (force-killed)", nil, true},
		{"bounded_cancel", "partial output", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := issue767GracefulStopResult([]byte(tc.output), tc.err); (err != nil) != tc.wantError {
				t.Fatalf("stop result: %v; wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestIssue767CleanupCancellationRequiresSuccessfulProcessState(t *testing.T) {
	// Use actual native child exit states rather than inventing an exit code.
	// Git is already a prerequisite for this acceptance fixture.
	states := make(map[string]*os.ProcessState)
	for name, argument := range map[string]string{"zero": "--version", "nonzero": "--issue767-intentional-failure"} {
		cmd := exec.Command("git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", argument)
		_ = cmd.Run()
		if cmd.ProcessState == nil {
			t.Fatal("fixture child did not start")
		}
		states[name] = cmd.ProcessState
	}
	for _, tc := range []struct {
		name, state string
		err         error
		wantClean   bool
	}{
		{"normal", "zero", nil, true},
		{"cleanup_canceled_zero", "zero", context.Canceled, true},
		{"deadline_graceful_zero", "zero", context.DeadlineExceeded, true},
		{"canceled_nonzero", "nonzero", context.Canceled, false},
		{"nonzero", "nonzero", errors.New("exit failed"), false},
		{"other_wait_error", "zero", errors.New("pipe failure"), false},
		{"missing_state", "missing", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if clean := issue767CleanChildExit(tc.err, states[tc.state]); clean != tc.wantClean {
				t.Fatalf("clean=%t; want=%t", clean, tc.wantClean)
			}
		})
	}
}

func TestIssue767StopOwnerSerializesOverlappingRequests(t *testing.T) {
	var requests atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	stop := issue767GracefulStopOwner(func() error {
		if requests.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	})
	done := make(chan error, 2)
	go func() { done <- stop() }()
	<-entered
	// Keep the first request in flight until the second caller is launched.
	// Counting after both complete needs no scheduler or timing threshold.
	secondStarted := make(chan struct{})
	go func() { close(secondStarted); done <- stop() }()
	<-secondStarted
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("graceful-stop requests = %d, want one per child", got)
	}
}

func TestIssue767StopOwnerPreservesFirstFailure(t *testing.T) {
	failure := errors.New("first graceful stop failed")
	var requests int
	stop := issue767GracefulStopOwner(func() error {
		requests++
		if requests == 1 {
			return failure
		}
		return nil
	})
	for range 2 {
		if err := stop(); err != failure {
			t.Errorf("stop error = %v, want original failure", err)
		}
	}
	if requests != 1 {
		t.Errorf("stop requests = %d, want one", requests)
	}
}

func TestIssue767StopOwnerIsPerChild(t *testing.T) {
	var requests [2]int
	for i := range requests {
		stop := issue767GracefulStopOwner(func() error { requests[i]++; return nil })
		for range 2 {
			if err := stop(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if requests != [2]int{1, 1} {
		t.Fatalf("requests per child = %v, want [1 1]", requests)
	}
}

func TestIssue767CleanupUsesCapturedStopOwner(t *testing.T) {
	// A real completed native child gives stop its actual successful exit
	// state. No daemon is launched and no successful exit is invented.
	cmd := exec.CommandContext(context.Background(), "git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "--version")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	var requests int
	cmd.Cancel = issue767GracefulStopOwner(func() error { requests++; return nil })
	done := make(chan error, 1)
	done <- nil
	fixture := &issue767Fixture{t: t, cmd: cmd, done: done}
	fixture.stop()
	if requests != 1 {
		t.Fatalf("cleanup used captured stop owner %d times, want one", requests)
	}
}
