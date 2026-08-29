package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// The search half of the routed-view fixture. newViewStack indexes the
// repositories and routes the worktree; this re-routes it at a pair of
// generations that also carry their own FTS rows, which is what the
// production builder writes and what candidate enumeration reads.
const (
	searchCommitLayerID = "layer-search-commit"
	searchDirtyLayerID  = "layer-search-dirty"

	searchNewID    = "repo/edit.go::New"
	searchFreshID  = "repo/added.go::Fresh"
	searchOldID    = "repo/edit.go::Old"
	searchKeeperID = "repo/keep.go::Keeper"
	searchDirtyID  = "repo/keep.go::Dirty"
	searchHiddenID = "repo/hidden.go::Hidden"

	// searchProseQuery reaches every corpus at once and names no symbol, so
	// nothing it returns can have come from the exact-name or substring lanes.
	searchProseQuery = "zephyr scheduler"
)

// searchNode is a fixture symbol carrying the same workspace and project
// binding the real index writes, so the session's scope filter admits it the
// way it admits an indexed one.
func searchNode(id, name, file string, startLine int) *graph.Node {
	n := viewRepoNode(id, name, graph.KindFunction, file, startLine)
	n.WorkspaceID = "main-ws"
	n.ProjectID = "repo"
	return n
}

func newSearchViewStack(t *testing.T) *viewStack {
	t.Helper()
	v := newViewStack(t)

	// The fixture's engine runs on the null backend; candidate enumeration
	// needs the store's own FTS on both sides of the composition.
	v.srv.engine.SetSearch(search.NewSymbolSearcherBackend(v.store))

	// One indexed symbol whose file a generation deletes, plus FTS rows for
	// the corpus symbols the real index already wrote nodes for.
	v.store.AddBatch([]*graph.Node{
		viewFileNode("repo/hidden.go", 6),
		searchNode(searchHiddenID, "Hidden", "repo/hidden.go", 3),
	}, nil)
	indexSearchSymbols(t, v.store, map[string]string{
		searchOldID:    "old zephyr scheduler",
		searchKeeperID: "keeper zephyr scheduler",
		searchHiddenID: "hidden zephyr scheduler",
	})
	if hits, err := v.store.SearchSymbols(searchProseQuery, 10); err != nil || len(hits) == 0 {
		t.Fatalf("the indexed corpus answers nothing: hits=%d err=%v", len(hits), err)
	}

	commit := writeSearchCommitGeneration(t, v.store, v.graphID)
	dirty := writeSearchDirtyGeneration(t, v.store, v.graphID, commit)
	v.setWorktreeHeadTree(t, "tree-search-commit")
	routeViewCheckout(t, v.store, v.graphID, commit, dirty, store_sqlite.RouteActive)
	v.commit, v.dirty = commit, dirty
	return v
}

func indexSearchSymbols(t *testing.T, handle *store_sqlite.Store, tokensByID map[string]string) {
	t.Helper()
	for id, tokens := range tokensByID {
		if err := handle.UpsertSymbolFTS(id, tokens); err != nil {
			t.Fatalf("UpsertSymbolFTS(%s): %v", id, err)
		}
	}
}

// writeSearchCommitGeneration republishes the commit slot with an FTS corpus:
// edit.go renamed, added.go new, hidden.go deleted.
func writeSearchCommitGeneration(t *testing.T, store *store_sqlite.Store, graphID string) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		LayerID:        searchCommitLayerID,
		CheckoutID:     viewTestWorktree,
		GenerationKind: "commit",
		TreeOID:        "tree-search-commit",
		CreatedAt:      5000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(commit): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/edit.go", 8),
		viewFileNode("repo/added.go", 6),
		searchNode(searchNewID, "New", "repo/edit.go", 3),
		searchNode(searchFreshID, "Fresh", "repo/added.go", 3),
	}, nil)
	indexSearchSymbols(t, handle, map[string]string{
		searchNewID:   "new zephyr scheduler",
		searchFreshID: "fresh zephyr scheduler",
	})
	appendSearchContent(t, handle, "repo/added.go", "repo/added.go::doc#1", "zephyr scheduler notes for the added file")
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/edit.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/added.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/hidden.go", Mode: store_sqlite.OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(commit): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 6000); err != nil {
		t.Fatalf("PublishPayloadGeneration(commit): %v", err)
	}
	return generationID
}

// writeSearchDirtyGeneration republishes the working-tree slot: keep.go
// re-derived with Keeper moved and Dirty added, the symbol that exists in this
// generation and nowhere else.
func writeSearchDirtyGeneration(t *testing.T, store *store_sqlite.Store, graphID string, base int64) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          graphID,
		LayerID:          searchDirtyLayerID,
		CheckoutID:       viewTestWorktree,
		GenerationKind:   "dirty",
		BaseGenerationID: base,
		TreeOID:          "tree-search-dirty",
		CreatedAt:        7000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(dirty): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/keep.go", 9),
		searchNode(searchKeeperID, "Keeper", "repo/keep.go", 70),
		searchNode(searchDirtyID, "Dirty", "repo/keep.go", 80),
	}, nil)
	indexSearchSymbols(t, handle, map[string]string{
		searchKeeperID: "keeper zephyr scheduler",
		searchDirtyID:  "dirty zephyr scheduler",
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/keep.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(dirty): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 8000); err != nil {
		t.Fatalf("PublishPayloadGeneration(dirty): %v", err)
	}
	return generationID
}

func appendSearchContent(t *testing.T, handle *store_sqlite.Store, filePath, nodeID, body string) {
	t.Helper()
	if err := handle.AppendContent("repo", []graph.ContentFTSItem{
		{NodeID: nodeID, FilePath: filePath, Ordinal: 1, Body: body},
	}); err != nil {
		t.Fatalf("AppendContent(%s): %v", filePath, err)
	}
}

// routedArgs names the worktree explicitly, from a session sitting in the
// repository. The automatic (cwd) binding routes the same view — see
// TestRoutedSearchFromTheWorktreeCWD, which drives the same composition with
// no selector at all — so the two arms differ only in how the view is chosen.
func routedArgs() map[string]any {
	return map[string]any{"view": map[string]any{
		"kind": "worktree", "checkout_id": viewTestWorktree,
	}}
}

// searchIDs runs one routed request through the whole tool middleware and
// returns the node IDs the engine produced for it, in rank order.
func (v *viewStack) searchIDs(t *testing.T, query string) []string {
	t.Helper()
	var ids []string
	_, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			for _, n := range v.srv.engineFor(ctx).SearchSymbols(query, 30) {
				ids = append(ids, n.ID)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("routed request failed: %v", err)
	}
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestRoutedSearchSymbolsFindsAGenerationOnlySymbol drives the real tool: the
// worktree's working-tree generation carries Dirty and nothing else does, and
// the query names no symbol, so only that generation's corpus can answer with
// it.
func TestRoutedSearchSymbolsFindsAGenerationOnlySymbol(t *testing.T) {
	v := newSearchViewStack(t)

	res, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			return v.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("search_symbols through the routed view: %v", err)
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, searchDirtyID) {
		t.Fatalf("search_symbols did not surface the generation-only symbol: %s", body)
	}
	if strings.Contains(body, searchHiddenID) {
		t.Fatalf("search_symbols answered with a symbol the view deleted: %s", body)
	}
}

// TestRoutedSearchFromTheWorktreeCWD is the same search with nobody naming a
// view: the session's working directory IS the automatic checkout, so the cwd
// binding both routes the request and has to produce a scope the routed
// answers survive. It is the whole seam end to end — the worktree lies inside
// no tracked repository, so a session that failed to bind it would apply the
// unresolved-workspace form and drop every row the composition just built.
func TestRoutedSearchFromTheWorktreeCWD(t *testing.T) {
	v := newSearchViewStack(t)

	res, err := v.callWithView(t, v.worktreeRoot, "search_symbols", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			if view := requestViewFromContext(ctx); !view.routed() {
				t.Fatal("the cwd binding did not route the request through the worktree's view")
			}
			return v.srv.handleSearchSymbols(ctx, searchToolRequest(searchProseQuery))
		})
	if err != nil {
		t.Fatalf("search_symbols from the worktree cwd: %v", err)
	}
	body := singleTextOrFail(t, res)
	if !strings.Contains(body, searchDirtyID) {
		t.Fatalf("a session bound in the worktree did not surface the generation-only symbol: %s", body)
	}
	if strings.Contains(body, searchHiddenID) {
		t.Fatalf("search_symbols answered with a symbol the view deleted: %s", body)
	}
}

// TestRoutedSearchComposesTheWholeStack: every corpus in the stack contributes,
// and everything a higher generation replaced or deleted is gone.
func TestRoutedSearchComposesTheWholeStack(t *testing.T) {
	v := newSearchViewStack(t)

	ids := v.searchIDs(t, searchProseQuery)
	for _, want := range []string{searchDirtyID, searchFreshID, searchNewID} {
		if !hasID(ids, want) {
			t.Errorf("the routed view lost %s; got %v", want, ids)
		}
	}
	for _, unwanted := range []string{searchOldID, searchHiddenID} {
		if hasID(ids, unwanted) {
			t.Errorf("the routed view answered with %s, which it replaced or deleted; got %v", unwanted, ids)
		}
	}
}

// TestRoutedSearchExactNameHidesADeletedSymbol: the strongest possible base hit
// — the deleted symbol's own identifier — still must not answer.
func TestRoutedSearchExactNameHidesADeletedSymbol(t *testing.T) {
	v := newSearchViewStack(t)

	if ids := v.searchIDs(t, "Hidden"); hasID(ids, searchHiddenID) {
		t.Fatalf("an exact-name query answered with a deleted symbol; got %v", ids)
	}
	if ids := v.searchIDs(t, "Old"); hasID(ids, searchOldID) {
		t.Fatalf("an exact-name query answered with a replaced symbol; got %v", ids)
	}
}

// TestRoutedSearchIgnoresAnUnroutedGeneration: a published generation the route
// does not name is not part of the view, and its rows must not reach a
// candidate set — the store's generation-scoped reads make that structural, and
// this is the guard that keeps it so.
func TestRoutedSearchIgnoresAnUnroutedGeneration(t *testing.T) {
	v := newSearchViewStack(t)

	const strayID = "repo/stray.go::Stray"
	generationID, handle, err := v.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        v.graphID,
		LayerID:        "layer-search-stray",
		CheckoutID:     viewTestWorktree,
		GenerationKind: "commit",
		TreeOID:        "tree-search-stray",
		CreatedAt:      9000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(stray): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/stray.go", 6),
		searchNode(strayID, "Stray", "repo/stray.go", 3),
	}, nil)
	indexSearchSymbols(t, handle, map[string]string{strayID: "stray zephyr scheduler"})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/stray.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(stray): %v", err)
	}
	if err := v.store.PublishPayloadGeneration(context.Background(), generationID, 10000); err != nil {
		t.Fatalf("PublishPayloadGeneration(stray): %v", err)
	}

	if ids := v.searchIDs(t, searchProseQuery); hasID(ids, strayID) {
		t.Fatalf("an unrouted generation reached the candidate set; got %v", ids)
	}
}

// TestBufferOverlayWinsOverTheRoutedStack: the editor buffer is the top layer
// of the composition and outranks every generation under it, both when it
// replaces an identity and when it hides one.
func TestBufferOverlayWinsOverTheRoutedStack(t *testing.T) {
	v := newSearchViewStack(t)

	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/keep.go", false)
	layer.AddNode("repo/keep.go", searchNode(searchKeeperID, "Keeper", "repo/keep.go", 500))

	var buffered, plain []string
	_, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			for _, n := range v.srv.engineFor(ctx).SearchSymbols(searchProseQuery, 30) {
				plain = append(plain, n.ID)
			}
			overlaid := graph.NewOverlaidView(v.srv.requestBaseReader(ctx), layer)
			eng := v.srv.engineFor(WithOverlayView(ctx, overlaid))
			for _, n := range eng.SearchSymbols(searchProseQuery, 30) {
				buffered = append(buffered, n.ID)
			}
			if n := eng.GetSymbol(searchKeeperID); n == nil || n.StartLine != 500 {
				t.Errorf("Keeper resolved to %v, want the buffer's line 500", n)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("routed request failed: %v", err)
	}
	if !hasID(plain, searchDirtyID) {
		t.Fatalf("the routed view without a buffer lost the generation-only symbol; got %v", plain)
	}
	// keep.go is covered by the buffer, which carries Keeper alone — so the
	// generation's other symbol in that file is hidden by the top layer.
	if hasID(buffered, searchDirtyID) {
		t.Fatalf("a buffer covering the file did not hide the generation's symbol; got %v", buffered)
	}
	if !hasID(buffered, searchFreshID) {
		t.Fatalf("the buffer hid a generation symbol in a file it does not cover; got %v", buffered)
	}
}

// TestRoutedContentSearchHonoursFileMasks: content lives in its own index, one
// corpus per generation, and the composition masks it by the same file claims.
func TestRoutedContentSearchHonoursFileMasks(t *testing.T) {
	v := newSearchViewStack(t)
	// A corpus content section in a file the commit generation deletes, and
	// one in a file no generation touches.
	appendSearchContent(t, v.store, "repo/hidden.go", "repo/hidden.go::doc#1", "zephyr scheduler notes that the view deletes")
	appendSearchContent(t, v.store, "repo/keep.go", "repo/keep.go::doc#1", "zephyr scheduler notes the working tree replaces")
	appendSearchContent(t, v.store, "repo/stay.go", "repo/stay.go::doc#1", "zephyr scheduler notes nothing touches")

	var hits []graph.ContentHit
	_, err := v.callWithView(t, v.repoRoot, "search_symbols", routedArgs(),
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			cs, ok := v.srv.contentSearcherFor(ctx)
			if !ok {
				t.Fatal("a routed request found no content corpus")
			}
			var err error
			hits, err = cs.SearchContent(searchProseQuery, "repo", 20)
			if err != nil {
				t.Fatalf("SearchContent: %v", err)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("routed request failed: %v", err)
	}

	byID := make(map[string]bool, len(hits))
	for _, hit := range hits {
		byID[hit.NodeID] = true
	}
	if !byID["repo/added.go::doc#1"] {
		t.Errorf("the commit generation's own content section never surfaced; got %v", hits)
	}
	if !byID["repo/stay.go::doc#1"] {
		t.Errorf("a corpus section in an untouched file was dropped; got %v", hits)
	}
	if byID["repo/hidden.go::doc#1"] {
		t.Errorf("a corpus section in a deleted file survived; got %v", hits)
	}
	if byID["repo/keep.go::doc#1"] {
		t.Errorf("a corpus section in a replaced file survived; got %v", hits)
	}
}

// TestBaseRequestKeepsTheBaseEngine is the zero-overhead guard: a request no
// view routes gets the server's own engine back, unwrapped and unallocated.
func TestBaseRequestKeepsTheBaseEngine(t *testing.T) {
	v := newSearchViewStack(t)

	var checked bool
	_, err := v.callWithView(t, v.repoRoot, "search_symbols", nil,
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			checked = true
			if got := v.srv.engineFor(ctx); got != v.srv.engine {
				t.Errorf("a base request wrapped the engine")
			}
			if allocs := testing.AllocsPerRun(100, func() { _ = v.srv.engineFor(ctx) }); allocs != 0 {
				t.Errorf("a base request allocated %v times resolving its engine", allocs)
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("base request failed: %v", err)
	}
	if !checked {
		t.Fatal("the leaf handler never ran")
	}
}

func searchToolRequest(query string) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": query, "limit": 30}
	return req
}

func singleTextOrFail(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	text, ok := singleTextContent(res)
	if !ok {
		t.Fatalf("the tool returned no text content")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		t.Fatalf("the tool returned non-JSON text: %v", err)
	}
	return text
}
