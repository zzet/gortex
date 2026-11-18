//go:build gortex_edit_gate_seed

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestEditGateACKSeedRunsInsideActualAdmittedWrite(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "ack-wait")
	srv, dir := setupTestServer(t)
	published := make(chan indexer.MutationResult, 1)
	published <- indexer.MutationResult{Reindexed: true}
	close(published)
	srv.SetWatcher(mutationTestWatcher{done: published})
	srv.mutationReindexWait = time.Nanosecond
	entered := make(chan struct{})
	editGateSeedACKEntered = func() { close(entered) }
	t.Cleanup(func() { editGateSeedACKEntered = nil })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": filepath.Join(dir, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(1) }", "format": "json"}
	done := make(chan error, 1)
	go func() { _, err := srv.runAcknowledgedEdit(ctx, req, srv.handleEditFile); done <- err }()
	select {
	case <-entered:
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled seed did not join the actual write handler")
		}
	case err := <-done:
		raw, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
		require.NoError(t, readErr)
		t.Fatalf("actual disk write returned before test seed admission: committed=%t error=%v", strings.Contains(string(raw), "println(1)"), err)
	case <-ctx.Done():
		t.Fatal("neither actual write nor seed admitted")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "println(1)", "seed cancellation must not undo the preceding physical commit")
}
