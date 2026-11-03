//go:build !windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"
)

func TestSmokeParentCancellationRetainsOwnedClientCleanup(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "owned-cleanup")
	t.Setenv("GORTEX_SMOKE_CLEANUP_HELPER", marker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if scanner.Text() == "SMOKE_CLEANUP_READY" {
				close(ready)
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- runLaneProcess(ctx, binary, []string{"-test.run=^TestSmokeOwnedCleanupHelper$"}, writer)
		_ = writer.Close()
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("owned client ended before cancellation readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("owned client never became ready")
	}
	cancel()
	// Drain the remaining stdout so the subprocess can finish normally.
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost parent cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not join owned cleanup")
	}
	bytes, err := os.ReadFile(marker)
	if err != nil || string(bytes) != "normal owned cleanup" {
		t.Fatalf("client cleanup was skipped or killed: %q %v", bytes, err)
	}
}

func TestSmokeOwnedCleanupHelper(t *testing.T) {
	marker := os.Getenv("GORTEX_SMOKE_CLEANUP_HELPER")
	if marker == "" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	fmt.Fprintln(os.Stdout, "SMOKE_CLEANUP_READY")
	<-ctx.Done()
	if err := os.WriteFile(marker, []byte("normal owned cleanup"), 0o600); err != nil {
		t.Fatal(err)
	}
}
