package store_sqlite

import (
	"os"
	"path/filepath"
	"testing"
)

// Removing a store removes every file that describes it: the database, its
// WAL, shared-memory and journal files, and the close-checkpoint progress and
// rate sidecars.
func TestRemoveStoreFilesRemovesTheCloseSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	files := []string{path, path + "-wal", path + "-shm", path + "-journal", CloseProgressPath(path), closeRatePath(path)}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeStoreFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s survived the store's removal (stat err=%v)", filepath.Base(f), err)
		}
	}
	// Absent files are not an error.
	if err := removeStoreFiles(path); err != nil {
		t.Fatalf("removing an absent store: %v", err)
	}
}
