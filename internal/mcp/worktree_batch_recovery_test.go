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
	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/graphview"
)

func preparedWorktreeBatch(t *testing.T) (*realCheckoutMutationFixture, batchTransactionReceipt, context.Context) {
	t.Helper()
	f := newWorktreeBatchFixture(t)
	writes := 0
	f.srv.batchWriteOverride = func(path string, bytes []byte, mode os.FileMode) error {
		require.NoError(t, agents.AtomicWriteFile(path, bytes, mode))
		writes++
		if writes == 2 {
			panic("simulate daemon exit after final disk write")
		}
		return nil
	}
	var crashed any
	func() {
		defer func() { crashed = recover() }()
		f.facade(t, f.worktree, "edit", worktreeBatchArgs(worktreeBatchChanges("New"), "prepared-recovery"))
	}()
	require.Equal(t, 2, writes, "simulated daemon exit must follow the complete disk commit")
	_ = crashed
	f.srv.batchWriteOverride = nil
	receipt, found, err := readBatchManifest("prepared-recovery")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "prepared", receipt.Status)
	require.NotEmpty(t, receipt.HeadCommit)
	f.srv.batchTransactions = sync.Map{}
	ctx := WithSessionCWD(WithSessionID(t.Context(), "prepared-recovery-control"), f.worktree)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	control, err := f.srv.resolveCheckoutControlScope(ctx, graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, &req)
	require.NoError(t, err)
	return f, receipt, withCheckoutControl(ctx, control)
}

func TestWorktreeBatchPreparedRecoveryAndConcurrentStatus(t *testing.T) {
	f, receipt, ctx := preparedWorktreeBatch(t)
	// Reconstruct the precise lock ordering window: recovery owns the cycle
	// lease and has proven all after-images, but has not admitted publication.
	ctx, release, err := f.srv.acquireBatchRecovery(ctx, receipt)
	require.NoError(t, err)
	require.NoError(t, verifyBatchAfterImages(ctx, receipt))
	state := &batchTransactionState{fingerprint: receipt.Fingerprint, done: make(chan struct{}), receipt: receipt}
	receipt.Status, receipt.DiskStatus, receipt.GraphStatus = "committed", "committed", "pending"
	state.publish(receipt, false)
	f.srv.batchTransactions.Store(receipt.TransactionID, state)
	statusReached := make(chan struct{})
	permitAdmission := make(chan struct{})
	statusDone := make(chan error, 1)
	admissionDone := make(chan struct{})
	var reachedOnce, permitOnce, releaseOnce sync.Once
	closePermit := func() { permitOnce.Do(func() { close(permitAdmission) }) }
	releaseLease := func() { releaseOnce.Do(release) }
	admissionStarted := false
	batchRecoveryBeforeAdmission = func(context.Context) { reachedOnce.Do(func() { close(statusReached) }); <-permitAdmission }
	t.Cleanup(func() {
		closePermit()
		releaseLease()
		select {
		case <-statusDone:
		case <-time.After(6 * time.Second):
			t.Error("status worker did not stop during cleanup")
		}
		if admissionStarted {
			select {
			case <-admissionDone:
			case <-time.After(6 * time.Second):
				t.Error("admission worker did not stop during cleanup")
			}
		}
		batchRecoveryBeforeAdmission = nil
	})
	go func() {
		defer close(statusDone)
		_, err := f.srv.batchTransactionStatus(withCheckoutControl(context.Background(), checkoutControlFromContext(ctx)), receipt.TransactionID)
		statusDone <- err
	}()
	select {
	case <-statusReached:
	case <-time.After(5 * time.Second):
		t.Fatal("status did not reach recovery admission")
	}
	if !state.graphMu.TryLock() {
		t.Fatal("status holds transaction graph lock while waiting for recovery cycle lease")
	}
	state.graphMu.Unlock()
	closePermit()
	admissionStarted = true
	go func() { defer close(admissionDone); f.srv.refreshBatchGraph(ctx, state); releaseLease() }()
	select {
	case <-admissionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery publication deadlocked with status")
	}
	select {
	case err := <-statusDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("status deadlocked behind recovery")
	}
	result, err := f.srv.batchTransactionStatus(withCheckoutControl(t.Context(), checkoutControlFromContext(ctx)), receipt.TransactionID)
	require.NoError(t, err)
	require.Equal(t, "committed", result.DiskStatus)
	requireWorktreeBatchFile(t, f.primary, "edit.go", "package repo\n\nfunc Old() {}\n", 0o600)
	require.Equal(t, f.checkoutID, result.CheckoutID)
}

func TestWorktreeBatchPreparedJournalRecoversThroughPublicStatus(t *testing.T) {
	f, _, _ := preparedWorktreeBatch(t)
	result := worktreeBatchStatus(t, f, f.worktree, f.worktree, "prepared-recovery")
	require.Equal(t, "committed", result["disk_status"])
	require.Equal(t, true, result["recovered"])
	require.Eventually(t, func() bool {
		return worktreeBatchStatus(t, f, f.worktree, f.worktree, "prepared-recovery")["graph_status"] == "fresh"
	}, 10*time.Second, 10*time.Millisecond)
	requireWorktreeBatchGraph(t, f, f.worktree, f.worktree, "repo/edit.go::BatchChanged", "repo/caller.go::BatchCaller")
}

func TestWorktreeBatchJournalRejectsIncarnationAndReplacedRoot(t *testing.T) {
	for _, change := range []string{"incarnation", "root"} {
		t.Run(change, func(t *testing.T) {
			f, receipt, _ := preparedWorktreeBatch(t)
			if change == "incarnation" {
				receipt.Incarnation = "different-incarnation"
				require.NoError(t, f.srv.persistBatchManifest(receipt))
			} else {
				saved := f.worktree + "-saved"
				require.NoError(t, os.Rename(f.worktree, saved))
				require.NoError(t, os.Mkdir(f.worktree, 0o755))
				for _, name := range []string{".git", "edit.go", "caller.go"} {
					bytes, err := os.ReadFile(filepath.Join(saved, name))
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(f.worktree, name), bytes, 0o600))
				}
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(f.worktree)); require.NoError(t, os.Rename(saved, f.worktree)) })
			}
			before, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			res := f.facade(t, f.worktree, "edit", map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": receipt.TransactionID, "status_only": true}, "view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID}})
			require.True(t, res.IsError, viewResultText(t, res))
			after, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestWorktreeBatchPreparedRecoveryRetriesAfterUnavailableCoordinator(t *testing.T) {
	f, _, _ := preparedWorktreeBatch(t)
	lifecycle := f.srv.lifecycle
	f.srv.lifecycle = nil
	first := worktreeBatchStatus(t, f, f.worktree, f.worktree, "prepared-recovery")
	require.Equal(t, "prepared", first["status"])
	require.Contains(t, first["error"], "checkout coordinator")
	f.srv.lifecycle = lifecycle
	retried := worktreeBatchStatus(t, f, f.worktree, f.worktree, "prepared-recovery")
	require.Equal(t, "committed", retried["disk_status"])
}

func TestWorktreeBatchPreWriteJournalRecoversWithoutChangingDisk(t *testing.T) {
	f, receipt, _ := preparedWorktreeBatch(t)
	for _, file := range receipt.Files {
		before, err := readBatchBackup(receipt, file)
		require.NoError(t, err)
		require.NoError(t, agents.AtomicWriteFile(file.Path, before, file.Mode))
	}
	receipt.HeadRef, receipt.HeadCommit, receipt.HeadTree = "", "", ""
	require.NoError(t, f.srv.persistBatchManifest(receipt))
	restored := worktreeBatchStatus(t, f, f.worktree, f.worktree, receipt.TransactionID)
	require.Equal(t, "aborted", restored["status"])
	require.Equal(t, "unchanged", restored["disk_status"])
	requireWorktreeBatchFile(t, f.worktree, "edit.go", primitiveWorktreeSource, 0o600)
	requireWorktreeBatchFile(t, f.worktree, "caller.go", "package repo\n\nfunc CallerNew() { New() }\n", 0o640)
}

func worktreeBatchSibling(t *testing.T, f *realCheckoutMutationFixture) string {
	t.Helper()
	sibling := filepath.Join(filepath.Dir(f.primary), "concurrent-sibling")
	checkoutMutationGit(t, f.primary, "worktree", "add", "-b", "batch-sibling", sibling)
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "edit.go"), []byte(primitiveWorktreeSource), 0o600))
	require.NoError(t, os.Chmod(filepath.Join(sibling, "edit.go"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "caller.go"), []byte("package repo\n\nfunc CallerNew() { New() }\n"), 0o640))
	deadline := time.Now().Add(20 * time.Second)
	awaitCheckoutAdmission(t, f.srv, WithSessionCWD(t.Context(), sibling), sibling, deadline)
	require.Eventually(t, func() bool {
		res := f.facade(t, sibling, "read", map[string]any{"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::New"}, "require_exact": true})
		return !res.IsError && resultFreshness(t, res)["exact"] == true
	}, 20*time.Second, 20*time.Millisecond)
	return sibling
}

func TestWorktreeBatchConcurrentDifferentCheckouts(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_ids", true: "same_id"}[sameID], func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			sibling := worktreeBatchSibling(t, f)
			type result struct {
				root string
				res  *mcplib.CallToolResult
			}
			done := make(chan result, 2)
			start := make(chan struct{})
			for i, root := range []string{f.worktree, sibling} {
				go func(i int, root string) {
					<-start
					id := "parallel-batch"
					if !sameID {
						id += string(rune('a' + i))
					}
					done <- result{root: root, res: f.facade(t, root, "edit", worktreeBatchArgs(worktreeBatchChanges("New"), id))}
				}(i, root)
			}
			close(start)
			committed := 0
			for range 2 {
				select {
				case got := <-done:
					if got.res.IsError {
						require.True(t, sameID, viewResultText(t, got.res))
						requireWorktreeBatchFile(t, got.root, "edit.go", primitiveWorktreeSource, 0o600)
					} else {
						payload := lifecycleResultPayload(t, got.res)
						require.Equal(t, "committed", payload["disk_status"])
						awaitWorktreeBatch(t, f, got.root, got.root, got.res)
						committed++
					}
				case <-time.After(10 * time.Second):
					t.Fatal("concurrent checkout batches did not finish")
				}
			}
			expected := 2
			if sameID {
				expected = 1
			}
			require.Equal(t, expected, committed)
			requireWorktreeBatchFile(t, f.primary, "edit.go", "package repo\n\nfunc Old() {}\n", 0o600)
		})
	}
}

func TestWorktreeBatchDryRunWithExistingIDRetainsCheckoutPathAuthority(t *testing.T) {
	f := newWorktreeBatchFixture(t)
	written := f.facade(t, f.worktree, "edit", worktreeBatchArgs(worktreeBatchChanges("New"), "dryrun-existing"))
	awaitWorktreeBatch(t, f, f.worktree, f.worktree, written)
	args := worktreeBatchArgs([]any{map[string]any{"op": "edit_file", "path": filepath.Join(f.primary, "edit.go"), "old_string": "func Old() {}", "new_string": "func Preview() {}"}}, "dryrun-existing")
	args["dry_run"] = true
	preview := f.facade(t, f.worktree, "edit", args)
	if !preview.IsError {
		payload := lifecycleResultPayload(t, preview)
		require.Equal(t, float64(1), payload["conflicts"])
	}
	requireWorktreeBatchFile(t, f.primary, "edit.go", "package repo\n\nfunc Old() {}\n", 0o600)
}
