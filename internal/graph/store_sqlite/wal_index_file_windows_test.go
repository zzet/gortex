//go:build windows

package store_sqlite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"golang.org/x/sys/windows"
)

func TestWALIndexHeaderWindowsReleasesObservationHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "header.sqlite")
	for _, header := range [][]byte{[]byte("first-header"), []byte("replacement!")} {
		if err := os.WriteFile(path+"-shm", header, 0o600); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(header))
		if err := readWALIndexHeader(path, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, header) {
			t.Fatalf("header = %q, want %q", got, header)
		}
		// A retained os.Open descriptor makes this fail with a Windows sharing
		// violation and also prevents replacing the header at the same path.
		if err := os.Remove(path + "-shm"); err != nil {
			t.Fatalf("observation retained a deletion-blocking handle: %v", err)
		}
	}
}

func TestWALIndexHeaderWindowsKeepsOtherHandleLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.sqlite")
	if err := os.WriteFile(path+"-shm", make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	keeper, err := os.OpenFile(path+"-shm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keeper.Close() }()
	region := windows.Overlapped{Offset: 128}
	if err := windows.LockFileEx(windows.Handle(keeper.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.UnlockFileEx(windows.Handle(keeper.Fd()), 0, 1, 0, &region); err != nil {
			t.Error(err)
		}
	}()
	if err := readWALIndexHeader(path, make([]byte, 48)); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(path+"-shm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	err = windows.LockFileEx(windows.Handle(other.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("observation released another handle's byte-range lock: got %v", err)
	}
}

func TestStoreCloseWindowsReleasesWALIndexObservationHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AddBatchChecked([]*graph.Node{{ID: "probe", Kind: graph.KindFunction, Name: "Probe"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := readWALIndexHeader(path, make([]byte, 48)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("closed store retained a WAL-index handle: %v", err)
	}
}
