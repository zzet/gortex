//go:build gortex_edit_gate_seed

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestEditGateACKSeedCoversWholeFrontierOnceAfterUnlock(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "ack-wait")
	s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	published := make(chan indexer.MutationResult, 2)
	for range 2 {
		published <- indexer.MutationResult{Reindexed: true}
	}
	close(published)
	s.SetWatcher(&renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	paths := []string{filepath.Join(root, "caller.go"), filepath.Join(root, "target.go")}
	entered := make(chan error, 1)
	var calls atomic.Int32
	editGateSeedACKEntered = func() {
		calls.Add(1)
		check, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, release, err := acquireMutationPaths(check, paths)
		stop()
		if release != nil {
			release()
		}
		select {
		case entered <- err:
		default:
		}
	}
	t.Cleanup(func() { editGateSeedACKEntered = nil })
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"}
	joined := make(chan struct{})
	go func() { defer close(joined); _, _ = s.runAcknowledgedEdit(ctx, request, s.handleRenameSymbol) }()
	defer func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned frontier seed did not join")
		}
	}()
	select {
	case err := <-entered:
		require.NoError(t, err, "tag-only ACK seed retained physical path locks")
	case <-joined:
		t.Fatal("completed multi-file source frontier returned before ACK seed")
	case <-time.After(3 * time.Second):
		t.Fatal("multi-file source frontier never reached ACK seed")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(raw), "Renamed", "seed must follow every physical source effect")
	}
	cancel()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled frontier seed did not join")
	}
	require.EqualValues(t, 1, calls.Load(), "one operation cannot acquire a seed wait per file")
}
