package serverstack

import (
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestOpenBackend_MemoryIsRetired asserts the in-memory backend names are
// refused rather than quietly served by the shared on-disk store: a caller
// who asked for a throwaway graph must not end up writing the daemon's
// database. The error has to name the replacement.
func TestOpenBackend_MemoryIsRetired(t *testing.T) {
	for _, name := range []string{"memory", "mem", "in-memory", "In-Memory", "  memory  "} {
		store, _, err := OpenBackend(name, "", zap.NewNop(), false)
		if err == nil {
			t.Fatalf("OpenBackend(%q): want an error, got a store", name)
		}
		if store != nil {
			t.Errorf("OpenBackend(%q): store must be nil on error, got %T", name, store)
		}
		msg := err.Error()
		for _, want := range []string{"retired", "--backend sqlite", "--backend-path"} {
			if !strings.Contains(msg, want) {
				t.Errorf("OpenBackend(%q): error %q should mention %q", name, msg, want)
			}
		}
	}
}

// TestOpenBackend_EmptyNameOpensSqlite asserts the default (empty) backend
// name still resolves — the one-shot and daemon flows pass it whenever the
// caller did not name a backend explicitly.
func TestOpenBackend_EmptyNameOpensSqlite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, cleanup, err := OpenBackend("", path, zap.NewNop(), true)
	if err != nil {
		t.Fatalf(`OpenBackend(""): %v`, err)
	}
	if store == nil {
		t.Fatal("nil store for the default backend")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("close sqlite backend: %v", err)
	}
}

// TestOpenBackend_SqliteOpensFile asserts the sqlite backend opens (and
// creates) a store at the resolved path.
func TestOpenBackend_SqliteOpensFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, cleanup, err := OpenBackend("sqlite", path, zap.NewNop(), true)
	if err != nil {
		t.Fatalf("OpenBackend(sqlite): %v", err)
	}
	if store == nil {
		t.Fatal("nil sqlite store")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("close sqlite backend: %v", err)
	}
}

// TestOpenBackend_Unknown asserts a stale backend name (e.g. the removed
// ladybug) errors rather than silently falling back.
func TestOpenBackend_Unknown(t *testing.T) {
	if _, _, err := OpenBackend("ladybug", "", zap.NewNop(), false); err == nil {
		t.Fatal("an unknown backend must error")
	}
}
