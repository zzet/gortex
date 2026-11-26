package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// Two editors share one file under local ACK. Editor A's immediate summary
// has selected and retained A's committed owner when editor B's registered
// edit commits a newer version that keeps A's definition. A verified newer
// tool commit earns the summary's second attempt, which answers from B's
// owner and stays pending. Bytes that no longer match B's commit still refuse.
func TestRegisteredSummaryAfterConcurrentToolCommit(t *testing.T) {
	for _, scenario := range []string{"second_editor_commit", "second_editor_commit_event", "external_write_after_commit"} {
		t.Run(scenario, func(t *testing.T) {
			testRegisteredSummaryAfterConcurrentToolCommit(t, scenario)
		})
	}
}

func testRegisteredSummaryAfterConcurrentToolCommit(t *testing.T, scenario string) {
	t.Helper()
	// Attach the shipped refusal predicate to the response for diagnosis.
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
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
	connect := func(name string) context.Context {
		require.NoError(t, s.ConnectSession(name, func([]byte) error { return nil }))
		t.Cleanup(func() { s.DisconnectSession(name) })
		ctx := s.WithClientSession(WithSessionID(t.Context(), name), name)
		s.NoteSessionToolPolicy(name, FacadeSurfaceVersion, "hide")
		require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"`+name+`","version":"1"}}}`)))
		return ctx
	}
	editorA, editorB := connect("summary-editor-a"), connect("summary-editor-b")
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

	const neighbor = "func Neighbor() int { return 9 }"
	const markerA = "func MarkerA() int { return 10 }"
	const markerB = "func MarkerB() int { return 11 }"
	ackA, err := edit(editorA, 10, "summary-editor-a-commit", neighbor, neighbor+"\n\n"+markerA)
	require.NoError(t, err)
	ownerA := liveEntry(pendingFileProjections, path)
	require.NotNil(t, ownerA)
	require.Positive(t, ownerA.commitSeq)
	require.Nil(t, store.GetNode("main.go::MarkerA"), "the summary must precede base publication")

	captured, found := s.facades.legacy("get_file_summary")
	require.True(t, found)
	allowance := &projectedSourceAllowance{tool: "get_file_summary", diagnostic: newProjectionPredicateTrace()}
	var ackB map[string]any
	var editErr error
	var ownerB *projectionEntry
	leafCalls := 0
	s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		leafCalls++
		response, readErr := captured.handler(ctx, req)
		if leafCalls != 1 || readErr != nil || response == nil || response.IsError {
			return response, readErr
		}
		facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
		if !ok {
			t.Error("first leaf lacks retained facts")
			return response, readErr
		}
		facts.mu.RLock()
		borrowed := facts.entries[ownerA.key]
		facts.mu.RUnlock()
		if borrowed != ownerA {
			t.Error("first leaf did not retain editor A's committed owner")
			return response, readErr
		}
		// Editor B commits through the registered tool while A's summary is
		// in flight, keeping A's definition and adding its own.
		ackB, editErr = edit(editorB, 30, "summary-editor-b-commit", markerA, markerA+"\n\n"+markerB)
		ownerB = liveEntry(pendingFileProjections, path)
		if editErr != nil || ownerB == nil {
			return response, readErr
		}
		switch scenario {
		case "second_editor_commit_event":
			// The watcher reports B's own write on the committed owner.
			pendingFileProjections.markSuspect(path)
		case "external_write_after_commit":
			// Same length and stamp as B's commit; only the bytes differ.
			editErr = rewriteKeepingStamp(path, ownerB.disk, "return 11", "return 12")
		}
		return response, readErr
	})

	// B's real registered commit runs inside A's request in this fixture,
	// so the shared allowance is widened as other lifecycle tests do.
	deadline := time.Now().Add(canonicalLifecycleFixtureBudget)
	if allowance.diagnostic != nil {
		allowance.diagnostic.originalDeadline = deadline
	}
	ctx := context.WithValue(editorA, projectedSourceDeadlineKey{}, deadline)
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
	response, err := externalVersionRegisteredCall(s, ctx, 20, "read", map[string]any{
		"operation": "summary", "target": map[string]any{"file": "main.go"}, "output": output(),
	})
	require.NoError(t, err)
	require.NoError(t, editErr)
	require.NotNil(t, ackB)
	require.Greater(t, ackB["commit_seq"].(float64), ackA["commit_seq"].(float64), "editor B must commit a newer version")
	require.NotNil(t, ownerB)
	require.NotSame(t, ownerA, ownerB)
	require.Positive(t, ownerB.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), markerA)
	require.Contains(t, string(raw), "func MarkerB() int")
	require.Nil(t, store.GetNode("main.go::MarkerB"), "both commits stay unpublished in the base graph")
	var predicate any
	if response.Meta != nil {
		predicate = response.Meta.AdditionalFields["gortex_projection_predicate"]
	}
	t.Logf("scenario=%s leaf_calls=%d attempts=%d predicate=%v response=%s", scenario, leafCalls, allowance.attempts.Load(), predicate, resultText(response))
	if scenario == "external_write_after_commit" {
		require.NotEqual(t, ackB["content_sha256"], fmtReadSHA(raw))
		require.True(t, response.IsError, "bytes that differ from the newer commit must not earn another attempt: %s", resultText(response))
		var failure StructuredError
		require.NoError(t, json.Unmarshal([]byte(resultText(response)), &failure))
		require.Equal(t, ErrCodeProjectionSuperseded, failure.ErrorCode)
		require.True(t, failure.Retriable)
		require.Equal(t, "read_only", failure.Data["retry_scope"])
		require.Equal(t, 1, leafCalls)
		require.Same(t, ownerB, liveEntry(pendingFileProjections, path))
		return
	}
	require.Equal(t, ackB["content_sha256"], fmtReadSHA(raw))
	require.False(t, response.IsError, "a verified newer tool commit must not refuse editor A's summary: %s", resultText(response))
	body := resultJSON(t, response)
	ids := map[string]bool{}
	for _, row := range body["nodes"].([]any) {
		ids[row.(map[string]any)["id"].(string)] = true
	}
	require.True(t, ids["main.go::MarkerA"], "editor A's definition must be in the summary")
	require.True(t, ids["main.go::MarkerB"], "the summary answers from the newer verified version")
	freshness, ok := body["freshness"].(map[string]any)
	require.True(t, ok, "the summary must carry freshness")
	require.Equal(t, "pending", freshness["graph_freshness"])
	require.Equal(t, false, freshness["complete"])
	require.Equal(t, 2, leafCalls)
	require.EqualValues(t, 2, allowance.attempts.Load())
	current := liveEntry(pendingFileProjections, path)
	require.NotNil(t, current)
	require.Equal(t, ownerB.commitSeq, current.sourceCommitSeq, "the answer comes from B's committed version")
	require.Same(t, ownerB.layer, current.layer)
}

// rewriteKeepingStamp changes bytes in place without changing the file's size
// or disk stamp, so only a byte check can notice the write.
func rewriteKeepingStamp(path string, stamp diskStamp, old, replacement string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	changed := strings.Replace(string(current), old, replacement, 1)
	if changed == string(current) || len(changed) != len(current) {
		return fmt.Errorf("rewrite of %q must change bytes and keep the length", old)
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		return err
	}
	if err := os.Chtimes(path, stamp.info.ModTime(), stamp.info.ModTime()); err != nil {
		return err
	}
	after, err := statDiskStamp(path)
	if err != nil {
		return err
	}
	if !stamp.matches(after) {
		return errors.New("rewrite changed the disk stamp")
	}
	return nil
}
