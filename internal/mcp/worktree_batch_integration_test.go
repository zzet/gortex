package mcp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// Each checkout starts with different target and caller names. A batch that
// accidentally plans from the primary graph cannot satisfy the worktree guards.
func newWorktreeBatchFixture(t *testing.T) *realCheckoutMutationFixture {
	t.Helper()
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	return newRealCheckoutMutationFixtureWithSetup(t, nil, func(_ *indexer.CheckoutLifecycle, worktree string) {
		for root, name := range map[string]string{
			filepath.Join(filepath.Dir(worktree), "repo"): "Old", worktree: "New",
		} {
			for file, source := range map[string]string{
				"edit.go":   "package repo\n\nfunc " + name + "() {}\n",
				"caller.go": "package repo\n\nfunc Caller" + name + "() { " + name + "() }\n",
			} {
				path := filepath.Join(root, file)
				require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
				mode := os.FileMode(0o600)
				if file == "caller.go" {
					mode = 0o640
				}
				require.NoError(t, os.Chmod(path, mode))
			}
		}
	})
}

func worktreeBatchChanges(name string) []any {
	return []any{
		map[string]any{"op": "edit_file", "path": "repo/edit.go", "old_string": "func " + name + "() {}", "new_string": "func BatchChanged() {}"},
		map[string]any{"op": "edit_file", "path": "repo/caller.go", "old_string": "func Caller" + name + "() { " + name + "() }", "new_string": "func BatchCaller() { BatchChanged() }"},
	}
}

func worktreeBatchArgs(changes []any, transaction string) map[string]any {
	return map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": transaction}}
}

func worktreeBatchStatus(t *testing.T, f *realCheckoutMutationFixture, cwd, root, transaction string) map[string]any {
	t.Helper()
	args := map[string]any{
		"operation": "batch", "options": map[string]any{"transaction_id": transaction, "status_only": true},
	}
	if root != f.primary {
		args["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	return lifecycleResultPayload(t, f.facade(t, cwd, "edit", args))
}

func awaitWorktreeBatch(t *testing.T, f *realCheckoutMutationFixture, cwd, root string, result *mcplib.CallToolResult) map[string]any {
	t.Helper()
	payload := lifecycleResultPayload(t, result)
	transaction, ok := payload["transaction_id"].(string)
	require.True(t, ok, "batch must expose its durable transaction ID: %+v", payload)
	require.NotEmpty(t, transaction)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		payload = worktreeBatchStatus(t, f, cwd, root, transaction)
		if payload["graph_status"] == "fresh" {
			return payload
		}
		require.Equal(t, "pending", payload["graph_status"], "unexpected batch publication outcome: %+v", payload)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("batch graph did not become fresh: %+v", payload)
	return nil
}

func requireWorktreeBatchFile(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, name)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode().Perm())
}

func worktreeBatchReadArgs(f *realCheckoutMutationFixture, root string, args map[string]any) map[string]any {
	args["require_exact"] = true
	if root != f.primary {
		args["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	return args
}

func requireWorktreeBatchGraph(t *testing.T, f *realCheckoutMutationFixture, cwd, root, target, caller string) {
	t.Helper()
	source := f.facade(t, cwd, "read", worktreeBatchReadArgs(f, root, map[string]any{
		"operation": "source", "target": map[string]any{"symbol": target},
	}))
	require.False(t, source.IsError, viewResultText(t, source))
	if root != f.primary {
		require.Equal(t, true, resultFreshness(t, source)["exact"])
	}
	require.Contains(t, viewResultText(t, source), "func BatchChanged()")
	callers := f.facade(t, cwd, "relations", worktreeBatchReadArgs(f, root, map[string]any{
		"operation": "callers", "target": map[string]any{"symbol": target},
	}))
	require.False(t, callers.IsError, viewResultText(t, callers))
	if root != f.primary {
		require.Equal(t, true, resultFreshness(t, callers)["exact"])
	}
	require.Contains(t, viewResultText(t, callers), caller)
	search := f.facade(t, cwd, "search", worktreeBatchReadArgs(f, root, map[string]any{
		"operation": "symbols", "query": "BatchChanged",
	}))
	require.False(t, search.IsError, viewResultText(t, search))
	require.Contains(t, viewResultText(t, search), target)
}

func TestWorktreeBatchFacadeGuardedTwoFileEdit(t *testing.T) {
	for _, selection := range []string{"primary", "cwd", "checkout_id", "path"} {
		t.Run(selection, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			root, cwd, name := f.worktree, f.worktree, "New"
			args := worktreeBatchArgs(worktreeBatchChanges(name), "guarded-two-file")
			if selection == "primary" {
				root, cwd, name = f.primary, f.primary, "Old"
				args = worktreeBatchArgs(worktreeBatchChanges(name), "guarded-two-file")
			} else if selection != "cwd" {
				cwd = f.primary
				view := map[string]any{"kind": "worktree"}
				if selection == "path" {
					view["path"] = f.worktree
				} else {
					view["checkout_id"] = f.checkoutID
				}
				args["view"] = view
			}
			written := f.facade(t, cwd, "edit", args)
			payload := lifecycleResultPayload(t, written)
			require.Equal(t, "committed", payload["status"])
			require.Equal(t, "committed", payload["disk_status"])
			if root == f.worktree {
				require.Equal(t, f.checkoutID, payload["checkout_id"])
				require.Equal(t, f.worktree, payload["checkout_root"])
			}
			requireWorktreeBatchFile(t, root, "edit.go", "package repo\n\nfunc BatchChanged() {}\n", 0o600)
			requireWorktreeBatchFile(t, root, "caller.go", "package repo\n\nfunc BatchCaller() { BatchChanged() }\n", 0o640)
			other, otherName := f.primary, "Old"
			if root == f.primary {
				other, otherName = f.worktree, "New"
			}
			requireWorktreeBatchFile(t, other, "edit.go", "package repo\n\nfunc "+otherName+"() {}\n", 0o600)
			requireWorktreeBatchFile(t, other, "caller.go", "package repo\n\nfunc Caller"+otherName+"() { "+otherName+"() }\n", 0o640)
			awaitWorktreeBatch(t, f, cwd, root, written)
			requireWorktreeBatchGraph(t, f, cwd, root, "repo/edit.go::BatchChanged", "repo/caller.go::BatchCaller")
		})
	}
}

func TestWorktreeBatchFacadeFailurePreservesBothFiles(t *testing.T) {
	for _, checkout := range []string{"primary", "worktree"} {
		for _, failure := range []string{"second_guard", "second_commit", "second_post_write"} {
			t.Run(checkout+"/"+failure, func(t *testing.T) {
				f := newWorktreeBatchFixture(t)
				root, name := f.worktree, "New"
				if checkout == "primary" {
					root, name = f.primary, "Old"
				}
				changes := worktreeBatchChanges(name)
				var writes atomic.Int64
				if failure == "second_guard" {
					changes[1].(map[string]any)["old_string"] = "guard no longer matches"
				}
				f.srv.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
					call := writes.Add(1)
					if call == 2 && failure == "second_commit" {
						return errors.New("injected second batch commit failure")
					}
					if err := agents.AtomicWriteFile(path, content, mode); err != nil {
						return err
					}
					if call == 2 && failure == "second_post_write" {
						return errors.New("injected second batch post-write failure")
					}
					return nil
				}
				result := f.facade(t, root, "edit", worktreeBatchArgs(changes, "failure-"+failure))
				payload := lifecycleResultPayload(t, result)
				require.Equal(t, "aborted", payload["status"])
				if failure == "second_guard" {
					require.Zero(t, writes.Load())
					require.Equal(t, "unchanged", payload["disk_status"])
				} else {
					require.GreaterOrEqual(t, writes.Load(), int64(2))
					require.Equal(t, "rolled_back", payload["disk_status"])
				}
				require.Equal(t, "not_started", payload["graph_status"])
				for otherRoot, otherName := range map[string]string{f.primary: "Old", f.worktree: "New"} {
					requireWorktreeBatchFile(t, otherRoot, "edit.go", "package repo\n\nfunc "+otherName+"() {}\n", 0o600)
					requireWorktreeBatchFile(t, otherRoot, "caller.go", "package repo\n\nfunc Caller"+otherName+"() { "+otherName+"() }\n", 0o640)
				}
			})
		}
	}
}

func TestWorktreeBatchFacadeMoveAndDelete(t *testing.T) {
	for _, checkout := range []string{"primary", "worktree"} {
		t.Run(checkout, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			root, name := f.worktree, "New"
			if checkout == "primary" {
				root, name = f.primary, "Old"
			}
			source := "package repo\n\nfunc " + name + "() {}\n"
			caller := "package repo\n\nfunc Caller" + name + "() { " + name + "() }\n"
			result := f.facade(t, root, "edit", worktreeBatchArgs([]any{
				map[string]any{"op": "move_file", "source": "repo/edit.go", "destination": "repo/moved.go", "expected_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(source)))},
				map[string]any{"op": "delete_file", "path": "repo/caller.go", "expected_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(caller)))},
			}, "move-and-delete"))
			require.Equal(t, "committed", lifecycleResultPayload(t, result)["status"])
			requireWorktreeBatchFile(t, root, "moved.go", source, 0o600)
			for _, file := range []string{"edit.go", "caller.go"} {
				_, err := os.Stat(filepath.Join(root, file))
				require.True(t, os.IsNotExist(err), "%s survived batch: %v", file, err)
			}
			awaitWorktreeBatch(t, f, root, root, result)
			read := f.facade(t, root, "read", worktreeBatchReadArgs(f, root, map[string]any{
				"operation": "source", "target": map[string]any{"symbol": "repo/moved.go::" + name},
			}))
			require.False(t, read.IsError, viewResultText(t, read))
			if root != f.primary {
				require.Equal(t, true, resultFreshness(t, read)["exact"])
			}
			for _, symbol := range []string{"repo/edit.go::" + name, "repo/caller.go::Caller" + name} {
				missing := f.facade(t, root, "read", worktreeBatchReadArgs(f, root, map[string]any{
					"operation": "source", "target": map[string]any{"symbol": symbol},
				}))
				require.True(t, missing.IsError, "removed symbol remains visible: %s: %s", symbol, viewResultText(t, missing))
			}
		})
	}
}

func TestWorktreeBatchFacadeMixedSymbolAndFileEdit(t *testing.T) {
	for _, checkout := range []string{"primary", "worktree"} {
		t.Run(checkout, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			root, name := f.worktree, "New"
			if checkout == "primary" {
				root, name = f.primary, "Old"
			}
			changes := worktreeBatchChanges(name)
			changes[0] = map[string]any{
				"op": "edit_symbol", "id": "repo/edit.go::" + name,
				"old_source": "func " + name + "() {}", "new_source": "func BatchChanged() {}",
			}
			written := f.facade(t, root, "edit", worktreeBatchArgs(changes, "mixed-symbol-file"))
			require.Equal(t, "committed", lifecycleResultPayload(t, written)["status"])
			requireWorktreeBatchFile(t, root, "edit.go", "package repo\n\nfunc BatchChanged() {}\n", 0o600)
			requireWorktreeBatchFile(t, root, "caller.go", "package repo\n\nfunc BatchCaller() { BatchChanged() }\n", 0o640)
			awaitWorktreeBatch(t, f, root, root, written)
			requireWorktreeBatchGraph(t, f, root, root, "repo/edit.go::BatchChanged", "repo/caller.go::BatchCaller")
		})
	}
}

func TestWorktreeBatchFacadeDryRunKeepsRouteAndFiles(t *testing.T) {
	for _, checkout := range []string{"primary", "worktree"} {
		t.Run(checkout, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			root, name := f.worktree, "New"
			if checkout == "primary" {
				root, name = f.primary, "Old"
			}
			before, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), f.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			args := worktreeBatchArgs(worktreeBatchChanges(name), "dry-run-only")
			args["dry_run"] = true
			preview := f.facade(t, root, "edit", args)
			payload := lifecycleResultPayload(t, preview)
			require.Equal(t, true, payload["dry_run"])
			require.Equal(t, float64(0), payload["conflicts"])
			require.Len(t, payload["plan"], 2)
			after, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), f.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, before, after, "dry run must retain the published worktree route")
			for otherRoot, otherName := range map[string]string{f.primary: "Old", f.worktree: "New"} {
				requireWorktreeBatchFile(t, otherRoot, "edit.go", "package repo\n\nfunc "+otherName+"() {}\n", 0o600)
				requireWorktreeBatchFile(t, otherRoot, "caller.go", "package repo\n\nfunc Caller"+otherName+"() { "+otherName+"() }\n", 0o640)
			}
		})
	}
}

func TestWorktreeBatchFacadeRejectsForeignAndSymlinkPaths(t *testing.T) {
	for _, target := range []string{"absolute_primary", "absolute_sibling", "nested_symlink"} {
		t.Run(target, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			sibling := filepath.Join(filepath.Dir(f.primary), "sibling")
			checkoutMutationGit(t, f.primary, "worktree", "add", "-b", "sibling", sibling)
			path := filepath.Join(f.primary, "edit.go")
			switch target {
			case "absolute_sibling":
				path = filepath.Join(sibling, "edit.go")
			case "nested_symlink":
				require.NoError(t, os.Mkdir(filepath.Join(f.worktree, "nested"), 0o755))
				link := filepath.Join(f.worktree, "nested", "outside")
				if err := os.Symlink(f.primary, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				path = "repo/nested/outside/edit.go"
			}
			primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			siblingBefore, err := os.ReadFile(filepath.Join(sibling, "edit.go"))
			require.NoError(t, err)
			changes := worktreeBatchChanges("New")
			changes[1] = map[string]any{"op": "edit_file", "path": path, "old_string": "Old", "new_string": "MustNotLand"}
			result := f.facade(t, f.worktree, "edit", worktreeBatchArgs(changes, "foreign-path"))
			if !result.IsError {
				payload := lifecycleResultPayload(t, result)
				require.Equal(t, "aborted", payload["status"])
				require.Equal(t, "unchanged", payload["disk_status"])
			}
			requireWorktreeBatchFile(t, f.worktree, "edit.go", primitiveWorktreeSource, 0o600)
			requireWorktreeBatchFile(t, f.worktree, "caller.go", "package repo\n\nfunc CallerNew() { New() }\n", 0o640)
			primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primaryBefore, primaryAfter)
			siblingAfter, err := os.ReadFile(filepath.Join(sibling, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, siblingBefore, siblingAfter)
		})
	}
}

func TestWorktreeBatchFacadeTransactionCannotCrossCheckout(t *testing.T) {
	for _, other := range []string{"primary", "sibling"} {
		t.Run(other, func(t *testing.T) {
			f := newWorktreeBatchFixture(t)
			otherRoot := f.primary
			if other == "sibling" {
				otherRoot = filepath.Join(filepath.Dir(f.primary), "sibling")
				checkoutMutationGit(t, f.primary, "worktree", "add", "-b", "sibling", otherRoot)
				// Real facade admission discovers and activates the new checkout.
				deadline := time.Now().Add(20 * time.Second)
				awaitCheckoutAdmission(t, f.srv, WithSessionCWD(t.Context(), otherRoot), otherRoot, deadline)
				require.Eventually(t, func() bool {
					read := f.facade(t, otherRoot, "read", map[string]any{
						"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::Old"}, "require_exact": true,
					})
					return !read.IsError && resultFreshness(t, read)["exact"] == true
				}, 20*time.Second, 20*time.Millisecond)
			}
			before, err := os.ReadFile(filepath.Join(otherRoot, "edit.go"))
			require.NoError(t, err)
			args := worktreeBatchArgs(worktreeBatchChanges("New"), "checkout-bound-id")
			written := f.facade(t, f.worktree, "edit", args)
			awaitWorktreeBatch(t, f, f.worktree, f.worktree, written)
			retry := f.facade(t, otherRoot, "edit", args)
			require.True(t, retry.IsError, "identical transaction payload must reject a different checkout: %s", viewResultText(t, retry))
			status := f.facade(t, otherRoot, "edit", map[string]any{
				"operation": "batch", "options": map[string]any{"transaction_id": "checkout-bound-id", "status_only": true},
			})
			require.True(t, status.IsError, "a foreign checkout must not read another checkout's transaction: %s", viewResultText(t, status))
			after, err := os.ReadFile(filepath.Join(otherRoot, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestWorktreeBatchFacadePendingStatusAndJournalReload(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	f, blocker := newBlockedLifecycleFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "notes.txt"), []byte("before\n"), 0o600))
	baselineTicket, err := f.srv.lifecycle.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	require.NoError(t, err)
	select {
	case ready := <-baselineTicket.Ticket.Done:
		require.NoError(t, ready.Err)
	case <-time.After(10 * time.Second):
		t.Fatal("notes baseline did not publish")
	}
	written := f.facade(t, f.worktree, "edit", worktreeBatchArgs([]any{
		map[string]any{"op": "edit_file", "path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}"},
		map[string]any{"op": "edit_file", "path": "repo/notes.txt", "old_string": "before", "new_string": "after"},
	}, "pending-reload"))
	payload := lifecycleResultPayload(t, written)
	require.Equal(t, "committed", payload["disk_status"])
	require.Equal(t, "pending", payload["graph_status"])
	select {
	case <-blocker.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("batch publication did not reach the blocked parser")
	}
	started := time.Now()
	status := worktreeBatchStatus(t, f, f.worktree, f.worktree, "pending-reload")
	require.Less(t, time.Since(started), time.Second, "durable status must not wait for graph readiness")
	require.Equal(t, "pending", status["graph_status"])
	require.Equal(t, "committed", status["disk_status"])
	require.Equal(t, f.checkoutID, status["checkout_id"])
	requireWorktreeBatchFile(t, f.worktree, "edit.go", "package repo\n\nfunc LifecycleBlocked() {}\n", 0o644)
	requireWorktreeBatchFile(t, f.worktree, "notes.txt", "after\n", 0o600)
	blocker.unblock()
	awaitWorktreeBatch(t, f, f.worktree, f.worktree, written)

	// A fresh server has no batch state or publication receipts in memory.
	// Keep the real catalog/lifecycle so the facade reloads and validates its
	// journal against exactly the same checkout authority as after a restart.
	reloaded := NewServer(query.NewEngine(f.store), f.store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer: f.srv.multiIndexer, ConfigManager: f.srv.configManager,
	})
	reloaded.SetMaterializer(f.srv.materializer)
	reloaded.lifecycle = f.srv.lifecycle
	reloadedFixture := *f
	reloadedFixture.srv = reloaded
	fromJournal := worktreeBatchStatus(t, &reloadedFixture, f.worktree, f.worktree, "pending-reload")
	require.Equal(t, "committed", fromJournal["disk_status"])
	require.Equal(t, f.checkoutID, fromJournal["checkout_id"])
	require.Equal(t, status["fingerprint"], fromJournal["fingerprint"])
	require.Equal(t, status["checkout_incarnation"], fromJournal["checkout_incarnation"])
	require.Equal(t, status["checkout_root_identity"], fromJournal["checkout_root_identity"])
	awaitWorktreeBatch(t, &reloadedFixture, f.worktree, f.worktree, written)
	exact := reloadedFixture.facade(t, f.worktree, "read", map[string]any{
		"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::LifecycleBlocked"}, "require_exact": true,
	})
	require.False(t, exact.IsError, viewResultText(t, exact))
	require.Equal(t, true, resultFreshness(t, exact)["exact"])
}
