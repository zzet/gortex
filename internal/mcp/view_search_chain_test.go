package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A worktree route may name a dirty generation (D2) that stands on the
// checkout's previous dirty generation (D1) instead of directly on the commit
// generation. The materializer composes D1 into the stack below D2, so D1's
// own search corpora join the request's per-generation enumeration — and
// everything D2 replaced or deleted must be hidden there exactly the way the
// composed reader hides D1's rows.
const (
	chainLayerID = "layer-search-chain"

	chainOldID      = "repo/chain.go::ChainOld"
	chainNewID      = "repo/chain.go::ChainNew"
	chainGoneID     = "repo/gone.go::GoneInParent"
	chainSurvivorID = "repo/survive.go::Survivor"

	chainOldDoc      = "repo/chain.go::doc#1"
	chainNewDoc      = "repo/chain.go::doc#2"
	chainGoneDoc     = "repo/gone.go::doc#1"
	chainSurvivorDoc = "repo/survive.go::doc#1"
)

func writeSearchChainGeneration(
	t *testing.T,
	store *store_sqlite.Store,
	graphID string,
	base int64,
	nodes []*graph.Node,
	symbols map[string]string,
	content map[string][2]string,
	masks []store_sqlite.FileMask,
) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          graphID,
		LayerID:          chainLayerID,
		CheckoutID:       viewTestWorktree,
		GenerationKind:   "dirty",
		BaseGenerationID: base,
		TreeOID:          "tree-search-chain",
		CreatedAt:        11000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(chain over %d): %v", base, err)
	}
	if len(nodes) > 0 {
		handle.AddBatch(nodes, nil)
	}
	indexSearchSymbols(t, handle, symbols)
	for nodeID, section := range content {
		appendSearchContent(t, handle, section[0], nodeID, section[1])
	}
	if err := handle.SetFileMasks(masks); err != nil {
		t.Fatalf("SetFileMasks(chain over %d): %v", base, err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 12000); err != nil {
		t.Fatalf("PublishPayloadGeneration(chain over %d): %v", base, err)
	}
	return generationID
}

// routeSearchChain writes D1 over the search stack's commit generation and D2
// over D1, then routes the worktree at [commit, D2].
func routeSearchChain(t *testing.T, v *viewStack) (d1, d2 int64) {
	t.Helper()
	d1 = writeSearchChainGeneration(t, v.store, v.graphID, v.commit,
		[]*graph.Node{
			viewFileNode("repo/chain.go", 6),
			viewFileNode("repo/gone.go", 6),
			viewFileNode("repo/survive.go", 6),
			searchNode(chainOldID, "ChainOld", "repo/chain.go", 3),
			searchNode(chainGoneID, "GoneInParent", "repo/gone.go", 3),
			searchNode(chainSurvivorID, "Survivor", "repo/survive.go", 3),
		},
		map[string]string{
			chainOldID:      "chainold zephyr scheduler",
			chainGoneID:     "goneinparent zephyr scheduler",
			chainSurvivorID: "survivor zephyr scheduler",
		},
		map[string][2]string{
			chainOldDoc:      {"repo/chain.go", "zephyr scheduler notes the chain parent wrote"},
			chainGoneDoc:     {"repo/gone.go", "zephyr scheduler notes in a file the top deletes"},
			chainSurvivorDoc: {"repo/survive.go", "zephyr scheduler notes the top leaves alone"},
		},
		[]store_sqlite.FileMask{
			{RepoPrefix: "repo", FilePath: "repo/chain.go", Mode: store_sqlite.OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/gone.go", Mode: store_sqlite.OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/survive.go", Mode: store_sqlite.OwnershipReplace},
		})
	d2 = writeSearchChainGeneration(t, v.store, v.graphID, d1,
		[]*graph.Node{
			viewFileNode("repo/chain.go", 8),
			searchNode(chainNewID, "ChainNew", "repo/chain.go", 5),
		},
		map[string]string{chainNewID: "chainnew zephyr scheduler"},
		map[string][2]string{chainNewDoc: {"repo/chain.go", "zephyr scheduler notes the chain top rewrote"}},
		[]store_sqlite.FileMask{
			{RepoPrefix: "repo", FilePath: "repo/chain.go", Mode: store_sqlite.OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/gone.go", Mode: store_sqlite.OwnershipDelete},
		})
	routeViewCheckout(t, v.store, v.graphID, v.commit, d2, store_sqlite.RouteActive)
	return d1, d2
}

func TestRoutedSearchMasksDirtyChainLayers(t *testing.T) {
	v := newSearchViewStack(t)
	d1, d2 := routeSearchChain(t, v)

	var generations []int64
	var symbolIDs []string
	var hits []graph.ContentHit
	_, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(ctx)
			if view == nil || view.materialized == nil {
				t.Fatal("the request was not served by a materialized worktree view")
			}
			generations = view.materialized.Generations()
			for _, n := range v.srv.engineFor(ctx).SearchSymbols(searchProseQuery, 50) {
				symbolIDs = append(symbolIDs, n.ID)
			}
			cs, ok := v.srv.contentSearcherFor(ctx)
			if !ok {
				t.Fatal("a routed request found no content corpus")
			}
			var err error
			hits, err = cs.SearchContent(searchProseQuery, "repo", 50)
			if err != nil {
				t.Fatalf("SearchContent: %v", err)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("routed request failed: %v", err)
	}
	if len(generations) < 3 || generations[len(generations)-3] != v.commit || generations[len(generations)-2] != d1 || generations[len(generations)-1] != d2 {
		t.Fatalf("the view did not compose the chain [commit %d, D1 %d, D2 %d]: %v", v.commit, d1, d2, generations)
	}

	// Symbol lanes: D2's replacement and D1's untouched file answer; what D2
	// replaced or deleted in D1 does not.
	for _, want := range []string{chainNewID, chainSurvivorID, searchFreshID} {
		if !hasID(symbolIDs, want) {
			t.Errorf("the chained view lost %s; got %v", want, symbolIDs)
		}
	}
	for _, unwanted := range []string{chainOldID, chainGoneID} {
		if hasID(symbolIDs, unwanted) {
			t.Errorf("a chain parent's symbol %s survived the top generation's claim; got %v", unwanted, symbolIDs)
		}
	}
	if ids := v.searchIDs(t, "ChainOld"); hasID(ids, chainOldID) {
		t.Errorf("an exact-name query answered with the chain parent's replaced symbol; got %v", ids)
	}
	if ids := v.searchIDs(t, "GoneInParent"); hasID(ids, chainGoneID) {
		t.Errorf("an exact-name query answered with the chain parent's deleted symbol; got %v", ids)
	}

	// Content lanes, composed per generation by view_search.go.
	byID := make(map[string]bool, len(hits))
	for _, hit := range hits {
		byID[hit.NodeID] = true
	}
	for _, want := range []string{chainNewDoc, chainSurvivorDoc} {
		if !byID[want] {
			t.Errorf("the chained view lost content section %s; got %v", want, hits)
		}
	}
	for _, unwanted := range []string{chainOldDoc, chainGoneDoc} {
		if byID[unwanted] {
			t.Errorf("a chain parent's content section %s survived the top generation's claim; got %v", unwanted, hits)
		}
	}

	// And the whole tool: nothing the top hid reaches the response.
	res, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			return v.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("search_symbols through the chained view: %v", err)
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, chainNewID) {
		t.Errorf("search_symbols did not surface the chain top's symbol: %s", body)
	}
	for _, unwanted := range []string{chainOldID, chainGoneID, "the chain parent wrote", "a file the top deletes"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("search_symbols answered with %q, which the chain top hid: %s", unwanted, body)
		}
	}
}
