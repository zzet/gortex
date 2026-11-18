package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Windows path-based FileInfo loads its file ID on the first SameFile call.
// This model makes that delayed path lookup explicit on every test host.
// Capturing the handle's Stat before a replacement must freeze the old ID.
type editProbeLazyFileInfo struct {
	os.FileInfo
	identity func() uint64
}

func (i editProbeLazyFileInfo) Sys() any { return i.identity() }

func TestEditProbeNativeSnapshotPinsIdentityBeforePathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.go")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	boundID := uint64(1)
	lazy := editProbeLazyFileInfo{FileInfo: info, identity: func() uint64 { return boundID }}
	captures := 0
	pinned, err := editProbeNativeSnapshot(lazy, func() (os.FileInfo, error) {
		captures++
		captured := boundID
		return editProbeLazyFileInfo{FileInfo: info, identity: func() uint64 { return captured }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The pathname now belongs to the replacement, before either retained
	// FileInfo is asked for its ID. A lazy path lookup would see the new file.
	boundID = 2
	if pinned.Sys() != uint64(1) || captures != 1 {
		t.Fatalf("before identity followed the replacement path: id=%v captures=%d", pinned.Sys(), captures)
	}
	if lazy.Sys() != uint64(2) {
		t.Fatal("control did not model the rebound pathname")
	}
}

func TestEditProbeNativeSnapshotRejectsUnobservedIdentity(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := editProbeNativeSnapshot(info, func() (os.FileInfo, error) {
		called = true
		return info, nil
	}); err == nil || called {
		t.Fatalf("nonregular target reached capture: called=%v err=%v", called, err)
	}
	path := filepath.Join(t.TempDir(), "fixture.go")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshotErr := errors.New("native identity snapshot or handle close failed")
	if _, err := editProbeNativeSnapshot(info, func() (os.FileInfo, error) { return nil, snapshotErr }); !errors.Is(err, snapshotErr) {
		t.Fatalf("failed identity snapshot was silently accepted: %v", err)
	}
}
