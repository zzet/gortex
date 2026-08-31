package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

const searchBaseOnlyID = "repo/base_only.go::BaseOnly"

func putViewWorktreeInAvailabilityGrace(t *testing.T, stack *viewStack) {
	t.Helper()
	stack.setWorktreeState(t, store_sqlite.CheckoutStateAvailabilityGrace)
	if err := stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("withdraw worktree route: %v", err)
	}
	if _, found, err := stack.store.Catalog().GetCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("read withdrawn worktree route: %v", err)
	} else if found {
		t.Fatal("availability grace retained the worktree route")
	}
}

func TestAvailabilityGraceRetainsIdentityForEligiblePrimaryFallback(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("eligible grace search: %v", err)
	}
	if res.IsError {
		t.Fatalf("eligible grace search was refused: %s", viewResultText(t, res))
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("availability grace did not fall back to the primary corpus")
	}
	if hasNode(reader, "repo/added.go::Fresh") || hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("withdrawn worktree layers leaked into the availability-grace fallback")
	}

	rider := resultFreshness(t, res)
	wantActual := "base:" + stack.graphID
	if rider["requested_view"] != "worktree:"+viewTestWorktree || rider["actual_view"] != wantActual {
		t.Errorf("rider = %v, want retained worktree request and %q answer", rider, wantActual)
	}
	if rider["exact"] != false || rider["fallback_reason"] != string(store_sqlite.CheckoutStateAvailabilityGrace) {
		t.Errorf("rider = %v, want labeled availability-grace fallback", rider)
	}
	if rider["graph_id"] != stack.graphID || rider["checkout_id"] != viewTestWorktree {
		t.Errorf("rider identity = %v, want graph %q checkout %q", rider, stack.graphID, viewTestWorktree)
	}
}

func writeSearchBaseGeneration(t *testing.T, store *store_sqlite.Store, graphID string) int64 {
	t.Helper()
	ctx := context.Background()
	_, found, err := store.Catalog().GetDedicatedGraph(ctx, graphID)
	if err != nil {
		t.Fatalf("GetDedicatedGraph(base): %v", err)
	}
	if !found {
		t.Fatalf("GetDedicatedGraph(base): graph %q not found", graphID)
	}
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_base",
		GraphID:          graphID,
		LayerID:          graphID + ":base",
		CheckoutID:       viewTestPrimary,
		GenerationKind:   "dedicated_base",
		BaseGenerationID: 0,
		TreeOID:          "tree-search-base",
		CreatedAt:        3000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(base): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/edit.go", 8),
		viewFileNode("repo/hidden.go", 6),
		viewFileNode("repo/base_only.go", 6),
		searchNode(searchOldID, "Old", "repo/edit.go", 3),
		searchNode(searchHiddenID, "Hidden", "repo/hidden.go", 3),
		searchNode(searchBaseOnlyID, "BaseOnly", "repo/base_only.go", 3),
	}, nil)
	indexSearchSymbols(t, handle, map[string]string{
		searchOldID:      "old zephyr scheduler",
		searchHiddenID:   "hidden zephyr scheduler",
		searchBaseOnlyID: "base-only zephyr scheduler",
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/edit.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/hidden.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/base_only.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(base): %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration(base): %v", err)
	}
	return generationID
}

func TestRemovalGraceSearchUsesPrimaryGenerationStack(t *testing.T) {
	v := newSearchViewStack(t)
	baseGenerationID := writeSearchBaseGeneration(t, v.store, v.graphID)
	dedicated, found, err := v.store.Catalog().GetDedicatedGraph(context.Background(), v.graphID)
	if err != nil {
		t.Fatalf("GetDedicatedGraph(activate base): %v", err)
	}
	if !found {
		t.Fatalf("GetDedicatedGraph(activate base): graph %q not found", v.graphID)
	}
	dedicated.ActiveGenerationID = baseGenerationID
	if err := v.store.Catalog().UpsertDedicatedGraph(context.Background(), dedicated); err != nil {
		t.Fatalf("UpsertDedicatedGraph(activate base): %v", err)
	}
	v.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)

	for _, selection := range []struct {
		name string
		cwd  string
		args func() map[string]any
	}{
		{name: "explicit selector", cwd: v.repoRoot, args: routedArgs},
		{name: "session cwd", cwd: v.worktreeRoot, args: func() map[string]any { return nil }},
	} {
		t.Run(selection.name, func(t *testing.T) {
			res, err := v.callWithView(t, selection.cwd, "search_symbols", selection.args(),
				func(ctx context.Context) (*mcplib.CallToolResult, error) {
					view := requestViewFromContext(ctx)
					if view == nil || view.materialized == nil || !view.baseFallback {
						t.Fatalf("removal grace did not materialize the labeled primary base: %+v", view)
					}
					if !v.leases.InUse(baseGenerationID) {
						t.Fatal("primary base lease is not held during grace request")
					}
					return v.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
				})
			if err != nil {
				t.Fatalf("removal-grace search: %v", err)
			}
			if res.IsError {
				t.Fatalf("removal-grace search was refused: %s", viewResultText(t, res))
			}
			if v.leases.InUse(baseGenerationID) {
				t.Fatal("primary base lease survived grace request completion")
			}
			body := singleTextOrFail(t, res)
			for _, id := range []string{searchOldID, searchHiddenID} {
				if !strings.Contains(body, id) {
					t.Errorf("removal-grace primary fallback omitted base symbol %q: %s", id, body)
				}
			}
			for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
				if strings.Contains(body, id) {
					t.Errorf("removal-grace primary fallback leaked worktree symbol %q: %s", id, body)
				}
			}

			freshness := resultFreshness(t, res)
			wantActual := "base:" + v.graphID
			if freshness["requested_view"] != "worktree:"+viewTestWorktree || freshness["actual_view"] != wantActual {
				t.Errorf("freshness = %v, want retained worktree request and %q answer", freshness, wantActual)
			}
			if freshness["fallback_reason"] != string(store_sqlite.CheckoutStateRemovalGrace) {
				t.Errorf("freshness = %v, want labeled removal-grace fallback", freshness)
			}
			baseScoped, ok := freshness["base_scoped"].([]any)
			if !ok {
				t.Fatalf("freshness = %v, want base_scoped capability list", freshness)
			}
			searchScoped := false
			for _, capability := range baseScoped {
				if capability == string(graphview.CapSearchSymbols) {
					searchScoped = true
					break
				}
			}
			if !searchScoped {
				t.Errorf("freshness = %v, want %q marked base-scoped", freshness, graphview.CapSearchSymbols)
			}
		})
	}
}

func TestAvailabilityGraceKeepsExactFileAndEditPoliciesStrict(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)

	for _, selection := range []struct {
		name string
		cwd  string
		args func() map[string]any
	}{
		{name: "explicit selector", cwd: stack.repoRoot, args: worktreeViewArgs},
		{name: "session cwd", cwd: stack.worktreeRoot, args: func() map[string]any { return nil }},
	} {
		t.Run(selection.name, func(t *testing.T) {
			for _, tool := range []string{"get_symbol", "read_file", "edit_file"} {
				t.Run(tool, func(t *testing.T) {
					ran := false
					res, err := stack.callWithView(t, selection.cwd, tool, selection.args(),
						func(context.Context) (*mcplib.CallToolResult, error) {
							ran = true
							return mcplib.NewToolResultText(`{"ok":true}`), nil
						})
					if err != nil {
						t.Fatalf("call: %v", err)
					}
					assertToolError(t, res, graphview.CodeCheckoutInaccessible)
					if ran {
						t.Errorf("%s reached its handler during availability grace", tool)
					}
				})
			}
		})
	}
}

func TestForgottenGraceCheckoutIDDoesNotCaptureBaseRequests(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)
	if err := stack.store.Catalog().DeleteCheckout(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("forget expired checkout: %v", err)
	}

	ran := false
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		func(context.Context) (*mcplib.CallToolResult, error) {
			ran = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("stale worktree selector: %v", err)
	}
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if ran {
		t.Fatal("a forgotten checkout ID reached the search handler")
	}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "default"},
		{name: "base", args: map[string]any{"view": map[string]any{"kind": "base", "graph_id": stack.graphID}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader graph.Reader
			res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", tc.args,
				captureReader(stack.srv, &reader))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.IsError {
				t.Fatalf("surviving %s selection was refused: %s", tc.name, viewResultText(t, res))
			}
			if !hasNode(reader, "repo/edit.go::Old") {
				t.Errorf("surviving %s selection did not read the primary corpus", tc.name)
			}
			if hasNode(reader, "repo/added.go::Fresh") {
				t.Errorf("surviving %s selection read the forgotten worktree layer", tc.name)
			}
		})
	}
}
