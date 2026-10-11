package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/semantic"
)

func freshReadFixtureContext(stack *viewStack) context.Context {
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	// viewStack's hand-authored layer nodes carry a repo prefix but no workspace
	// label. Bind this fixture to that repo so source reads exercise the handler
	// instead of being refused by an unrelated missing workspace attribute.
	sess := stack.srv.sessionFor(ctx)
	sess.mu.Lock()
	sess.scopeResolved = true
	sess.scopeCWD = stack.worktreeRoot
	sess.scopeBound = true
	sess.scopeRepoPrefix = "repo"
	sess.scopeRepoAllow = map[string]bool{"repo": true}
	sess.mu.Unlock()
	return ctx
}

// The actual facade and traversal handler run before a witnessed publication
// withdraws the first answer. This reproduces the release callers refusal.
func TestFreshReadFacadesReassembleAfterWitnessedBaseChange(t *testing.T) {
	for _, operation := range []string{"callers", "dependencies", "dependents", "call_chain", "file", "source"} {
		t.Run(operation, func(t *testing.T) {
			stack, advance := freshSymbolFixture(t)
			stack.srv.NoteSessionToolPolicy(viewTestSession, FacadeSurfaceVersion, "")
			// Configured semantic enrichment remains safe: this linked checkout
			// has no writable base output and never caches a denied confirmation.
			stack.srv.semanticMgr = &semantic.Manager{}
			require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "keep.go"), []byte("package repo\n\nfunc Keeper() {}\n"), 0o644))
			facade := "relations"
			target := map[string]any{"symbol": "repo/keep.go::Keeper"}
			if operation == "call_chain" {
				facade = "trace"
			}
			if operation == "file" || operation == "source" {
				facade = "read"
			}
			if operation == "file" {
				target = map[string]any{"file": "keep.go"}
			}
			req := makeReq(facade, map[string]any{
				"operation": operation, "target": target,
				"options": map[string]any{requireExactArgName: true, requireFreshArgName: true},
				"output":  map[string]any{"format": "json"},
			})
			if operation == "file" {
				// A graph-dependent keep rule selects the whole-view path;
				// source-only file reads do not depend on a base witness.
				req.GetArguments()["context"] = map[string]any{"keep": "Keeper"}
			}
			calls := 0
			var views []*requestView
			h := stack.srv.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				view := requestViewFromContext(ctx)
				if view == nil || !view.basePin.Witnessed() || !view.readsOwnCheckout() {
					return nil, errors.New("fixture lacks witnessed request-owned checkout")
				}
				views = append(views, view)
				result, err := stack.srv.handleFacade(ctx, facade, req)
				if err != nil || result == nil || result.IsError {
					return result, err
				}
				if operation == "file" || operation == "source" {
					if got := stack.srv.tokenStatsFor(ctx).snapshot()["calls_counted"]; got != int64(0) {
						return nil, fmt.Errorf("unaccepted read booked savings: %v", got)
					}
					if len(stack.srv.sessionFor(ctx).snapshot()["viewed_files"].([]string)) != 0 {
						return nil, errors.New("unaccepted read booked file consumption")
					}
				}
				if calls == 1 {
					if err := advance(); err != nil {
						return nil, err
					}
				}
				return result, nil
			})
			ctx := freshReadFixtureContext(stack)
			result, err := h(ctx, req)
			require.NoError(t, err)
			require.False(t, result.IsError, viewResultText(t, result))
			require.Equal(t, 2, calls)
			require.NotSame(t, views[0], views[1])
			require.Equal(t, true, resultFreshness(t, result)["exact"])
			require.Zero(t, stack.leases.Held())
			if facade == "read" {
				require.Equal(t, int64(1), stack.srv.tokenStatsFor(ctx).snapshot()["calls_counted"])
				require.Equal(t, []string{"repo/keep.go"}, stack.srv.sessionFor(ctx).snapshot()["viewed_files"])
			}
		})
	}
}

func TestFreshReadRetryKeepsOriginalDeadlineAndCancellation(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelCaller), func(t *testing.T) {
			srv, _ := setupTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadline := time.Now().Add(100 * time.Millisecond)
			req := makeReq("relations", map[string]any{"operation": "callers", "target": map[string]any{"symbol": "target"},
				"options": map[string]any{requireExactArgName: true, requireFreshArgName: true, waitDeadlineArgName: deadline.Format(time.RFC3339Nano)}})
			calls := 0
			h := srv.retryFreshSymbolSearch(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				if got := takeRequestFreshness(&req).effectiveDeadline(time.Now().Add(time.Hour), ctx); !got.Equal(deadline) {
					return nil, errors.New("relations retry renewed original deadline")
				}
				ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).withdrawn = true
				if cancelCaller {
					cancel()
				}
				return mcplib.NewToolResultError("base_changed"), nil
			})
			result, err := h(ctx, req)
			if cancelCaller {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				require.True(t, result.IsError)
				require.Greater(t, calls, 1)
				require.False(t, time.Now().Before(deadline))
			}
		})
	}
}

func TestFreshReadRetryExcludesCursorMutationAndSessionControl(t *testing.T) {
	for _, name := range []string{"cursor", "mutation", "session_control", "ordinary_error", "go_error", "route_moved", "exact_only"} {
		t.Run(name, func(t *testing.T) {
			stack, advance := freshSymbolFixture(t)
			tool := "get_callers"
			args := map[string]any{"id": "repo/edit.go::New", requireExactArgName: true, requireFreshArgName: true}
			if name == "mutation" {
				tool = "write_file"
			}
			if name == "cursor" {
				args["cursor"] = "existing"
			}
			if name == "exact_only" {
				delete(args, requireFreshArgName)
			}
			calls := 0
			_, err := stack.callWithView(t, stack.worktreeRoot, tool, args, func(ctx context.Context) (*mcplib.CallToolResult, error) {
				calls++
				if name == "session_control" {
					ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).replayUnsafe = true
				}
				if name == "route_moved" {
					view := requestViewFromContext(ctx)
					view.mu.Lock()
					err := view.rider.MarkFallback(view.rider.ActualView, "route_moved")
					view.mu.Unlock()
					if err != nil {
						return nil, err
					}
				} else if err := advance(); err != nil {
					return nil, err
				}
				if name == "ordinary_error" {
					return mcplib.NewToolResultError("validation failed"), nil
				}
				if name == "go_error" {
					return nil, errors.New("handler failed")
				}
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
			if name == "go_error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if name == "mutation" {
				require.Zero(t, calls, "strict linked-checkout gate must refuse a write before dispatch")
			} else {
				require.Equal(t, 1, calls)
			}
			require.Zero(t, stack.leases.Held())
		})
	}
}

func TestFreshReadDoesNotReplayConsumedLocalizationAllowance(t *testing.T) {
	stack, advance := freshSymbolFixture(t)
	stack.srv.NoteSessionToolPolicy(viewTestSession, FacadeSurfaceVersion, "")
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "keep.go"), []byte("package repo\n\nfunc Keeper() {}\n"), 0o644))
	ctx := freshReadFixtureContext(stack)
	terminal := stack.srv.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(false, "repo/keep.go::Keeper"), "inspect Keeper")
	args := map[string]any{"operation": "source", "target": map[string]any{"symbol": "repo/keep.go::Keeper"},
		"options": map[string]any{requireExactArgName: true, requireFreshArgName: true}}
	calls := 0
	h := stack.srv.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		result, err := stack.srv.handleFacade(ctx, "read", req)
		if err != nil || result == nil || result.IsError {
			return result, err
		}
		if !ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).replayUnsafe {
			return nil, errors.New("finite localization read was marked replayable")
		}
		if err := advance(); err != nil {
			return nil, err
		}
		return result, nil
	})
	result, err := h(ctx, makeReq("read", args))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, viewResultText(t, result), "base_changed")
	require.Equal(t, 1, calls, "a finite localization permission must not be consumed twice")
	require.Equal(t, localizationStateAnswerReady, terminal.state)
	require.Zero(t, terminal.readReservationToken)
	require.Equal(t, int64(0), stack.srv.tokenStatsFor(ctx).snapshot()["calls_counted"])
	require.Zero(t, stack.leases.Held())
}
