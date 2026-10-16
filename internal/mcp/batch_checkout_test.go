package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

func TestCheckoutBatchDeletionCoordinatorEndToEnd(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	for _, selection := range []string{"cwd", "checkout_id", "path"} {
		t.Run(selection, func(t *testing.T) {
			f := newRealCheckoutMutationFixture(t)
			primaryFile := filepath.Join(f.primary, "edit.go")
			primaryBefore, err := os.ReadFile(primaryFile)
			require.NoError(t, err)
			ctx := context.Background()
			before, found, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			cwd := f.worktree
			var view any
			if selection != "cwd" {
				cwd = f.primary
				selector := map[string]any{"kind": "worktree"}
				if selection == "path" {
					selector["path"] = f.worktree
				} else {
					selector["checkout_id"] = f.checkoutID
				}
				view = selector
			}
			callCtx := WithSessionCWD(WithSessionID(ctx, "batch-checkout-"+selection), cwd)
			tool := f.srv.mcpServer.GetTool("edit")
			require.NotNil(t, tool)
			call := func(args map[string]any) *mcplib.CallToolResult {
				t.Helper()
				args["operation"] = "batch"
				if view != nil {
					args["view"] = view
				}
				req := mcplib.CallToolRequest{}
				req.Params.Name, req.Params.Arguments = "edit", args
				res, callErr := tool.Handler(callCtx, req)
				require.NoError(t, callErr)
				require.False(t, res.IsError, viewResultText(t, res))
				return res
			}
			changes := []any{map[string]any{"op": "delete_file", "path": "repo/edit.go", "expected_sha256": testSHA256(primitiveWorktreeSource)}}
			call(map[string]any{"changes": changes, "dry_run": true})
			assert.FileExists(t, filepath.Join(f.worktree, "edit.go"))
			afterPreview, _, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
			require.NoError(t, err)
			assert.Equal(t, before, afterPreview)
			id := "checkout-delete-" + selection
			result := call(map[string]any{"changes": changes, "options": map[string]any{"transaction_id": id}})
			payload := decodeFileOpsResult(t, result)
			require.Equal(t, "committed", payload["disk_status"])
			assert.NoFileExists(t, filepath.Join(f.worktree, "edit.go"))
			require.Eventually(t, func() bool {
				route, exists, routeErr := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
				return routeErr == nil && exists && route.State == store_sqlite.RouteActive && route.DirtyGenerationID != before.DirtyGenerationID
			}, 10*time.Second, 20*time.Millisecond)
			value, loaded := f.srv.batchTransactions.Load(id)
			require.True(t, loaded)
			receiptID := value.(*batchTransactionState).snapshot().Files[0].ReindexReceipt
			require.NotEmpty(t, receiptID)
			require.Eventually(t, func() bool {
				outcome, exists := f.srv.mutationReceiptState(receiptID)
				return exists && outcome.Reindexed && !outcome.Pending && outcome.Err == nil
			}, 10*time.Second, 20*time.Millisecond)
			status := call(map[string]any{"options": map[string]any{"transaction_id": id, "status_only": true}})
			assert.Equal(t, "fresh", decodeFileOpsResult(t, status)["graph_status"])
			replayed := call(map[string]any{"changes": changes, "options": map[string]any{"transaction_id": id}})
			assert.Equal(t, "committed", decodeFileOpsResult(t, replayed)["disk_status"])
			primaryAfter, err := os.ReadFile(primaryFile)
			require.NoError(t, err)
			assert.Equal(t, primaryBefore, primaryAfter)

			read := mcplib.CallToolRequest{}
			read.Params.Name = "read"
			read.Params.Arguments = map[string]any{"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::Old"}, "view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}}
			res, err := f.srv.wrapToolHandler(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				reader := f.srv.readerFor(ctx)
				assert.False(t, hasNode(reader, "repo/edit.go::New"))
				assert.False(t, hasNode(reader, "repo/edit.go::Old"), "deletion must mask the primary's copy")
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})(callCtx, read)
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
		})
	}
}

func TestCheckoutBatchReceiptCannotBeRecoveredFromAnotherView(t *testing.T) {
	root := t.TempDir()
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	scope := checkoutMutationFromContext(ctx)
	scope.checkoutID, scope.incarnation = "selected", "incarnation"
	receipt := batchTransactionReceipt{CheckoutID: "selected", CheckoutIncarnation: "incarnation", CheckoutRoot: root}
	require.NoError(t, guardBatchCheckout(ctx, receipt))
	assert.Error(t, guardBatchCheckout(context.Background(), receipt))
	receipt.CheckoutIncarnation = "recreated"
	assert.Error(t, guardBatchCheckout(ctx, receipt))
	assert.Error(t, guardBatchCheckout(ctx, batchTransactionReceipt{}))
	receipt.CheckoutIncarnation = "incarnation"
	receipt.Files = []batchTransactionFile{{Path: filepath.Join(t.TempDir(), "foreign.sql")}}
	assert.Error(t, guardBatchCheckout(ctx, receipt))
}

func TestCheckoutBatchEditsAndMovesUseCoordinator(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	for _, operation := range []string{"edit_file", "move_file"} {
		t.Run(operation, func(t *testing.T) {
			f := newRealCheckoutMutationFixture(t)
			primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			ctx := WithSessionCWD(WithSessionID(context.Background(), "batch-"+operation), f.primary)
			before, _, err := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
			require.NoError(t, err)
			change := map[string]any{"op": operation, "path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func Changed() {}"}
			if operation == "move_file" {
				change = map[string]any{"op": operation, "source": "repo/edit.go", "destination": "repo/moved.go"}
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name = "edit"
			req.Params.Arguments = map[string]any{
				"operation": "batch", "changes": []any{change},
				"view": map[string]any{"kind": "worktree", "path": f.worktree},
			}
			res, err := f.srv.mcpServer.GetTool("edit").Handler(ctx, req)
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.Equal(t, "committed", decodeFileOpsResult(t, res)["disk_status"])
			require.Eventually(t, func() bool {
				route, found, routeErr := f.store.Catalog().GetCheckoutRoute(ctx, f.checkoutID)
				return routeErr == nil && found && route.State == store_sqlite.RouteActive && route.DirtyGenerationID != before.DirtyGenerationID
			}, 10*time.Second, 20*time.Millisecond)
			primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			assert.Equal(t, primaryBefore, primaryAfter)
			if operation == "move_file" {
				assert.NoFileExists(t, filepath.Join(f.worktree, "edit.go"))
				assert.Equal(t, primitiveWorktreeSource, readAtomicBatchFixture(t, filepath.Join(f.worktree, "moved.go")))
			} else {
				assert.Contains(t, readAtomicBatchFixture(t, filepath.Join(f.worktree, "edit.go")), "func Changed() {}")
			}
		})
	}
}

func TestCheckoutBatchWrongViewLookupDoesNotPoisonRecovery(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	root := t.TempDir()
	path := filepath.Join(root, "deleted.sql")
	lease := &fakeCheckoutMutationLifecycle{}
	ctx := withCheckoutMutation(context.Background(), lease, root)
	scope := checkoutMutationFromContext(ctx)
	scope.checkoutID, scope.incarnation = "selected", "incarnation"
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: "prepared-checkout", Fingerprint: "fingerprint",
		CheckoutID: scope.checkoutID, CheckoutIncarnation: scope.incarnation, CheckoutRoot: root,
		Status: "prepared", DiskStatus: "unchanged", GraphStatus: "not_started",
		Files: []batchTransactionFile{{Path: path, RelativePath: "deleted.sql", BeforeSHA256: testSHA256("old"), AfterAbsent: true}},
	}
	require.NoError(t, s.persistBatchManifest(receipt))
	_, err := s.batchTransactionStatus(context.Background(), receipt.TransactionID)
	require.Error(t, err)
	_, cached := s.batchTransactions.Load(receipt.TransactionID)
	require.False(t, cached, "a rejected view must not claim the recovery state")
	recovered, err := s.batchTransactionStatus(ctx, receipt.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "committed", recovered.DiskStatus)
	assert.Equal(t, "fresh", recovered.GraphStatus)
}

func TestCheckoutBatchRestartDoesNotReuseDaemonLocalReceipt(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	root := t.TempDir()
	refreshed := false
	lease := &fakeCheckoutMutationLifecycle{refresh: func(context.Context) (indexer.CheckoutCycle, error) {
		refreshed = true
		return indexer.CheckoutCycle{CommitGenerationID: 1, DirtyGenerationID: 2}, nil
	}}
	ctx := withCheckoutMutation(context.Background(), lease, root)
	scope := checkoutMutationFromContext(ctx)
	scope.checkoutID, scope.incarnation = "selected", "incarnation"
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: "pending-checkout", Fingerprint: "fingerprint",
		CheckoutID: scope.checkoutID, CheckoutIncarnation: scope.incarnation, CheckoutRoot: root,
		Status: "committed", DiskStatus: "committed", GraphStatus: "pending",
		Files: []batchTransactionFile{{Path: filepath.Join(root, "deleted.sql"), AfterAbsent: true, ReindexReceipt: "mutation-1", ReindexGeneration: 17}},
	}
	require.NoError(t, s.persistBatchManifest(receipt))
	s.mutationReceipts.Store("mutation-1", &mutationReceipt{id: "mutation-1", generation: 17, completed: true, result: indexer.MutationResult{Reindexed: true}})
	recovered, err := s.batchTransactionStatus(ctx, receipt.TransactionID)
	require.NoError(t, err)
	assert.True(t, refreshed, "restart must re-admit the original checkout instead of trusting a reused receipt ID")
	assert.Equal(t, "fresh", recovered.GraphStatus)
}

func TestCheckoutBatchAfterImageAcceptsUnchangedBytes(t *testing.T) {
	root := t.TempDir()
	path := writeAtomicBatchFixture(t, root, "source.go", "package fixture\n")
	file := batchTransactionFile{Path: path, BeforeSHA256: testSHA256("package fixture\n"), AfterSHA256: testSHA256("package fixture\n")}
	require.NoError(t, verifyBatchAfterImages([]batchTransactionFile{file}))
	require.NoError(t, os.WriteFile(path, []byte("changed"), 0o644))
	assert.Error(t, verifyBatchAfterImages([]batchTransactionFile{file}))
	file.AfterAbsent = true
	assert.Error(t, verifyBatchAfterImages([]batchTransactionFile{file}))
	require.NoError(t, os.Remove(path))
	require.NoError(t, verifyBatchAfterImages([]batchTransactionFile{file}))
}

func TestCheckoutBatchRetryRecoversFreshnessBarrier(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	mutation, done, path := newReceiptCheckoutMutation(t)
	root := filepath.Dir(path)
	mutation.ticket.Ticket.Path = root
	ctx := withCheckoutMutation(context.Background(), mutation, root)
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: "retry-checkout", Fingerprint: "fingerprint",
		CheckoutID: mutation.ticket.CheckoutID, CheckoutIncarnation: mutation.ticket.Incarnation, CheckoutRoot: root,
		Status: "committed", DiskStatus: "committed", GraphStatus: "pending",
		Files: []batchTransactionFile{{Path: path, AfterSHA256: testSHA256("package fixture\n")}},
	}
	state := &batchTransactionState{receipt: receipt, done: make(chan struct{})}
	s.refreshCheckoutBatchGraph(ctx, state, receipt)
	firstID := state.snapshot().Files[0].ReindexReceipt
	failure := errors.New("first publication failed")
	done <- indexer.MutationResult{RequestedGeneration: 7, Err: failure}
	waitCheckoutReceipt(t, s, firstID)
	require.Error(t, s.awaitMutationFreshnessForRepos(ctx, "fixture"))

	retried := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(retried) })
	mutation.ticket = &indexer.CheckoutRefreshTicket{
		CheckoutID: receipt.CheckoutID, Incarnation: receipt.CheckoutIncarnation, Root: root, RepoPrefix: "fixture",
		Ticket: &indexer.MutationTicket{Path: root, Generation: 8, Done: retried},
	}
	s.refreshCheckoutBatchGraph(ctx, state, state.snapshot())
	secondID := state.snapshot().Files[0].ReindexReceipt
	require.NotEqual(t, firstID, secondID)
	retried <- indexer.MutationResult{RequestedGeneration: 8, AppliedGeneration: 29, Reindexed: true}
	waitCheckoutReceipt(t, s, secondID)
	s.refreshCheckoutBatchGraph(ctx, state, state.snapshot())
	assert.Equal(t, "fresh", state.snapshot().GraphStatus)
	assert.NoError(t, s.awaitMutationFreshnessForRepos(ctx, "fixture"))
	original, found := s.mutationReceiptState(firstID)
	require.True(t, found)
	assert.ErrorIs(t, original.Err, failure, "recovery must retain the original failure as historical evidence")
}
