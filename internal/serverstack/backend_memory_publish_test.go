package serverstack

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// TestMemorySnapshotClearsStaleSqliteSidecars pins review zzet/gortex#901
// minor point 3: the snapshot is renamed over the file the sqlite backend
// uses, so a -wal/-shm left by a crashed sqlite session would be replayed
// against the brand-new database. The publish path removes both before the
// rename.
func TestMemorySnapshotClearsStaleSqliteSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, closeFn, err := openMemoryBackend(path, nil)
	if err != nil {
		t.Fatalf("openMemoryBackend: %v", err)
	}
	store.AddNode(&graph.Node{ID: "r/x.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "x.go", Language: "go", RepoPrefix: "r"})

	// A crashed sqlite session leaves its WAL and shared-memory sidecar
	// beside the database it wrote.
	for _, stale := range []string{path + "-wal", path + "-shm"} {
		if err := os.WriteFile(stale, []byte("stale-sqlite-sidecar"), 0o600); err != nil {
			t.Fatalf("seed stale sidecar %s: %v", stale, err)
		}
	}
	closeFn()

	for _, stale := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale sidecar %s survived the snapshot publish (err=%v)", stale, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("published snapshot missing: %v", err)
	}
}

// TestOpenBackendWarnsOnMemoryPublishedStore pins the declared engine switch
// (review zzet/gortex#901 minor point 3, option iii): a store the memory
// backend published carries nodes/edges but no SQLite generation state, so a
// sqlite-mode boot is a full reindex. It must say so, and the data must stay
// readable.
func TestOpenBackendWarnsOnMemoryPublishedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	mstore, closeFn, err := openMemoryBackend(path, nil)
	if err != nil {
		t.Fatalf("openMemoryBackend: %v", err)
	}
	mstore.AddNode(&graph.Node{ID: "r/x.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "x.go", Language: "go", RepoPrefix: "r"})
	closeFn()

	core, observed := observer.New(zap.WarnLevel)
	store, cleanup, err := OpenBackend("sqlite", path, zap.New(core), false)
	if err != nil {
		t.Fatalf("OpenBackend(sqlite) on a memory-published store: %v", err)
	}
	defer cleanup()

	if store.GetNode("r/x.go::Foo") == nil {
		t.Fatal("node from the memory snapshot must be readable in sqlite mode")
	}
	entries := observed.FilterMessageSnippet("published by the memory backend").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one provenance warning, got %d: %v", len(entries), observed.All())
	}
	if got := entries[0].ContextMap()["nodes"]; got != int64(1) {
		t.Errorf("warning nodes field = %v, want 1", got)
	}
}

// TestOpenBackendNoProvenanceWarningOnFreshStore is the false-positive guard:
// an empty sqlite store is not "memory published", so it must not warn.
func TestOpenBackendNoProvenanceWarningOnFreshStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	core, observed := observer.New(zap.WarnLevel)
	store, cleanup, err := OpenBackend("sqlite", path, zap.New(core), true)
	if err != nil {
		t.Fatalf("OpenBackend(sqlite) on a fresh store: %v", err)
	}
	defer cleanup()
	if store == nil {
		t.Fatal("nil store")
	}
	if entries := observed.FilterMessageSnippet("published by the memory backend").All(); len(entries) != 0 {
		t.Fatalf("fresh store must not carry the provenance warning: %v", entries)
	}
}
