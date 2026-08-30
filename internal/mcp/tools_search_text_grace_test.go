package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestSearchTextRemovalGraceRefusesWithoutReadingAnyWorkingCopy(t *testing.T) {
	stack := newWorktreeSearchStack(t)

	// Give the live checkout text no canonical checkout holds. The exact
	// control proves its coordinator/searcher is still available immediately
	// before grace, so the later refusal is deliberate rather than a side
	// effect of tearing the searcher down.
	previous := stack.dirtyGeneration(t)
	refWriteFiles(t, stack.worktree, map[string]string{
		"keep.go": "package repo\n\nfunc WorktreeOnly() {\n\t// zephyr-checkout-only-marker\n}\n",
	})
	stack.awaitDirtyGenerationAfter(t, previous)
	if got := stack.search(t, stack.worktree, "zephyr-checkout-only-marker"); len(got) != 1 || got[0] != "repo/keep.go" {
		t.Fatalf("concrete worktree search = %v, want repo/keep.go", got)
	}
	if got := stack.search(t, stack.primary, "zephyr-checkout-only-marker"); len(got) != 0 {
		t.Fatalf("canonical primary unexpectedly contains checkout marker: %v", got)
	}
	if got := stack.search(t, stack.primary, "func Keeper"); len(got) != 1 || got[0] != "repo/keep.go" {
		t.Fatalf("canonical primary control = %v, want repo/keep.go", got)
	}

	// A pushed buffer would replace keep.go in an ordinary request. Grace is a
	// sealed base fallback, so the refusal must be chosen before that buffer is
	// parsed or searched.
	manager := daemon.NewOverlayManager(time.Hour)
	stack.srv.SetOverlayManager(manager)
	if err := manager.RegisterWithID(wtSearchSession, ""); err != nil {
		t.Fatalf("register overlay session: %v", err)
	}
	if err := manager.Push(wtSearchSession, daemon.OverlayFile{
		Path:    "repo/keep.go",
		Content: "package repo\n\nfunc GraceBufferMustNotLeak() {}\n",
	}, nil); err != nil {
		t.Fatalf("push overlay buffer: %v", err)
	}

	ctx := context.Background()
	checkout, found, err := stack.store.Catalog().GetCheckout(ctx, stack.checkoutID)
	if err != nil {
		t.Fatalf("read routed checkout: %v", err)
	}
	if !found {
		t.Fatalf("routed checkout %q disappeared", stack.checkoutID)
	}
	if err := stack.store.Catalog().UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    checkout.CheckoutID,
		Incarnation:   checkout.Incarnation,
		State:         store_sqlite.CheckoutStateRemovalGrace,
		DesiredMode:   checkout.DesiredMode,
		EffectiveMode: checkout.EffectiveMode,
		LastSeen:      checkout.LastSeen,
	}); err != nil {
		t.Fatalf("put checkout in removal grace: %v", err)
	}

	call := func(query string) *mcplib.CallToolResult {
		t.Helper()
		req := newSearchTextRequest(query)
		req.Params.Arguments.(map[string]any)["view"] = map[string]any{
			"kind":        "worktree",
			"checkout_id": stack.checkoutID,
		}
		callCtx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
		res, err := stack.srv.wrapToolHandler(stack.srv.handleSearchText)(callCtx, req)
		if err != nil {
			t.Fatalf("grace search %q: %v", query, err)
		}
		return res
	}

	wantActual := (graphview.Selector{Kind: graphview.SelectorBase, GraphID: checkoutFamilyPrimaryGraphID(t, stack)}).String()
	for _, query := range []string{"func Keeper", "zephyr-checkout-only-marker"} {
		result := call(query)
		assertNamesTextCapability(t, result)
		body := viewResultText(t, result)
		if !strings.Contains(body, wantActual) {
			t.Errorf("grace refusal for %q did not name sealed fallback %q: %s", query, wantActual, body)
		}
		if strings.Contains(body, "GraceBufferMustNotLeak") || strings.Contains(body, "zephyr-checkout-only-marker") {
			t.Errorf("grace refusal for %q leaked checkout/buffer content: %s", query, body)
		}

		freshness := metaFreshness(t, result)
		if freshness["exact"] != false || freshness["actual_view"] != wantActual {
			t.Errorf("freshness = %v, want labeled base refusal %q", freshness, wantActual)
		}
		if freshness["actual_state"] != string(store_sqlite.CheckoutStateRemovalGrace) ||
			freshness["fallback_reason"] != string(store_sqlite.CheckoutStateRemovalGrace) {
			t.Errorf("freshness = %v, want removal-grace state and reason", freshness)
		}
		if _, claimed := freshness["base_scoped"]; claimed {
			t.Errorf("freshness = %v, refused search.text must not claim a base-scoped answer", freshness)
		}
	}

	// The compact facade is the agent-facing path. It must resolve the same
	// refusal-only grace identity before lowering to the legacy handler.
	registered := stack.srv.MCPServer().GetTool("search")
	if registered == nil {
		t.Fatal("search facade is not registered")
	}
	facadeSession := wtSearchSession + "-facade"
	stack.srv.NoteSessionClient(facadeSession, "codex", "1")
	facade := mcplib.CallToolRequest{}
	facade.Params.Name = "search"
	facade.Params.Arguments = map[string]any{
		"operation": "text",
		"query":     "func Keeper",
		"view":      map[string]any{"kind": "worktree", "checkout_id": stack.checkoutID},
	}
	facadeResult, err := registered.Handler(
		WithSessionCWD(WithSessionID(context.Background(), facadeSession), stack.worktree), facade)
	if err != nil {
		t.Fatalf("facade grace search: %v", err)
	}
	assertNamesTextCapability(t, facadeResult)
	if freshness := metaFreshness(t, facadeResult); freshness["actual_view"] != wantActual || freshness["exact"] != false {
		t.Errorf("facade freshness = %v, want labeled base refusal %q", freshness, wantActual)
	}

	// The text-search exception is intentionally narrow. A neighboring
	// filesystem read with the same selector remains strict and must be
	// rejected before its handler can observe the retired checkout.
	strict := mcplib.CallToolRequest{}
	strict.Params.Name = "read_file"
	strict.Params.Arguments = map[string]any{
		"path": "repo/keep.go",
		"view": map[string]any{"kind": "worktree", "checkout_id": stack.checkoutID},
	}
	strictRan := false
	strictResult, err := stack.srv.wrapToolHandler(func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		strictRan = true
		return mcplib.NewToolResultText(`{"unexpected":true}`), nil
	})(WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree), strict)
	if err != nil {
		t.Fatalf("strict neighbor read: %v", err)
	}
	assertToolError(t, strictResult, graphview.CodeCheckoutInaccessible)
	if strictRan {
		t.Fatal("read_file reached its handler during removal grace")
	}
}

func TestSearchTextGraceRefusalViewPolicyIsNarrow(t *testing.T) {
	stack := newRefStack(t)
	request := func(name string, args map[string]any) *mcplib.CallToolRequest {
		req := &mcplib.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		return req
	}

	for _, tc := range []struct {
		name          string
		request       *mcplib.CallToolRequest
		wantFallback  bool
		wantRefusalID bool
	}{
		{name: "legacy text", request: request("search_text", nil), wantRefusalID: true},
		{name: "facade text", request: request("search", map[string]any{"operation": "text"}), wantRefusalID: true},
		{name: "text with file target", request: request("search", map[string]any{
			"operation": "text", "target": map[string]any{"file": "repo/keep.go"},
		})},
		{name: "symbol search", request: request("search", map[string]any{"operation": "symbols"}), wantFallback: true},
		{name: "AST search", request: request("search", map[string]any{"operation": "ast"})},
		{name: "source read", request: request("read", map[string]any{"operation": "source"})},
		{name: "file edit", request: request("edit", map[string]any{"operation": "file"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := stack.srv.requestViewPolicy(tc.request)
			if policy.allowGraceBaseFallback != tc.wantFallback {
				t.Errorf("allowGraceBaseFallback = %v, want %v", policy.allowGraceBaseFallback, tc.wantFallback)
			}
			if policy.allowGraceRefusalView != tc.wantRefusalID {
				t.Errorf("allowGraceRefusalView = %v, want %v", policy.allowGraceRefusalView, tc.wantRefusalID)
			}
		})
	}
}

func checkoutFamilyPrimaryGraphID(t testing.TB, stack *worktreeSearchStack) string {
	t.Helper()
	checkout, found, err := stack.store.Catalog().GetCheckout(context.Background(), stack.checkoutID)
	if err != nil || !found {
		t.Fatalf("read checkout family: found=%v err=%v", found, err)
	}
	primary, err := stack.srv.familyPrimaryRegistration(context.Background(), checkout.FamilyID)
	if err != nil {
		t.Fatalf("read family primary: %v", err)
	}
	return primary.GraphID
}

// BenchmarkSearchTextRemovalGraceHandler measures the complete handler path
// through scope resolution and response construction. The grace case carries
// a materialized reader deliberately: it proves the handler rejects on the
// fallback identity, before consulting either a checkout or canonical text
// searcher, rather than merely timing a boolean helper.
func BenchmarkSearchTextRemovalGraceHandler(b *testing.B) {
	stack := newRefStack(b)
	req := newSearchTextRequest("func Keeper")
	baseCtx := WithSessionCWD(WithSessionID(context.Background(), "grace-search-benchmark"), stack.repo)
	rider := graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: "retired"})
	if err := rider.MarkFallback((graphview.Selector{Kind: graphview.SelectorBase, GraphID: stack.graphID}).String(),
		string(store_sqlite.CheckoutStateRemovalGrace)); err != nil {
		b.Fatal(err)
	}
	graceCtx := withRequestView(baseCtx, &requestView{
		reader:                stack.srv.graph,
		baseFallback:          true,
		suppressBufferOverlay: true,
		rider:                 rider,
	})

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "canonical_base", ctx: baseCtx},
		{name: "grace_capability_refusal", ctx: graceCtx},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := stack.srv.handleSearchText(tc.ctx, req)
				if err != nil {
					b.Fatal(err)
				}
				if tc.name == "canonical_base" && res.IsError {
					text, _ := singleTextContent(res)
					b.Fatalf("search was refused: %s", text)
				}
				if tc.name == "grace_capability_refusal" && !res.IsError {
					b.Fatal("grace search unexpectedly succeeded")
				}
			}
		})
	}
}
