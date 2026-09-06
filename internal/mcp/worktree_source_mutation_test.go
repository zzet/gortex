package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// Handler tests use the published view fixture and an observable mutation
// lease. The coordinator integration below exercises the actual publication
// path; these cases pin that every primitive honors its lease at the disk seam.
type primitiveCheckoutMutation struct {
	prepared   int
	refreshed  int
	prepareErr error
	refreshErr error
	cycle      indexer.CheckoutCycle
}

func (m *primitiveCheckoutMutation) Prepare(context.Context) error {
	m.prepared++
	return m.prepareErr
}

func (m *primitiveCheckoutMutation) Refresh(context.Context) (indexer.CheckoutCycle, error) {
	m.refreshed++
	return m.cycle, m.refreshErr
}

const primitiveWorktreeSource = "package repo\n\nfunc New() {}\n"

func callCheckoutPrimitive(
	t *testing.T,
	stack *viewStack,
	cwd string,
	viewArgs map[string]any,
	tool string,
	args map[string]any,
	mutation *primitiveCheckoutMutation,
) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	// Acquire the fixture's genuine routed reader, then supply the mutation
	// authority at the handler boundary. This deliberately separates primitive
	// behavior from middleware admission, which the real lifecycle tests cover.
	res, err := stack.callWithView(t, cwd, "get_symbol", viewArgs,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			ctx = withCheckoutMutation(ctx, mutation, stack.worktreeRoot)
			switch tool {
			case "edit_file":
				return stack.srv.handleEditFile(ctx, req)
			case "write_file":
				return stack.srv.handleWriteFile(ctx, req)
			case "edit_symbol":
				return stack.srv.handleEditSymbol(ctx, req)
			default:
				t.Fatalf("unsupported primitive test tool %q", tool)
				return nil, nil
			}
		})
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func TestCheckoutSourcePrimitivesHonorDryRunAndIsolateDiskWrites(t *testing.T) {
	for _, tool := range []string{"edit_file", "write_file"} {
		for _, selection := range []string{"cwd", "explicit"} {
			for _, dryRun := range []bool{true, false} {
				name := tool + "/" + selection + "/write"
				if dryRun {
					name = tool + "/" + selection + "/dry_run"
				}
				t.Run(name, func(t *testing.T) {
					stack := newViewStack(t)
					primaryFile := filepath.Join(stack.repoRoot, "edit.go")
					primaryBefore, err := os.ReadFile(primaryFile)
					require.NoError(t, err)
					worktreeFile := filepath.Join(stack.worktreeRoot, "edit.go")
					require.NoError(t, os.WriteFile(worktreeFile, []byte(primitiveWorktreeSource), 0o644))
					mutation := &primitiveCheckoutMutation{cycle: indexer.CheckoutCycle{
						CommitGenerationID: stack.commit,
						DirtyGenerationID:  stack.dirty,
					}}
					cwd := stack.worktreeRoot
					var viewArgs map[string]any
					if selection == "explicit" {
						cwd = stack.repoRoot
						viewArgs = worktreeViewArgs()
					}
					const changed = "package repo\n\nfunc AfterCheckoutEdit() {}\n"
					args := map[string]any{"path": "repo/edit.go", "dry_run": dryRun}
					if tool == "edit_file" {
						args["old_string"] = "func New() {}"
						args["new_string"] = "func AfterCheckoutEdit() {}"
					} else {
						args["content"] = changed
					}
					res := callCheckoutPrimitive(t, stack, cwd, viewArgs, tool, args, mutation)
					require.False(t, res.IsError, viewResultText(t, res))
					payload := decodeFileOpsResult(t, res)
					gotPrimary, err := os.ReadFile(primaryFile)
					require.NoError(t, err)
					require.Equal(t, primaryBefore, gotPrimary, "the canonical checkout must not be edited")
					gotWorktree, err := os.ReadFile(worktreeFile)
					require.NoError(t, err)
					if dryRun {
						require.Equal(t, primitiveWorktreeSource, string(gotWorktree))
						require.Zero(t, mutation.prepared, "preview must not invalidate the checkout route")
						require.Zero(t, mutation.refreshed, "preview must not rebuild the checkout")
						require.Equal(t, true, payload["dry_run"])
						require.Equal(t, false, payload["reindexed"])
					} else {
						require.Equal(t, changed, string(gotWorktree))
						require.Equal(t, 1, mutation.prepared)
						require.Equal(t, 1, mutation.refreshed)
						require.Equal(t, true, payload["reindexed"])
					}
				})
			}
		}
	}
}

func TestCheckoutSourcePrimitiveRejectsForeignCheckoutBeforePrepare(t *testing.T) {
	for _, target := range []string{"primary", "sibling"} {
		t.Run(target, func(t *testing.T) {
			stack := newViewStack(t)
			path := filepath.Join(stack.repoRoot, "edit.go")
			if target == "sibling" {
				path = filepath.Join(stack.otherRoot, "other.go")
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			mutation := &primitiveCheckoutMutation{}
			res := callCheckoutPrimitive(t, stack, stack.worktreeRoot, nil, "write_file",
				map[string]any{"path": path, "content": "must not land"}, mutation)
			require.True(t, res.IsError)
			require.Zero(t, mutation.prepared)
			require.Zero(t, mutation.refreshed)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestCheckoutSourceSymbolPrimitiveUsesSelectedPhysicalSpan(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		name := "write"
		if dryRun {
			name = "dry_run"
		}
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			const before = "package repo\n\nfunc New() {\n}\n"
			path := filepath.Join(stack.worktreeRoot, "edit.go")
			require.NoError(t, os.WriteFile(path, []byte(before), 0o644))
			primaryBefore, err := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
			require.NoError(t, err)
			mutation := &primitiveCheckoutMutation{cycle: indexer.CheckoutCycle{
				CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty,
			}}
			res := callCheckoutPrimitive(t, stack, stack.repoRoot, worktreeViewArgs(), "edit_symbol",
				map[string]any{
					"id": "repo/edit.go::New", "old_source": "func New() {\n}",
					"new_source": "func ChangedBySymbol() {\n}", "dry_run": dryRun,
				}, mutation)
			require.False(t, res.IsError, viewResultText(t, res))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			if dryRun {
				require.Equal(t, before, string(after))
				require.Zero(t, mutation.prepared)
				require.Zero(t, mutation.refreshed)
			} else {
				require.Equal(t, "package repo\n\nfunc ChangedBySymbol() {\n}\n", string(after))
				require.Equal(t, 1, mutation.prepared)
				require.Equal(t, 1, mutation.refreshed)
				require.Equal(t, true, decodeFileOpsResult(t, res)["reindexed"])
			}
			primaryAfter, err := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primaryBefore, primaryAfter)
		})
	}
}

func TestCheckoutSourcePrimitivePrepareFailurePreservesFile(t *testing.T) {
	stack := newViewStack(t)
	path := filepath.Join(stack.worktreeRoot, "edit.go")
	require.NoError(t, os.WriteFile(path, []byte(primitiveWorktreeSource), 0o644))
	mutation := &primitiveCheckoutMutation{prepareErr: errors.New("checkout route changed")}
	res := callCheckoutPrimitive(t, stack, stack.worktreeRoot, nil, "edit_file",
		map[string]any{"path": path, "old_string": "New", "new_string": "Changed"}, mutation)
	require.True(t, res.IsError)
	require.Equal(t, 1, mutation.prepared)
	require.Zero(t, mutation.refreshed)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, primitiveWorktreeSource, string(after))
}

func TestCheckoutSourcePrimitiveReportsPublicationFailureAfterDiskCommit(t *testing.T) {
	stack := newViewStack(t)
	path := filepath.Join(stack.worktreeRoot, "edit.go")
	require.NoError(t, os.WriteFile(path, []byte(primitiveWorktreeSource), 0o644))
	mutation := &primitiveCheckoutMutation{refreshErr: errors.New("injected checkout publication failure")}
	res := callCheckoutPrimitive(t, stack, stack.worktreeRoot, nil, "edit_file",
		map[string]any{"path": path, "old_string": "New", "new_string": "CommittedButUnpublished"}, mutation)
	require.Equal(t, 1, mutation.prepared)
	require.Equal(t, 1, mutation.refreshed)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(after), "CommittedButUnpublished", "failed publication must not pretend to roll disk back")
	require.Contains(t, viewResultText(t, res), "injected checkout publication failure")
	if !res.IsError {
		require.Equal(t, false, decodeFileOpsResult(t, res)["reindexed"], "disk commit is not a successful graph publication")
	}
}

func TestCheckoutSourcePrimitivesStillRequireCoordinatorForPreview(t *testing.T) {
	stack := newViewStack(t)
	for _, tool := range []string{"edit_file", "write_file", "edit_symbol"} {
		t.Run(tool, func(t *testing.T) {
			ran := false
			res, err := stack.callWithView(t, stack.worktreeRoot, tool,
				map[string]any{"dry_run": true}, func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeViewReadOnly)
			require.False(t, ran)
		})
	}
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.True(t, hasNode(reader, "repo/added.go::Fresh"))
}

func TestCheckoutSourceFallbackRemainsReadOnly(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)
	for _, dryRun := range []bool{true, false} {
		ran := false
		res, err := stack.callWithView(t, stack.worktreeRoot, "edit_file",
			map[string]any{"dry_run": dryRun}, func(context.Context) (*mcplib.CallToolResult, error) {
				ran = true
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
		require.NoError(t, err)
		assertToolError(t, res, graphview.CodeViewReadOnly)
		require.False(t, ran, "base fallback must not grant permission to edit the primary checkout")
	}
}

// realCheckoutMutationFixture uses production registration, activation,
// publication, middleware and handlers. Only its Git family and store are
// temporary: it never connects to the installed daemon.
type realCheckoutMutationFixture struct {
	srv        *Server
	store      *store_sqlite.Store
	primary    string
	worktree   string
	checkoutID string
}

func checkoutMutationGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Gortex Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Gortex Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, output)
	return strings.TrimSpace(string(output))
}

func newRealCheckoutMutationFixture(t testing.TB) *realCheckoutMutationFixture {
	return newRealCheckoutMutationFixtureWithRegistry(t, nil)
}

func newRealCheckoutMutationFixtureWithRegistry(t testing.TB, configure func(*parser.Registry)) *realCheckoutMutationFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "wt")
	require.NoError(t, os.Mkdir(primary, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(primary, "edit.go"), []byte("package repo\n\nfunc Old() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(primary, ".gortex.yaml"), []byte("workspace: checkout-mutations\n"), 0o644))
	checkoutMutationGit(t, primary, "init", "--initial-branch=main")
	checkoutMutationGit(t, primary, "add", "-A")
	checkoutMutationGit(t, primary, "commit", "-m", "base")
	checkoutMutationGit(t, primary, "worktree", "add", "-b", "feature", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "edit.go"), []byte(primitiveWorktreeSource), 0o644))

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	cfgPath := filepath.Join(base, "config.yaml")
	global := &config.GlobalConfig{}
	global.SetConfigPath(cfgPath)
	require.NoError(t, global.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	if configure != nil {
		configure(registry)
	}
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(store, registry, bm, cm, zap.NewNop())
	leases := graphview.NewLeaseManager()
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store,
		Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registered, err := lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "repo"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	overview, err := lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	checkoutID := ""
	for _, family := range overview.Families {
		for _, checkout := range family.Checkouts {
			// Git emits slash-separated worktree roots on Windows, while
			// filepath.Join uses native separators. Compare path identities.
			if pathkey.EqualPaths(checkout.RootPath, worktree) {
				checkoutID = checkout.CheckoutID
			}
		}
	}
	require.NotEmpty(t, checkoutID, "linked worktree %q must be discovered through registration: %+v", worktree, overview.Families)
	require.True(t, lifecycle.ActivateCheckout(checkoutID, "mutation-test"))
	require.Eventually(t, func() bool {
		route, found, routeErr := store.Catalog().GetCheckoutRoute(ctx, checkoutID)
		return routeErr == nil && found && route.State == store_sqlite.RouteActive &&
			route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
	}, 20*time.Second, 20*time.Millisecond, "automatic worktree did not publish its initial route")

	engine := query.NewEngine(store)
	engine.SetSearch(bm)
	srv := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer: mi, ConfigManager: cm,
	})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	srv.lifecycle = lifecycle
	return &realCheckoutMutationFixture{
		srv: srv, store: store,
		primary: primary, worktree: worktree, checkoutID: checkoutID,
	}
}

func (f *realCheckoutMutationFixture) edit(t testing.TB, cwd string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = args
	ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-mutation"), cwd)
	result, err := f.srv.wrapToolHandler(f.srv.handleEditFile)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func TestWorktreeMutationCoordinatorEndToEnd(t *testing.T) {
	for _, selection := range []string{"cwd", "checkout_id", "path"} {
		t.Run(selection, func(t *testing.T) {
			fixture := newRealCheckoutMutationFixture(t)
			primaryFile := filepath.Join(fixture.primary, "edit.go")
			primaryBefore, err := os.ReadFile(primaryFile)
			require.NoError(t, err)
			ctx := context.Background()
			before, found, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			cwd := fixture.worktree
			args := func(dryRun bool) map[string]any {
				out := map[string]any{
					"path": "repo/edit.go", "old_string": "func New() {}",
					"new_string": "func ChangedInWorktree() {}", "dry_run": dryRun,
				}
				if selection != "cwd" {
					cwd = fixture.primary
					selector := map[string]any{"kind": "worktree"}
					if selection == "path" {
						selector["path"] = fixture.worktree
					} else {
						selector["checkout_id"] = fixture.checkoutID
					}
					out["view"] = selector
				}
				return out
			}
			previewArgs := args(true)
			preview := fixture.edit(t, cwd, previewArgs)
			require.False(t, preview.IsError, viewResultText(t, preview))
			require.Equal(t, "would_apply", decodeFileOpsResult(t, preview)["status"])
			afterPreview, found, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, before, afterPreview, "preview must leave the published route untouched")
			worktreeBefore, err := os.ReadFile(filepath.Join(fixture.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primitiveWorktreeSource, string(worktreeBefore))

			writeArgs := args(false)
			written := fixture.edit(t, cwd, writeArgs)
			require.False(t, written.IsError, viewResultText(t, written))
			fixture.awaitMutation(t, cwd, written)
			afterWrite, found, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
			require.NoError(t, err)
			require.True(t, found)
			require.Greater(t, afterWrite.RouteEpoch, before.RouteEpoch)
			require.NotEqual(t, before.DirtyGenerationID, afterWrite.DirtyGenerationID)
			primaryAfter, err := os.ReadFile(primaryFile)
			require.NoError(t, err)
			require.Equal(t, primaryBefore, primaryAfter)
			worktreeAfter, err := os.ReadFile(filepath.Join(fixture.worktree, "edit.go"))
			require.NoError(t, err)
			require.Contains(t, string(worktreeAfter), "func ChangedInWorktree() {}")

			var reader graph.Reader
			readReq := mcplib.CallToolRequest{}
			readReq.Params.Name = "get_symbol"
			readReq.Params.Arguments = map[string]any{"require_exact": true}
			readCtx := WithSessionCWD(WithSessionID(ctx, "real-checkout-read-after-write"), fixture.worktree)
			readResult, err := fixture.srv.wrapToolHandler(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				reader = fixture.srv.readerFor(ctx)
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})(readCtx, readReq)
			require.NoError(t, err)
			require.False(t, readResult.IsError, viewResultText(t, readResult))
			require.Equal(t, true, resultFreshness(t, readResult)["exact"])
			require.True(t, hasNode(reader, "repo/edit.go::ChangedInWorktree"))
			require.False(t, hasNode(reader, "repo/edit.go::New"))
		})
	}
}

func TestWorktreeMutationFacadeEndToEnd(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	for _, operation := range []string{"file", "write", "symbol"} {
		for _, inferred := range []bool{false, true} {
			name := operation + "/explicit_operation"
			if inferred {
				name = operation + "/inferred_operation"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newRealCheckoutMutationFixture(t)
				primaryBefore, err := os.ReadFile(filepath.Join(fixture.primary, "edit.go"))
				require.NoError(t, err)
				tool := fixture.srv.mcpServer.GetTool("edit")
				require.NotNil(t, tool, "exercise the registered facade, including its middleware")
				ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-facade"), fixture.primary)
				const after = "package repo\n\nfunc EditedThroughFacade() {}\n"
				for _, dryRun := range []bool{true, false} {
					args := map[string]any{
						"dry_run": dryRun,
						"view":    map[string]any{"kind": "worktree", "path": fixture.worktree},
					}
					if !inferred {
						args["operation"] = operation
					}
					if operation == "symbol" {
						args["target"] = map[string]any{"symbol": "repo/edit.go::New"}
					} else {
						args["target"] = map[string]any{"file": "repo/edit.go"}
					}
					if operation == "write" {
						args["content"] = after
					} else {
						args["match"] = "func New() {}"
						args["replacement"] = "func EditedThroughFacade() {}"
					}
					req := mcplib.CallToolRequest{}
					req.Params.Name = "edit"
					req.Params.Arguments = args
					result, err := tool.Handler(ctx, req)
					require.NoError(t, err)
					require.False(t, result.IsError, viewResultText(t, result))
					body, err := os.ReadFile(filepath.Join(fixture.worktree, "edit.go"))
					require.NoError(t, err)
					if dryRun {
						require.Equal(t, primitiveWorktreeSource, string(body))
					} else {
						require.Equal(t, after, string(body))
						fixture.awaitMutation(t, fixture.primary, result)
					}
				}
				primaryAfter, err := os.ReadFile(filepath.Join(fixture.primary, "edit.go"))
				require.NoError(t, err)
				require.Equal(t, primaryBefore, primaryAfter)

				read := fixture.srv.mcpServer.GetTool("read")
				require.NotNil(t, read)
				req := mcplib.CallToolRequest{}
				req.Params.Name = "read"
				req.Params.Arguments = map[string]any{
					"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
					"view":          map[string]any{"kind": "worktree", "path": fixture.worktree},
					"require_exact": true,
				}
				result, err := read.Handler(ctx, req)
				require.NoError(t, err)
				require.False(t, result.IsError, viewResultText(t, result))
				require.Contains(t, viewResultText(t, result), "EditedThroughFacade")
				require.NotContains(t, viewResultText(t, result), "func Old()")
				require.Equal(t, true, resultFreshness(t, result)["exact"])
			})
		}
	}
}

func TestWorktreeMutationInactiveRefRemainsReadOnly(t *testing.T) {
	fixture := newRealCheckoutMutationFixture(t)
	before, err := os.ReadFile(filepath.Join(fixture.primary, "edit.go"))
	require.NoError(t, err)
	for _, dryRun := range []bool{true, false} {
		result := fixture.edit(t, fixture.primary, map[string]any{
			"path": "repo/edit.go", "old_string": "func Old() {}", "new_string": "func MustNotLand() {}",
			"dry_run": dryRun,
			"view":    map[string]any{"kind": "git_ref", "graph_id": indexer.GraphIDFor("repo"), "value": "refs/heads/main"},
		})
		assertToolError(t, result, graphview.CodeViewReadOnly)
	}
	after, err := os.ReadFile(filepath.Join(fixture.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func BenchmarkWorktreeMutationDryRun(b *testing.B) {
	fixture := newRealCheckoutMutationFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := fixture.edit(b, fixture.worktree, map[string]any{
			"path": "repo/edit.go", "old_string": "func New() {}",
			"new_string": "func PreviewOnly() {}", "dry_run": true,
		})
		if result.IsError {
			b.Fatalf("dry-run refused: %+v", result.Content)
		}
	}
}
