package query

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestFindUsagesScoped_ExcludeTestsCoversUnstampedKinds pins the
// exclude_tests contract for node kinds the indexer's test-edge pass
// never stamps. The pass writes Meta["is_test"] on function/method
// symbols only — a file-level from-node under a test directory carries
// is_test_file instead, and a parameter node carries no flag at all —
// so a filter that trusts the stamp alone leaks exactly those kinds
// into a "production-only" answer while correctly dropping the stamped
// method callers. The graph below mirrors that indexer output shape.
func TestFindUsagesScoped_ExcludeTestsCoversUnstampedKinds(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "src/WidgetService.cs::WidgetService", Kind: graph.KindType,
		Name: "WidgetService", FilePath: "src/WidgetService.cs", StartLine: 5,
	}
	prodCaller := &graph.Node{
		ID: "src/Consumer.cs::Consumer.Use", Kind: graph.KindMethod,
		Name: "Use", FilePath: "src/Consumer.cs", StartLine: 12,
	}
	// Stamped test method — the case the filter already handles.
	testMethod := &graph.Node{
		ID: `Test\WidgetService_Tests.cs::WidgetServiceTests.Run`, Kind: graph.KindMethod,
		Name: "Run", FilePath: `Test\WidgetService_Tests.cs`, StartLine: 20,
		Meta: map[string]any{"is_test": true},
	}
	// File-level from-node: the pass stamps is_test_file, never is_test.
	testFile := &graph.Node{
		ID: `Test\WidgetService_Tests.cs`, Kind: graph.KindFile,
		Name: "WidgetService_Tests.cs", FilePath: `Test\WidgetService_Tests.cs`,
		Meta: map[string]any{"is_test_file": true},
	}
	// Parameter node: no test metadata of any kind.
	testParam := &graph.Node{
		ID: `Test\WidgetService_Tests.cs::WidgetServiceTests.Run#param:svc`, Kind: graph.KindParam,
		Name: "svc", FilePath: `Test\WidgetService_Tests.cs`, StartLine: 20,
	}
	for _, n := range []*graph.Node{target, prodCaller, testMethod, testFile, testParam} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: prodCaller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/Consumer.cs", Line: 14})
	g.AddEdge(&graph.Edge{From: testMethod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: testMethod.FilePath, Line: 22})
	g.AddEdge(&graph.Edge{From: testFile.ID, To: target.ID, Kind: graph.EdgeImports, FilePath: testFile.FilePath, Line: 1})
	g.AddEdge(&graph.Edge{From: testParam.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: testParam.FilePath, Line: 20})

	eng := NewEngine(g)
	sg := eng.FindUsagesScoped(target.ID, QueryOptions{ExcludeTests: true})

	require.Len(t, sg.Edges, 1, "exclude_tests must drop every test-path from-node, not just stamped methods")
	require.Equal(t, prodCaller.ID, sg.Edges[0].From, "the production caller is the only edge that may survive")
	for _, n := range sg.Nodes {
		require.NotEqual(t, testFile.ID, n.ID, "test file node leaked into a production-only result")
		require.NotEqual(t, testParam.ID, n.ID, "test param node leaked into a production-only result")
	}
}

// TestFindUsagesScoped_ExcludeTestsCoversAnnotationOwnerChildren pins
// the classifier's owner hop: an annotation-discovered test (a #[test]
// fn in a production-looking path) is stamped on the function node
// only, so its parameter child has neither a stamp nor a test-looking
// path. The child must inherit the owner's test identity through the
// `<owner-id>#...` ID convention.
func TestFindUsagesScoped_ExcludeTestsCoversAnnotationOwnerChildren(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "src/widget.rs::Widget", Kind: graph.KindType,
		Name: "Widget", FilePath: "src/widget.rs", StartLine: 3,
	}
	// As the annotation pass leaves it: the fn is stamped, the child not.
	testFn := &graph.Node{
		ID: "src/lib.rs::check_widget", Kind: graph.KindFunction,
		Name: "check_widget", FilePath: "src/lib.rs", StartLine: 40,
		Meta: map[string]any{"is_test": true, "test_role": "test"},
	}
	testParam := &graph.Node{
		ID: "src/lib.rs::check_widget#param:w", Kind: graph.KindParam,
		Name: "w", FilePath: "src/lib.rs", StartLine: 40,
	}
	prodCaller := &graph.Node{
		ID: "src/main.rs::run", Kind: graph.KindFunction,
		Name: "run", FilePath: "src/main.rs", StartLine: 10,
	}
	for _, n := range []*graph.Node{target, testFn, testParam, prodCaller} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: prodCaller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/main.rs", Line: 12})
	g.AddEdge(&graph.Edge{From: testFn.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/lib.rs", Line: 42})
	g.AddEdge(&graph.Edge{From: testParam.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: "src/lib.rs", Line: 40})

	eng := NewEngine(g)
	sg := eng.FindUsagesScoped(target.ID, QueryOptions{ExcludeTests: true})

	require.Len(t, sg.Edges, 1, "the stamped owner's param child must be excluded with it")
	require.Equal(t, prodCaller.ID, sg.Edges[0].From)
}

// TestGetCallers_ExcludeTestsSurvivesRawPageOfTests pins the SQLite
// starvation case: the batched frontier expansion caps RAW rows per
// call, so when the first raw page is all test callers a post-fetch
// filter used to discard the entire page and report zero production
// callers with truncated=false — while a real production caller sat
// beyond the cap. The filter must never consume the eligible-result
// limit with rows it then drops.
func TestGetCallers_ExcludeTestsSurvivesRawPageOfTests(t *testing.T) {
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "callers.sqlite"))
	require.NoError(t, err)
	defer g.Close()

	target := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(target)
	// Ten stamped test callers whose IDs sort ahead of the production
	// caller, inserted first, so any raw page of five rows is all tests.
	for i := 0; i < 10; i++ {
		file := fmt.Sprintf("pkg/a%02d_test.go", i)
		caller := &graph.Node{
			ID: fmt.Sprintf("%s::TestUse%02d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("TestUse%02d", i), FilePath: file, StartLine: 3,
			Meta: map[string]any{"is_test": true},
		}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5})
	}
	prod := &graph.Node{ID: "pkg/z_prod.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/z_prod.go", StartLine: 8}
	g.AddNode(prod)
	g.AddEdge(&graph.Edge{From: prod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "pkg/z_prod.go", Line: 9})

	eng := NewEngine(g)
	sg := eng.GetCallers(target.ID, QueryOptions{ExcludeTests: true, Limit: 5, Depth: 1})

	froms := make(map[string]bool)
	for _, e := range sg.Edges {
		froms[e.From] = true
	}
	require.True(t, froms[prod.ID],
		"the production caller must be found even when a raw page of test rows precedes it")
}
