//go:build windows

package agents

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// openExclusiveNoDelete opens path with a share mode that omits
// FILE_SHARE_DELETE, reproducing the handle an editor's language server,
// antivirus, a search indexer, or Gortex's own file watcher holds while
// touching a file. MoveFileEx — and therefore os.Rename — cannot replace
// a destination held this way and fails with ERROR_SHARING_VIOLATION.
func openExclusiveNoDelete(t *testing.T, path string) syscall.Handle {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("utf16 %s: %v", path, err)
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ, // deliberately no FILE_SHARE_DELETE / WRITE
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}
	return h
}

// TestAtomicWriteFileRetriesPastSharingViolation is the Windows regression
// test for the "The process cannot access the file because it is being
// used by another process" failures: when the destination is transiently
// held open without FILE_SHARE_DELETE, AtomicWriteFile must retry the
// rename and succeed once the holder releases the handle, instead of
// surfacing the spurious error.
//
// CI's Windows job is build-only (see .github/workflows/ci.yml), so this
// runs locally / on demand, not in the cross-platform test matrix.
func TestAtomicWriteFileRetriesPastSharingViolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "held.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := openExclusiveNoDelete(t, path)

	// Release the handle beyond the old ~225ms retry budget so this pins the
	// longer deadline used by editor, antivirus and indexer handles.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(350 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()

	if err := AtomicWriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("AtomicWriteFile should retry past a transient hold, got: %v", err)
	}
	wg.Wait()

	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("content after retry: got %q want %q", got, "new")
	}
}

// TestAtomicWriteFileFailsWhenHeldThroughout bounds the retry: a
// destination held open for longer than the whole retry budget must make
// AtomicWriteFile give up and return an error — not hang forever — and
// must leave the original file intact.
func TestAtomicWriteFileFailsWhenHeldThroughout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := openExclusiveNoDelete(t, path)
	defer func() { _ = syscall.CloseHandle(h) }()

	if err := AtomicWriteFile(path, []byte("new"), 0o644); err == nil {
		t.Fatal("expected AtomicWriteFile to fail while the file is held open throughout")
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("a failed write must not corrupt the destination: got %q", got)
	}
}
