package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Exercise registered middleware/facade lowering through the same HandleMessage
// entry as daemon dispatch. This is protocol integration, not a real client run.
func callEditOperationFrame(t *testing.T, s *Server, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s.EnsureToolPromotedForSession(ctx, name)
	ctx = WithAuthorizedToolCall(ctx, name)
	out := callToolFrame(t, s, ctx, name, args)
	var reply struct {
		Error  any                 `json:"error"`
		Result *mcp.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Nil(t, reply.Error, "%s", out)
	require.NotNil(t, reply.Result, "%s", out)
	return reply.Result
}

func TestEditOperationFiveRealToolSeamsLegacyAndFacade(t *testing.T) {
	for _, facade := range []bool{false, true} {
		for _, operation := range []string{"write", "file", "symbol", "batch", "move"} {
			t.Run(map[bool]string{false: "legacy", true: "facade"}[facade]+"/"+operation, func(t *testing.T) {
				t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
				s, root := setupMoveInlineRepo(t, map[string]string{
					"pkg/a.go":  "package pkg\n\nfunc Foo() int { return 42 }\n",
					"pkg/b.go":  "package pkg\n\nfunc Bar() int { return 1 }\n",
					"notes.txt": "before\n", "other.txt": "before\n",
				})
				s.ToolCallTimeout = -1
				const session = "operation-counter"
				preset := "full"
				if facade {
					preset = "facade-v1"
				}
				s.NoteSessionToolPolicy(session, preset, "hide")
				ctx := WithSessionID(t.Context(), session)
				legacy := map[string]string{"write": "write_file", "file": "edit_file", "symbol": "edit_symbol", "batch": "batch_edit", "move": "move_symbol"}[operation]
				name := legacy
				var args map[string]any
				switch operation {
				case "write":
					args = map[string]any{"path": "notes.txt", "content": "after\n", "mutation_id": "count-write"}
				case "file":
					args = map[string]any{"path": "notes.txt", "old_string": "before", "new_string": "after", "mutation_id": "count-file"}
				case "symbol":
					args = map[string]any{"id": "pkg/a.go::Foo", "old_source": "return 42", "new_source": "return 43", "mutation_id": "count-symbol"}
				case "batch":
					args = map[string]any{"transaction_id": "count-batch", "edits": []any{
						map[string]any{"op": "edit_file", "path": filepath.Join(root, "notes.txt"), "old_string": "before", "new_string": "after"},
						map[string]any{"op": "edit_file", "path": filepath.Join(root, "other.txt"), "old_string": "before", "new_string": "after"},
					}}
				case "move":
					args = map[string]any{"id": "pkg/a.go::Foo", "target_file": "pkg/b.go"}
				}
				if facade {
					name = "edit"
					args = map[string]any{"operation": operation, "output": map[string]any{"format": "json", "max_bytes": 0}}
					switch operation {
					case "write":
						args["target"], args["content"] = map[string]any{"file": "notes.txt"}, "after\n"
						args["options"] = map[string]any{"mutation_id": "count-write"}
					case "file":
						args["target"], args["match"], args["replacement"] = map[string]any{"file": "notes.txt"}, "before", "after"
						args["options"] = map[string]any{"mutation_id": "count-file"}
					case "symbol":
						args["target"], args["match"], args["replacement"] = map[string]any{"symbol": "pkg/a.go::Foo"}, "return 42", "return 43"
						args["options"] = map[string]any{"mutation_id": "count-symbol"}
					case "batch":
						args["changes"] = []any{
							map[string]any{"op": "edit_file", "path": filepath.Join(root, "notes.txt"), "old_string": "before", "new_string": "after"},
							map[string]any{"op": "edit_file", "path": filepath.Join(root, "other.txt"), "old_string": "before", "new_string": "after"},
						}
						args["options"] = map[string]any{"transaction_id": "count-batch"}
					case "move":
						name = "refactor"
						args["target"], args["destination"] = map[string]any{"symbol": "pkg/a.go::Foo"}, "pkg/b.go"
					}
				}
				preview := maps.Clone(args)
				preview["dry_run"] = true
				result := callEditOperationFrame(t, s, ctx, name, preview)
				require.False(t, result.IsError, "%v", result.Content)
				require.Zero(t, s.editOperations.completed.Load(), "preview is not a completed edit")
				result = callEditOperationFrame(t, s, ctx, name, args)
				require.False(t, result.IsError, "%v", result.Content)
				if operation == "write" {
					written := decodeFileOpsResult(t, result)
					receipt, ok := written["mutation_receipt"].(string)
					require.True(t, ok)
					require.NotEmpty(t, receipt)
					require.Equal(t, "count-write", written["mutation_id"], "facade options keep the caller mutation identity")
					statusTool := "mutation_status"
					statusArgs := map[string]any{"receipt": receipt, "format": "json"}
					if facade {
						statusTool = "change"
						statusArgs = map[string]any{"operation": "receipt", "options": map[string]any{"receipt": receipt}, "output": map[string]any{"format": "json", "max_bytes": 0}}
					}
					statusResult := callEditOperationFrame(t, s, ctx, statusTool, statusArgs)
					require.False(t, statusResult.IsError, "%v", statusResult.Content)
					status := decodeFileOpsResult(t, statusResult)
					require.Equal(t, receipt, status["receipt"], "status is bound to the write ACK, not its logical id")
					require.NotEqual(t, "count-write", receipt)
				}
				require.EqualValues(t, 1, s.editOperations.completed.Load(), "one logical operation, not per-file commits")
				require.Zero(t, s.editOperations.committedWithError.Load())
				require.Zero(t, s.editOperations.failedAfterSourceEffect.Load())
				if operation == "symbol" || operation == "move" {
					a, err := os.ReadFile(filepath.Join(root, "pkg/a.go"))
					require.NoError(t, err)
					if operation == "symbol" {
						require.Contains(t, string(a), "return 43")
					} else {
						require.NotContains(t, string(a), "func Foo")
						b, err := os.ReadFile(filepath.Join(root, "pkg/b.go"))
						require.NoError(t, err)
						require.Contains(t, string(b), "func Foo")
					}
				} else {
					require.Equal(t, "after\n", readAtomicBatchFixture(t, filepath.Join(root, "notes.txt")))
					if operation == "batch" {
						require.Equal(t, "after\n", readAtomicBatchFixture(t, filepath.Join(root, "other.txt")))
					}
				}
				if operation != "move" {
					// The real idempotency/transaction replay writes nothing.
					result = callEditOperationFrame(t, s, ctx, name, args)
					require.False(t, result.IsError, "%v", result.Content)
					require.EqualValues(t, 1, s.editOperations.completed.Load())
				}
				if operation == "batch" && !facade {
					result = callEditOperationFrame(t, s, ctx, name, map[string]any{"transaction_id": "count-batch", "status_only": true})
					require.False(t, result.IsError, "%v", result.Content)
					require.EqualValues(t, 1, s.editOperations.completed.Load(), "status is not another source operation")
				}
			})
		}
	}
}

func TestEditOperationWriteNoopAndRefusalDoNotCount(t *testing.T) {
	s, root := setupTestServer(t)
	s.ToolCallTimeout = -1
	ctx := WithSessionID(t.Context(), "operation-noop")
	s.NoteSessionToolPolicy("operation-noop", "full", "hide")
	path := filepath.Join(root, "unchanged.txt")
	require.NoError(t, os.WriteFile(path, []byte("same\n"), 0o644))
	result := callEditOperationFrame(t, s, ctx, "write_file", map[string]any{"path": path, "content": "same\n"})
	require.False(t, result.IsError, "%v", result.Content)
	result = callEditOperationFrame(t, s, ctx, "edit_file", map[string]any{"path": path, "old_string": "missing", "new_string": "after"})
	require.True(t, result.IsError)
	require.Zero(t, s.editOperations.completed.Load())
	require.Zero(t, s.editOperations.failedAfterSourceEffect.Load())
	require.Equal(t, "same\n", readAtomicBatchFixture(t, path))
}

func TestEditOperationBatchRollbackAndCommittedJournalWarning(t *testing.T) {
	for _, warning := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "committed-warning"}[warning], func(t *testing.T) {
			s := newAtomicBatchTestServer(t, mutationTestWatcher{})
			s.ToolCallTimeout = -1
			dir := t.TempDir()
			a := writeAtomicBatchFixture(t, dir, "a.txt", "before\n")
			b := writeAtomicBatchFixture(t, dir, "b.txt", "before\n")
			if warning {
				s.batchDurabilityOverride = &batchDurabilityOps{writeFile: func(path string, data []byte, mode os.FileMode) error {
					if filepath.Base(path) == "manifest.json" {
						var receipt batchTransactionReceipt
						if err := json.Unmarshal(data, &receipt); err != nil {
							return err
						}
						if receipt.Status == "committed" {
							return errors.New("test terminal journal unavailable")
						}
					}
					return durableAtomicWriteFile(path, data, mode)
				}}
			} else {
				s.batchWriteOverride = func(path string, data []byte, mode os.FileMode) error {
					if path == b {
						return errors.New("test second target refused")
					}
					return durableAtomicWriteFile(path, data, mode)
				}
			}
			req := editOperationRequest("batch_edit", "")
			req.Params.Arguments = map[string]any{"transaction_id": "typed-outcome", "edits": []any{
				map[string]any{"op": "edit_file", "path": a, "old_string": "before", "new_string": "after"},
				map[string]any{"op": "edit_file", "path": b, "old_string": "before", "new_string": "after"},
			}}
			result, err := s.boundToolHandler(s.handleAtomicBatchEdit)(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError, "typed receipt is carried by a non-tool-error result")
			require.Zero(t, s.editOperations.completed.Load(), "errnil/non-IsError alone is insufficient")
			if warning {
				require.EqualValues(t, 1, s.editOperations.committedWithError.Load())
				require.Zero(t, s.editOperations.failedAfterSourceEffect.Load())
				require.Equal(t, "after\n", readAtomicBatchFixture(t, a))
			} else {
				require.EqualValues(t, 1, s.editOperations.failedAfterSourceEffect.Load())
				require.EqualValues(t, 1, s.editOperations.rolledBack.Load())
				require.Zero(t, s.editOperations.committedWithError.Load())
				require.Equal(t, "before\n", readAtomicBatchFixture(t, a))
			}
		})
	}
}

func TestEditOperationRealBatchDetachedOriginalAndDuplicate(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	s.ToolCallTimeout = time.Minute
	path := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.batchWriteOverride = func(target string, data []byte, mode os.FileMode) error {
		close(entered)
		<-release // actual first transaction already passed its cancellation gate
		return durableAtomicWriteFile(target, data, mode)
	}
	originalJoined := make(chan struct{})
	t.Cleanup(func() {
		unblock()
		select {
		case <-originalJoined:
		case <-time.After(5 * time.Second):
			t.Error("owned physical batch handler did not join before fixture cleanup")
		}
		require.Eventually(t, func() bool { return AbandonedToolCalls() == 0 }, 5*time.Second, time.Millisecond)
	})
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "detached-original", "edits": []any{
		map[string]any{"op": "edit_file", "path": path, "old_string": "before", "new_string": "after"},
	}}
	h := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		defer close(originalJoined)
		return s.handleAtomicBatchEdit(ctx, req)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type callerReply struct {
		result *mcp.CallToolResult
		err    error
	}
	replied := make(chan callerReply, 1)
	go func() {
		result, err := h(ctx, req)
		replied <- callerReply{result: result, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("original did not reach actual physical commit boundary")
	}
	cancel()
	select {
	case reply := <-replied:
		require.ErrorIs(t, reply.err, context.Canceled, "caller cancellation is a Go error, not an in-band tool result")
		require.Nil(t, reply.result, "actual disk work remains held after the caller stops waiting")
	case <-time.After(5 * time.Second):
		t.Fatal("original caller did not receive its cancellation reply")
	}
	require.Zero(t, s.editOperations.completed.Load())
	stateValue, found := s.batchTransactions.Load("detached-original")
	require.True(t, found)
	state := stateValue.(*batchTransactionState)
	timingID := state.acknowledgementReceipt
	statusReq := editOperationRequest("mutation_status", "")
	statusReq.Params.Arguments = map[string]any{"receipt": timingID, "format": "json"}
	statusResult, statusErr := s.handleMutationStatus(t.Context(), statusReq)
	beforeTiming := decodeAcknowledgementResult(t, statusResult, statusErr)["acknowledgement"].(map[string]any)
	require.Equal(t, "measured", beforeTiming["caller_response_state"])
	require.Nil(t, beforeTiming["actual_handler_ms"])
	duplicateCtx, stop := context.WithCancel(t.Context())
	stop()
	_, _ = s.boundToolHandler(s.handleAtomicBatchEdit)(duplicateCtx, req) // existing reply, no physical write
	require.Zero(t, s.editOperations.completed.Load())
	unblock()
	require.Eventually(t, func() bool { return s.editOperations.completed.Load() == 1 }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		payload, _ := s.batchAcknowledgementReceiptPayload(t.Context(), timingID)
		return payload["acknowledgement"].(acknowledgementPayload).ActualState == "measured"
	}, 5*time.Second, time.Millisecond)
	statusResult, statusErr = s.handleMutationStatus(t.Context(), statusReq)
	afterTiming := decodeAcknowledgementResult(t, statusResult, statusErr)["acknowledgement"].(map[string]any)
	require.Equal(t, beforeTiming["caller_response_ready_ms"], afterTiming["caller_response_ready_ms"])
	require.Equal(t, true, afterTiming["actual_finished_after_reply"])
	require.Eventually(t, func() bool { return AbandonedToolCalls() == 0 }, 5*time.Second, time.Millisecond)
	require.Zero(t, s.editOperations.committedWithError.Load())
	require.Equal(t, "after\n", readAtomicBatchFixture(t, path))
}
