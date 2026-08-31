package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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

// TestMaterializeSelectedBaseRejectsPrimaryRoleMovement closes the small
// window between selecting the family primary and pinning its generation. A
// primary move changes only the graph's role flag; serving the old graph after
// that move would mislabel a stale family base as the current fallback.
func TestMaterializeSelectedBaseRejectsPrimaryRoleMovement(t *testing.T) {
	stack := newSearchViewStack(t)
	baseGenerationID := activateSearchBaseGeneration(t, stack)
	ctx := context.Background()

	selected, found, err := stack.store.Catalog().GetDedicatedGraph(ctx, stack.graphID)
	if err != nil {
		t.Fatalf("GetDedicatedGraph(selected): %v", err)
	}
	if !found || !selected.IsPrimaryBase {
		t.Fatalf("selected primary = %+v, found=%v", selected, found)
	}
	sibling := store_sqlite.DedicatedGraph{
		GraphID:         "graph-new-primary",
		OwnerCheckoutID: viewTestWorktree,
		RepoPrefix:      "repo-new-primary",
		FamilyID:        viewTestFamily,
		State:           store_sqlite.DedicatedGraphStateReady,
	}
	if err := stack.store.Catalog().UpsertDedicatedGraph(ctx, sibling); err != nil {
		t.Fatalf("UpsertDedicatedGraph(new primary): %v", err)
	}
	family, found, err := stack.store.Catalog().GetRepositoryFamily(ctx, viewTestFamily)
	if err != nil {
		t.Fatalf("GetRepositoryFamily: %v", err)
	}
	if !found {
		t.Fatalf("repository family %q is missing", viewTestFamily)
	}
	if err := stack.store.Catalog().SetPrimaryDedicatedGraph(ctx, store_sqlite.SetPrimaryDedicatedGraphRequest{
		FamilyID:             viewTestFamily,
		GraphID:              sibling.GraphID,
		ExpectedPrimaryEpoch: family.PrimaryEpoch,
		LastSeen:             family.LastSeen + 1,
	}); err != nil {
		t.Fatalf("move primary base: %v", err)
	}

	base, err := stack.srv.materializeSelectedBase(ctx, selected)
	if base != nil {
		base.Close()
		t.Fatal("superseded primary base was materialized")
	}
	if got := graphview.CodeOf(err); got != graphview.CodeViewBuilding {
		t.Fatalf("materialize superseded primary = %v (code %q), want %s", err, got, graphview.CodeViewBuilding)
	}
	if stack.leases.InUse(baseGenerationID) {
		t.Fatal("superseded primary generation lease was not released")
	}
}

// TestSessionCWDRouteFreeDedicatedUsesActivePrimaryFallback proves that
// omitting `view` does not send a generation-backed primary checkout through
// generation zero while its exact commit/dirty route is still unavailable.
// The active base remains useful, but is labeled and read-only.
func TestSessionCWDRouteFreeDedicatedUsesActivePrimaryFallback(t *testing.T) {
	stack := newSearchViewStack(t)
	baseGenerationID := activateSearchBaseGeneration(t, stack)

	var reader graph.Reader
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(ctx)
			if view == nil || view.materialized == nil || !view.baseFallback {
				t.Fatal("session CWD did not materialize the active dedicated fallback")
			}
			if got := requestViewKind(view); got != viewmetrics.ViewBase {
				t.Fatalf("request view kind = %q, want %q", got, viewmetrics.ViewBase)
			}
			if got := view.materialized.ID.BaseGeneration; got != baseGenerationID {
				t.Fatalf("base generation = %d, want %d", got, baseGenerationID)
			}
			if view.rider == nil {
				t.Fatal("route-free dedicated fallback carries no freshness rider")
			}
			if view.viewRoot != "" || view.checkoutID != "" || view.mutableCheckout() {
				t.Fatalf("route-free dedicated fallback became writable: %+v", view)
			}
			if got := viewCheckoutID(view); got != viewTestPrimary {
				t.Fatalf("search checkout identity = %q, want %q", got, viewTestPrimary)
			}
			if !stack.leases.InUse(baseGenerationID) {
				t.Fatal("base generation lease is not held during the request")
			}
			reader = stack.srv.readerFor(ctx)
			return stack.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("CWD-selected dedicated-base search: %v", err)
	}
	if res.IsError {
		t.Fatalf("CWD-selected dedicated-base search was refused: %s", viewResultText(t, res))
	}
	if stack.leases.InUse(baseGenerationID) {
		t.Fatal("base generation lease survived request completion")
	}
	if !hasNode(reader, searchBaseOnlyID) {
		t.Error("generation-only base symbol is absent from graph traversal")
	}
	for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
		if hasNode(reader, id) {
			t.Errorf("CWD-selected base leaked worktree symbol %q", id)
		}
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, searchBaseOnlyID) {
		t.Fatalf("generation-owned FTS row is absent from search: %s", body)
	}
	for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
		if strings.Contains(body, id) {
			t.Errorf("CWD-selected base leaked worktree FTS row %q: %s", id, body)
		}
	}
	freshness := resultFreshness(t, res)
	if freshness["exact"] != false || freshness["fallback_reason"] != graphview.CodeViewBuilding {
		t.Fatalf("fallback freshness = %v, want labeled view_building", freshness)
	}
	if freshness["requested_view"] != "worktree:"+viewTestPrimary || freshness["graph_id"] != stack.graphID {
		t.Errorf("fallback identity = %v, want checkout %q graph %q", freshness, viewTestPrimary, stack.graphID)
	}
	after := viewmetrics.Read()
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Errorf("base views served = %d, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 0 {
		t.Errorf("implicit materialized base was counted as a worktree (%d)", got)
	}
}

// TestSessionCWDPendingRouteUsesActivePrimaryFallback covers the automatic
// half of selector-free routing. A cold/incomplete overlay may fall back, but
// the labeled answer must be the main graph's active sealed generation—not
// the obsolete generation-zero corpus.
func TestSessionCWDPendingRouteUsesActivePrimaryFallback(t *testing.T) {
	stack := newSearchViewStack(t)
	baseGenerationID := activateSearchBaseGeneration(t, stack)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(ctx)
			if view == nil || view.materialized == nil || !view.baseFallback {
				t.Fatalf("pending route did not materialize the primary fallback: %+v", view)
			}
			if got := view.materialized.ID.BaseGeneration; got != baseGenerationID {
				t.Fatalf("fallback generation = %d, want active primary %d", got, baseGenerationID)
			}
			if !stack.leases.InUse(baseGenerationID) {
				t.Fatal("primary base lease is not held during the fallback request")
			}
			reader = stack.srv.readerFor(ctx)
			return stack.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("pending-route search: %v", err)
	}
	if res.IsError {
		t.Fatalf("pending-route fallback was refused: %s", viewResultText(t, res))
	}
	if stack.leases.InUse(baseGenerationID) {
		t.Fatal("primary base lease survived fallback request completion")
	}
	if !hasNode(reader, searchBaseOnlyID) {
		t.Error("active-primary-only symbol is absent from fallback traversal")
	}
	for _, id := range []string{searchNewID, searchFreshID, searchDirtyID} {
		if hasNode(reader, id) {
			t.Errorf("pending-route fallback leaked worktree symbol %q", id)
		}
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, searchBaseOnlyID) {
		t.Fatalf("active-primary FTS row is absent from fallback: %s", body)
	}
	freshness := resultFreshness(t, res)
	if freshness["exact"] != false || freshness["fallback_reason"] != graphview.CodeViewBuilding {
		t.Fatalf("fallback freshness = %v, want labeled view_building", freshness)
	}
	if freshness["requested_view"] != "worktree:"+viewTestWorktree ||
		freshness["graph_id"] != stack.graphID {
		t.Errorf("fallback identity = %v, want checkout %q graph %q", freshness, viewTestWorktree, stack.graphID)
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
