package store_sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// packageScratch is one directory for fixtures that outlive the test that first
// asked for them. TestMain removes it when the package is done.
var (
	packageScratchOnce sync.Once
	packageScratchDir  string
	packageScratchErr  error
)

func packageScratch(tb testing.TB) string {
	tb.Helper()
	packageScratchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gortex-store-fixtures")
		if err != nil {
			packageScratchErr = err
			return
		}
		packageScratchDir = dir
	})
	if packageScratchErr != nil {
		tb.Fatalf("create the package fixture directory: %v", packageScratchErr)
	}
	return packageScratchDir
}

// copyStoreFile copies a closed store's database and any log it left beside it,
// so the caller opens its own private copy of the same bytes. A destination
// must be fresh: exclusive creation never truncates an existing fixture.
func copyStoreFile(src, dst string) error {
	// Refuse foreign sidecars even when the closed source has none to copy.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(dst + suffix); err == nil {
			return &os.PathError{Op: "copy store", Path: dst + suffix, Err: os.ErrExist}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var created []string
	complete := false
	defer func() {
		if !complete {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		payload, err := os.ReadFile(src + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		file, err := os.OpenFile(dst+suffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		created = append(created, dst+suffix)
		_, writeErr := file.Write(payload)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	complete = true
	return nil
}

// Opening a database file that does not exist yet runs the whole schema — every
// table, index, trigger and FTS shadow of the current schema version — before
// the caller gets a *Store back. Under the race detector that is ~0.7 s of the
// ~0.8 s an average fixture in this package costs, and the package opens
// hundreds of them.
//
// pristineSeedFile builds that empty database exactly once per package run and
// openPristine hands every later caller a byte copy of it. The copy is what
// Open would have produced on a fresh path — same tables, same stamped schema
// version, same page layout — so a fixture that only wants "an empty store"
// keeps the store it asked for and stops paying for the schema again. It costs
// ~0.13 s instead.
//
// Use Open directly (not this helper) whenever the act of creating or upgrading
// the schema is the thing under test: the schema-version, downgrade, migration
// and fresh-open tests all have to watch Open do the work.
var (
	pristineSeedOnce sync.Once
	pristineSeedPath string
	pristineSeedErr  error
)

// pristineSeedFile returns the path of the package's one schema-initialised
// database file, building it on first use.
func pristineSeedFile(tb testing.TB) string {
	tb.Helper()
	scratch := packageScratch(tb)
	pristineSeedOnce.Do(func() {
		seed := filepath.Join(scratch, "pristine", "pristine.sqlite")
		if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
			pristineSeedErr = err
			return
		}
		s, err := Open(seed)
		if err != nil {
			pristineSeedErr = fmt.Errorf("open the pristine seed: %w", err)
			return
		}
		if err := s.Close(); err != nil {
			pristineSeedErr = fmt.Errorf("close the pristine seed: %w", err)
			return
		}
		pristineSeedPath = seed
	})
	if pristineSeedErr != nil {
		tb.Fatalf("build the pristine store seed: %v", pristineSeedErr)
	}
	return pristineSeedPath
}

// openPristine opens an empty store at a fresh path without re-running the
// schema. Existing paths retain Open's reopen behavior. The caller owns the
// returned store exactly as it would own Open's.
func openPristine(tb testing.TB, path string, opts ...Option) (*Store, error) {
	tb.Helper()
	// An in-memory or URI target has no file to seed; it is already cheap.
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return Open(path, opts...)
	}
	// Lstat also recognizes symbolic links, which Open must resolve itself.
	if _, err := os.Lstat(path); err == nil {
		return Open(path, opts...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := copyStoreFile(pristineSeedFile(tb), path); err != nil {
		return nil, err
	}
	return Open(path, opts...)
}

func TestOpenPristinePreservesReopenedDataAndPrivateCopies(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "first.sqlite")
	first, err := openPristine(t, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	first.AddBatch([]*graph.Node{{ID: "fixture/node", Name: "Written", Kind: graph.KindType}}, nil)
	if got := first.GetNode("fixture/node"); got == nil || got.Name != "Written" {
		_ = first.Close()
		t.Fatalf("write private fixture node: %+v", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openPristine(t, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.GetNode("fixture/node"); got == nil || got.Name != "Written" {
		t.Fatalf("reopen replaced populated fixture with the empty seed: %+v", got)
	}

	second, err := openPristine(t, filepath.Join(t.TempDir(), "second.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if got := second.NodeCount(); got != 0 {
		t.Fatalf("a fresh private copy inherited another fixture's %d nodes", got)
	}
	if got := reopened.GetNode("fixture/node"); got == nil || got.Name != "Written" {
		t.Fatalf("opening another copy changed the populated fixture: %+v", got)
	}
}

func TestCopyStoreFileRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	if err := os.WriteFile(src, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyStoreFile(src, dst); !os.IsExist(err) {
		t.Fatalf("copy over existing fixture returned %v, want an existence error", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "preserved" {
		t.Fatalf("existing fixture changed: %q, %v", got, err)
	}
	// A sidecar conflict must preserve every pre-existing file.
	if err := os.WriteFile(src+"-wal", []byte("seed log"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dir, "partial")
	if err := os.WriteFile(partial+"-wal", []byte("preserved log"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyStoreFile(src, partial); !os.IsExist(err) {
		t.Fatalf("copy over an existing sidecar returned %v", err)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("failed copy left a partial database: %v", err)
	}
	got, err = os.ReadFile(partial + "-wal")
	if err != nil || string(got) != "preserved log" {
		t.Fatalf("failed copy changed an existing sidecar: %q, %v", got, err)
	}
	if err := copyStoreFile(filepath.Join(dir, "missing"), filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Fatalf("missing seed returned %v, want a missing-file error", err)
	}
}

func TestCopyStoreFileRefusesOrphanDestinationSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
			if err := os.WriteFile(src, []byte("seed without sidecars"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst+suffix, []byte("foreign sidecar"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := copyStoreFile(src, dst); !os.IsExist(err) {
				t.Fatalf("foreign sidecar returned %v, want an existence error", err)
			}
			if _, err := os.Lstat(dst); !os.IsNotExist(err) {
				t.Fatalf("copy created a database beside a foreign sidecar: %v", err)
			}
			got, err := os.ReadFile(dst + suffix)
			if err != nil || string(got) != "foreign sidecar" {
				t.Fatalf("copy changed a foreign sidecar: %q, %v", got, err)
			}
		})
	}
}

func TestCopyStoreFileRemovesPartialCopyOnSourceReadFailure(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	if err := os.WriteFile(src, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory cannot be read as a sidecar file on any supported platform.
	if err := os.Mkdir(src+"-wal", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyStoreFile(src, dst); err == nil {
		t.Fatal("copy accepted an unreadable source sidecar")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("failed copy left a partial database: %v", err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if packageScratchDir != "" {
		_ = os.RemoveAll(packageScratchDir)
	}
	os.Exit(code)
}
