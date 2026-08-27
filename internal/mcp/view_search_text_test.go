package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// newSearchTextRequest is one search_text call with nothing but a query on it.
func newSearchTextRequest(query string) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_text"
	req.Params.Arguments = map[string]any{"query": query}
	return req
}

// searchTextMatchPaths reads the paths a search_text answer reported.
func searchTextMatchPaths(t *testing.T, res *mcplib.CallToolResult) []string {
	t.Helper()
	var obj struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &obj); err != nil {
		t.Fatalf("unmarshal search_text result: %v\n%s", err, viewResultText(t, res))
	}
	out := make([]string, 0, len(obj.Matches))
	for _, m := range obj.Matches {
		out = append(out, m.Path)
	}
	return out
}

// assertNamesTextCapability pins that a refusal is legible as the one it is:
// the capability the view cannot serve, by name.
func assertNamesTextCapability(t *testing.T, res *mcplib.CallToolResult) {
	t.Helper()
	assertToolError(t, res, string(graphview.CodeCapabilityUnavailable))
	if text := viewResultText(t, res); !strings.Contains(text, string(graphview.CapSearchText)) {
		t.Errorf("the refusal does not name the capability:\n%s", text)
	}
}

// TestSearchTextRefusesAViewOfACommittedTree is the defining claim for a view
// with no working copy: there is nothing on disk holding that tree, so the
// search is refused rather than answered out of the canonical checkout — whose
// bytes would have matched.
func TestSearchTextRefusesAViewOfACommittedTree(t *testing.T) {
	stack := newRefStack(t)

	res, err := stack.call(t, "search_text", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"query": "func Keeper"}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search through the ref view: %v", err)
	}
	assertNamesTextCapability(t, res)

	// The control: the canonical checkout really does hold the query, so the
	// refusal above is a refusal to answer and not an empty corpus.
	plain, err := stack.call(t, "search_text", nil,
		map[string]any{"query": "func Keeper"}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search the canonical checkout: %v", err)
	}
	if plain.IsError {
		t.Fatalf("the base search failed: %s", viewResultText(t, plain))
	}
	if paths := searchTextMatchPaths(t, plain); len(paths) != 1 || paths[0] != "repo/keep.go" {
		t.Fatalf("the base search answered %v, want repo/keep.go", paths)
	}
	if rider := resultFreshness(t, plain); rider != nil {
		t.Errorf("a base search_text answer carries a view rider: %v", rider)
	}
}

// TestViewForSessionCWDLeavesLegacyDedicatedCheckoutOnBase isolates the
// canonical-checkout control above. A ready dedicated graph created before
// checkout routes existed remains the base corpus until it owns an explicit
// route; merely finding its CWD must not manufacture a composed view.
func TestViewForSessionCWDLeavesLegacyDedicatedCheckoutOnBase(t *testing.T) {
	stack := newRefStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), refTestSession), stack.repo)

	view, err := stack.srv.viewForSessionCWD(ctx)
	if err != nil {
		t.Fatalf("resolve the legacy dedicated checkout: %v", err)
	}
	if view != nil {
		t.Fatalf("legacy dedicated checkout resolved to a composed view: %+v", view)
	}
}

func BenchmarkViewForSessionCWDLegacyDedicatedBase(b *testing.B) {
	stack := newRefStack(b)
	ctx := WithSessionCWD(WithSessionID(context.Background(), refTestSession), stack.repo)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := stack.srv.viewForSessionCWD(ctx)
		if err != nil {
			b.Fatalf("resolve the legacy dedicated checkout: %v", err)
		}
		if view != nil {
			b.Fatalf("legacy dedicated checkout resolved to a composed view: %+v", view)
		}
	}
}

// TestSearchTextRefusalIsTheCapabilityEvaluationsRefusal pins the one source of
// truth: a caller that required search.text is refused by the capability gate
// before the handler runs, and a caller that required nothing is refused by the
// handler — with the same code, naming the same capability.
func TestSearchTextRefusalIsTheCapabilityEvaluationsRefusal(t *testing.T) {
	stack := newRefStack(t)

	required, err := stack.call(t, "search_text", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"query": "func Keeper", requireCompleteArgName: true}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search with a required capability: %v", err)
	}
	assertNamesTextCapability(t, required)
}

// TestSearchTextRefusesARoutedViewNothingIndexes pins the other half of the
// same rule: a routed checkout whose working copy has no searcher is refused
// too. Falling through would answer out of the canonical checkout, which is a
// different working tree.
func TestSearchTextRefusesARoutedViewNothingIndexes(t *testing.T) {
	stack := newViewStack(t)
	// The view declares the capability the way a coordinator's generation
	// does; what it lacks is the coordinator holding the checkout.
	stack.declareProducer(t, stack.dirty, graphview.CapSearchText, store_sqlite.ProducerStateComplete)

	res, err := stack.callHandler(t, stack.worktreeRoot, "search_text",
		map[string]any{"query": "func Keeper"}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search through the routed view: %v", err)
	}
	assertNamesTextCapability(t, res)

	// The control: the same query against the base corpus answers, so the
	// refusal is the view's and not the query's.
	plain, err := stack.callHandler(t, stack.repoRoot, "search_text",
		map[string]any{"query": "func Keeper"}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search the canonical checkout: %v", err)
	}
	if paths := searchTextMatchPaths(t, plain); len(paths) == 0 {
		t.Fatalf("the base search answered nothing: %s", viewResultText(t, plain))
	}
}
