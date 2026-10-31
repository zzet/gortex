package serverstack

import (
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestMemoryBackendRoundTrips asserts the memory-resident store
// (odd/tasks/tier3-memory-store.md, issue davalillo/gortex#19) opens, mutates,
// and restores state across close + reopen: the close dumps a sqlite snapshot
// plus journal, and the reopen replays them into an equivalent graph. The
// JSON-excluded fields (Meta, Confidence) must survive the round trip.
//
// It drives openMemoryBackend directly because the CLI name stays gated until
// the daemon wiring lands (zzet/gortex#902); TestOpenBackend_RejectsMemory…
// pins that gate.
func TestMemoryBackendRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.sqlite")

	store, closeFn, err := openMemoryBackend(path, zap.NewNop())
	if err != nil {
		t.Fatalf("openMemoryBackend: %v", err)
	}
	node := &graph.Node{
		ID: "gortex/internal/x.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "internal/x.go", Language: "go", RepoPrefix: "gortex",
	}
	edge := &graph.Edge{
		From: node.ID, To: "gortex/internal/y.go::Bar", Kind: graph.EdgeCalls,
		FilePath: "internal/x.go", Line: 1,
		Meta: map[string]any{"semantic_source": "lsp"}, Confidence: 0.9,
	}
	store.AddNode(node)
	store.AddEdge(edge)
	if store.GetNode(node.ID) == nil {
		t.Fatal("node missing before close")
	}
	closeFn()

	reopened, closeReopened, err := openMemoryBackend(path, zap.NewNop())
	if err != nil {
		t.Fatalf("openMemoryBackend reopen: %v", err)
	}
	defer closeReopened()
	got := reopened.GetNode(node.ID)
	if got == nil {
		t.Fatal("node missing after reopen (snapshot+journal replay failed)")
	}
	if got.Name != "Foo" || got.FilePath != "internal/x.go" {
		t.Errorf("node fields lost in round trip: %+v", got)
	}
	out := reopened.GetOutEdges(node.ID)
	if len(out) != 1 {
		t.Fatalf("edges after reopen: want 1, got %d", len(out))
	}
	if out[0].To != edge.To {
		t.Errorf("edge To lost: got %q", out[0].To)
	}
	if out[0].Meta["semantic_source"] != "lsp" {
		t.Errorf("edge Meta lost in round trip: %+v", out[0].Meta)
	}
	if out[0].Confidence != 0.9 {
		t.Errorf("edge Confidence lost in round trip: got %v", out[0].Confidence)
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

// TestOpenBackend_RejectsMemoryUntilDaemonWiring pins review zzet/gortex#901
// R2: the memory name must not be selectable while the daemon still needs its
// SQLite-only runtimes (contract analysis, checkout-lifecycle views, the base
// runtime, edit deltas, shadow-drain retirement). The rejection is explicit
// and actionable — never a silent fallback to sqlite, which would run a
// different engine than the one the caller asked for.
func TestOpenBackend_RejectsMemoryUntilDaemonWiring(t *testing.T) {
	for _, name := range []string{"memory", "mem", "in-memory", "In-Memory"} {
		store, cleanup, err := OpenBackend(name, filepath.Join(t.TempDir(), "store.sqlite"), zap.NewNop(), false)
		if err == nil {
			t.Fatalf("OpenBackend(%q) must be rejected until the daemon wiring lands (zzet/gortex#902); got store %T", name, store)
		}
		if store != nil || cleanup != nil {
			t.Fatalf("OpenBackend(%q) returned a store alongside its rejection", name)
		}
		msg := err.Error()
		// The message must name the aliases too: a caller who typed `mem`
		// still has to learn the canonical backend it was rejected under.
		for _, want := range []string{"not selectable", "memory", "mem", "in-memory", "#902", "--backend sqlite"} {
			if !strings.Contains(msg, want) {
				t.Errorf("OpenBackend(%q) error %q must mention %q", name, msg, want)
			}
		}
	}
}

// TestOpenBackend_Unknown asserts a stale backend name (e.g. the removed
// ladybug) errors rather than silently falling back.
func TestOpenBackend_Unknown(t *testing.T) {
	if _, _, err := OpenBackend("ladybug", "", zap.NewNop(), false); err == nil {
		t.Fatal("an unknown backend must error")
	}
}
