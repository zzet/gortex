package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func consistencyRequest(args map[string]any) *mcplib.CallToolRequest {
	req := &mcplib.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func TestTakeViewConsistencyRequest(t *testing.T) {
	deadline := time.Date(2030, 4, 5, 6, 7, 8, 123, time.UTC)
	args := map[string]any{
		requireExactArgName: true,
		requireFreshArgName: "true",
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
		"query":             "kept",
	}
	want, err := takeViewConsistencyRequest(consistencyRequest(args))
	if err != nil {
		t.Fatalf("take consistency: %v", err)
	}
	if !want.requireExact || !want.requireFresh || !want.hasDeadline || !want.waitDeadline.Equal(deadline) {
		t.Fatalf("parsed consistency = %+v", want)
	}
	if len(args) != 1 || args["query"] != "kept" {
		t.Fatalf("middleware arguments were not stripped precisely: %#v", args)
	}
}

func TestTakeViewConsistencyRequestRejectsMalformedValues(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"exact":    {requireExactArgName: 1},
		"fresh":    {requireFreshArgName: "sometimes"},
		"deadline": {waitDeadlineArgName: "tomorrow"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := takeViewConsistencyRequest(consistencyRequest(args))
			if graphview.CodeOf(err) != graphview.CodeInvalidViewSelector {
				t.Fatalf("error = %v, want %s", err, graphview.CodeInvalidViewSelector)
			}
		})
	}
}

func TestRequireExactRefusesLabeledBuildingFallbackWithMetadata(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	ran := false
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireExactArgName: true},
		func(context.Context) (*mcplib.CallToolResult, error) {
			ran = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeViewBuilding)
	if ran {
		t.Fatal("an inexact building fallback reached the handler")
	}
	if res.Meta == nil {
		t.Fatal("refusal omitted response metadata")
	}
	freshness, _ := res.Meta.AdditionalFields["freshness"].(map[string]any)
	if freshness["exact"] != false || freshness["fallback_reason"] != graphview.CodeViewBuilding ||
		freshness["actual_view"] != string(graphview.SelectorBase) {
		t.Fatalf("refusal freshness = %#v", freshness)
	}
	if _, ok := freshness["error"].(string); !ok {
		t.Fatalf("refusal omitted coded error: %#v", freshness)
	}
}

func TestRequireExactWaitsForPublishedRoute(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	published := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		ctx := context.Background()
		route, found, err := stack.store.Catalog().GetCheckoutRoute(ctx, viewTestWorktree)
		if err == nil && !found {
			err = store_sqlite.ErrCatalogNotFound
		}
		if err == nil {
			err = stack.store.Catalog().FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
				CheckoutID: viewTestWorktree, ExpectedRouteEpoch: route.RouteEpoch,
				GraphID: stack.graphID, CommitGenerationID: stack.commit,
				DirtyGenerationID: stack.dirty, State: store_sqlite.RouteActive,
			})
		}
		published <- err
	}()

	deadline := time.Now().Add(2 * time.Second).Format(time.RFC3339Nano)
	ran := false
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireExactArgName: true, waitDeadlineArgName: deadline},
		func(context.Context) (*mcplib.CallToolResult, error) {
			ran = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if publishErr := <-published; publishErr != nil {
		t.Fatalf("publish route: %v", publishErr)
	}
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError || !ran {
		t.Fatalf("waited request did not reach exact handler: %+v", res)
	}
	rider := resultFreshness(t, res)
	if rider["exact"] != true || rider["checkout_id"] != viewTestWorktree ||
		rider["view_id"] != viewTestWorktree || rider["active_generation_id"] != float64(stack.dirty) ||
		rider["view_fingerprint"] == "" {
		t.Fatalf("exact rider after wait = %#v", rider)
	}
	after := viewmetrics.Read()
	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 1 {
		t.Fatalf("waited request recorded %d terminal worktree selections, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 0 {
		t.Fatalf("intermediate fallbacks were recorded as served %d times", got)
	}
	if got := fallbackDelta(before, after, graphview.CodeViewBuilding); got != 0 {
		t.Fatalf("intermediate building fallbacks were recorded %d times", got)
	}
}

func TestPastWaitDeadlineAttemptsOnceAndReturnsViewBuilding(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols", map[string]any{
		requireExactArgName: true,
		waitDeadlineArgName: time.Now().Add(-time.Second).Format(time.RFC3339Nano),
	}, func(context.Context) (*mcplib.CallToolResult, error) {
		t.Fatal("expired strict wait reached handler")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeViewBuilding)
	if res.Meta == nil {
		t.Fatal("timed-out refusal omitted metadata")
	}
	freshness, _ := res.Meta.AdditionalFields["freshness"].(map[string]any)
	if freshness["exact"] != false || freshness["requested_state"] != "building" || freshness["error"] == "" {
		t.Fatalf("timed-out refusal metadata = %#v", freshness)
	}
}

func TestViewConsistencyWaitHonorsCancellation(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	ctx, cancel := context.WithCancel(WithSessionCWD(context.Background(), stack.worktreeRoot))
	done := make(chan error, 1)
	go func() {
		view, err := stack.srv.resolveRequestViewConsistently(ctx,
			graphview.Selector{Kind: graphview.SelectorAuto},
			requestViewPolicy{allowBuildingBaseFallback: true},
			requestViewConsistency{requireExact: true, hasDeadline: true, waitDeadline: time.Now().Add(5 * time.Second)},
		)
		if view != nil {
			view.close()
		}
		done <- err
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("view wait did not stop promptly after cancellation")
	}
}

func TestRequireExactDoesNotProbeFilesystemFreshness(t *testing.T) {
	stack := newViewStack(t)
	if err := os.WriteFile(filepath.Join(stack.worktreeRoot, "unpublished.go"), []byte("package repo\n"), 0o644); err != nil {
		t.Fatalf("write unpublished worktree file: %v", err)
	}
	probes := 0
	ctx := WithSessionCWD(context.Background(), stack.worktreeRoot)
	view, err := stack.srv.resolveRequestViewConsistently(ctx,
		graphview.Selector{Kind: graphview.SelectorAuto},
		requestViewPolicy{allowBuildingBaseFallback: true},
		requestViewConsistency{
			requireExact: true,
			freshnessProbe: func(context.Context, string) (indexer.CheckoutFreshness, error) {
				probes++
				return indexer.CheckoutFreshness{Building: true}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("require_exact selection: %v", err)
	}
	defer view.close()
	if probes != 0 {
		t.Fatalf("require_exact invoked the filesystem freshness seam %d times", probes)
	}
}

func TestRequireExactAndFreshRefuseGraceImmediately(t *testing.T) {
	for _, field := range []string{requireExactArgName, requireFreshArgName} {
		t.Run(field, func(t *testing.T) {
			stack := newViewStack(t)
			stack.setWorktreeState(t, store_sqlite.CheckoutStateAvailabilityGrace)
			started := time.Now()
			res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols", map[string]any{
				field: true, waitDeadlineArgName: time.Now().Add(5 * time.Second).Format(time.RFC3339Nano),
			}, func(context.Context) (*mcplib.CallToolResult, error) {
				t.Fatal("grace refusal reached handler")
				return nil, nil
			})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeCheckoutInaccessible)
			if time.Since(started) > time.Second {
				t.Fatalf("grace refusal waited instead of failing immediately")
			}
		})
	}
}

func TestRequireFreshPublishesCurrentTrackedUntrackedAndDeletedState(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	tests := []struct {
		name   string
		query  string
		mutate func(*testing.T)
		want   string
	}{
		{
			name: "tracked edit", query: "fresh-tracked-marker", want: "repo/keep.go",
			mutate: func(t *testing.T) {
				refWriteFiles(t, stack.worktree, map[string]string{
					"keep.go": "package repo\n\n// fresh-tracked-marker\nfunc Keeper() {}\n",
				})
			},
		},
		{
			name: "untracked file", query: "fresh-untracked-marker", want: "repo/new.go",
			mutate: func(t *testing.T) {
				refWriteFiles(t, stack.worktree, map[string]string{
					"new.go": "package repo\n\n// fresh-untracked-marker\nfunc NewFile() {}\n",
				})
			},
		},
		{
			name: "deleted file", query: "func Gone", want: "",
			mutate: func(t *testing.T) {
				if err := os.Remove(filepath.Join(stack.worktree, "gone.go")); err != nil {
					t.Fatalf("remove gone.go: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := stack.dirtyGeneration(t)
			tc.mutate(t)
			req := newSearchTextRequest(tc.query)
			args := req.Params.Arguments.(map[string]any)
			args[requireFreshArgName] = true
			args[waitDeadlineArgName] = time.Now().Add(20 * time.Second).Format(time.RFC3339Nano)
			ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
			res, err := stack.srv.wrapToolHandler(stack.srv.handleSearchText)(ctx, req)
			if err != nil {
				t.Fatalf("require_fresh search: %v", err)
			}
			if res.IsError {
				t.Fatalf("require_fresh search refused: %s", viewResultText(t, res))
			}
			if current := stack.dirtyGeneration(t); current == previous {
				t.Fatalf("require_fresh returned before publishing the changed working tree")
			}
			paths := searchTextMatchPaths(t, res)
			if tc.want == "" {
				if len(paths) != 0 {
					t.Fatalf("deleted content answered from stale route: %v", paths)
				}
			} else if len(paths) != 1 || paths[0] != tc.want {
				t.Fatalf("fresh search paths = %v, want %s", paths, tc.want)
			}
		})
	}
}

func BenchmarkTakeViewConsistencyRequest(b *testing.B) {
	deadline := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	for _, bench := range []struct {
		name string
		args func() map[string]any
	}{
		{name: "absent", args: func() map[string]any { return map[string]any{"query": "x"} }},
		{name: "exact", args: func() map[string]any { return map[string]any{requireExactArgName: true} }},
		{name: "fresh", args: func() map[string]any { return map[string]any{requireFreshArgName: true} }},
		{name: "deadline", args: func() map[string]any { return map[string]any{waitDeadlineArgName: deadline} }},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			if bench.name == "absent" {
				req := consistencyRequest(bench.args())
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := takeViewConsistencyRequest(req); err != nil {
						b.Fatal(err)
					}
				}
				return
			}
			for i := 0; i < b.N; i++ {
				if _, err := takeViewConsistencyRequest(consistencyRequest(bench.args())); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
