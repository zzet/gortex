package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// usagesLimitServer builds a server whose graph references `Hot` from
// callerCount distinct call sites, one caller per file, so the limit
// tests can count returned usage rows exactly.
func usagesLimitServer(t *testing.T, callerCount int) (srv *Server, hotID string) {
	t.Helper()
	g := graph.New()
	hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(hot)
	for i := 0; i < callerCount; i++ {
		file := fmt.Sprintf("pkg/use%d.go", i)
		caller := &graph.Node{
			ID: fmt.Sprintf("%s::Use%d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Use%d", i), FilePath: file, StartLine: 3,
		}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), hot.ID
}

type usagesLimitResponse struct {
	Edges        []*graph.Edge       `json:"edges"`
	Nodes        []json.RawMessage   `json:"nodes"`
	TotalEdges   int                 `json:"total_edges"`
	Truncated    bool                `json:"truncated"`
	UsageSummary *query.UsageSummary `json:"usage_summary"`
}

// TestFindUsages_LimitCapsUsageRows pins the advertised `limit` option:
// a call asking for 2 rows gets exactly 2, marked truncated, with the
// full row count still legible on total_edges and the completeness
// rollup still covering the whole usage set.
func TestFindUsages_LimitCapsUsageRows(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2})), &resp))
	require.Len(t, resp.Edges, 2, "limit:2 must return exactly 2 usage rows")
	require.True(t, resp.Truncated, "a capped response must be marked truncated")
	require.Equal(t, 6, resp.TotalEdges, "total_edges must keep the full row count")
	require.NotNil(t, resp.UsageSummary)
	require.Equal(t, 6, resp.UsageSummary.NRefs, "the completeness rollup must describe the full set, not the page")
}

// TestFindUsages_DefaultLimitApplies pins the schema's "default: 50":
// with no limit argument a 55-caller symbol answers with 50 rows and a
// truncation marker instead of the whole set.
func TestFindUsages_DefaultLimitApplies(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID})), &resp))
	require.Len(t, resp.Edges, 50, "the advertised default limit is 50")
	require.True(t, resp.Truncated)
	require.Equal(t, 55, resp.TotalEdges)
}

// TestFindUsages_LimitOptOut pins limit:0 as the explicit no-cap
// escape hatch, mirroring the max_bytes/max_tokens opt-out semantics.
func TestFindUsages_LimitOptOut(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 0})), &resp))
	require.Len(t, resp.Edges, 55, "limit:0 opts out of the cap")
	require.False(t, resp.Truncated)
}

// TestFindUsages_LimitTruncationMetaGCX pins the truncation indicator
// on the GCX wire: a capped response carries truncated=true plus the
// full total, an uncapped response keeps its wire shape unchanged.
func TestFindUsages_LimitTruncationMetaGCX(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "gcx", "limit": 2})
	require.Contains(t, out, "truncated=true")
	require.Contains(t, out, "total_edges=6")

	full := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "gcx", "limit": 0})
	require.NotContains(t, full, "truncated=true", "an uncapped response must not carry truncation meta")
}

// TestFindUsages_LimitKeepsStrongestEvidence pins the order the cap
// consumes: rows are ranked by evidence tier before the page is cut, so
// a weak ast_inferred row inserted earlier can never evict an
// lsp_resolved usage from the page.
func TestFindUsages_LimitKeepsStrongestEvidence(t *testing.T) {
	g := graph.New()
	hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(hot)
	// Weak rows first in insertion order, strong rows last.
	for i := 0; i < 4; i++ {
		file := fmt.Sprintf("pkg/weak%d.go", i)
		caller := &graph.Node{ID: file + "::Weak", Kind: graph.KindFunction, Name: "Weak", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginASTInferred, Confidence: 0.6})
	}
	for i := 0; i < 2; i++ {
		file := fmt.Sprintf("pkg/strong%d.go", i)
		caller := &graph.Node{ID: file + "::Strong", Kind: graph.KindFunction, Name: "Strong", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginLSPResolved, Confidence: 1.0})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hot.ID, "limit": 2})), &resp))
	require.Len(t, resp.Edges, 2)
	for _, e := range resp.Edges {
		require.Equal(t, graph.OriginLSPResolved, e.Origin,
			"the capped page must keep the strongest evidence, not the earliest-inserted rows")
	}
}

// TestFindUsages_LimitPageBackendParity pins that the capped page is
// identical on the in-memory and SQLite backends. The two stores
// iterate edges in different orders (insertion vs kind/id), so without
// one global sort before the cut the default page depends on which
// backend served it.
func TestFindUsages_LimitPageBackendParity(t *testing.T) {
	build := func(g graph.Store) string {
		hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
		g.AddNode(hot)
		// Mixed edge kinds in descending file order: the memory store
		// iterates in-edges by insertion, SQLite groups them by kind, so
		// without one global sort the two pages diverge.
		kinds := []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls, graph.EdgeImports, graph.EdgeInstantiates}
		for i := 9; i >= 0; i-- {
			file := fmt.Sprintf("pkg/use%d.go", i)
			caller := &graph.Node{ID: fmt.Sprintf("%s::Use%d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Use%d", i), FilePath: file, StartLine: 3}
			g.AddNode(caller)
			g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: kinds[i%len(kinds)], FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
		}
		return hot.ID
	}
	page := func(g graph.Store, hotID string) []string {
		eng := query.NewEngine(g)
		eng.SetSearch(search.NewNull())
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
		var resp usagesLimitResponse
		require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 3})), &resp))
		froms := make([]string, 0, len(resp.Edges))
		for _, e := range resp.Edges {
			froms = append(froms, e.From)
		}
		return froms
	}

	mem := graph.New()
	memID := build(mem)

	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "usages.sqlite"))
	require.NoError(t, err)
	defer sqlite.Close()
	sqliteID := build(sqlite)

	memPage := page(mem, memID)
	sqlitePage := page(sqlite, sqliteID)
	require.Len(t, memPage, 3)
	require.Equal(t, memPage, sqlitePage, "the capped page must not depend on the store backend")
}

// TestFindUsages_LimitOrdersReExportUnion pins the stable order on the
// barrel re-export union path: the page over facade + canonical usages
// is cut after the merged set is sorted, so the strongest canonical
// rows win over the facade's weaker direct refs regardless of merge
// order, and the totals describe the whole union.
func TestFindUsages_LimitOrdersReExportUnion(t *testing.T) {
	g := graph.New()
	facade := &graph.Node{
		ID: "src/index.ts::persist", Kind: graph.KindFunction, Name: "persist",
		FilePath: "src/index.ts", StartLine: 1, Meta: map[string]any{"reexport": true},
	}
	canon := &graph.Node{ID: "src/middleware.ts::persist", Kind: graph.KindFunction, Name: "persist", FilePath: "src/middleware.ts", StartLine: 10}
	g.AddNode(facade)
	g.AddNode(canon)
	g.AddEdge(&graph.Edge{From: facade.ID, To: canon.ID, Kind: graph.EdgeReExports, FilePath: "src/index.ts", Line: 1})
	// Facade's own direct refs are weak; the canonical's usages are
	// lsp_resolved. The merge appends the canonical set second, so an
	// unsorted cut would keep the weak facade rows.
	for i := 0; i < 2; i++ {
		file := fmt.Sprintf("src/facadeuse%d.ts", i)
		caller := &graph.Node{ID: file + "::useFacade", Kind: graph.KindFunction, Name: "useFacade", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: facade.ID, Kind: graph.EdgeImports, FilePath: file, Line: 1, Origin: graph.OriginASTInferred, Confidence: 0.6})
	}
	for i := 0; i < 4; i++ {
		file := fmt.Sprintf("src/canonuse%d.ts", i)
		caller := &graph.Node{ID: file + "::useCanon", Kind: graph.KindFunction, Name: "useCanon", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: canon.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginLSPResolved, Confidence: 1.0})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": facade.ID, "limit": 3})), &resp))
	require.Len(t, resp.Edges, 3)
	for _, e := range resp.Edges {
		require.Equal(t, graph.OriginLSPResolved, e.Origin,
			"the union page must keep the strongest rows across facade + canonical")
	}
	require.True(t, resp.Truncated)
	// 2 facade imports + 1 re-export edge is not a usage of the facade
	// per se; the union total counts the merged usage rows.
	require.Equal(t, resp.TotalEdges, 6+1, "total_edges covers the merged union")
}

// TestFindUsages_GroupByFileHonorsLimit pins the row cap on the
// group_by:"file" shape: the buckets cover the capped page, and a
// truncated grouped response carries the full total alongside its
// per-page counts so the cut stays legible.
func TestFindUsages_GroupByFileHonorsLimit(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	var resp struct {
		TotalUses  int  `json:"total_uses"`
		TotalEdges int  `json:"total_edges"`
		Truncated  bool `json:"truncated"`
	}
	out := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2, "group_by": "file"})
	require.NoError(t, json.Unmarshal([]byte(out), &resp))
	require.Equal(t, 2, resp.TotalUses, "the grouped page covers the capped rows")
	require.True(t, resp.Truncated)
	require.Equal(t, 6, resp.TotalEdges, "a truncated grouped page must carry the full total")
}

// TestFindUsages_CappedCompactCarriesFullTotal pins the capped-response
// contract on the compact format: a capped page must be legible as
// partial, so the edges footer names both the page size and the full
// row count. (JSON and GCX are pinned by the limit tests above.)
func TestFindUsages_CappedCompactCarriesFullTotal(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)
	out := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2, "compact": true})
	require.Contains(t, out, "edges: 2 of 6 total",
		"a capped compact response must name the full row count, not label the page as the total")

	full := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 0, "compact": true})
	require.Contains(t, full, "edges: 6 total", "the uncapped compact footer keeps its wire shape")
}

// TestFindUsages_CappedTOONCarriesFullTotal pins the same contract on
// the TOON wire: truncated rides with the full edge total.
func TestFindUsages_CappedTOONCarriesFullTotal(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)
	out := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2, "format": "toon"})
	require.Contains(t, out, "truncated: true")
	require.Contains(t, out, "total_edges: 6", "a capped TOON response must carry the full edge total")

	full := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 0, "format": "toon"})
	require.NotContains(t, full, "total_edges", "the uncapped TOON response keeps its wire shape")
}

// TestFindUsages_CompactHonorsByteBudget pins max_bytes on the compact
// path, including the compact-over-gcx-default composition: routing a
// budgeted request to the compact renderer must not shed the budget.
func TestFindUsages_CompactHonorsByteBudget(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)
	out := findUsagesText(t, srv, map[string]any{
		"id": hotID, "limit": 0, "compact": true, "format": "gcx", "max_bytes": 300,
	})
	require.LessOrEqual(t, len(out), 300, "compact output must respect max_bytes")
	require.Contains(t, out, "trimmed", "a budget-trimmed compact response must say so")
}

// TestFindUsages_TestLabelsMatchTheFilter pins that the output
// metadata classifies test rows with the same classifier the
// exclude_tests filter uses: a file-level node (is_test_file only) and
// a bare param node under a test directory count in
// usage_summary.n_test_refs and carry from_is_test=true on the GCX
// wire, instead of being filtered as tests but labeled as production.
func TestFindUsages_TestLabelsMatchTheFilter(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "src/WidgetService.cs::WidgetService", Kind: graph.KindType, Name: "WidgetService", FilePath: "src/WidgetService.cs", StartLine: 5}
	prod := &graph.Node{ID: "src/Consumer.cs::Consumer.Use", Kind: graph.KindMethod, Name: "Use", FilePath: "src/Consumer.cs", StartLine: 12}
	testFile := &graph.Node{
		ID: `Test\WidgetService_Tests.cs`, Kind: graph.KindFile,
		Name: "WidgetService_Tests.cs", FilePath: `Test\WidgetService_Tests.cs`,
		Meta: map[string]any{"is_test_file": true},
	}
	testParam := &graph.Node{
		ID: `Test\WidgetService_Tests.cs::WidgetServiceTests.Run#param:svc`, Kind: graph.KindParam,
		Name: "svc", FilePath: `Test\WidgetService_Tests.cs`, StartLine: 20,
	}
	for _, n := range []*graph.Node{target, prod, testFile, testParam} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: prod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/Consumer.cs", Line: 14})
	g.AddEdge(&graph.Edge{From: testFile.ID, To: target.ID, Kind: graph.EdgeImports, FilePath: testFile.FilePath, Line: 1})
	g.AddEdge(&graph.Edge{From: testParam.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: testParam.FilePath, Line: 20})
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var resp struct {
		UsageSummary *query.UsageSummary `json:"usage_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": target.ID})), &resp))
	require.NotNil(t, resp.UsageSummary)
	require.Equal(t, 3, resp.UsageSummary.NRefs)
	require.Equal(t, 2, resp.UsageSummary.NTestRefs,
		"n_test_refs must count the unstamped test-path rows the filter would drop")

	gcx := findUsagesText(t, srv, map[string]any{"id": target.ID, "format": "gcx"})
	for _, line := range strings.Split(gcx, "\n") {
		if strings.HasPrefix(line, testParam.ID+"\t") || strings.HasPrefix(line, testFile.ID+"\t") {
			require.Contains(t, line, "\ttrue", "test-path rows must carry from_is_test=true: %s", line)
		}
		if strings.HasPrefix(line, prod.ID+"\t") {
			require.NotContains(t, line, "\ttrue", "the production row must stay from_is_test=false: %s", line)
		}
	}
}

// TestFindUsages_OverlayOwnerClassifiesChildren pins that the output
// classifiers read the same request-scoped graph view as the query: an
// annotation test that exists only in the session overlay stamps its
// owner there, so the owner hop for its param child must go through
// the overlay reader — resolving against the base graph classifies the
// child as production while the filter (running on the overlay engine)
// would exclude it.
func TestFindUsages_OverlayOwnerClassifiesChildren(t *testing.T) {
	base := graph.New()
	target := &graph.Node{ID: "src/widget.rs::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "src/widget.rs", StartLine: 3}
	prod := &graph.Node{ID: "src/main.rs::run", Kind: graph.KindFunction, Name: "run", FilePath: "src/main.rs", StartLine: 10}
	base.AddNode(target)
	base.AddNode(prod)
	base.AddEdge(&graph.Edge{From: prod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/main.rs", Line: 12})

	layer := graph.NewOverlayLayer()
	owner := &graph.Node{
		ID: "src/lib.rs::check_widget", Kind: graph.KindFunction, Name: "check_widget",
		FilePath: "src/lib.rs", StartLine: 40, Meta: map[string]any{"is_test": true, "test_role": "test"},
	}
	child := &graph.Node{
		ID: "src/lib.rs::check_widget#param:w", Kind: graph.KindParam, Name: "w",
		FilePath: "src/lib.rs", StartLine: 40,
	}
	layer.AddNode("src/lib.rs", owner)
	layer.AddNode("src/lib.rs", child)
	layer.AddEdge(&graph.Edge{From: child.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: "src/lib.rs", Line: 40})

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": target.ID}
	res, err := srv.handleFindUsages(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp struct {
		UsageSummary *query.UsageSummary `json:"usage_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.NotNil(t, resp.UsageSummary)
	require.Equal(t, 2, resp.UsageSummary.NRefs)
	require.Equal(t, 1, resp.UsageSummary.NTestRefs,
		"the overlay-stamped owner's child must classify as a test ref through the overlay view")
}

// TestFindUsages_OverlayOnlySymbolZeroEdgeCaveat pins the zero-edge
// caveat under an overlay session: a symbol the request's own view
// resolved is not a mistyped id, so its empty result classifies
// through the deeper extraction-gap path, never the not-found message.
func TestFindUsages_OverlayOnlySymbolZeroEdgeCaveat(t *testing.T) {
	base := graph.New()
	layer := graph.NewOverlayLayer()
	fresh := &graph.Node{ID: "src/new.go::FreshFn", Kind: graph.KindFunction, Name: "FreshFn", FilePath: "src/new.go", StartLine: 1}
	layer.AddNode("src/new.go", fresh)

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": fresh.ID}
	res, err := srv.handleFindUsages(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	out := res.Content[0].(mcplib.TextContent).Text
	require.NotContains(t, out, "mistyped",
		"a symbol the overlay view resolved must not be classified as a mistyped id")
}

// TestFindUsages_TOONRowsClassifyTheRightNode pins the TOON is_test
// join: nodesToTOONRows skips File/Import nodes, so classification must
// join rows to nodes by ID — positional indexing would shift every
// classification after a skipped node and hand a production function a
// skipped test file's flag.
func TestFindUsages_TOONRowsClassifyTheRightNode(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "src/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "src/hot.go", StartLine: 1}
	// The file node sorts first by ID (capital T), is a test path, and
	// is skipped by the TOON row builder.
	testFile := &graph.Node{
		ID: `Test\Suite_Tests.cs`, Kind: graph.KindFile,
		Name: "Suite_Tests.cs", FilePath: `Test\Suite_Tests.cs`,
		Meta: map[string]any{"is_test_file": true},
	}
	prod := &graph.Node{ID: "src/a.go::ProdUse", Kind: graph.KindFunction, Name: "ProdUse", FilePath: "src/a.go", StartLine: 3}
	testFn := &graph.Node{
		ID: "src/z_test.go::TestUse", Kind: graph.KindFunction, Name: "TestUse", FilePath: "src/z_test.go", StartLine: 5,
		Meta: map[string]any{"is_test": true},
	}
	for _, n := range []*graph.Node{target, testFile, prod, testFn} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: testFile.ID, To: target.ID, Kind: graph.EdgeImports, FilePath: testFile.FilePath, Line: 1})
	g.AddEdge(&graph.Edge{From: prod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/a.go", Line: 4})
	g.AddEdge(&graph.Edge{From: testFn.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/z_test.go", Line: 6})
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	out := findUsagesText(t, srv, map[string]any{"id": target.ID, "format": "toon"})
	// Node rows carry the kind + name columns; edge rows carry from/to
	// ids only, so gate on the node-row shape.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, ",function,ProdUse,") {
			require.NotContains(t, line, "true", "the production row must not inherit a skipped node's test flag: %s", line)
		}
		if strings.Contains(line, ",function,TestUse,") {
			require.Contains(t, line, "true", "the stamped test row keeps its flag: %s", line)
		}
	}
}

// TestTrimTextToBudget_Boundaries pins that the budget is a hard
// ceiling at every size: the marker itself must fit inside the budget,
// including budgets smaller than the marker.
func TestTrimTextToBudget_Boundaries(t *testing.T) {
	text := strings.Repeat("row line here\n", 40)
	for _, budget := range []int{0, 1, len(trimBudgetMarker) - 1, len(trimBudgetMarker), len(trimBudgetMarker) + 1, 100} {
		out := trimTextToBudget(text, budget)
		require.LessOrEqual(t, len(out), budget, "budget %d is a hard ceiling", budget)
	}
}

// TestFindUsages_CompactHonorsTinyTokenBudget pins the same ceiling
// through the handler on the token axis.
func TestFindUsages_CompactHonorsTinyTokenBudget(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)
	out := findUsagesText(t, srv, map[string]any{
		"id": hotID, "limit": 0, "compact": true, "max_tokens": 1,
	})
	require.LessOrEqual(t, len(out), tokensToBytes(1), "max_tokens:1 must bound compact output")
}

// TestFindUsages_CompactWinsOverGCX pins the `compact` option against
// the GCX format path: compact is an explicit caller choice, so it
// takes precedence exactly as it does in the shared returnSubGraph
// renderer — including for sessions whose default format is gcx, where
// isGCX(ctx, req) is true without any format argument. The explicit
// format:"gcx" arg reproduces that same isGCX=true condition without
// needing a session handshake.
func TestFindUsages_CompactWinsOverGCX(t *testing.T) {
	srv, fooID, _ := usagesSummaryServer(t)

	out := findUsagesText(t, srv, map[string]any{"id": fooID, "format": "gcx", "compact": true})
	require.Contains(t, out, "edges: 4 total", "compact:true must render the one-line-per-symbol text format")
	require.Contains(t, out, "function Use1 pkg/a.go:3", "compact rows carry kind, name, and location")
	require.NotContains(t, out, "from_is_test", "compact output must not be the GCX row encoding")
}
