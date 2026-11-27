package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// Two editors share one file under local ACK. Editor A's file read, a summary
// or an editing context, has selected and retained A's committed owner when
// editor B's registered edit commits a newer version of the same file. The
// read absorbs that change inside its own allowance: it selects B's version
// on its second attempt and answers pending, without the typed retriable
// projection_superseded refusal. When B's owner was already replaced by a
// checked copy of its version (B's write event, then B's readback), that copy
// is accepted the same way. A same-stamp physical change after B's commit is
// not a recorded version and still refuses after one attempt.
func TestRegisteredFileReadAbsorbsConcurrentSameFileCommit(t *testing.T) {
	for _, read := range []struct{ legacy, operation, rows string }{
		{legacy: "get_file_summary", operation: "summary", rows: "nodes"},
		{legacy: "get_editing_context", operation: "editing_context", rows: "defines"},
	} {
		for _, scenario := range []string{"second_editor_commit", "second_editor_readback_copy", "physical_change_same_stamp"} {
			t.Run(read.operation+"/"+scenario, func(t *testing.T) {
				testRegisteredFileReadConcurrentCommit(t, read.legacy, read.operation, read.rows, scenario)
			})
		}
	}
}

func testRegisteredFileReadConcurrentCommit(t *testing.T, legacy, operation, rowsField, scenario string) {
	t.Helper()
	selectedProjectionRegistry(t)
	s, store, path := canonicalPromotionReadFixture(t)
	published := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: published})
	var receipts []*mutationReceipt
	t.Cleanup(func() {
		close(published)
		for _, receipt := range receipts {
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("held mutation did not join")
			}
		}
	})
	const sessionA, sessionB = "file-read-editor-a", "file-read-editor-b"
	connect := func(name string) context.Context {
		require.NoError(t, s.ConnectSession(name, func([]byte) error { return nil }))
		t.Cleanup(func() { s.DisconnectSession(name) })
		ctx := s.WithClientSession(WithSessionID(t.Context(), name), name)
		s.NoteSessionToolPolicy(name, FacadeSurfaceVersion, "hide")
		require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"`+name+`","version":"1"}}}`)))
		return ctx
	}
	editorA, editorB := connect(sessionA), connect(sessionB)
	output := func() map[string]any { return map[string]any{"format": "json", "max_bytes": 0} }
	edit := func(ctx context.Context, id int, mutation, match, replacement string) (map[string]any, error) {
		result, err := externalVersionRegisteredCall(s, ctx, id, "edit", map[string]any{
			"operation": "file", "target": map[string]any{"file": "main.go"}, "match": match,
			"replacement": replacement, "mutation_id": mutation, "output": output(),
		})
		if err != nil {
			return nil, err
		}
		var ack map[string]any
		if err := json.Unmarshal([]byte(resultText(result)), &ack); err != nil {
			return nil, err
		}
		if result.IsError || ack["ack_mode"] != "local" || ack["disk_status"] != "committed" {
			return ack, fmt.Errorf("registered edit was not a committed local ACK: %s", resultText(result))
		}
		if value, found := s.mutationReceipts.Load(ack["reindex_receipt"].(string)); found {
			receipts = append(receipts, value.(*mutationReceipt))
		}
		return ack, nil
	}
	read := func(ctx context.Context, id int) (*mcplib.CallToolResult, error) {
		return externalVersionRegisteredCall(s, ctx, id, "read", map[string]any{
			"operation": operation, "target": map[string]any{"file": "main.go"}, "output": output(),
		})
	}

	const neighbor = "func Neighbor() int { return 9 }"
	const markerA = "func MarkerA() int { return 10 }"
	const markerB = "func MarkerB() int { return 11 }"
	ackA, err := edit(editorA, 10, "file-read-editor-a-commit", neighbor, neighbor+"\n\n"+markerA)
	require.NoError(t, err)
	ownerA := liveEntry(pendingFileProjections, path)
	require.NotNil(t, ownerA)
	require.Positive(t, ownerA.commitSeq)
	require.Nil(t, store.GetNode("main.go::MarkerA"), "the read must precede base publication")

	captured, found := s.facades.legacy(legacy)
	require.True(t, found)
	allowance := &projectedSourceAllowance{tool: legacy}
	var ackB map[string]any
	var stepErr error
	var ownerB, copyB *projectionEntry
	var leafCalls atomic.Int32
	s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		// Editor B's own readback passes straight through.
		if SessionIDFromContext(ctx) != sessionA {
			return captured.handler(ctx, req)
		}
		call := leafCalls.Add(1)
		response, readErr := captured.handler(ctx, req)
		if call != 1 || readErr != nil || response == nil || response.IsError {
			return response, readErr
		}
		facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
		if !ok {
			stepErr = fmt.Errorf("first leaf lacks retained facts")
			return response, readErr
		}
		facts.mu.RLock()
		borrowed := facts.entries[ownerA.key]
		facts.mu.RUnlock()
		if borrowed != ownerA {
			stepErr = fmt.Errorf("first leaf did not retain editor A's committed owner")
			return response, readErr
		}
		// Editor B commits the same file through the registered tool while
		// A's read is in flight, keeping A's definition and adding its own.
		ackB, stepErr = edit(editorB, 30, "file-read-editor-b-commit", markerA, markerA+"\n\n"+markerB)
		ownerB = liveEntry(pendingFileProjections, path)
		if stepErr != nil || ownerB == nil {
			return response, readErr
		}
		switch scenario {
		case "second_editor_readback_copy":
			// B's write event reaches its committed owner, and B's readback
			// checks the same bytes again and publishes a checked copy.
			pendingFileProjections.markSuspect(path)
			readback, readbackErr := read(editorB, 31)
			switch {
			case readbackErr != nil:
				stepErr = readbackErr
			case readback == nil || readback.IsError:
				stepErr = fmt.Errorf("editor B's readback refused: %s", resultText(readback))
			}
			copyB = liveEntry(pendingFileProjections, path)
		case "physical_change_same_stamp":
			// Same length and stamp as B's commit; only the bytes differ, and
			// the write event marks B's owner suspect.
			stepErr = rewriteKeepingStamp(path, ownerB.disk, "return 11", "return 12")
			if stepErr == nil {
				pendingFileProjections.markSuspect(path)
			}
		}
		return response, readErr
	})

	// B's real registered commit runs inside A's request in this fixture,
	// so the shared allowance is widened as other lifecycle tests do.
	deadline := time.Now().Add(canonicalLifecycleFixtureBudget)
	ctx := context.WithValue(editorA, projectedSourceDeadlineKey{}, deadline)
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
	response, err := read(ctx, 20)
	require.NoError(t, err)
	require.NoError(t, stepErr)
	require.NotNil(t, ackB)
	require.Greater(t, ackB["commit_seq"].(float64), ackA["commit_seq"].(float64), "editor B must commit a newer version")
	require.NotNil(t, ownerB)
	require.NotSame(t, ownerA, ownerB)
	require.Positive(t, ownerB.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Nil(t, store.GetNode("main.go::MarkerB"), "both commits stay unpublished in the base graph")
	retries := int(allowance.attempts.Load()) - 1
	t.Logf("FILE_READ_CONCURRENT_COMMIT operation=%s scenario=%s leaf_calls=%d attempts=%d retries=%d refused=%t response=%s",
		operation, scenario, leafCalls.Load(), allowance.attempts.Load(), retries, response.IsError, resultText(response))

	if scenario == "physical_change_same_stamp" {
		require.NotEqual(t, ackB["content_sha256"], fmtReadSHA(raw))
		require.True(t, response.IsError, "bytes that differ from the newer commit must not earn another attempt: %s", resultText(response))
		requireReadFreshnessContract(t, response, "projection_superseded")
		require.EqualValues(t, 1, leafCalls.Load())
		require.EqualValues(t, 1, allowance.attempts.Load())
		require.Same(t, ownerB, liveEntry(pendingFileProjections, path), "a failed proof cannot replace the current owner")
		return
	}
	require.Equal(t, ackB["content_sha256"], fmtReadSHA(raw))
	if scenario == "second_editor_readback_copy" {
		require.NotNil(t, copyB)
		require.NotSame(t, ownerB, copyB, "B's readback must publish a checked copy of its version")
		require.Zero(t, copyB.commitSeq)
		require.Equal(t, ownerB.commitSeq, copyB.sourceCommitSeq)
		require.Same(t, ownerB.layer, copyB.layer)
	}
	require.False(t, response.IsError, "a verified newer version of the same file must not refuse editor A's read: %s", resultText(response))
	body := resultJSON(t, response)
	ids := map[string]bool{}
	rows, ok := body[rowsField].([]any)
	require.True(t, ok, "the read must carry %s rows: %s", rowsField, resultText(response))
	for _, row := range rows {
		ids[row.(map[string]any)["id"].(string)] = true
	}
	require.True(t, ids["main.go::MarkerA"], "editor A's definition must be in the answer")
	require.True(t, ids["main.go::MarkerB"], "the read answers from the newer verified version")
	freshness, ok := body["freshness"].(map[string]any)
	require.True(t, ok, "the answer must carry freshness")
	require.Equal(t, "pending", freshness["graph_freshness"])
	require.Equal(t, false, freshness["complete"])
	// One absorbed refusal: the first attempt is discarded and the second
	// answers, within the same shared allowance and deadline.
	require.EqualValues(t, 2, leafCalls.Load())
	require.EqualValues(t, 2, allowance.attempts.Load())
	require.Equal(t, 1, retries)
	current := liveEntry(pendingFileProjections, path)
	require.NotNil(t, current)
	require.Equal(t, ownerB.commitSeq, current.sourceCommitSeq, "the answer comes from B's committed version")
	require.Same(t, ownerB.layer, current.layer)
}
