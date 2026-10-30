//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// A real wrapper subprocess receives SIGTERM addressed only to its PID. Its
// collector subprocess must receive cancellation, finish delayed cleanup and
// be joined before the wrapper exits. No daemon or real benchmark is launched.
func TestReleaseWrapperSIGTERMForwardsAndJoins(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "cleanup-receipt")
	cmd := exec.Command(binary, "-test.run=^TestReleaseSubprocessHelper$", "--", "wrapper", receipt)
	cmd.Env = append(os.Environ(), "PERF_RELEASE_TEST_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case <-done:
			t.Fatalf("wrapper exited before collector readiness: %v", waitErr)
		case <-deadline.C:
			t.Fatal("collector never became ready")
		case <-ticker.C:
			_, err := os.Stat(receipt + ".ready")
			ready = err == nil
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper did not join interrupted collector cleanup")
	}
	if waitErr == nil {
		t.Fatal("interrupted release incorrectly returned success")
	}
	for _, path := range []string{receipt, receipt + ".joined"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("wrapper exited without retained cleanup/join receipt: %s: %v", path, err)
		}
	}
}

func TestReleaseSubprocessHelper(t *testing.T) {
	if os.Getenv("PERF_RELEASE_TEST_HELPER") != "1" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 || len(os.Args) < index+3 {
		os.Exit(2)
	}
	mode, receipt := os.Args[index+1], os.Args[index+2]
	switch mode {
	case "wrapper":
		binary, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		prepare := func(context.Context, io.Writer, io.Writer) (collectorProgram, error) {
			return collectorProgram{binary: binary, prefix: []string{"-test.run=^TestReleaseSubprocessHelper$", "--", "collector", receipt}, cleanup: func() error { return os.WriteFile(receipt+".joined", []byte("joined after collector exit\n"), 0o600) }}, nil
		}
		os.Exit(releaseEntry(nil, os.Stdout, os.Stderr, prepare))
	case "collector":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := os.WriteFile(receipt+".ready", []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(receipt, []byte("owned cleanup complete\n"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}
