package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestResolveAll_InternalCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "pkg/b.go", Language: "go"})

	// Foo calls Bar (unresolved).
	callEdge := &graph.Edge{From: "pkg/a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Bar", callEdge.To)
	// A unique same-package bare-name call is structurally unambiguous, so it
	// is stamped ast_resolved — not left to backfill to the name-only tier
	// that find_usages suppresses by default.
	assert.Equal(t, graph.OriginASTResolved, callEdge.Origin,
		"a unique same-package bare-name call must be ast_resolved, not text_matched")
}

// TestResolveFunctionCall_UniqueBareCallTier is the F2.1 regression: a
// bare-name call to the sole same-file or same-package definition is
// grammar-grounded and structurally unambiguous, so it must be stamped
// ast_resolved (above the name-only tier find_usages suppresses by default).
// Ambiguous picks and non-call edges stay untagged so suppression can still
// drop them.
func TestResolveFunctionCall_UniqueBareCallTier(t *testing.T) {
	t.Run("same-file unique call is ast_resolved", func(t *testing.T) {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/a.go::Baz", Kind: graph.KindFunction, Name: "Baz", FilePath: "pkg/a.go", Language: "go"})
		e := &graph.Edge{From: "pkg/a.go::Baz", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 3}
		g.AddEdge(e)

		New(g).ResolveAll()

		assert.Equal(t, "pkg/a.go::Foo", e.To, "same-file call must resolve to Foo")
		assert.Equal(t, graph.OriginASTResolved, e.Origin, "unique same-file call must be ast_resolved")
	})

	t.Run("ambiguous same-package pick stays untagged", func(t *testing.T) {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "pkg/c.go", Language: "go"})
		// Two same-package definitions carry the name Foo (a function and a
		// method) — enough to make the bare-name pick ambiguous.
		g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/b.go::T.Foo", Kind: graph.KindMethod, Name: "Foo", FilePath: "pkg/b.go", Language: "go"})
		g.AddNode(&graph.Node{ID: "pkg/c.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/c.go", Language: "go"})
		e := &graph.Edge{From: "pkg/c.go::Caller", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/c.go", Line: 3}
		g.AddEdge(e)

		New(g).ResolveAll()

		assert.Equal(t, "", e.Origin,
			"an ambiguous same-package pick must stay untagged so suppression can drop it")
	})
}

func TestResolveAll_ExternalImport(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"})

	importEdge := &graph.Edge{From: "main.go", To: "unresolved::import::fmt", Kind: graph.EdgeImports, FilePath: "main.go", Line: 3}
	g.AddEdge(importEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "external::fmt", importEdge.To)
}

func TestResolveAll_ImportQualNamePrefersCallerRepoAcrossDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reverse bool
	}{
		{name: "forward_insert"},
		{name: "reverse_insert", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			const qualName = "github.com/acme/shared"

			callerA := &graph.Node{ID: "a-repo/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "a-repo/main.go", Language: "go", RepoPrefix: "a-repo", WorkspaceID: "ws-a"}
			callerZ := &graph.Node{ID: "z-repo/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "z-repo/main.go", Language: "go", RepoPrefix: "z-repo", WorkspaceID: "ws-z"}
			pkgA := &graph.Node{ID: "a-repo/shared/pkg.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "a-repo/shared/pkg.go", Language: "go", RepoPrefix: "a-repo", WorkspaceID: "ws-a"}
			pkgZ := &graph.Node{ID: "z-repo/shared/pkg.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "z-repo/shared/pkg.go", Language: "go", RepoPrefix: "z-repo", WorkspaceID: "ws-z"}

			g.AddNode(callerA)
			g.AddNode(callerZ)
			packages := []*graph.Node{pkgA, pkgZ}
			if tc.reverse {
				packages[0], packages[1] = packages[1], packages[0]
			}
			for _, pkg := range packages {
				g.AddNode(pkg)
			}

			edgeA := &graph.Edge{From: callerA.ID, To: "unresolved::import::" + qualName, Kind: graph.EdgeImports, FilePath: callerA.FilePath, Line: 3}
			edgeZ := &graph.Edge{From: callerZ.ID, To: "unresolved::import::" + qualName, Kind: graph.EdgeImports, FilePath: callerZ.FilePath, Line: 3}
			g.AddEdge(edgeA)
			g.AddEdge(edgeZ)

			stats := New(g).ResolveAll()

			require.Equal(t, 2, stats.Resolved)
			assert.Equal(t, pkgA.ID, edgeA.To)
			assert.Equal(t, pkgZ.ID, edgeZ.To)
			assert.False(t, edgeA.CrossRepo)
			assert.False(t, edgeZ.CrossRepo)
		})
	}
}

func TestResolveAll_ImportQualNameLeavesEquivalentForeignCandidatesUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reverse bool
	}{
		{name: "forward_insert"},
		{name: "reverse_insert", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			const qualName = "github.com/acme/shared"

			caller := &graph.Node{ID: "consumer/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "consumer/main.go", Language: "go", RepoPrefix: "consumer", WorkspaceID: "consumer-ws"}
			candidateA := &graph.Node{ID: "a-foreign/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "a-foreign/shared.go", Language: "go", RepoPrefix: "a-foreign", WorkspaceID: "foreign-ws"}
			candidateZ := &graph.Node{ID: "z-foreign/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "z-foreign/shared.go", Language: "go", RepoPrefix: "z-foreign", WorkspaceID: "foreign-ws"}

			g.AddNode(caller)
			candidates := []*graph.Node{candidateA, candidateZ}
			if tc.reverse {
				candidates[0], candidates[1] = candidates[1], candidates[0]
			}
			for _, candidate := range candidates {
				g.AddNode(candidate)
			}

			unresolvedID := "unresolved::import::" + qualName
			edge := &graph.Edge{From: caller.ID, To: unresolvedID, Kind: graph.EdgeImports, FilePath: caller.FilePath, Line: 3}
			g.AddEdge(edge)

			stats := New(g).ResolveAll()

			require.Equal(t, 0, stats.Resolved)
			assert.Equal(t, unresolvedID, edge.To)
			assert.False(t, edge.CrossRepo)
		})
	}
}

func TestResolveAll_MethodCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "pkg/b.go", Language: "go"})

	callEdge := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Server.Start", callEdge.To)
}

func TestResolveAll_Unresolvable(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})

	callEdge := &graph.Edge{From: "a.go::Foo", To: "unresolved::NonExistent", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Unresolved)
}

// Stdlib classification: Go stdlib paths have no dot in the first
// segment ("fmt", "encoding/json"), and extern:: edges for them get a
// `stdlib::` prefix after resolution.
func TestResolveAll_ExternStdlib(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"})

	callEdge := &graph.Edge{
		From: "a.go::Caller", To: "unresolved::extern::encoding/json::NewEncoder",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "stdlib::encoding/json::NewEncoder", callEdge.To)
}

// Third-party module paths always carry a dot in the first segment
// (github.com/..., golang.org/...) and resolve to a `dep::` stub.
func TestResolveAll_ExternDep(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"})

	callEdge := &graph.Edge{
		From: "a.go::Caller", To: "unresolved::extern::github.com/pkg/errors::Wrap",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "dep::github.com/pkg/errors::Wrap", callEdge.To)
}

// A TypeScript selector call into a String built-in (`.startsWith`) has
// no in-graph target, but we classify it rather than mark it unresolved
// — flow traces render `builtin (js · string)` instead of plain
// `unresolved`.
func TestResolveAll_BuiltinFromTS(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/foo.ts", Kind: graph.KindFile, Name: "foo.ts", FilePath: "src/foo.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/foo.ts::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "src/foo.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/foo.ts::Caller", To: "unresolved::*.startsWith",
		Kind: graph.EdgeCalls, FilePath: "src/foo.ts", Line: 5,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "builtin::ts::string::startsWith", callEdge.To)
}

// Python `list.append` — verifies the py classifier branch.
func TestResolveAll_BuiltinFromPython(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.py", Kind: graph.KindFile, Name: "a.py", FilePath: "a.py", Language: "python"})
	g.AddNode(&graph.Node{ID: "a.py::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.py", Language: "python"})

	callEdge := &graph.Edge{
		From: "a.py::caller", To: "unresolved::*.append",
		Kind: graph.EdgeCalls, FilePath: "a.py", Line: 2,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "builtin::py::list::append", callEdge.To)
}

// Unknown method on a known language still falls through to Unresolved
// — the classifier is not a catch-all.
func TestResolveAll_UnknownBuiltinStaysUnresolved(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/foo.ts", Kind: graph.KindFile, Name: "foo.ts", FilePath: "src/foo.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/foo.ts::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "src/foo.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/foo.ts::caller", To: "unresolved::*.mySuperObscureThing",
		Kind: graph.EdgeCalls, FilePath: "src/foo.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Unresolved)
}

// When the referenced symbol is actually indexed (e.g. another repo in
// a multi-repo setup), the extern edge rewires to the real node.
func TestResolveAll_ExternResolvesToIndexedSymbol(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "consumer/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "consumer/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "consumer/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "consumer/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "mylib/pkg/pkg.go", Kind: graph.KindFile, Name: "pkg.go", FilePath: "mylib/pkg/pkg.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "mylib/pkg/pkg.go::DoThing", Kind: graph.KindFunction, Name: "DoThing", FilePath: "mylib/pkg/pkg.go", Language: "go"})

	callEdge := &graph.Edge{
		From: "consumer/main.go::Caller", To: "unresolved::extern::mylib/pkg::DoThing",
		Kind: graph.EdgeCalls, FilePath: "consumer/main.go", Line: 5,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "mylib/pkg/pkg.go::DoThing", callEdge.To)
}

func TestInferImplements(t *testing.T) {
	g := graph.New()

	// Interface with two methods.
	g.AddNode(&graph.Node{
		ID: "pkg/store.go::Repository", Kind: graph.KindInterface, Name: "Repository",
		FilePath: "pkg/store.go", Language: "go", StartLine: 1,
		Meta: map[string]any{"methods": []string{"FindByID", "Save"}},
	})

	// Type that satisfies the interface.
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo", Kind: graph.KindType, Name: "SQLRepo",
		FilePath: "pkg/db.go", Language: "go", StartLine: 1,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo.FindByID", Kind: graph.KindMethod, Name: "FindByID",
		FilePath: "pkg/db.go", Language: "go", StartLine: 5,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo.Save", Kind: graph.KindMethod, Name: "Save",
		FilePath: "pkg/db.go", Language: "go", StartLine: 10,
	})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::SQLRepo.FindByID", To: "pkg/db.go::SQLRepo", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 5})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::SQLRepo.Save", To: "pkg/db.go::SQLRepo", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 10})

	// Type that does NOT satisfy (missing Save).
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::ReadOnly", Kind: graph.KindType, Name: "ReadOnly",
		FilePath: "pkg/db.go", Language: "go", StartLine: 20,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::ReadOnly.FindByID", Kind: graph.KindMethod, Name: "FindByID",
		FilePath: "pkg/db.go", Language: "go", StartLine: 22,
	})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::ReadOnly.FindByID", To: "pkg/db.go::ReadOnly", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 22})

	r := New(g)
	added := r.InferImplements()

	assert.Equal(t, 1, added)

	// Verify the implements edge was added.
	edges := g.GetInEdges("pkg/store.go::Repository")
	var implEdges []*graph.Edge
	for _, e := range edges {
		if e.Kind == graph.EdgeImplements {
			implEdges = append(implEdges, e)
		}
	}
	assert.Len(t, implEdges, 1)
	assert.Equal(t, "pkg/db.go::SQLRepo", implEdges[0].From)
}

func TestInferImplements_EmptyInterface(t *testing.T) {
	g := graph.New()

	// Empty interface should not match anything.
	g.AddNode(&graph.Node{
		ID: "pkg/any.go::Any", Kind: graph.KindInterface, Name: "Any",
		FilePath: "pkg/any.go", Language: "go", StartLine: 1,
		Meta: map[string]any{"methods": []string{}},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::Foo", Kind: graph.KindType, Name: "Foo",
		FilePath: "pkg/db.go", Language: "go", StartLine: 1,
	})

	r := New(g)
	added := r.InferImplements()
	assert.Equal(t, 0, added)
}

func TestResolveAll_PackageQualifiedFunctionCall(t *testing.T) {
	g := graph.New()
	// Caller in languages package calls parser.ParseFile — dispatched as "*.ParseFile"
	g.AddNode(&graph.Node{ID: "internal/parser/languages/golang.go::GoExtractor.Extract", Kind: graph.KindMethod, Name: "Extract", FilePath: "internal/parser/languages/golang.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "internal/parser/treesitter.go::ParseFile", Kind: graph.KindFunction, Name: "ParseFile", FilePath: "internal/parser/treesitter.go", Language: "go"})

	// This is how the Go extractor encodes pkg.Func() calls: "unresolved::*.ParseFile"
	callEdge := &graph.Edge{From: "internal/parser/languages/golang.go::GoExtractor.Extract", To: "unresolved::*.ParseFile", Kind: graph.EdgeCalls, FilePath: "internal/parser/languages/golang.go", Line: 94}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "internal/parser/treesitter.go::ParseFile", callEdge.To)
}

func TestResolveMethodCall_TypeAware(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})

	// Two methods named "Start" on different types.
	g.AddNode(&graph.Node{
		ID: "pkg/server.go::Server.Start", Kind: graph.KindMethod, Name: "Start",
		FilePath: "pkg/server.go", Language: "go",
		Meta: map[string]any{"receiver": "Server"},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/client.go::Client.Start", Kind: graph.KindMethod, Name: "Start",
		FilePath: "pkg/client.go", Language: "go",
		Meta: map[string]any{"receiver": "Client"},
	})

	// Call edge with type hint → should resolve to Server.Start.
	callEdge := &graph.Edge{
		From: "pkg/a.go::Caller", To: "unresolved::*.Start",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10,
		Meta: map[string]any{"receiver_type": "Server"},
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/server.go::Server.Start", callEdge.To)
	assert.Equal(t, 0.95, callEdge.Confidence) // same directory + exact type
}

func TestResolveMethodCall_TypeAware_Fallback(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start",
		FilePath: "pkg/b.go", Language: "go",
		Meta: map[string]any{"receiver": "Server"},
	})

	// Call edge with unknown type → falls back to name-only.
	callEdge := &graph.Edge{
		From: "pkg/a.go::Caller", To: "unresolved::*.Start",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10,
		Meta: map[string]any{"receiver_type": "UnknownType"},
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Server.Start", callEdge.To) // falls back to any method match
}

func TestResolveMethodCall_NoMeta(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start",
		FilePath: "pkg/b.go", Language: "go",
		Meta: map[string]any{"receiver": "Server"},
	})

	// Call edge with nil Meta → existing behavior.
	callEdge := &graph.Edge{
		From: "pkg/a.go::Caller", To: "unresolved::*.Start",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Server.Start", callEdge.To)
}

// Regression: a method passed as a *value* (e.g. `mux.HandleFunc("/p", h.foo)`)
// is captured by the Go extractor as `unresolved::*.foo` with kind=EdgeReads.
// Before the fix the resolver would re-target the edge to the method but
// leave the kind as EdgeReads, hiding it from both get_callers and
// find_usages. The fix promotes the kind to EdgeReferences when the
// field-then-method fallback lands on a method — so HTTP handlers,
// command tables, callback maps all become visible.
func TestResolveAll_MethodValuePromotesReadToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/routes.go::RegisterRoutes", Kind: graph.KindFunction, Name: "RegisterRoutes",
		FilePath: "pkg/routes.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/handler.go::Handler.HandleHealth", Kind: graph.KindMethod, Name: "HandleHealth",
		FilePath: "pkg/handler.go", Language: "go",
		Meta: map[string]any{"receiver": "Handler"},
	})

	// Mirror what the Go extractor emits for `h.HandleHealth` in arg position.
	edge := &graph.Edge{
		From: "pkg/routes.go::RegisterRoutes", To: "unresolved::*.HandleHealth",
		Kind: graph.EdgeReads, FilePath: "pkg/routes.go", Line: 42,
		Meta: map[string]any{"receiver_type": "Handler"},
	}
	g.AddEdge(edge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved, "method-value reference should resolve")
	assert.Equal(t, "pkg/handler.go::Handler.HandleHealth", edge.To,
		"edge should point at the real method")
	assert.Equal(t, graph.EdgeReferences, edge.Kind,
		"kind should be promoted Reads→References so get_callers/find_usages surface it")
}

// Regression: a return-type or param-type reference must resolve to
// a TYPE, never to a same-named function or method. Before the fix
// the resolver's default case routed `EdgeReturns` / `EdgeTypedAs`
// through resolveFunctionCall, so `func GetLanguage() *tsitter.Language`
// landed on a method named `Language` somewhere unrelated in the
// repo — leaving the `Language` type alias visibly unused and
// triggering false-positive dead-code skulls on every cross-package
// type re-export pattern.
func TestResolveAll_ReturnsAndTypedAsResolveToType(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/tsitter/tsitter.go::Language", Kind: graph.KindType, Name: "Language",
		FilePath: "pkg/tsitter/tsitter.go", StartLine: 30, Language: "go",
	})
	// Decoy method with the same name — pre-fix the resolver picked
	// this because it tried resolveFunctionCall first.
	g.AddNode(&graph.Node{
		ID: "pkg/apex/apex.go::Apex.Language", Kind: graph.KindMethod, Name: "Language",
		FilePath: "pkg/apex/apex.go", StartLine: 11, Language: "go",
		Meta: map[string]any{"receiver": "Apex"},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/ocaml/ocaml.go::GetLanguage", Kind: graph.KindFunction, Name: "GetLanguage",
		FilePath: "pkg/ocaml/ocaml.go", StartLine: 10, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/ocaml/ocaml.go::Run#param:lang", Kind: graph.KindParam, Name: "lang",
		FilePath: "pkg/ocaml/ocaml.go", StartLine: 20, Language: "go",
	})

	// `func GetLanguage() *tsitter.Language` → EdgeReturns
	retEdge := &graph.Edge{
		From: "pkg/ocaml/ocaml.go::GetLanguage", To: "unresolved::Language",
		Kind: graph.EdgeReturns, FilePath: "pkg/ocaml/ocaml.go", Line: 10,
	}
	g.AddEdge(retEdge)

	// `func Run(lang *tsitter.Language)` → EdgeTypedAs on the param
	typedAsEdge := &graph.Edge{
		From: "pkg/ocaml/ocaml.go::Run#param:lang", To: "unresolved::Language",
		Kind: graph.EdgeTypedAs, FilePath: "pkg/ocaml/ocaml.go", Line: 20,
	}
	g.AddEdge(typedAsEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 2, stats.Resolved)
	assert.Equal(t, "pkg/tsitter/tsitter.go::Language", retEdge.To,
		"EdgeReturns must resolve to the type, not the same-named method")
	assert.Equal(t, "pkg/tsitter/tsitter.go::Language", typedAsEdge.To,
		"EdgeTypedAs must resolve to the type, not the same-named method")
}

// Regression: a *bare* function name passed as a value (e.g.
// `&cobra.Command{RunE: runClean}`) is extracted as EdgeReads with
// To=`unresolved::runClean`. The heuristic default case calls
// resolveFunctionCall which lands on the function — but until the
// promotion was added the kind stayed as EdgeReads, hiding the
// wire-up site from get_callers / find_usages. Every cobra subcommand
// (and every other "function pointer as struct field" pattern)
// looked like dead code.
func TestResolveAll_BareFunctionValuePromotesReadToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "cmd/x/main.go::registerClean", Kind: graph.KindFunction, Name: "registerClean",
		FilePath: "cmd/x/main.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "cmd/x/clean.go::runClean", Kind: graph.KindFunction, Name: "runClean",
		FilePath: "cmd/x/clean.go", Language: "go",
	})

	edge := &graph.Edge{
		From: "cmd/x/main.go::registerClean", To: "unresolved::runClean",
		Kind: graph.EdgeReads, FilePath: "cmd/x/main.go", Line: 7,
	}
	g.AddEdge(edge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved, "bare function-value reference should resolve")
	assert.Equal(t, "cmd/x/clean.go::runClean", edge.To, "edge should point at the function")
	assert.Equal(t, graph.EdgeReferences, edge.Kind,
		"kind should be promoted Reads→References so get_callers/find_usages surface the wire-up site")
}

// Regression guard: a *real* field read should stay as EdgeReads even after
// the method-value promotion path was added. We add a struct field named
// `count` and a same-named method on a different type, and make sure the
// field-typed receiver picks the field (not the method) and keeps EdgeReads.
func TestResolveAll_FieldReadStaysAsReads(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "pkg/a.go", Language: "go",
	})
	// The actual field we want resolution to pick.
	g.AddNode(&graph.Node{
		ID: "pkg/types.go::Counter.count", Kind: graph.KindField, Name: "count",
		FilePath: "pkg/types.go", Language: "go",
		Meta: map[string]any{"receiver": "Counter"},
	})

	edge := &graph.Edge{
		From: "pkg/a.go::Caller", To: "unresolved::*.count",
		Kind: graph.EdgeReads, FilePath: "pkg/a.go", Line: 7,
		Meta: map[string]any{"receiver_type": "Counter"},
	}
	g.AddEdge(edge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/types.go::Counter.count", edge.To)
	assert.Equal(t, graph.EdgeReads, edge.Kind,
		"field-typed resolution must NOT be promoted — only method-typed fallback should be")
}

func TestResolveFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go", Language: "go"})

	callEdge := &graph.Edge{From: "a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveFile("a.go")

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.go::Bar", callEdge.To)
}

type countingPassIndexStore struct {
	graph.Store
	nodesByKindCalls int
}

func (s *countingPassIndexStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.nodesByKindCalls++
	return s.Store.NodesByKind(kind)
}

func TestResolveFileAndIncomingSkipsPassIndexesWithoutPendingEdges(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "results.json", Kind: graph.KindFile, Name: "results.json", FilePath: "results.json", Language: "json"})
	g.AddNode(&graph.Node{ID: "results.json::records", Kind: graph.KindVariable, Name: "records", FilePath: "results.json", Language: "json"})
	g.AddEdge(&graph.Edge{From: "results.json", To: "results.json::records", Kind: graph.EdgeDefines, FilePath: "results.json"})

	store := &countingPassIndexStore{Store: g}
	stats := New(store).ResolveFileAndIncoming("results.json")

	assert.Zero(t, stats.Resolved)
	assert.Zero(t, stats.Unresolved)
	assert.Zero(t, store.nodesByKindCalls,
		"a file with no pending forward or incoming edges must not build graph-wide pass indexes")
}

func TestResolveFileAndIncomingBuildsPassIndexesForPendingEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go", Language: "go"})
	callEdge := &graph.Edge{From: "a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(callEdge)

	store := &countingPassIndexStore{Store: g}
	stats := New(store).ResolveFileAndIncoming("a.go")

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.go::Bar", callEdge.To)
	assert.NotZero(t, store.nodesByKindCalls,
		"pending work must retain the normal pass-indexed resolver path")
}

// TestResolveMethodCall_ImportReachabilityFilter exercises Pass 0:
// when two methods named "Register" exist in different packages and
// only one of those packages is imported by the caller's file, the
// resolver must pick the imported package's method — not the
// alphabetically-first one across the whole graph (which was the
// original RegisterAll → OverlayManager.Register bug).
func TestResolveMethodCall_ImportReachabilityFilter(t *testing.T) {
	g := graph.New()

	// Caller file in pkg/parser/languages/. Imports pkg/parser only.
	g.AddNode(&graph.Node{ID: "pkg/parser/languages/register.go", Kind: graph.KindFile, Name: "register.go", FilePath: "pkg/parser/languages/register.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/parser/languages/register.go::RegisterAll", Kind: graph.KindFunction, Name: "RegisterAll", FilePath: "pkg/parser/languages/register.go", Language: "go"})

	// Imported package: pkg/parser
	g.AddNode(&graph.Node{ID: "pkg/parser/registry.go", Kind: graph.KindFile, Name: "registry.go", FilePath: "pkg/parser/registry.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/parser/registry.go::Registry.Register", Kind: graph.KindMethod, Name: "Register",
		FilePath: "pkg/parser/registry.go", Language: "go",
		Meta: map[string]any{"receiver": "Registry"},
	})

	// Unrelated package: pkg/daemon. NOT imported.
	g.AddNode(&graph.Node{ID: "pkg/daemon/overlay.go", Kind: graph.KindFile, Name: "overlay.go", FilePath: "pkg/daemon/overlay.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/daemon/overlay.go::OverlayManager.Register", Kind: graph.KindMethod, Name: "Register",
		FilePath: "pkg/daemon/overlay.go", Language: "go",
		Meta: map[string]any{"receiver": "OverlayManager"},
	})

	// Import edge: register.go → pkg/parser/registry.go (pre-resolved).
	g.AddEdge(&graph.Edge{
		From:     "pkg/parser/languages/register.go",
		To:       "pkg/parser/registry.go",
		Kind:     graph.EdgeImports,
		FilePath: "pkg/parser/languages/register.go", Line: 3,
	})

	// Unresolved method call without receiver_type — exercises the
	// fallback path. With Pass 0 in place, only Registry.Register
	// survives the reachability filter.
	callEdge := &graph.Edge{
		From:     "pkg/parser/languages/register.go::RegisterAll",
		To:       "unresolved::*.Register",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/parser/languages/register.go",
		Line:     7,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/parser/registry.go::Registry.Register", callEdge.To,
		"reachability filter should pick the imported package's method")
}

// TestResolveMethodCall_AmbiguousUntypedStaysUnresolved covers the locality
// fallback boundary: when multiple reachable methods survive but the call has
// no receiver-type evidence, locality is not enough to invent a concrete edge.
func TestResolveMethodCall_AmbiguousUntypedStaysUnresolved(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "pkg/a/main.go", Kind: graph.KindFile, FilePath: "pkg/a/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a/main.go", Language: "go"})

	// Same-package candidate: pkg/a/helpers.go::Helper.Do
	g.AddNode(&graph.Node{ID: "pkg/a/helpers.go", Kind: graph.KindFile, FilePath: "pkg/a/helpers.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/a/helpers.go::Helper.Do", Kind: graph.KindMethod, Name: "Do",
		FilePath: "pkg/a/helpers.go", Language: "go",
		Meta: map[string]any{"receiver": "Helper"},
	})

	// Imported but cross-package: pkg/aaa/util.go::AAA.Do (sorts first
	// alphabetically — the old fallback would pick this).
	g.AddNode(&graph.Node{ID: "pkg/aaa/util.go", Kind: graph.KindFile, FilePath: "pkg/aaa/util.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "pkg/aaa/util.go::AAA.Do", Kind: graph.KindMethod, Name: "Do",
		FilePath: "pkg/aaa/util.go", Language: "go",
		Meta: map[string]any{"receiver": "AAA"},
	})

	// Caller's file imports pkg/aaa, so both packages are reachable.
	g.AddEdge(&graph.Edge{
		From:     "pkg/a/main.go",
		To:       "pkg/aaa/util.go",
		Kind:     graph.EdgeImports,
		FilePath: "pkg/a/main.go", Line: 3,
	})

	callEdge := &graph.Edge{
		From:     "pkg/a/main.go::Caller",
		To:       "unresolved::*.Do",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/a/main.go",
		Line:     10,
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
	assert.Equal(t, "unresolved::*.Do", callEdge.To,
		"an untyped call with multiple viable methods must remain unresolved")
}

// TestResolveMethodCall_InterfaceDispatchFanOut verifies that when the
// receiver type names a graph interface AND multiple reachable methods
// of that name exist, the edge is annotated as interface dispatch.
func TestResolveMethodCall_InterfaceDispatchFanOut(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "pkg/main.go", Kind: graph.KindFile, FilePath: "pkg/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/main.go", Language: "go"})

	// Interface "Notifier" with method "Notify".
	g.AddNode(&graph.Node{
		ID: "pkg/notifier.go::Notifier", Kind: graph.KindInterface, Name: "Notifier",
		FilePath: "pkg/notifier.go", Language: "go",
	})

	// Two implementations of Notify in the same package as the caller.
	g.AddNode(&graph.Node{
		ID: "pkg/email.go::EmailNotifier.Notify", Kind: graph.KindMethod, Name: "Notify",
		FilePath: "pkg/email.go", Language: "go",
		Meta: map[string]any{"receiver": "EmailNotifier"},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/sms.go::SMSNotifier.Notify", Kind: graph.KindMethod, Name: "Notify",
		FilePath: "pkg/sms.go", Language: "go",
		Meta: map[string]any{"receiver": "SMSNotifier"},
	})

	// Receiver type is the interface itself — ambiguous dispatch.
	callEdge := &graph.Edge{
		From:     "pkg/main.go::Caller",
		To:       "unresolved::*.Notify",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/main.go",
		Line:     10,
		Meta:     map[string]any{"receiver_type": "Notifier"},
	}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "interface", callEdge.Meta["dispatch"],
		"edge should be marked as interface dispatch when receiver is an interface and multiple impls exist")
	assert.Equal(t, graph.OriginASTInferred, callEdge.Origin,
		"a locality-heuristic pick is ast_inferred — stamping an LSP tier without server evidence poisons min_tier filtering")
}
