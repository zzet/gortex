package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func activateSearchBaseGeneration(t *testing.T, stack *viewStack) int64 {
	t.Helper()
	ctx := context.Background()
	generationID := writeSearchBaseGeneration(t, stack.store, stack.graphID)
	dedicated, found, err := stack.store.Catalog().GetDedicatedGraph(ctx, stack.graphID)
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if !found {
		t.Fatalf("dedicated graph %q is missing", stack.graphID)
	}
	dedicated.ActiveGenerationID = generationID
	if err := stack.store.Catalog().UpsertDedicatedGraph(ctx, dedicated); err != nil {
		t.Fatalf("activate dedicated base generation: %v", err)
	}
	return generationID
}

func baseViewArgs(graphID string) map[string]any {
	return map[string]any{"view": map[string]any{
		"kind": "base", "graph_id": graphID,
	}}
}

// TestBaseSelectorMaterializesActiveGeneration proves that a VNext dedicated
// base reads its published generation rather than silently answering from the
// generation-zero compatibility corpus. It also pins search-source binding,
// lease lifetime, rider identity, and metrics classification at the request
// boundary where the regression occurred.
func TestBaseSelectorMaterializesActiveGeneration(t *testing.T) {
	stack := newSearchViewStack(t)
	baseGenerationID := activateSearchBaseGeneration(t, stack)

	var reader graph.Reader
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols", baseViewArgs(stack.graphID),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(ctx)
			if view == nil || view.materialized == nil {
				t.Fatal("active dedicated base was not materialized")
			}
			if got := requestViewKind(view); got != viewmetrics.ViewBase {
				t.Fatalf("request view kind = %q, want %q", got, viewmetrics.ViewBase)
			}
			if got := view.materialized.ID.BaseGeneration; got != baseGenerationID {
				t.Fatalf("base generation = %d, want %d", got, baseGenerationID)
			}
			if !stack.leases.InUse(baseGenerationID) {
				t.Fatal("base generation lease is not held during the request")
			}
			reader = stack.srv.readerFor(ctx)
			return stack.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("base-selected search: %v", err)
	}
	if res.IsError {
		t.Fatalf("base-selected search was refused: %s", viewResultText(t, res))
	}
	if stack.leases.InUse(baseGenerationID) {
		t.Fatal("base generation lease survived request completion")
	}
	if !hasNode(reader, searchBaseOnlyID) {
		t.Error("generation-only base symbol is absent from graph traversal")
	}
	for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
		if hasNode(reader, id) {
			t.Errorf("base selector leaked worktree symbol %q", id)
		}
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, searchBaseOnlyID) {
		t.Fatalf("generation-owned FTS row is absent from search: %s", body)
	}
	for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
		if strings.Contains(body, id) {
			t.Errorf("base-selected search leaked worktree symbol %q: %s", id, body)
		}
	}
	rider := resultFreshness(t, res)
	if rider["actual_view"] != "base:"+stack.graphID || rider["exact"] != true {
		t.Errorf("rider = %v, want exact base:%s", rider, stack.graphID)
	}
	after := viewmetrics.Read()
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Errorf("base views served = %d, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 0 {
		t.Errorf("materialized base was counted as a worktree (%d)", got)
	}
}

// BenchmarkViewForBaseSelectorActiveGeneration measures the full selector
// path above MaterializeBase: graph lookup, scope/readiness checks, source
// binding, and lease release.
func BenchmarkViewForBaseSelectorActiveGeneration(b *testing.B) {
	stack := newRefStack(b)
	ctx := WithSessionCWD(WithSessionID(context.Background(), refTestSession), stack.repo)
	selector := graphview.Selector{Kind: graphview.SelectorBase, GraphID: stack.graphID}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := stack.srv.viewForBaseSelector(ctx, selector)
		if err != nil {
			b.Fatalf("select active base: %v", err)
		}
		view.close()
	}
}
