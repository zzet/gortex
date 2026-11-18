package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func waitShutdownHelperPID(ctx context.Context, path, expected string, readFile func(string) ([]byte, error)) ([]byte, error) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var observed []byte
	for {
		if err := ctx.Err(); err != nil {
			return observed, fmt.Errorf("ready PID %q, want exact owned PID %q: %w", observed, expected, err)
		}
		pid, err := readFile(path)
		observed = pid
		if canceled := ctx.Err(); canceled != nil {
			return observed, fmt.Errorf("ready PID %q, want exact owned PID %q: %w", observed, expected, canceled)
		}
		if err == nil && string(pid) == expected {
			return pid, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return observed, fmt.Errorf("ready PID %q, want exact owned PID %q: %w", observed, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestShutdownHelperReadyWaitsForCompleteOwnedPIDPublication(t *testing.T) {
	for _, initial := range []string{"", "303", "99999"} {
		t.Run(fmt.Sprintf("initial_%q", initial), func(t *testing.T) { testShutdownHelperPIDPublication(t, initial) })
	}
}

func testShutdownHelperPIDPublication(t *testing.T, initial string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "child-ready")
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	const expected = "30386"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	firstRead := make(chan struct{})
	nextRead := make(chan struct{})
	published := make(chan struct{})
	var closePublish sync.Once
	release := func() { closePublish.Do(func() { close(published) }) }
	defer release()
	reads := 0
	read := func(path string) ([]byte, error) {
		reads++
		if reads == 1 {
			value, err := os.ReadFile(path)
			close(firstRead)
			return value, err // Cache the incomplete/foreign observation before publication.
		}
		if reads == 2 {
			close(nextRead)
			select {
			case <-published:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return os.ReadFile(path)
	}
	type result struct {
		pid []byte
		err error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		pid, err := waitShutdownHelperPID(ctx, path, expected, read)
		done <- result{pid, err}
	}()
	defer func() { cancel(); release(); <-finished }()
	select {
	case <-firstRead:
	case <-ctx.Done():
		t.Fatal("readiness waiter never observed the existing incomplete readiness file")
	}
	select {
	case got := <-done:
		t.Fatalf("readiness completed before exact PID publication: pid=%q error=%v", got.pid, got.err)
	case <-nextRead:
	case <-ctx.Done():
		t.Fatal("readiness waiter did not continue after the incomplete readiness observation")
	}
	require.NoError(t, os.WriteFile(path, []byte(expected), 0o600))
	release()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, expected, string(got.pid))
	case <-ctx.Done():
		t.Fatal("complete exact PID publication did not release readiness")
	}
}
