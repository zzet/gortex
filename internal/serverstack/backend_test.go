package serverstack

import (
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestOpenBackend_MemoryRoundTrips asserts the re-introduced memory backend
// (odd/tasks/tier3-memory-store.md, issue davalillo/gortex#19) opens, mutates,
// and restores state across close + reopen: the close dumps a sqlite snapshot
// plus journal, and the reopen replays them into an equivalent graph. The
// JSON-excluded fields (Meta, Confidence) must survive the round trip.
func TestOpenBackend_MemoryRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.sqlite")

	store, closeFn, err := OpenBackend("memory", path, zap.NewNop(), false)
	if err != nil {
		t.Fatalf("OpenBackend(memory): %v", err)
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

	reopened, closeReopened, err := OpenBackend("In-Memory", path, zap.NewNop(), false)
	if err != nil {
		t.Fatalf("OpenBackend(In-Memory) reopen: %v", err)
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
	cleanup()
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
	cleanup()
}

// TestOpenBackend_Unknown asserts a stale backend name (e.g. the removed
// ladybug) errors rather than silently falling back.
func TestOpenBackend_Unknown(t *testing.T) {
	if _, _, err := OpenBackend("ladybug", "", zap.NewNop(), false); err == nil {
		t.Fatal("an unknown backend must error")
	}
}
