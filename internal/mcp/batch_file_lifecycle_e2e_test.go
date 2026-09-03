package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// TestAtomicBatchLifecycleRefreshesRealGraph drives move_file and delete_file
// through the real indexer rather than the mutation watcher stub. Every other
// lifecycle test proves what lands on disk; this one proves the graph follows —
// that a moved file's symbols are reachable under their new path and a deleted
// file's symbols are gone, which is what an agent's next query depends on.
func TestAtomicBatchLifecycleRefreshesRealGraph(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	srv, dir := setupTestServer(t)
	extra := filepath.Join(dir, "extra.go")
	if err := os.WriteFile(extra, []byte("package main\n\nfunc extraFn() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.indexer.IncrementalReindexPaths(dir, []string{extra}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	engine := srv.engineFor(ctx)
	if engine.GetSymbol("main.go::helper") == nil || engine.GetSymbol("extra.go::extraFn") == nil {
		t.Fatal("fixture symbols are not indexed before the batch")
	}

	source := filepath.Join(dir, "main.go")
	destination := filepath.Join(dir, "moved", "main.go")
	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{"op": "move_file", "source": source, "destination": destination},
		map[string]any{"op": "delete_file", "path": extra},
	}})
	if res.IsError {
		t.Fatalf("batch failed: %s", readText(t, res))
	}
	var receipt batchTransactionReceipt
	if err := json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || receipt.GraphStatus != "fresh" {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, result := range receipt.Results {
		if !result.Reindexed {
			t.Fatalf("result %+v was not reindexed", result)
		}
	}

	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists or stat failed: %v", err)
	}
	if got := readAtomicBatchFixture(t, destination); got == "" {
		t.Fatal("destination is empty")
	}
	if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists or stat failed: %v", err)
	}

	engine = srv.engineFor(ctx)
	if engine.GetSymbol("main.go::helper") != nil {
		t.Error("the graph still resolves the moved file at its old path")
	}
	if engine.GetSymbol("moved/main.go::helper") == nil {
		t.Error("the graph does not resolve the moved file at its new path")
	}
	if engine.GetSymbol("extra.go::extraFn") != nil {
		t.Error("the graph still resolves a symbol from the deleted file")
	}
}
