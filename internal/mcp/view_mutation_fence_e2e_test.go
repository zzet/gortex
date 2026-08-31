package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

func TestRoutedMutationCommitsDiskAndReleasesFenceThroughLifecycleClose(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	beforeDisk := make(chan struct{})
	allowDisk := make(chan struct{})
	wrapped := stack.srv.wrapToolHandler(func(callCtx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		// The request middleware has already resolved the exact checkout and
		// acquired its mutation token. Hold the leaf immediately before it can
		// write so shutdown races the precise historical TOCTOU window.
		close(beforeDisk)
		<-allowDisk
		return stack.srv.handleEditFile(callCtx, req)
	})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{
		"path":       "keep.go",
		"old_string": "func Keeper() {}",
		"new_string": "func PinnedKeeper() {}",
	}
	type callResult struct {
		result *mcplib.CallToolResult
		err    error
	}
	called := make(chan callResult, 1)
	go func() {
		result, err := wrapped(ctx, req)
		called <- callResult{result: result, err: err}
	}()
	select {
	case <-beforeDisk:
	case <-time.After(time.Second):
		t.Fatal("routed edit did not reach the pre-disk token boundary")
	}

	closed := make(chan error, 1)
	go func() { closed <- stack.lifecycle.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("lifecycle closed through a pre-disk mutation token: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowDisk)

	var call callResult
	select {
	case call = <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("routed edit did not complete through lifecycle close")
	}
	if call.err != nil {
		t.Fatalf("routed edit transport error: %v", call.err)
	}
	if call.result == nil || call.result.IsError {
		t.Fatalf("routed edit did not report its committed disk state: %v", call.result)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(singleTextOrFail(t, call.result)), &payload); err != nil {
		t.Fatalf("decode routed edit result: %v", err)
	}
	if got := payload["checkout_id"]; got != stack.checkoutID {
		t.Fatalf("checkout_id = %v, want %s; payload=%v", got, stack.checkoutID, payload)
	}
	if got := payload["disk_status"]; got != mutationDiskCommitted {
		t.Fatalf("disk_status = %v, want %s; payload=%v", got, mutationDiskCommitted, payload)
	}
	if got := payload["graph_status"]; got != mutationGraphFailed {
		t.Fatalf("graph_status = %v, want %s; payload=%v", got, mutationGraphFailed, payload)
	}
	if detail, _ := payload["reindex_error"].(string); !strings.Contains(detail, "coordinator is closed") {
		t.Fatalf("shutdown publication error is missing: %v", payload)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("lifecycle close after failed publication released its token: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle did not close after failed publication released its token")
	}
	bytes, err := os.ReadFile(filepath.Join(stack.worktree, "keep.go"))
	if err != nil || !strings.Contains(string(bytes), "func PinnedKeeper() {}") {
		t.Fatalf("pinned routed edit was not committed: bytes=%q err=%v", bytes, err)
	}
}
