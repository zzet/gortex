package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

func ackModeServer(t *testing.T, mode string) (*Server, string) {
	t.Helper()
	s, root := setupTestServer(t)
	file := filepath.Join(t.TempDir(), "config.yaml")
	body := ""
	if mode != "" {
		body = "edit:\n  ack: " + mode + "\n"
	}
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	cm, err := config.NewConfigManager(file)
	require.NoError(t, err)
	s = NewServer(s.engine, s.graph, s.indexer, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: cm})
	return s, root
}

func TestEditAckLocalConfigurationReturnsBeforeQueuedPublication(t *testing.T) {
	s, root := ackModeServer(t, "local")
	published := make(chan indexer.MutationResult, 1)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	complete := sync.OnceFunc(func() { published <- indexer.MutationResult{Reindexed: true}; close(published) })
	joined := make(chan struct{})
	var result *mcplib.CallToolResult
	var resultErr error
	request := editFileRequest(map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(1) }", "format": "json"})
	go func() { defer close(joined); result, resultErr = s.runAcknowledgedEdit(ctx, request, s.handleEditFile) }()
	defer func() {
		cancel()
		complete()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned local ACK did not join")
		}
	}()
	select {
	case <-watcher.announced:
	case <-time.After(3 * time.Second):
		t.Fatal("local ACK did not admit physical write")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("explicit edit.ack=local waited for unpublished background work")
	}
	require.NoError(t, resultErr)
	require.False(t, result.IsError, "%v", result.Content)
	text := toolResultText(result)
	require.Contains(t, text, `"ack_mode":"local"`)
	require.Contains(t, text, `"reindex_pending":true`)
	require.NotContains(t, text, `"graph_status":"fresh"`)
}

func TestEditAckDefaultAndExplicitFullWaitForActualPublication(t *testing.T) {
	for _, mode := range []string{"", "full"} {
		t.Run("mode="+mode, func(t *testing.T) {
			s, root := ackModeServer(t, mode)
			published := make(chan indexer.MutationResult, 1)
			s.SetWatcher(mutationTestWatcher{done: published})
			s.mutationReindexWait = time.Minute
			waiting := make(chan struct{}, 1)
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool { waiting <- struct{}{}; return true }
			complete := sync.OnceFunc(func() { published <- indexer.MutationResult{Reindexed: true}; close(published) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			joined := make(chan struct{})
			var result *mcplib.CallToolResult
			var resultErr error
			request := editFileRequest(map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(1) }", "format": "json"})
			go func() { defer close(joined); result, resultErr = s.runAcknowledgedEdit(ctx, request, s.handleEditFile) }()
			defer func() {
				cancel()
				complete()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("owned full ACK did not join")
				}
			}()
			select {
			case <-waiting:
			case <-time.After(3 * time.Second):
				t.Fatal("full ACK did not enter publication wait")
			}
			select {
			case <-joined:
				t.Fatal("full ACK claimed completion before actual publication")
			default:
			}
			complete()
			select {
			case <-joined:
			case <-time.After(3 * time.Second):
				t.Fatal("published full ACK did not return")
			}
			require.NoError(t, resultErr)
			require.False(t, result.IsError, "%v", result.Content)
			require.Contains(t, toolResultText(result), `"ack_mode":"full"`)
		})
	}
}
