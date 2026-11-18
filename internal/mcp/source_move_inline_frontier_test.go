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
	"github.com/zzet/gortex/internal/indexer"
)

func TestMoveAndInlineAdmitWholeFrontierBeforeWaiting(t *testing.T) {
	for _, tool := range []string{"move_symbol", "inline_symbol"} {
		t.Run(tool, func(t *testing.T) {
			files := map[string]string{"pkg/a.go": "package pkg\nfunc Foo() int { return 42 }\n", "pkg/b.go": "package pkg\nfunc Bar() int { return 1 }\n"}
			args := map[string]any{"id": "pkg/a.go::Foo", "target_file": "pkg/b.go", "format": "json"}
			if tool == "inline_symbol" {
				files = map[string]string{"pkg/a.go": "package pkg\nfunc GetX(s *S) int { return s.X }\n", "pkg/b.go": "package pkg\nfunc Use() int { s := &S{X: 3}; return GetX(s) }\n", "pkg/types.go": "package pkg\ntype S struct { X int }\n"}
				args = map[string]any{"id": "pkg/a.go::GetX", "format": "json"}
			}
			s, root := setupMoveInlineRepo(t, files)
			s.ToolCallTimeout = -1
			s.mutationReindexWait = time.Minute
			publications := make(chan indexer.MutationResult, 2)
			watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
			s.SetWatcher(watcher)
			paths := []string{filepath.Join(root, "pkg", "a.go"), filepath.Join(root, "pkg", "b.go")}
			waiting := make(chan error, 1)
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
				check, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				_, release, err := acquireMutationPaths(check, paths)
				if err == nil {
					release()
				}
				waiting <- err
				return true
			}
			request := mcplib.CallToolRequest{}
			request.Params.Name, request.Params.Arguments = tool, args
			handler := s.boundToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				if tool == "move_symbol" {
					return s.handleMoveSymbol(ctx, req)
				}
				return s.handleInlineSymbol(ctx, req)
			})
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan struct{})
			complete := sync.OnceFunc(func() {
				for range 2 {
					publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
				}
				close(publications)
			})
			var result *mcplib.CallToolResult
			var resultErr error
			go func() { defer close(finished); result, resultErr = handler(ctx, request) }()
			defer func() {
				cancel()
				complete()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("owned mutation did not join")
				}
			}()
			select {
			case admitted := <-watcher.announced:
				require.ElementsMatch(t, paths, admitted)
			case <-finished:
				t.Fatalf("%s returned without admitting the committed source frontier", tool)
			case <-time.After(3 * time.Second):
				t.Fatal("mutation did not admit its committed source frontier")
			}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotEqual(t, files["pkg/"+filepath.Base(path)], string(raw), "all physical source effects must precede admission")
			}
			require.EqualValues(t, 1, watcher.sets.Load())
			require.Zero(t, watcher.points.Load())
			select {
			case err := <-waiting:
				require.NoError(t, err, "path set must be released before passive graph wait")
			case <-time.After(time.Second):
				t.Fatal("mutation never entered passive wait")
			}
			complete()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("published mutation did not return")
			}
			require.NoError(t, resultErr)
			require.False(t, result.IsError, "%v", result.Content)
			if tool == "move_symbol" {
				response := decodeFileOpsResult(t, result)
				for _, raw := range response["file_observations"].([]any) {
					id := raw.(map[string]any)["receipt"].(string)
					status, ok := s.moveFileObservationPayload(ctx, id)
					require.True(t, ok)
					require.Equal(t, "admitted", status["requested_background_state"])
					require.Equal(t, "unavailable_queued_publication", status["direct_reconcile_state"])
					require.Nil(t, status["direct_reconcile"])
					require.Nil(t, status["read_assisted_reconcile"])
					require.Equal(t, map[string]string{"publish": "applicable", "reindex_wait": "applicable"}, status["acknowledgement_phase_applicability"])
				}
			}
		})
	}
}

func TestInlineSameFileKeepsCallerRewriteWhenDeletingCallee(t *testing.T) {
	s, root := setupMoveInlineRepo(t, map[string]string{
		"pkg/a.go": "package pkg\ntype S struct { X int }\nfunc GetX(s *S) int { return s.X }\nfunc Use() int { s := &S{X: 3}; return GetX(s) }\n",
	})
	result := callTool(t, s, "inline_symbol", map[string]any{"id": "pkg/a.go::GetX", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := os.ReadFile(filepath.Join(root, "pkg", "a.go"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "func GetX")
	require.NotContains(t, string(raw), "return GetX(s)", "deleting the callee must not overwrite the caller rewrite in the same physical file")
	require.Contains(t, string(raw), "return s.X")
}

func TestMovePartialIOFailureStillAdmitsCommittedTarget(t *testing.T) {
	s, root := setupMoveInlineRepo(t, map[string]string{
		"pkg/a.go": "package pkg\nfunc Foo() int { return 42 }\n",
		"pkg/b.go": "package pkg\nfunc Bar() int { return 1 }\n",
	})
	publications := make(chan indexer.MutationResult)
	defer close(publications)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Nanosecond
	source := filepath.Join(root, "pkg", "a.go")
	target := filepath.Join(root, "pkg", "b.go")
	s.mutationPreCommitHook = func(record *mutationCommitRecord) {
		if record.absPath == source {
			// The target write already succeeded. Model a real independent
			// directory replacement that makes the next atomic rename fail.
			require.NoError(t, os.Remove(source))
			require.NoError(t, os.Mkdir(source, 0o755))
		}
	}
	request := mcplib.CallToolRequest{}
	request.Params.Arguments = map[string]any{"id": "pkg/a.go::Foo", "target_file": "pkg/b.go", "format": "json"}
	result, err := s.handleMoveSymbol(t.Context(), request)
	require.NoError(t, err)
	require.True(t, result.IsError)
	select {
	case admitted := <-watcher.announced:
		require.Equal(t, []string{target}, admitted, "already committed source effects retain publication even when the next file fails")
	default:
		t.Fatal("partial failure abandoned the successfully committed target")
	}
	raw, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(raw), "func Foo")
	require.EqualValues(t, 1, watcher.points.Load())
	require.Zero(t, watcher.sets.Load())
}
