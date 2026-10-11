package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

func freshSymbolFixture(t *testing.T) (*viewStack, func() error) {
	t.Helper()
	stack := newViewStack(t)
	require.NoError(t, stack.leases.RegisterRepositoryOwner(viewTestOwner()))
	authority := indexer.NewOutputGenerationAuthority(stack.leases)
	advance := func() error {
		receipt, err := authority.Begin(context.Background(), indexer.OutputEntryWatcherDirScan,
			indexer.OutputMutationTarget{Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:" + stack.repoRoot, RepoPrefix: "repo", RootPath: stack.repoRoot})
		if err != nil {
			return err
		}
		if !receipt.Witnessed() {
			return errors.New("fixture mutation lacks source witness")
		}
		return receipt.Complete()
	}
	require.NoError(t, advance())
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{answer: func(_ int, checkout, root string) (*indexer.CheckoutRefreshTicket, error) {
		return settledTicket(checkout, root, uint64(stack.dirty)), nil
	}}
	return stack, advance
}

func TestFreshSymbolSearchReassemblesAfterWitnessedBaseChange(t *testing.T) {
	stack, advance := freshSymbolFixture(t)
	calls := 0
	var views []*requestView
	args := map[string]any{"query": "Fresh", "view": map[string]any{"kind": "auto"}, requireExactArgName: true, requireFreshArgName: true}
	result, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols", args, func(ctx context.Context) (*mcplib.CallToolResult, error) {
		calls++
		view := requestViewFromContext(ctx)
		if view == nil || !view.basePin.Witnessed() {
			return nil, errors.New("missing actual base source witness")
		}
		views = append(views, view)
		if calls == 1 {
			if err := advance(); err != nil {
				return nil, err
			}
		}
		return mcplib.NewToolResultText(fmt.Sprintf(`{"attempt":%d}`, calls)), nil
	})
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	require.Equal(t, 2, calls)
	require.NotSame(t, views[0], views[1], "retry must select a new view")
	require.Contains(t, viewResultText(t, result), `"attempt":2`)
	require.Equal(t, true, resultFreshness(t, result)["exact"])
	require.Zero(t, stack.leases.Held(), "both attempt lifetimes must end")
	require.Equal(t, map[string]any{"kind": "auto"}, args["view"], "selection must not mutate the original request")
}

func TestFreshSymbolSearchPersistentDriftAndCancellationStayBounded(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelCaller), func(t *testing.T) {
			srv, _ := setupTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadline := time.Now().Add(120 * time.Millisecond)
			req := makeReq("search_symbols", map[string]any{requireExactArgName: true, requireFreshArgName: true, waitDeadlineArgName: deadline.Format(time.RFC3339Nano)})
			calls := 0
			h := srv.retryFreshSymbolSearch(func(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				if got := takeRequestFreshness(&request).effectiveDeadline(time.Now().Add(time.Hour), ctx); !got.Equal(deadline) {
					return nil, errors.New("retry renewed its deadline")
				}
				ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).withdrawn = true
				if cancelCaller {
					cancel()
				}
				return mcplib.NewToolResultError("witnessed base_changed"), nil
			})
			result, err := h(ctx, req)
			if cancelCaller {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				require.True(t, result.IsError)
				require.Greater(t, calls, 1)
				require.False(t, time.Now().Before(deadline), "persistent drift ended without exhausting its fixed ceiling")
			}
		})
	}
}

func TestFreshSymbolSearchNeverReplaysOtherOutcomes(t *testing.T) {
	for _, name := range []string{"tool_error", "go_error", "route_moved", "write", "continuation", "exact_only"} {
		t.Run(name, func(t *testing.T) {
			stack, advance := freshSymbolFixture(t)
			tool := "search_symbols"
			args := map[string]any{"query": "Fresh", requireExactArgName: true, requireFreshArgName: true}
			if name == "write" {
				tool = "unknown_write"
			} // no broad unknown-tool read-only inference
			if name == "continuation" {
				args["cursor"] = "existing_cursor"
			}
			if name == "exact_only" {
				delete(args, requireFreshArgName)
			}
			calls := 0
			_, _ = stack.callWithView(t, stack.worktreeRoot, tool, args, func(ctx context.Context) (*mcplib.CallToolResult, error) {
				calls++
				if name == "route_moved" {
					view := requestViewFromContext(ctx)
					view.mu.Lock()
					defer view.mu.Unlock()
					if err := view.rider.MarkFallback(view.rider.ActualView, "route_moved"); err != nil {
						return nil, err
					}
				} else if err := advance(); err != nil {
					return nil, err
				}
				if name == "tool_error" {
					return mcplib.NewToolResultError("validation failed"), nil
				}
				if name == "go_error" {
					return nil, errors.New("handler failed")
				}
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
			require.Equal(t, 1, calls, "only actual successful base-change assembly can replay")
			require.Zero(t, stack.leases.Held())
		})
	}
}

func TestFreshSymbolFirstPagesRemainProvisionalAndKeepContinuationIdentity(t *testing.T) {
	for _, explicitDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(explicitDeadline), func(t *testing.T) {
			stack, advance := freshSymbolFixture(t)
			args := map[string]any{"query": "Fresh", "limit": 1, "max_bytes": 0, requireExactArgName: true, requireFreshArgName: true}
			if explicitDeadline {
				args[waitDeadlineArgName] = time.Now().Add(2 * time.Second).Format(time.RFC3339Nano)
			}
			ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
			cache := stack.srv.symbolPages(ctx)
			var rejected *symbolPageSequence
			calls := 0
			h := stack.srv.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				if req.GetString("cursor", "") != "" {
					res, err, handled := stack.srv.continueSymbolPage(ctx, req)
					if !handled {
						return nil, errors.New("not a real continuation")
					}
					return res, err
				}
				calls++
				nodes := []*graph.Node{{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"}, {ID: "repo/b.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "repo/b.go"}}
				scope, refusal := stack.srv.resolveScope(ctx, req, IntentLocate)
				if refusal != nil {
					return refusal, nil
				}
				res, err := stack.srv.publishSymbolPage(ctx, req, map[string]any{"total": 2}, nodes, nil, 2, false, scope, "Fresh")
				if err != nil {
					return nil, err
				}
				state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
				if state == nil || state.page == nil {
					return nil, errors.New("first page not provisional")
				}
				cache.mu.Lock()
				visible := len(cache.entries)
				cache.mu.Unlock()
				if visible != 0 {
					return nil, errors.New("rejected sequence became shared")
				}
				if calls == 1 {
					rejected = state.page.entry
					if err := advance(); err != nil {
						return nil, err
					}
				}
				return res, nil
			})
			req := makeReq("search_symbols", args)
			first, err := h(ctx, req)
			require.NoError(t, err)
			require.False(t, first.IsError, viewResultText(t, first))
			require.Equal(t, 2, calls)
			require.True(t, rejected.retired.Load())
			body := decodeJSONResult(t, first)
			cursor, ok := body["next_cursor"].(string)
			require.True(t, ok)
			original := cloneFreshSymbolArguments(args)
			original["cursor"] = cursor
			second, err := h(ctx, makeReq("search_symbols", original))
			require.NoError(t, err)
			require.False(t, second.IsError, viewResultText(t, second))
			require.Len(t, decodeJSONResult(t, second)["results"], 1)
			require.Zero(t, stack.leases.Held())
		})
	}
}

func TestFreshSymbolArgumentCloningAndDefaultCeiling(t *testing.T) {
	srv, _ := setupTestServer(t)
	args := map[string]any{"operation": "symbols", "query": "Fresh", "options": map[string]any{requireExactArgName: true, requireFreshArgName: true, "path": "literal/"}}
	var ceilings []time.Time
	calls := 0
	h := srv.retryFreshSymbolSearch(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		if _, limited := ctx.Deadline(); limited {
			return nil, errors.New("default freshness wait became a whole-handler deadline")
		}
		freshness := takeRequestFreshness(&req)
		ceilings = append(ceilings, freshness.effectiveDeadline(time.Now().Add(time.Hour), ctx))
		options := req.GetArguments()["options"].(map[string]any)
		if options["path"] != "literal/" {
			return nil, errors.New("nested original options were mutated")
		}
		options["path"] = "changed/"
		if calls == 1 {
			ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).withdrawn = true
		}
		return mcplib.NewToolResultText(`{"ok":true}`), nil
	})
	_, err := h(context.Background(), makeReq("search", args))
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, ceilings[0], ceilings[1])
	require.WithinDuration(t, time.Now().Add(freshnessDefaultWait), ceilings[0], time.Second)
	require.NotContains(t, args, waitDeadlineArgName)
	require.Equal(t, "literal/", args["options"].(map[string]any)["path"])
}

func TestFreshSymbolProvisionalAbortPreservesSharedDedupAndFacadeEnvelope(t *testing.T) {
	srv, _, req := symbolLifecycleServer(t)
	ctx := WithSessionID(context.Background(), "provisional-dedup")
	first, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	cursor := decodeJSONResult(t, first)["next_cursor"]
	parsed, ok := parseSymbolCursor(cursor.(string))
	require.True(t, ok)
	cache := srv.symbolPages(ctx)
	winner := cache.lookup(parsed.Sequence)
	require.NotNil(t, winner)
	for _, refuse := range []bool{true, false} {
		state := &freshSymbolAttempt{}
		checks := 0
		var loser *symbolPageSequence
		result, err := runFreshSymbolAttempt(ctx, req, state, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			_, err := srv.handleSearchSymbols(ctx, req)
			if err != nil {
				return nil, err
			}
			if state.page == nil {
				return nil, errors.New("expected provisional dedup candidate")
			}
			loser = state.page.entry
			page := state.page
			state.page = nil
			result, err := page.publish(ctx, func() *mcplib.CallToolResult {
				checks++
				if refuse && checks == 2 {
					return mcplib.NewToolResultError("base_changed after replay")
				}
				return nil
			})
			return result, err
		})
		require.NoError(t, err)
		require.Equal(t, refuse, result.IsError)
		require.Equal(t, 2, checks, "guard must follow shared-winner replay")
		require.True(t, loser.retired.Load())
		require.False(t, winner.retired.Load(), "another call's advertised cursor survives rejection")
		if !refuse {
			require.Equal(t, cursor, decodeJSONResult(t, result)["next_cursor"])
		}
	}
	req.Params.Arguments = cloneFreshSymbolArguments(req.GetArguments())
	req.GetArguments()["cursor"] = cursor
	continued, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, continued.IsError, viewResultText(t, continued))
	cache.mu.Lock()
	entries, calls := len(cache.entries), len(cache.calls)
	cache.mu.Unlock()
	require.Equal(t, 1, entries)
	require.Zero(t, calls)
}

func TestFreshSymbolTerminalSessionJoinsUnpublishedPage(t *testing.T) {
	srv, _, req := symbolLifecycleServer(t)
	ctx, cancel := context.WithTimeout(WithSessionID(context.Background(), "provisional-close"), 5*time.Second)
	defer cancel()
	cache := srv.symbolPages(ctx)
	state := &freshSymbolAttempt{}
	prepared := make(chan *symbolPageSequence, 1)
	done := make(chan error, 1)
	go func() {
		_, err := runFreshSymbolAttempt(ctx, req, state, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			result, err := srv.handleSearchSymbols(ctx, req)
			if err != nil {
				return nil, err
			}
			if state.page == nil {
				return nil, errors.New("missing pending page")
			}
			prepared <- state.page.entry
			<-state.page.ctx.Done() // terminal session cancels this owned call
			return result, state.page.ctx.Err()
		})
		done <- err
	}()
	var entry *symbolPageSequence
	select {
	case entry = <-prepared:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cache.beginClose()()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.True(t, entry.retired.Load())
	cache.mu.Lock()
	calls := len(cache.calls)
	cache.mu.Unlock()
	require.Zero(t, calls, "terminal close joins pending publication ownership")
}

func TestFreshSymbolAcceptedDedupKeepsActualFacadeIdentity(t *testing.T) {
	srv, _, _ := symbolLifecycleServer(t)
	ctx := WithSessionID(context.Background(), "provisional-facade")
	req := makeReq("search", map[string]any{"operation": "symbols", "query": "Needle", "options": map[string]any{"limit": 4, "max_bytes": 0}})
	spec, ok := srv.viewFacadeOperation(&req)
	require.True(t, ok)
	first, err := srv.invokeFacadeSpec(ctx, req, spec)
	require.NoError(t, err)
	require.False(t, first.IsError, viewResultText(t, first))
	state := &freshSymbolAttempt{}
	var candidate *symbolPageSequence
	second, err := runFreshSymbolAttempt(ctx, req, state, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		res, err := srv.invokeFacadeSpec(ctx, req, spec)
		if err != nil {
			return nil, err
		}
		if state.page == nil {
			return nil, errors.New("actual facade failed to stage its page")
		}
		candidate = state.page.entry
		return srv.commitFreshSymbolPage(ctx, res, "search", nil)
	})
	require.NoError(t, err)
	require.False(t, second.IsError, viewResultText(t, second))
	require.True(t, candidate.retired.Load(), "accepted dedup reuses the shared winner")
	require.Equal(t, decodeJSONResult(t, first), decodeJSONResult(t, second), "dedup must retain the facade's decorated envelope")
	require.Equal(t, first.Meta.AdditionalFields["gortex_facade"], second.Meta.AdditionalFields["gortex_facade"])
	require.NotNil(t, second.Meta.AdditionalFields["gortex_facade"])
}
