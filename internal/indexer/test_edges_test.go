package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestMarkTestSymbolsAndEmitEdges_GoStyle(t *testing.T) {
	g := graph.New()

	// Two files: one prod, one test.
	g.AddNode(&graph.Node{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"})

	// Subject under test plus a helper, both in pkg/foo.go.
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "pkg/foo.go", Language: "go"})

	// Test function in pkg/foo_test.go.
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})

	// Calls.
	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "pkg/foo.go::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 10})
	g.AddEdge(&graph.Edge{From: "pkg/foo.go::Foo", To: "pkg/foo.go::helper", Kind: graph.EdgeCalls, FilePath: "pkg/foo.go", Line: 5})

	marked, emitted := markTestSymbolsAndEmitEdges(g)

	if marked != 1 {
		t.Fatalf("expected 1 test symbol marked, got %d", marked)
	}
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests, got %d", emitted)
	}

	// TestFoo must be flagged.
	testFn := g.GetNode("pkg/foo_test.go::TestFoo")
	if isTest, _ := testFn.Meta["is_test"].(bool); !isTest {
		t.Fatalf("TestFoo should be flagged is_test, got %v", testFn.Meta["is_test"])
	}

	// EdgeTests must point from TestFoo → Foo.
	var found bool
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeTests && e.From == "pkg/foo_test.go::TestFoo" && e.To == "pkg/foo.go::Foo" {
			found = true
			if e.Line != 10 {
				t.Errorf("EdgeTests line = %d, want 10", e.Line)
			}
		}
	}
	if !found {
		t.Fatalf("EdgeTests TestFoo→Foo not found")
	}

	// Foo → helper is a prod-to-prod call; no EdgeTests should be emitted.
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeTests && e.From == "pkg/foo.go::Foo" {
			t.Fatalf("unexpected EdgeTests from prod fn: %+v", e)
		}
	}
}

func TestMarkTestSymbolsAndEmitEdges_NameAloneIsNotEnough(t *testing.T) {
	g := graph.New()

	// A production file holding a function whose name happens to match
	// the Go test prefix — e.g. TestRole in testpattern.go. go test
	// never picks this up, so it must not be flagged.
	g.AddNode(&graph.Node{ID: "pkg/p.go", Kind: graph.KindFile, Name: "pkg/p.go", FilePath: "pkg/p.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/p.go::TestRole", Kind: graph.KindFunction, Name: "TestRole", FilePath: "pkg/p.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/p.go::BenchmarkConfig", Kind: graph.KindFunction, Name: "BenchmarkConfig", FilePath: "pkg/p.go", Language: "go"})

	marked, _ := markTestSymbolsAndEmitEdges(g)
	if marked != 0 {
		t.Fatalf("expected 0 test symbols marked for prod file, got %d", marked)
	}
	for _, id := range []string{"pkg/p.go::TestRole", "pkg/p.go::BenchmarkConfig"} {
		n := g.GetNode(id)
		if v, _ := n.Meta["is_test"].(bool); v {
			t.Fatalf("%s in a non-test file must not be flagged is_test", id)
		}
		if _, ok := n.Meta["test_role"]; ok {
			t.Fatalf("%s in a non-test file must not carry test_role", id)
		}
	}
}

func TestMarkTestSymbolsAndEmitEdges_RoleRefinement(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x_test.go", Kind: graph.KindFile, Name: "x_test.go", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::TestA", Kind: graph.KindFunction, Name: "TestA", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::BenchmarkA", Kind: graph.KindFunction, Name: "BenchmarkA", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::FuzzA", Kind: graph.KindFunction, Name: "FuzzA", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::ExampleA", Kind: graph.KindFunction, Name: "ExampleA", FilePath: "x_test.go", Language: "go"})
	// Support code in the test file with no recognised prefix → "test".
	g.AddNode(&graph.Node{ID: "x_test.go::setup", Kind: graph.KindFunction, Name: "setup", FilePath: "x_test.go", Language: "go"})

	markTestSymbolsAndEmitEdges(g)

	want := map[string]string{
		"x_test.go::TestA":      "test",
		"x_test.go::BenchmarkA": "benchmark",
		"x_test.go::FuzzA":      "fuzz",
		"x_test.go::ExampleA":   "example",
		"x_test.go::setup":      "test",
	}
	for id, role := range want {
		got, _ := g.GetNode(id).Meta["test_role"].(string)
		if got != role {
			t.Errorf("%s: test_role = %q, want %q", id, got, role)
		}
	}
}

func TestMarkTestSymbolsAndEmitEdges_PythonStyle(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/svc.py", Kind: graph.KindFile, Name: "app/svc.py", FilePath: "app/svc.py", Language: "python"})
	g.AddNode(&graph.Node{ID: "tests/test_svc.py", Kind: graph.KindFile, Name: "tests/test_svc.py", FilePath: "tests/test_svc.py", Language: "python"})

	g.AddNode(&graph.Node{ID: "app/svc.py::greet", Kind: graph.KindFunction, Name: "greet", FilePath: "app/svc.py", Language: "python"})
	g.AddNode(&graph.Node{ID: "tests/test_svc.py::test_greet", Kind: graph.KindFunction, Name: "test_greet", FilePath: "tests/test_svc.py", Language: "python"})

	g.AddEdge(&graph.Edge{From: "tests/test_svc.py::test_greet", To: "app/svc.py::greet", Kind: graph.EdgeCalls, FilePath: "tests/test_svc.py", Line: 3})

	_, emitted := markTestSymbolsAndEmitEdges(g)
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests, got %d", emitted)
	}
}

func TestMarkTestSymbolsAndEmitEdges_DropsTestToTestCalls(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x_test.go", Kind: graph.KindFile, Name: "x_test.go", FilePath: "x_test.go", Language: "go"})

	g.AddNode(&graph.Node{ID: "x_test.go::TestA", Kind: graph.KindFunction, Name: "TestA", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::setupFixture", Kind: graph.KindFunction, Name: "setupFixture", FilePath: "x_test.go", Language: "go"})

	// Test calls a helper in the same test file — no EdgeTests should emit.
	g.AddEdge(&graph.Edge{From: "x_test.go::TestA", To: "x_test.go::setupFixture", Kind: graph.EdgeCalls, FilePath: "x_test.go", Line: 4})

	_, emitted := markTestSymbolsAndEmitEdges(g)
	if emitted != 0 {
		t.Fatalf("expected 0 EdgeTests for test→test call, got %d", emitted)
	}
}

func TestMarkTestSymbolsAndEmitEdges_RustInlineCfgTest(t *testing.T) {
	g := graph.New()

	// A production-path Rust file: src/lib.rs is NOT a test file by name
	// or directory, but it holds an inline `#[cfg(test)] mod tests`
	// whose functions carry #[test].
	g.AddNode(&graph.Node{ID: "src/lib.rs", Kind: graph.KindFile, Name: "src/lib.rs", FilePath: "src/lib.rs", Language: "rust"})
	g.AddNode(&graph.Node{ID: "src/lib.rs::add", Kind: graph.KindFunction, Name: "add", FilePath: "src/lib.rs", Language: "rust"})
	g.AddNode(&graph.Node{ID: "src/lib.rs::it_adds", Kind: graph.KindFunction, Name: "it_adds", FilePath: "src/lib.rs", Language: "rust"})
	g.AddNode(&graph.Node{ID: "src/lib.rs::bench_add", Kind: graph.KindFunction, Name: "bench_add", FilePath: "src/lib.rs", Language: "rust"})

	// Annotation nodes + EdgeAnnotated edges, as emitRustAnnotationEdges
	// would emit them. #[test] → annotation::rust::test; #[bench] →
	// annotation::rust::bench; #[cfg(test)] on the module → annotation::rust::cfg.
	g.AddNode(&graph.Node{ID: "annotation::rust::test", Kind: graph.KindType, Name: "test", FilePath: "src/lib.rs", Language: "rust", Meta: map[string]any{"kind": "annotation", "synthetic": true}})
	g.AddNode(&graph.Node{ID: "annotation::rust::bench", Kind: graph.KindType, Name: "bench", FilePath: "src/lib.rs", Language: "rust", Meta: map[string]any{"kind": "annotation", "synthetic": true}})
	g.AddNode(&graph.Node{ID: "annotation::rust::cfg", Kind: graph.KindType, Name: "cfg", FilePath: "src/lib.rs", Language: "rust", Meta: map[string]any{"kind": "annotation", "synthetic": true}})
	g.AddEdge(&graph.Edge{From: "src/lib.rs::it_adds", To: "annotation::rust::test", Kind: graph.EdgeAnnotated, FilePath: "src/lib.rs", Line: 12})
	g.AddEdge(&graph.Edge{From: "src/lib.rs::bench_add", To: "annotation::rust::bench", Kind: graph.EdgeAnnotated, FilePath: "src/lib.rs", Line: 18})

	// The test calls the subject under test.
	g.AddEdge(&graph.Edge{From: "src/lib.rs::it_adds", To: "src/lib.rs::add", Kind: graph.EdgeCalls, FilePath: "src/lib.rs", Line: 13})

	marked, emitted := markTestSymbolsAndEmitEdges(g)

	if marked != 2 {
		t.Fatalf("expected 2 annotation-driven test symbols marked, got %d", marked)
	}
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests (it_adds→add), got %d", emitted)
	}

	itAdds := g.GetNode("src/lib.rs::it_adds")
	if v, _ := itAdds.Meta["is_test"].(bool); !v {
		t.Fatalf("it_adds (#[test] in prod file) should be flagged is_test")
	}
	if r, _ := itAdds.Meta["test_role"].(string); r != "test" {
		t.Errorf("it_adds test_role = %q, want test", r)
	}
	if r, _ := itAdds.Meta["test_runner"].(string); r != "cargo-test" {
		t.Errorf("it_adds test_runner = %q, want cargo-test", r)
	}

	bench := g.GetNode("src/lib.rs::bench_add")
	if r, _ := bench.Meta["test_role"].(string); r != "benchmark" {
		t.Errorf("bench_add test_role = %q, want benchmark", r)
	}

	// The non-test production function must NOT be flagged.
	if v, _ := g.GetNode("src/lib.rs::add").Meta["is_test"].(bool); v {
		t.Fatalf("add (no test annotation) must not be flagged is_test")
	}

	// EdgeTests must point it_adds → add.
	var found bool
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeTests && e.From == "src/lib.rs::it_adds" && e.To == "src/lib.rs::add" {
			found = true
		}
	}
	if !found {
		t.Fatalf("EdgeTests it_adds→add not found")
	}
}

func TestMarkTestSymbolsAndEmitEdges_JavaAtTestInProdPathFile(t *testing.T) {
	g := graph.New()

	// Service.java is a production-path file (no Test/Tests suffix) but
	// holds a @Test method — JUnit discovers it by annotation.
	g.AddNode(&graph.Node{ID: "src/Service.java", Kind: graph.KindFile, Name: "src/Service.java", FilePath: "src/Service.java", Language: "java"})
	g.AddNode(&graph.Node{ID: "src/Service.java::Service.handle", Kind: graph.KindMethod, Name: "handle", FilePath: "src/Service.java", Language: "java"})
	g.AddNode(&graph.Node{ID: "src/Service.java::Service.shouldHandle", Kind: graph.KindMethod, Name: "shouldHandle", FilePath: "src/Service.java", Language: "java"})

	g.AddNode(&graph.Node{ID: "annotation::java::Test", Kind: graph.KindType, Name: "Test", FilePath: "src/Service.java", Language: "java", Meta: map[string]any{"kind": "annotation", "synthetic": true}})
	g.AddEdge(&graph.Edge{From: "src/Service.java::Service.shouldHandle", To: "annotation::java::Test", Kind: graph.EdgeAnnotated, FilePath: "src/Service.java", Line: 20})
	g.AddEdge(&graph.Edge{From: "src/Service.java::Service.shouldHandle", To: "src/Service.java::Service.handle", Kind: graph.EdgeCalls, FilePath: "src/Service.java", Line: 21})

	marked, emitted := markTestSymbolsAndEmitEdges(g)
	if marked != 1 {
		t.Fatalf("expected 1 @Test method marked, got %d", marked)
	}
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests, got %d", emitted)
	}
	m := g.GetNode("src/Service.java::Service.shouldHandle")
	if v, _ := m.Meta["is_test"].(bool); !v {
		t.Fatalf("@Test method should be flagged is_test")
	}
	if r, _ := m.Meta["test_runner"].(string); r != "junit" {
		t.Errorf("test_runner = %q, want junit", r)
	}
	// A plain @Component / non-test annotation must not flag the symbol.
	if v, _ := g.GetNode("src/Service.java::Service.handle").Meta["is_test"].(bool); v {
		t.Fatalf("non-annotated production method must not be flagged is_test")
	}
}

func TestMarkTestSymbolsAndEmitEdges_NonTestAnnotationIgnored(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/m.rs", Kind: graph.KindFile, Name: "src/m.rs", FilePath: "src/m.rs", Language: "rust"})
	g.AddNode(&graph.Node{ID: "src/m.rs::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "src/m.rs", Language: "rust"})
	// #[derive(Debug)] is an annotation but not a test.
	g.AddNode(&graph.Node{ID: "annotation::rust::Debug", Kind: graph.KindType, Name: "Debug", FilePath: "src/m.rs", Language: "rust", Meta: map[string]any{"kind": "annotation", "synthetic": true}})
	g.AddEdge(&graph.Edge{From: "src/m.rs::Widget", To: "annotation::rust::Debug", Kind: graph.EdgeAnnotated, FilePath: "src/m.rs", Line: 3})

	marked, _ := markTestSymbolsAndEmitEdges(g)
	if marked != 0 {
		t.Fatalf("non-test annotation must not flag any symbol, got %d marked", marked)
	}
}

func TestMarkTestSymbolsAndEmitEdges_DedupesParallelCalls(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x_test.go", Kind: graph.KindFile, Name: "x_test.go", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x.go", Kind: graph.KindFile, Name: "x.go", FilePath: "x.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x_test.go::TestA", Kind: graph.KindFunction, Name: "TestA", FilePath: "x_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "x.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "x.go", Language: "go"})

	// Same call twice on different lines (the test calls F at line 5 and 12).
	g.AddEdge(&graph.Edge{From: "x_test.go::TestA", To: "x.go::F", Kind: graph.EdgeCalls, FilePath: "x_test.go", Line: 5})
	g.AddEdge(&graph.Edge{From: "x_test.go::TestA", To: "x.go::F", Kind: graph.EdgeCalls, FilePath: "x_test.go", Line: 12})

	_, emitted := markTestSymbolsAndEmitEdges(g)
	if emitted != 1 {
		t.Fatalf("expected 1 deduped EdgeTests, got %d", emitted)
	}
}

func TestMarkTestSymbolsAndEmitEdges_PersistsMetadataAcrossSQLiteReload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.sqlite")
	g, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}

	g.AddBatch([]*graph.Node{
		{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"},
		{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go", Language: "go"},
		{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"},
		{ID: "pkg/foo_test.go::Suite.TestBar", Kind: graph.KindMethod, Name: "TestBar", FilePath: "pkg/foo_test.go", Language: "go"},
		{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", Language: "go"},
	}, []*graph.Edge{
		{From: "pkg/foo_test.go::TestFoo", To: "pkg/foo.go::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 10},
		{From: "pkg/foo_test.go::Suite.TestBar", To: "pkg/foo.go::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 20},
	})

	marked, emitted := markTestSymbolsAndEmitEdges(g)
	if marked != 2 || emitted != 2 {
		t.Fatalf("mark/emit counts = %d/%d, want 2/2", marked, emitted)
	}
	markedAgain, emittedAgain := markTestSymbolsAndEmitEdges(g)
	if markedAgain != 2 || emittedAgain != 2 {
		t.Fatalf("repeat mark/emit counts = %d/%d, want 2/2", markedAgain, emittedAgain)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	reloaded, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	defer func() { _ = reloaded.Close() }()

	file := reloaded.GetNode("pkg/foo_test.go")
	if file == nil {
		t.Fatal("test file missing after reload")
	}
	if got, _ := file.Meta["is_test_file"].(bool); !got {
		t.Fatalf("test file is_test_file = %v, want true", file.Meta["is_test_file"])
	}
	if got, _ := file.Meta["test_runner"].(string); got != "gotest" {
		t.Fatalf("test file test_runner = %q, want gotest", got)
	}

	for _, id := range []string{"pkg/foo_test.go::TestFoo", "pkg/foo_test.go::Suite.TestBar"} {
		n := reloaded.GetNode(id)
		if n == nil {
			t.Fatalf("test symbol %s missing after reload", id)
		}
		if got, _ := n.Meta["is_test"].(bool); !got {
			t.Errorf("%s is_test = %v, want true", id, n.Meta["is_test"])
		}
		if got, _ := n.Meta["test_role"].(string); got != "test" {
			t.Errorf("%s test_role = %q, want test", id, got)
		}
		if got, _ := n.Meta["test_runner"].(string); got != "gotest" {
			t.Errorf("%s test_runner = %q, want gotest", id, got)
		}
	}

	var persistedEdges int
	for edge := range reloaded.EdgesByKind(graph.EdgeTests) {
		if edge != nil && edge.To == "pkg/foo.go::Foo" {
			persistedEdges++
		}
	}
	if persistedEdges != 2 {
		t.Fatalf("persisted EdgeTests = %d, want 2", persistedEdges)
	}
}

func TestMarkTestSymbolsAndEmitEdges_SkipsUnresolvedCallTargets(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})

	// One resolved subject call, one still-unresolved call (an assertion
	// framework member the repo can never bind). Cloning the unresolved
	// call would mint a tests edge with NONE of the original's receiver
	// evidence - a later resolve pass then re-binds the naked clone
	// without the guards that protect the calls edge.
	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "pkg/foo.go::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 10})
	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "unresolved::*.Equal", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 11})

	_, emitted := markTestSymbolsAndEmitEdges(g)
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests (the resolved subject only), got %d", emitted)
	}
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeTests && graph.IsUnresolvedTarget(e.To) {
			t.Fatalf("tests edge cloned an unresolved call: %+v", e)
		}
	}
}

// The scoped sibling: markTestSymbolsAndEmitEdgesForFilesLocked is the
// emitter behind every warm save's file-scoped sweep, and it carries its
// own unresolved-call skip independent of the whole-graph emitter above.
// Same two-call fixture, driven through the file-scoped entry point.
func TestMarkTestSymbolsAndEmitEdgesScoped_SkipsUnresolvedCallTargets(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})

	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "pkg/foo.go::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 10})
	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "unresolved::*.Equal", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 11})

	_, emitted := markTestSymbolsAndEmitEdgesScoped(g, nil, "pkg/foo_test.go")
	if emitted != 1 {
		t.Fatalf("expected 1 EdgeTests (the resolved subject only), got %d", emitted)
	}
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeTests && graph.IsUnresolvedTarget(e.To) {
			t.Fatalf("scoped sweep cloned an unresolved call: %+v", e)
		}
	}
}

// findEdge returns the first edge with the given kind, from, and to.
func findEdge(g graph.Store, kind graph.EdgeKind, from, to string) *graph.Edge {
	for _, e := range g.AllEdges() {
		if e != nil && e.Kind == kind && e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

// The rebind sibling of the declaration-only test below: the subject moved
// to another file, the caller file itself never changed, and a stale
// projection to the old location is still on the books. The plan names only
// the NEW subject file, so without the retarget frontier the caller is
// never swept: the stale row survives and the moved projection is absent.
// (An IncrementalReindexPaths-shaped version of this scenario is vacuous as
// a pin - its derived plan names the caller file, so the pre-existing
// scoped sweep produces the expected result with the frontier removed.)
func TestDeclarationOnlyPlanMovesTestsProjectionOnRebind(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})
	// The post-restub state after the subject's old file was evicted: the
	// call is back to a stub, but the caller's derived projection to the
	// old location was cloned earlier and still persists.
	call := &graph.Edge{From: "pkg/foo_test.go::TestFoo", To: graph.UnresolvedMarker + "Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 5}
	g.AddEdge(call)
	g.AddEdge(&graph.Edge{From: "pkg/foo_test.go::TestFoo", To: "pkg/foo.go::Foo", Kind: graph.EdgeTests, FilePath: "pkg/foo_test.go", Line: 5, Origin: graph.OriginASTInferred})

	// The subject reappears in another file; the incoming pass rebinds.
	g.AddNode(&graph.Node{ID: "pkg/bar.go", Kind: graph.KindFile, Name: "pkg/bar.go", FilePath: "pkg/bar.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/bar.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/bar.go", Language: "go"})
	idx := newTestIndexer(g)
	idx.resolver.ResolveIncomingForFile("pkg/bar.go")
	if call.To != "pkg/bar.go::Foo" {
		t.Fatalf("fixture: the pending call must rebind, got %q", call.To)
	}

	idx.runStandaloneIncrementalDerivedPasses(DerivedInvalidationPlan{
		Flags: DerivedInvalidatesDeclarations,
		Files: []string{"pkg/bar.go"},
	})

	if e := findEdge(g, graph.EdgeTests, "pkg/foo_test.go::TestFoo", "pkg/bar.go::Foo"); e == nil {
		t.Fatalf("rebound test call has no EdgeTests projection")
	}
	if e := findEdge(g, graph.EdgeTests, "pkg/foo_test.go::TestFoo", "pkg/foo.go::Foo"); e != nil {
		t.Fatalf("stale projection to the evicted subject survived: %+v", e)
	}
}

// The declaration-only shape: a definition-side derived plan carries
// neither the runtime nor the tests flag, so the coordinator previously
// skipped test projection entirely - even though the resolution step of
// the same apply just bound a pending TEST caller to the new subject.
// The resolution pass must report its retargeted test-call frontier and
// the coordinator must reconcile those callers regardless of plan flags.
func TestDeclarationOnlyPlanReconcilesLaterResolvedTestCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, Name: "pkg/foo_test.go", FilePath: "pkg/foo_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})
	call := &graph.Edge{From: "pkg/foo_test.go::TestFoo", To: graph.UnresolvedMarker + "Foo", Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 5}
	g.AddEdge(call)
	if _, emitted := markTestSymbolsAndEmitEdges(g); emitted != 0 {
		t.Fatalf("fixture: an unresolved call must not project, emitted %d", emitted)
	}

	// The subject arrives; the incoming pass binds the pending caller.
	g.AddNode(&graph.Node{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", Language: "go"})
	idx := newTestIndexer(g)
	idx.resolver.ResolveIncomingForFile("pkg/foo.go")
	if call.To != "pkg/foo.go::Foo" {
		t.Fatalf("fixture: the pending call must bind, got %q", call.To)
	}

	idx.runStandaloneIncrementalDerivedPasses(DerivedInvalidationPlan{
		Flags: DerivedInvalidatesDeclarations,
		Files: []string{"pkg/foo.go"},
	})

	if e := findEdge(g, graph.EdgeTests, "pkg/foo_test.go::TestFoo", "pkg/foo.go::Foo"); e == nil {
		t.Fatalf("resolved test call has no EdgeTests projection")
	}
}

// The cross-repository sibling: an inbound test call from repoA binds into
// a repoB subject through the cross-repo pass, which has no derived plan
// at all - the reconcile must ride the pass itself.
func TestCrossRepoResolveReconcilesRetargetedTestCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/pkg/a_test.go", Kind: graph.KindFile, Name: "a_test.go", FilePath: "repoA/pkg/a_test.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
	g.AddNode(&graph.Node{ID: "repoA/pkg/a_test.go::TestCaller", Kind: graph.KindFunction, Name: "TestCaller", FilePath: "repoA/pkg/a_test.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB", WorkspaceID: "ws"})
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB", WorkspaceID: "ws"})
	// Import-reachability evidence for the cross-repo fallback.
	g.AddEdge(&graph.Edge{From: "repoA/pkg/a_test.go", To: "repoB/lib/c.go", Kind: graph.EdgeImports, FilePath: "repoA/pkg/a_test.go", Line: 1})
	call := &graph.Edge{From: "repoA/pkg/a_test.go::TestCaller", To: graph.UnresolvedMarker + "Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a_test.go", Line: 5}
	g.AddEdge(call)

	if _, emitted := markTestSymbolsAndEmitEdges(g); emitted != 0 {
		t.Fatalf("fixture: an unresolved call must not project, emitted %d", emitted)
	}

	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), nil, zap.NewNop())
	if err := mi.runCrossRepoResolveContext(context.Background(), false); err != nil {
		t.Fatalf("cross-repo resolve: %v", err)
	}
	if call.To != "repoB/lib/c.go::Helper" {
		t.Fatalf("fixture: the cross-repo pass must bind the call, got %q", call.To)
	}
	if e := findEdge(g, graph.EdgeTests, "repoA/pkg/a_test.go::TestCaller", "repoB/lib/c.go::Helper"); e == nil {
		t.Fatalf("cross-repo resolved test call has no EdgeTests projection")
	}
}

// The whole-graph master lane: runMasterResolveHookedContext runs
// master.ResolveAllContext and is the funnel for every whole-graph master
// pass, including resolveDeferredMutations' fail-closed fullFallback taken
// whenever a mutation receipt comes back incomplete. newMasterResolver
// returns a fresh resolver per call, so a frontier left undrained there is
// garbage-collected unread - the file-scoped lane (the control) drains,
// the whole-graph lane must too.
func TestMasterResolveWholeGraphReconcilesRetargetedTestCalls(t *testing.T) {
	build := func() (*MultiIndexer, *graph.Edge, graph.Store) {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "repoA/pkg/foo_test.go", Kind: graph.KindFile, Name: "foo_test.go", FilePath: "repoA/pkg/foo_test.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
		g.AddNode(&graph.Node{ID: "repoA/pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "repoA/pkg/foo_test.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
		g.AddNode(&graph.Node{ID: "repoA/pkg/foo.go", Kind: graph.KindFile, Name: "foo.go", FilePath: "repoA/pkg/foo.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
		g.AddNode(&graph.Node{ID: "repoA/pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/pkg/foo.go", Language: "go", RepoPrefix: "repoA", WorkspaceID: "ws"})
		call := &graph.Edge{From: "repoA/pkg/foo_test.go::TestFoo", To: graph.UnresolvedMarker + "Foo", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/foo_test.go", Line: 5}
		g.AddEdge(call)
		if _, emitted := markTestSymbolsAndEmitEdges(g); emitted != 0 {
			t.Fatalf("fixture: unresolved call must not project, emitted %d", emitted)
		}
		return NewMultiIndexer(g, newTestRegistry(), search.NewNull(), nil, zap.NewNop()), call, g
	}

	t.Run("control_runMasterResolveFiles", func(t *testing.T) {
		mi, call, g := build()
		mi.runMasterResolveFiles([]string{"repoA/pkg/foo.go"}, false)
		if call.To != "repoA/pkg/foo.go::Foo" {
			t.Fatalf("fixture: call must bind, got %q", call.To)
		}
		if e := findEdge(g, graph.EdgeTests, "repoA/pkg/foo_test.go::TestFoo", "repoA/pkg/foo.go::Foo"); e == nil {
			t.Fatalf("control: expected EdgeTests projection")
		}
	})

	t.Run("subject_runMasterResolve_wholegraph", func(t *testing.T) {
		mi, call, g := build()
		mi.runMasterResolve(nil, false) // the fullFallback lane
		if call.To != "repoA/pkg/foo.go::Foo" {
			t.Fatalf("fixture: call must bind, got %q", call.To)
		}
		if e := findEdge(g, graph.EdgeTests, "repoA/pkg/foo_test.go::TestFoo", "repoA/pkg/foo.go::Foo"); e == nil {
			t.Fatalf("master ResolveAll bound the test call but no EdgeTests was reconciled")
		}
	})
}

// A cold index runs its own graph-wide test projection after ResolveAll,
// so every test caller the resolver noted on the retarget frontier is
// already covered - a frontier left parked there would make the first
// warm save re-project the entire test corpus under ResolveMutex for
// nothing. The cold path must discard it.
func TestColdIndexDiscardsRetargetedTestCallFrontier(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "foo.go"), "package pkg\n\nfunc Foo() {}\n")
	writeFile(t, filepath.Join(dir, "foo_test.go"),
		"package pkg\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) { Foo() }\n")
	g := graph.New()
	idx := newTestIndexer(g)
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("cold index: %v", err)
	}
	if e := findEdge(g, graph.EdgeTests, "foo_test.go::TestFoo", "foo.go::Foo"); e == nil {
		t.Fatalf("fixture: cold index must project the resolved test call")
	}
	if files := idx.resolver.TakeRetargetedTestCallFiles(); len(files) != 0 {
		t.Fatalf("cold index parked %d test caller(s) on the retarget frontier "+
			"after its own graph-wide projection: %v", len(files), files)
	}
}
