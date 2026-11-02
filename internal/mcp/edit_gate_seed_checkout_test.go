//go:build gortex_edit_gate_seed

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestEditGateACKSeedReachesActualFacadeCheckoutWrite(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "ack-wait")
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	after := strings.Replace(string(before), "func helper() {}", "func helper() { println(1) }", 1)
	require.NotEqual(t, string(before), after)
	sum := sha256.Sum256([]byte(after))
	results := make(chan indexer.MutationResult, 1)
	results <- indexer.MutationResult{Reindexed: true}
	close(results)
	mutation := &receiptCheckoutMutation{ticket: &indexer.CheckoutRefreshTicket{CheckoutID: "checkout-seed", Incarnation: "incarnation-seed", Root: dir, RepoPrefix: "fixture", ContentHash: hex.EncodeToString(sum[:]), Ticket: &indexer.MutationTicket{Path: path, Generation: 7, Done: results}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = withCheckoutMutation(WithSessionID(ctx, "seed-facade"), mutation, dir)
	// Negotiate and dispatch the actual facade shape used by the load harness.
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-harness","version":"1.0"}}}`)))
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "edit", "arguments": map[string]any{"operation": "file", "target": map[string]any{"file": "main.go"}, "match": "func helper() {}", "replacement": "func helper() { println(1) }", "output": map[string]any{"format": "json"}}}})
	require.NoError(t, err)
	entered := make(chan struct{})
	editGateSeedACKEntered = func() { close(entered) }
	t.Cleanup(func() { editGateSeedACKEntered = nil })
	done := make(chan any, 1)
	go func() { done <- srv.MCPServer().HandleMessage(ctx, frame) }()
	select {
	case <-entered:
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled checkout seed did not join handler")
		}
	case reply := <-done:
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		t.Fatalf("facade checkout returned before seed; committed=%t reply=%+v", strings.Contains(string(raw), "println(1)"), reply)
	case <-ctx.Done():
		t.Fatal("checkout write neither admitted seed nor returned")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, string(raw))
}
