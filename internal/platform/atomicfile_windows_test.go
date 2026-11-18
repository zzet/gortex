//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func openExclusiveNoDelete(t *testing.T, path string) syscall.Handle {
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

// TestRemoveFileRetriesPastLongSharingViolation pins the delete half of the
// shared policy. Batch delete/move used os.Remove directly, so a reader that
// held the destination for longer than the old 60ms platform retry failed the
// whole transaction even though the handle was released shortly afterwards.
func TestRemoveFileRetriesPastLongSharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openExclusiveNoDelete(t, path)
	var released sync.WaitGroup
	released.Add(1)
	go func() {
		defer released.Done()
		time.Sleep(350 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()

	if err := RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile should retry past a transient hold: %v", err)
	}
	released.Wait()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("held file survived RemoveFile: %v", err)
	}
}

func TestTransientSharingErrorIncludesMappedFile(t *testing.T) {
	if !transientSharingError(&os.PathError{Op: "rename", Path: "held.txt", Err: windows.ERROR_USER_MAPPED_FILE}) {
		t.Fatal("ERROR_USER_MAPPED_FILE should be retried")
	}
	if transientSharingError(&os.PathError{Op: "rename", Path: "missing.txt", Err: windows.ERROR_FILE_NOT_FOUND}) {
		t.Fatal("ERROR_FILE_NOT_FOUND should not be retried")
	}
}
