package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestCheckoutControlReceiptBypassesUnreadyGraph(t *testing.T) {
	for _, ownCWD := range []bool{false, true} {
		name := "explicit_checkout"
		if ownCWD {
			name = "own_cwd"
		}
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
			cwd, args := stack.repoRoot, worktreeViewArgs()
			if ownCWD {
				cwd, args = stack.worktreeRoot, nil
			}
			ran := false
			res, err := stack.callWithView(t, cwd, "mutation_status", args,
				func(ctx context.Context) (*mcp.CallToolResult, error) {
					ran = true
					scope := checkoutControlFromContext(ctx)
					require.NotNil(t, scope)
					require.True(t, scope.CheckoutScoped)
					require.Equal(t, viewTestWorktree, scope.Checkout.CheckoutID)
					require.Equal(t, "inc-worktree", scope.Checkout.Incarnation)
					require.Nil(t, scope.Route)
					require.Nil(t, requestViewFromContext(ctx), "receipt metadata must not claim an exact graph")
					return mcp.NewToolResultText(`{"receipts":[]}`), nil
				})
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.True(t, ran)
			graphResult, err := stack.callWithView(t, stack.repoRoot, "get_symbol", worktreeViewArgs(), captureReader(stack.srv, new(graph.Reader)))
			require.NoError(t, err)
			assertToolError(t, graphResult, graphview.CodeViewBuilding)
		})
	}
}

func TestCheckoutControlScopesBeforeReadiness(t *testing.T) {
	for _, state := range []store_sqlite.CheckoutState{store_sqlite.CheckoutStateReady, store_sqlite.CheckoutStateUnavailable} {
		t.Run(string(state), func(t *testing.T) {
			stack := newViewStack(t)
			stack.setWorktreeState(t, state)
			require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
			for _, tool := range []string{"mutation_status", "reindex_repository", "detect_changes"} {
				ran := false
				res, err := stack.callWithView(t, stack.otherRoot, tool, worktreeViewArgs(),
					func(context.Context) (*mcp.CallToolResult, error) {
						ran = true
						return mcp.NewToolResultText(`{"ok":true}`), nil
					})
				require.NoError(t, err)
				assertToolError(t, res, graphview.CodeSelectorOutOfScope)
				require.False(t, ran, tool)
			}
		})
	}
}

func TestCheckoutControlDistinguishesCanonicalAndSelectedWorktree(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callWithView(t, stack.repoRoot, "mutation_status", nil,
		func(ctx context.Context) (*mcp.CallToolResult, error) {
			scope := checkoutControlFromContext(ctx)
			require.NotNil(t, scope)
			require.False(t, scope.CheckoutScoped)
			require.Equal(t, viewTestPrimary, scope.Checkout.CheckoutID)
			return mcp.NewToolResultText(`{"ok":true}`), nil
		})
	require.NoError(t, err)
	require.False(t, res.IsError)
}

func TestCheckoutControlDoesNotAdmitImmutableRecovery(t *testing.T) {
	stack := newViewStack(t)
	for _, kind := range []string{"git_ref", "commit"} {
		value := "refs/heads/main"
		if kind == "commit" {
			value = strings.Repeat("a", 40)
		}
		ran := false
		res, err := stack.callWithView(t, stack.repoRoot, "reindex_repository",
			map[string]any{"view": map[string]any{"kind": kind, "value": value}},
			func(context.Context) (*mcp.CallToolResult, error) {
				ran = true
				return mcp.NewToolResultText(`{"ok":true}`), nil
			})
		require.NoError(t, err)
		assertToolError(t, res, graphview.CodeViewReadOnly)
		require.False(t, ran)
	}
}

func TestCheckoutControlPathAndScopeConfinement(t *testing.T) {
	stack := newViewStack(t)
	scope := &checkoutControlScope{RepoPrefix: "repo", Checkout: store_sqlite.Checkout{RootPath: stack.worktreeRoot}}
	for _, paths := range [][]string{{"../repo/edit.go"}, {filepath.Join(stack.repoRoot, "edit.go")}, {filepath.Join(stack.otherRoot, "other.go")}} {
		require.Error(t, scope.validateReindexPaths("", paths))
	}
	require.NoError(t, scope.validateReindexPaths(stack.worktreeRoot, []string{"repo/edit.go", "new/subdir.go"}))
	require.NoError(t, scope.validateRepoSelector("repo"))
	require.NoError(t, scope.validateRepoSelector(stack.worktreeRoot))
	require.Error(t, scope.validateRepoSelector(stack.repoRoot))
	require.Error(t, scope.validateReindexPaths(stack.repoRoot, nil))
	alias := filepath.Join(t.TempDir(), "checkout-alias")
	require.NoError(t, os.Symlink(stack.worktreeRoot, alias))
	require.NoError(t, scope.validateReindexPaths(alias, []string{filepath.Join(alias, "new/subdir.go")}))
	escape := filepath.Join(stack.worktreeRoot, "escape")
	require.NoError(t, os.Symlink(stack.otherRoot, escape))
	require.Error(t, scope.validateReindexPaths("", []string{filepath.Join(escape, "not-created.go")}))
}

func TestCheckoutControlBaseNeverFallsBackToCWDWorkingTree(t *testing.T) {
	stack := newViewStack(t)
	route, found, err := stack.store.Catalog().GetCheckoutRoute(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	require.True(t, found)
	for _, operation := range []string{"reindex_repository", "detect_changes", "mutation_status"} {
		t.Run(operation, func(t *testing.T) {
			ran := false
			res, err := stack.callWithView(t, stack.worktreeRoot, operation,
				map[string]any{"view": map[string]any{"kind": "base", "graph_id": route.GraphID}},
				func(context.Context) (*mcp.CallToolResult, error) {
					ran = true
					return mcp.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeViewReadOnly)
			require.False(t, ran)
		})
	}
}

func TestCheckoutControlNeverRecoversUnregisteredNestedCheckoutAsParent(t *testing.T) {
	stack := newViewStack(t)
	nested := filepath.Join(stack.repoRoot, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: /not-yet-cataloged"), 0o644))
	for _, tc := range []struct {
		name, cwd, operation string
		args                 map[string]any
	}{
		{"reindex-path", stack.repoRoot, "reindex_repository", map[string]any{"path": nested}},
		{"receipt-cwd", nested, "mutation_status", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			res, err := stack.callWithView(t, tc.cwd, tc.operation, tc.args,
				func(context.Context) (*mcp.CallToolResult, error) {
					ran = true
					return mcp.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeCheckoutInaccessible)
			require.False(t, ran)
		})
	}
}

func TestCheckoutControlResolvesLegacyReindexPathToAutomaticCheckout(t *testing.T) {
	stack := newViewStack(t)
	require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
	res, err := stack.callWithView(t, stack.repoRoot, "reindex_repository",
		map[string]any{"path": stack.worktreeRoot}, func(ctx context.Context) (*mcp.CallToolResult, error) {
			scope := checkoutControlFromContext(ctx)
			require.NotNil(t, scope)
			require.Equal(t, viewTestWorktree, scope.Checkout.CheckoutID)
			require.True(t, scope.CheckoutScoped)
			return mcp.NewToolResultText(`{"ok":true}`), nil
		})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
}

func TestCheckoutControlOwnCWDRepoPrefixNeverReindexesPrimary(t *testing.T) {
	stack := newViewStack(t)
	require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
	res, err := stack.callWithView(t, stack.worktreeRoot, "reindex_repository",
		map[string]any{"path": "repo"}, func(ctx context.Context) (*mcp.CallToolResult, error) {
			scope := checkoutControlFromContext(ctx)
			require.NotNil(t, scope)
			require.Equal(t, viewTestWorktree, scope.Checkout.CheckoutID)
			require.True(t, scope.CheckoutScoped)
			return mcp.NewToolResultText(`{"ok":true}`), nil
		})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
}

func TestCheckoutBindingUnknownFamilyDoesNotInventScope(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	ctx := context.Background()
	before, err := f.store.Catalog().ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	unknown := t.TempDir()
	checkoutMutationGit(t, unknown, "init")
	require.False(t, f.srv.CheckoutServesCWD(ctx, unknown))
	_, found, err := f.srv.checkoutForRequestPath(ctx, unknown)
	require.NoError(t, err)
	require.False(t, found)
	after, err := f.store.Catalog().ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "ordinary request binding must not track an unknown Git family")
}

func TestCheckoutAdmissionContentionIsRetryableWithoutInventingScope(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	root := filepath.Join(filepath.Dir(f.primary), "admission-busy")
	checkoutMutationGit(t, f.primary, "worktree", "add", "-b", "admission-busy", root)
	ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-lifecycle"), root)
	rec := f.srv.lifecycle.Reconciler()
	reconcile.WithHEADSampler(func(ctx context.Context, _ string) (gitstate.HEADState, error) {
		<-ctx.Done()
		return gitstate.HEADState{}, errors.New("injected Git subprocess killed")
	})(rec)
	t.Cleanup(func() { reconcile.WithHEADSampler(gitstate.SampleHEAD)(rec) })
	started := time.Now()
	served, err := f.srv.CheckoutServesCWDChecked(ctx, root)
	require.False(t, served, "pending observation is not a scoped checkout")
	require.ErrorIs(t, err, indexer.ErrCheckoutMutationBusy)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(started), 200*time.Millisecond)
	require.Less(t, time.Since(started), time.Second)
	_, found, lookupErr := f.srv.registeredCheckoutForPath(context.Background(), root)
	require.NoError(t, lookupErr)
	require.False(t, found, "timed-out metadata observation must not invent a catalog identity")

	// Exercise the same session's scope cache during the failed observation.
	// It must not remain unresolved once a retry can establish the checkout.
	before := f.facade(t, root, "search", map[string]any{
		"operation": "symbols", "query": "Old", "require_exact": true,
	})
	require.True(t, before.IsError)
	reconcile.WithHEADSampler(gitstate.SampleHEAD)(rec)
	served, err = f.srv.CheckoutServesCWDChecked(ctx, root)
	require.NoError(t, err)
	require.True(t, served)
	var exact *mcp.CallToolResult
	require.Eventually(t, func() bool {
		exact = f.facade(t, root, "search", map[string]any{
			"operation": "symbols", "query": "Old", "require_exact": true,
		})
		return !exact.IsError && resultFreshness(t, exact)["exact"] == true
	}, 10*time.Second, 20*time.Millisecond)
	payload := lifecycleResultPayload(t, exact)
	require.NotEmpty(t, payload["results"], "retry must recover the session's selected primary scope, not return silent zeros")
}

func TestCheckoutAdmissionPreservesPrefixCatalogFailure(t *testing.T) {
	stack := newViewStack(t)
	var catalogErr error
	called := false
	workspace, project, prefix, served, err := stack.srv.scopeForAutomaticCheckoutWithPrefix(
		context.Background(), stack.worktreeRoot, func(_ context.Context, checkout store_sqlite.Checkout) (string, error) {
			called = true
			require.Equal(t, viewTestWorktree, checkout.CheckoutID, "checkout lookup must succeed before this injected catalog failure")
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			// Execute the real ListDedicatedGraphs read with a failing context,
			// rather than failing the earlier checkout/family lookup.
			result, lookupErr := stack.srv.repoPrefixForCheckoutChecked(canceled, checkout)
			catalogErr = lookupErr
			require.ErrorIs(t, catalogErr, context.Canceled)
			return result, lookupErr
		})
	require.True(t, called)
	require.False(t, served)
	require.Empty(t, workspace)
	require.Empty(t, project)
	require.Empty(t, prefix)
	require.Equal(t, catalogErr, err, "catalog failure must not become false,nil/untracked")
	served, err = stack.srv.CheckoutServesCWDChecked(context.Background(), stack.worktreeRoot)
	require.NoError(t, err)
	require.True(t, served)
}

func TestCheckoutControlClassifiesOnlySupportedOperations(t *testing.T) {
	stack := newViewStack(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"mutation_status", nil, "mutation_status"},
		{"change", map[string]any{"operation": "receipt"}, "mutation_status"},
		{"change", map[string]any{"operation": "detect"}, "detect_changes"},
		{"workspace_admin", map[string]any{"operation": "reindex"}, "reindex_repository"},
		{"edit", map[string]any{"target": map[string]any{"file": "edit.go"}, "match": "a", "replacement": "b"}, ""},
		{"change", map[string]any{"operation": "impact"}, ""},
		{"search_symbols", nil, ""},
	} {
		req := mcp.CallToolRequest{}
		req.Params.Name, req.Params.Arguments = tc.name, tc.args
		require.Equal(t, tc.want, stack.srv.checkoutControlOperation(&req), tc.name)
	}
}

func TestCheckoutControlDoesNotRelaxExactAutoCWDQueries(t *testing.T) {
	// Before the boundary guard, auto-CWD ignored require_exact and invoked
	// the leaf successfully with an exact:false base fallback.
	for _, tc := range []struct {
		name, operation string
		args            map[string]any
	}{
		{"legacy-source", "get_symbol", map[string]any{"require_exact": true}},
		{"facade-source", "read", map[string]any{"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::New"}, "require_exact": true}},
		{"facade-search-options", "search", map[string]any{"operation": "symbols", "query": "New", "options": map[string]any{"require_exact": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.operation != "get_symbol" {
				t.Setenv("GORTEX_TOOLS", "facade-v1")
			}
			stack := newViewStack(t)
			require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
			ran := false
			res, err := stack.callWithView(t, stack.worktreeRoot, tc.operation, tc.args,
				func(context.Context) (*mcp.CallToolResult, error) {
					ran = true
					return mcp.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeViewBuilding)
			require.False(t, ran)
		})
	}
}

func TestDetectChangesUsesSelectedCheckoutAndLabelsPendingGraph(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "ready"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			for _, dir := range []string{stack.repoRoot, stack.worktreeRoot} {
				checkoutMutationGit(t, dir, "init", "--initial-branch=main")
				if dir == stack.worktreeRoot {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "edit.go"), []byte(primitiveWorktreeSource), 0o644))
				}
				checkoutMutationGit(t, dir, "add", "-A")
				checkoutMutationGit(t, dir, "commit", "-m", "before")
			}
			require.NoError(t, os.WriteFile(filepath.Join(stack.repoRoot, "keep.go"), []byte("package repo\nfunc PrimaryOnlyChanged() {}\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"), []byte("package repo\n\nfunc New() { println(\"changed\") }\n"), 0o644))
			if pending {
				require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
			}
			args := worktreeViewArgs()
			args["scope"] = "unstaged"
			res, err := stack.callWithView(t, stack.repoRoot, "detect_changes", args,
				func(ctx context.Context) (*mcp.CallToolResult, error) {
					req := mcp.CallToolRequest{}
					req.Params.Name, req.Params.Arguments = "detect_changes", args
					return stack.srv.handleDetectChanges(ctx, req)
				})
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			payload := decodeFileOpsResult(t, res)
			require.Equal(t, stack.worktreeRoot, payload["repo_root"])
			files, ok := payload["changed_files"].([]any)
			require.True(t, ok)
			require.Len(t, files, 1)
			require.True(t, strings.HasSuffix(files[0].(string), "edit.go"))
			if pending {
				require.Equal(t, false, payload["complete"])
				require.Equal(t, "pending", payload["graph_status"])
				require.Equal(t, "UNKNOWN", payload["risk"])
				require.Empty(t, payload["changed_symbols"])
			} else {
				require.NotEmpty(t, payload["changed_symbols"])
			}
		})
	}
}

func BenchmarkCheckoutControlReceiptWhileGraphPending(b *testing.B) {
	fixture := newRealCheckoutMutationFixture(b)
	require.NoError(b, fixture.store.Catalog().DeleteCheckoutRoute(context.Background(), fixture.checkoutID))
	ctx := WithSessionCWD(WithSessionID(context.Background(), "benchmark-checkout-control"), fixture.primary)
	handler := fixture.srv.wrapControlToolHandler(fixture.srv.handleMutationStatus)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := mcp.CallToolRequest{}
		req.Params.Name = "mutation_status"
		req.Params.Arguments = map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": fixture.checkoutID}}
		result, err := handler(ctx, req)
		if err != nil || result == nil || result.IsError {
			b.Fatalf("receipt control failed: result=%+v error=%v", result, err)
		}
	}
}
