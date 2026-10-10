//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func openBatchTargetNoDelete(t *testing.T, path string) syscall.Handle {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("utf16 %s: %v", path, err)
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}
	return h
}

// TestDurableAtomicWriteFileRetriesPastLongSharingViolation pins the batch
// commit path. Unlike the ordinary editor writer, this used to call os.Rename
// directly, so every sharing violation aborted the whole transaction and forced
// rollback even when the holder released within a few hundred milliseconds.
func TestDurableAtomicWriteFileRetriesPastLongSharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := openBatchTargetNoDelete(t, path)
	var released sync.WaitGroup
	released.Add(1)
	go func() {
		defer released.Done()
		time.Sleep(350 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()

	if err := durableAtomicWriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("durableAtomicWriteFile should retry past a transient hold: %v", err)
	}
	released.Wait()
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("content after retry = %q, err=%v", got, err)
	}
}

// TestBatchDurabilityRemoveRetriesPastLongSharingViolation verifies that the
// transaction's default delete operation inherits the longer shared retry
// budget. This is the path used by batch delete and source removal during a
// failed move rollback.
func TestBatchDurabilityRemoveRetriesPastLongSharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := openBatchTargetNoDelete(t, path)
	var released sync.WaitGroup
	released.Add(1)
	go func() {
		defer released.Done()
		time.Sleep(350 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()

	if err := new(Server).batchDurability().removeFile(path); err != nil {
		t.Fatalf("batch remove should retry past a transient hold: %v", err)
	}
	released.Wait()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("held file survived batch remove: %v", err)
	}
}
