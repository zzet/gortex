package mcp

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestAcknowledgementBatchForwardDurabilityAndSetFallback(t *testing.T) {
	for _, mode := range []string{"point", "set", "set-fallback"} {
		t.Run(mode, func(t *testing.T) {
			var watcher watcherHistory = mutationTestWatcher{}
			if mode != "point" {
				watcher = &batchSetSchedulerSpy{unsupported: mode == "set-fallback"}
			}
			s := newAtomicBatchTestServer(t, watcher)
			s.ToolCallTimeout = -1
			dir := t.TempDir()
			paths := []string{writeAtomicBatchFixture(t, dir, "a.txt", "before\n"), writeAtomicBatchFixture(t, dir, "b.txt", "before\n")}
			var a *editAcknowledgement
			h := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				a = editAcknowledgementFrom(ctx, s)
				return s.handleAtomicBatchEdit(ctx, req)
			})
			req := editOperationRequest("batch_edit", "")
			req.Params.Arguments = map[string]any{"transaction_id": "ack-durability", "edits": []any{
				map[string]any{"op": "edit_file", "path": paths[0], "old_string": "before", "new_string": "after"},
				map[string]any{"op": "edit_file", "path": paths[1], "old_string": "before", "new_string": "after"},
			}}
			result, err := h(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			for _, path := range paths {
				require.Equal(t, "after\n", readAtomicBatchFixture(t, path))
			}
			snapshot := a.snapshot()
			require.EqualValues(t, 3, snapshot.DiskWrite.Completed, "two forward writes plus one deduplicated directory-sync envelope; no journal/backup/cleanup laps")
			wantAdmissions := map[string]uint64{"point": 2, "set": 1, "set-fallback": 3}[mode]
			require.Equal(t, wantAdmissions, snapshot.TicketAdmission.Completed)
			require.True(t, snapshot.LapsDisjoint)
			require.NotNil(t, snapshot.DiskWrite.AckMS)
			require.Equal(t, "not_applicable", snapshot.ParseGate.State)
			require.Nil(t, snapshot.ParseGate.ObservedMS, "text replacement does not gain a fake parser duration")
			state, _ := s.batchTransactions.Load("ack-durability")
			original := s.batchAcknowledgement(state.(*batchTransactionState))
			result, err = h(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError)
			require.Equal(t, original, s.batchAcknowledgement(state.(*batchTransactionState)), "duplicate cannot add durability/admission laps to the original")
		})
	}
}

func TestAcknowledgementBatchFailedForwardWriteExcludesRollbackAndJournal(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	s.ToolCallTimeout = -1
	path := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
	sentinel := errors.New("forward write refused")
	s.batchWriteOverride = func(string, []byte, os.FileMode) error { return sentinel }
	var a *editAcknowledgement
	h := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a = editAcknowledgementFrom(ctx, s)
		return s.handleAtomicBatchEdit(ctx, req)
	})
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "ack-failure", "edits": []any{
		map[string]any{"op": "edit_file", "path": path, "old_string": "before", "new_string": "after"},
	}}
	_, err := h(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "before\n", readAtomicBatchFixture(t, path))
	snapshot := a.snapshot()
	require.EqualValues(t, 1, snapshot.DiskWrite.Completed, "failed forward call is measured; rollback/journal durability is not silently relabelled")
	require.NotNil(t, snapshot.DiskWrite.ObservedMS)
	require.Zero(t, snapshot.TicketAdmission.Started)
	require.Zero(t, s.editOperations.completed.Load())
}

func TestAcknowledgementBatchAdmissionKeepsOriginalSchedulerContext(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, a, _ := s.withEditAcknowledgement(parent)
	outcome := s.beginBatchGraphRefresh(ctx, filepath.Join(t.TempDir(), "target.go"), batchTransactionReceipt{})
	require.NoError(t, outcome.Err, "the original canonical batch scheduler uses Background even when caller observation is cancelled")
	a.finishActualAt(time.Now())
	a.finishReplyAt(time.Now(), "go_error")
	require.EqualValues(t, 1, a.snapshot().TicketAdmission.Completed)
	require.NotNil(t, a.snapshot().TicketAdmission.ObservedMS)
}

func TestAcknowledgementBatchMoveDeleteCountsForwardCallsOnly(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	s.ToolCallTimeout = -1
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "source.txt", "move me\n")
	deleted := writeAtomicBatchFixture(t, dir, "delete.txt", "delete me\n")
	destination := filepath.Join(dir, "destination.txt")
	var a *editAcknowledgement
	h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a = editAcknowledgementFrom(ctx, s)
		receipt, err := s.runBatchTransaction(ctx, []batchEditItem{
			atomicFileMove(source, destination, testSHA256("move me\n")),
			atomicFileDelete(deleted, testSHA256("delete me\n")),
		}, "ack-move-delete")
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultJSON(receipt)
	})
	result, err := h(t.Context(), editOperationRequest("batch_edit", ""))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NoFileExists(t, source)
	require.NoFileExists(t, deleted)
	require.Equal(t, "move me\n", readAtomicBatchFixture(t, destination))
	snapshot := a.snapshot()
	require.EqualValues(t, 4, snapshot.DiskWrite.Completed, "one forward write, two forward removals and one deduplicated directory-sync envelope")
	require.EqualValues(t, 3, snapshot.TicketAdmission.Completed)
	require.True(t, snapshot.LapsDisjoint)
}

func TestAcknowledgementMoveRealParserAndSourceDurability(t *testing.T) {
	s, root := setupMoveInlineRepo(t, map[string]string{
		"pkg/a.go": "package pkg\n\nfunc Foo() int { return 42 }\n",
		"pkg/b.go": "package pkg\n\nfunc Bar() int { return 1 }\n",
	})
	s.ToolCallTimeout = -1
	var a *editAcknowledgement
	h := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a = editAcknowledgementFrom(ctx, s)
		return s.handleMoveSymbol(ctx, req)
	})
	req := editOperationRequest("move_symbol", "")
	req.Params.Arguments = map[string]any{"id": "pkg/a.go::Foo", "target_file": "pkg/b.go"}
	result, err := h(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.NotContains(t, readAtomicBatchFixture(t, filepath.Join(root, "pkg/a.go")), "func Foo")
	require.Contains(t, readAtomicBatchFixture(t, filepath.Join(root, "pkg/b.go")), "func Foo")
	snapshot := a.snapshot()
	require.EqualValues(t, 2, snapshot.DiskWrite.Completed)
	require.GreaterOrEqual(t, snapshot.ParseGate.Completed, uint64(2), "real source and existing-target Go parser calls")
	require.NotNil(t, snapshot.ParseGate.AckMS)
	require.True(t, snapshot.LapsDisjoint)
	require.Equal(t, "explicit_go_parser_calls_formatting_unattributed", snapshot.ParserScope)
	require.Zero(t, snapshot.TicketAdmission.Started, "synchronous move reindex is not a queue admission")
}

func TestAcknowledgementParserPreservesErrorAndRawAllocationBehaviour(t *testing.T) {
	source := []byte("package p\nfunc F(\n")
	_, plainErr := parser.ParseFile(token.NewFileSet(), "p.go", source, parser.ParseComments)
	_, observedErr := parseAcknowledgedGoFile(nil, token.NewFileSet(), "p.go", source, parser.ParseComments)
	require.EqualError(t, observedErr, plainErr.Error())
	plain := testing.AllocsPerRun(100, func() { _, _ = parser.ParseFile(token.NewFileSet(), "p.go", source, parser.ParseComments) })
	observed := testing.AllocsPerRun(100, func() { _, _ = parseAcknowledgedGoFile(nil, token.NewFileSet(), "p.go", source, parser.ParseComments) })
	require.Equal(t, plain, observed, "nil observer adds no parser timing/context allocation")
	a := &editAcknowledgement{entered: time.Now()}
	_, err := parseAcknowledgedGoFile(a, token.NewFileSet(), "p.go", source, parser.ParseComments)
	require.EqualError(t, err, plainErr.Error())
	a.finishActualAt(time.Now())
	a.finishReplyAt(time.Now(), "tool_error")
	require.EqualValues(t, 1, a.snapshot().ParseGate.Completed)
	require.NotNil(t, a.snapshot().ParseGate.ObservedMS, "a real rejected parse still took measured work")
}

func TestAcknowledgementMoveUnsupportedLanguagesKeepNullParserTime(t *testing.T) {
	for _, extension := range []string{"py", "ts"} {
		t.Run(extension, func(t *testing.T) {
			s, _ := setupMoveInlineRepo(t, map[string]string{"pkg/a.go": "package pkg\nfunc F() {}\n"})
			s.ToolCallTimeout = -1
			path := "pkg/unsupported." + extension
			id := path + "::F"
			s.graph.AddNode(&graph.Node{ID: id, Name: "F", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 1})
			var a *editAcknowledgement
			h := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				a = editAcknowledgementFrom(ctx, s)
				return s.handleMoveSymbol(ctx, req)
			})
			req := editOperationRequest("move_symbol", "")
			req.Params.Arguments = map[string]any{"id": id, "target_file": "pkg/target." + extension}
			result, err := h(t.Context(), req)
			require.NoError(t, err)
			require.True(t, result.IsError)
			snapshot := a.snapshot()
			require.Equal(t, "unsupported_non_go_move", snapshot.ParserScope)
			require.Equal(t, "not_applicable", snapshot.ParseGate.State)
			require.Nil(t, snapshot.ParseGate.ObservedMS)
			require.Zero(t, snapshot.DiskWrite.Started)
		})
	}
}
