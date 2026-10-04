//go:build windows

package fsnotify

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Removing a watch cancels its outstanding ReadDirectoryChangesW, but the
// cancellation completes later, and a read that completed with data just before
// the removal is still queued on the completion port. Those packets must not be
// acted on: the watch's handle is already closed (and may have been reissued),
// and the watch must stay reachable until the kernel is done with its buffer.
func TestWindowsTeardownWithQueuedCompletions(t *testing.T) {
	t.Run("remove", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "dir")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		w := newTestWatcher(t)
		errs := collectHandleErrors(w)
		stop := churn(dir)

		for i := 0; i < 300; i++ {
			if err := w.Add(dir); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := w.Remove(dir); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if i%10 == 0 {
				runtime.GC()
			}
		}
		stop()
		closeAndCheck(t, w, errs)
	})

	t.Run("delete watched directory", func(t *testing.T) {
		tmp := t.TempDir()
		w := newTestWatcher(t)
		errs := collectHandleErrors(w)

		for i := 0; i < 50; i++ {
			dir := filepath.Join(tmp, fmt.Sprintf("dir%d", i))
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := w.Add(dir); err != nil {
				t.Fatalf("Add: %v", err)
			}
			stop := churn(dir)
			time.Sleep(time.Millisecond)
			stop()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("RemoveAll: %v", err)
			}
			runtime.GC()
		}
		closeAndCheck(t, w, errs)
	})
}

func newTestWatcher(t *testing.T) *Watcher {
	t.Helper()
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	return w
}

// churn keeps writing files in dir until the returned function is called, so
// completion packets carrying data are queued continuously.
func churn(dir string) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i%16)), []byte("x"), 0o644)
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// collectHandleErrors drains w's channels and records the errors that mean a
// torn-down watch was acted on: its closed handle was cancelled or closed again.
func collectHandleErrors(w *Watcher) <-chan []string {
	out := make(chan []string, 1)
	go func() {
		var bad []string
		events, errors := w.Events, w.Errors
		for events != nil || errors != nil {
			select {
			case _, ok := <-events:
				if !ok {
					events = nil
				}
			case err, ok := <-errors:
				if !ok {
					errors = nil
					continue
				}
				msg := err.Error()
				if strings.Contains(msg, "CancelIo") || strings.Contains(msg, "CloseHandle") ||
					strings.Contains(msg, "handle is invalid") {
					bad = append(bad, msg)
				}
			}
		}
		out <- bad
	}()
	return out
}

func closeAndCheck(t *testing.T, w *Watcher, errs <-chan []string) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return")
	}
	if bad := <-errs; len(bad) > 0 {
		t.Fatalf("a torn-down watch was acted on (%d errors), first: %s", len(bad), bad[0])
	}
	// Close returns after the reader has exited, so its state is safe to read.
	if n := len(w.b.(*readDirChangesW).ops); n != 0 {
		t.Fatalf("%d reads still outstanding after Close", n)
	}
	abandonedOpsMu.Lock()
	abandoned := len(abandonedOps)
	abandonedOpsMu.Unlock()
	if abandoned != 0 {
		t.Fatalf("Close gave up waiting for %d cancelled reads", abandoned)
	}
}
