package resolver

import (
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type goModuleNameCountingStore struct {
	graph.Store
	lookups int
	scans   int
	visited int
}

func (s *goModuleNameCountingStore) FindNodesByName(name string) []*graph.Node {
	s.lookups++
	return s.Store.FindNodesByName(name)
}

func (s *goModuleNameCountingStore) ExternalCallCandidateEdgesSeq() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		s.scans++
		externalCallCandidateEdges(s.Store)(func(row externalCallCandidate) bool {
			s.visited++
			return yield(row.edge)
		})
	}
}

func TestCrossRepoGoModuleRetry(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, targetWS := range []string{"shared", "other"} {
			t.Run(backend+"/"+targetWS, func(t *testing.T) {
				var base graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					base = s
				}
				g := &goModuleNameCountingStore{Store: base}
				g.AddNode(&graph.Node{ID: "a/main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
				g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
				g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Kind: graph.KindFunction, Name: "Hello", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: targetWS, Language: "go"})
				for line := 1; line <= 3; line++ {
					g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/library::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
				}
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/library2::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: 4})
				state := &GoModuleDepRetry{}
				g.AddEdge(&graph.Edge{From: "b/lib.go::Hello", To: "b/lib.go::Hello", Kind: graph.EdgeCalls, FilePath: "b/lib.go", Line: 10})
				cr := NewCrossRepo(g)
				cr.SetGoModuleDepRetry(state, 1)
				cr.SetGoModules(map[string]string{"b": "example.org/library"})
				cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
				cr.ResolveAll()
				assert.Equal(t, 1, g.scans, "use the external-terminal pushdown")
				assert.Equal(t, 4, g.visited, "never enumerate resolved call rows")
				cr = NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/library"})
				cr.SetGoModuleDepRetry(state, 1)
				cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
				cr.ResolveAll()
				assert.Equal(t, 1, g.scans, "same store/context must not rescan legacy terminals")
				assert.Equal(t, 1, g.lookups, "one symbol lookup for repeated tracked dep terminals only")
				for _, edge := range g.GetOutEdges("a/main.go::Caller") {
					if edge.Line == 4 {
						assert.Equal(t, "dep::example.org/library2::Hello", edge.To)
						continue
					}
					if targetWS == "shared" {
						assert.Equal(t, "b/lib.go::Hello", edge.To)
						assert.Equal(t, 0.6, edge.Confidence)
						assert.Equal(t, graph.OriginASTInferred, edge.Origin)
					} else {
						assert.Equal(t, "dep::example.org/library::Hello", edge.To)
					}
				}
				cr.SetGoModuleDepRetry(state, 2)
				cr.ResolveAll()
				assert.Equal(t, 2, g.scans, "a new config/store context permits one new sweep")
			})
		}
	}
}

func TestGoModuleExternBindingBoundaries(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range []struct {
			name    string
			modules map[string]string
			targets []string
			kind    graph.NodeKind
			want    string
		}{
			{"missing", map[string]string{"b": "example.org/library"}, nil, graph.KindFunction, "dep::example.org/library::Hello"},
			{"denied_workspace", map[string]string{"b": "example.org/library"}, []string{"b/lib.go::Hello"}, graph.KindFunction, "dep::example.org/library::Hello"},
			{"variable", map[string]string{"b": "example.org/library"}, []string{"b/lib.go::Hello"}, graph.KindVariable, "dep::example.org/library::Hello"},
			{"constant", map[string]string{"b": "example.org/library"}, []string{"b/lib.go::Hello"}, graph.KindConstant, "dep::example.org/library::Hello"},
			{"ambiguous", map[string]string{"library": "example.org/library", "fork": "example.org/library"}, []string{"library/lib.go::Hello", "fork/lib.go::Hello"}, graph.KindFunction, "dep::example.org/library::Hello"},
			{"wrong_module_suffix", map[string]string{"library": "example.org/unrelated"}, []string{"library/lib.go::Hello"}, graph.KindFunction, "dep::example.org/library::Hello"},
			{"build_tags", map[string]string{"b": "example.org/library"}, []string{"b/hello_linux.go::Hello", "b/hello_darwin.go::Hello"}, graph.KindFunction, "b/hello_darwin.go::Hello"},
			{"nested_module", map[string]string{"b": "example.org", "child": "example.org/library"}, []string{"b/library/lib.go::Hello", "child/lib.go::Hello"}, graph.KindFunction, "child/lib.go::Hello"},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				g.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
				for _, id := range tc.targets {
					file, _, _ := strings.Cut(id, "::")
					repo, _, _ := strings.Cut(file, "/")
					ws := "shared"
					if tc.name == "denied_workspace" {
						ws = "other"
					}
					g.AddNode(&graph.Node{ID: id, Name: "Hello", Kind: tc.kind, FilePath: file, RepoPrefix: repo, WorkspaceID: ws, Language: "go"})
				}
				kind := graph.EdgeCalls
				if tc.kind == graph.KindVariable || tc.kind == graph.KindConstant {
					kind = graph.EdgeReferences
				}
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/library::Hello", Kind: kind, FilePath: "a/main.go", Line: 1})
				r := New(g)
				r.SetGoModules(tc.modules)
				r.SetDeferGoModuleMisses(true)
				r.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
				r.ResolveAll()
				cr := NewCrossRepo(g)
				cr.SetGoModules(tc.modules)
				cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
				cr.ResolveAll()
				edges := g.GetOutEdges("a/main.go::Caller")
				require.Len(t, edges, 1)
				assert.Equal(t, tc.want, edges[0].To)
			})
		}
	}
}

func TestGoModuleTerminalClassificationParity(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, g := range []graph.Store{graph.New(), s} {
		r := New(g)
		r.SetGoModules(map[string]string{"b": "example.org/library"})
		r.SetDeferGoModuleMisses(true)
		e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/library::Gone", Kind: graph.EdgeCalls, FilePath: "a/main.go"}
		reason, terminal := r.classifyTerminal(e)
		assert.Empty(t, reason)
		assert.False(t, terminal)
		reasons, terminals := r.classifyTerminalBulk([]*graph.Edge{e}, s)
		assert.Equal(t, []string{reason}, reasons)
		assert.Equal(t, []bool{terminal}, terminals)
	}
}

func TestCrossRepoNonGoExternStillProbes(t *testing.T) {
	g, edge := stitchFixture(true)
	edge.FilePath = "repoA/pkg/a.py"
	oldTo := edge.To
	edge.To = "unresolved::extern::remote.module::Helper"
	g.ReindexEdge(edge, oldTo)
	cr := NewCrossRepo(g)
	prober := &fakeProber{}
	cr.EnableRemoteStitch(prober, 100)
	cr.ResolveAll()
	assert.Positive(t, prober.calls, "non-Go externs retain the remote-stitch path")
}

func TestGoModuleEmptyContextDoesNotScan(t *testing.T) {
	g := &goModuleNameCountingStore{Store: graph.New()}
	NewCrossRepo(g).ResolveAll()
	assert.Zero(t, g.scans)
}

func TestGoModuleMasterAndShadowMissClassification(t *testing.T) {
	for _, importPath := range []string{"example.org/library", "local/library"} {
		for _, shadow := range []bool{false, true} {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", RepoPrefix: "a", FilePath: "a/main.go", Language: "go"})
			edge := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::" + importPath + "::Gone", Kind: graph.EdgeCalls, FilePath: "a/main.go"}
			g.AddEdge(edge)
			r := New(g)
			r.SetGoModules(map[string]string{"b": importPath})
			r.SetDeferGoModuleMisses(shadow)
			r.ResolveAll()
			if shadow {
				assert.Equal(t, "unresolved::extern::"+importPath+"::Gone", edge.To)
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": importPath})
			cr.ResolveAll()
			want := "dep::" + importPath + "::Gone"
			assert.Equal(t, want, edge.To)
		}
	}
}

// Multi-repo mode must retain the master's existing same-repo directory fallback.
func TestGoModuleSameRepoFallback(t *testing.T) {
	for _, route := range []string{"master", "cross_repo"} {
		for _, tc := range []struct{ name, module, importPath, target string }{
			{"nested", "k8s.io/kubernetes", "k8s.io/api/core", "mono/staging/src/k8s.io/api/core/types.go::Serve"},
			{"replace", "example.org/mono", "example.org/third/lib", "mono/third_party/lib/lib.go::Serve"},
			{"vendor", "example.org/mono", "example.org/third/lib", "mono/vendor/example.org/third/lib/lib.go::Serve"},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				g := graph.New()
				g.AddNode(&graph.Node{ID: "mono/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "mono/main.go", RepoPrefix: "mono", WorkspaceID: "shared", Language: "go"})
				file, _, _ := strings.Cut(tc.target, "::")
				g.AddNode(&graph.Node{ID: tc.target, Name: "Serve", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "mono", WorkspaceID: "shared", Language: "go"})
				e := &graph.Edge{From: "mono/main.go::Caller", To: "unresolved::extern::" + tc.importPath + "::Serve", Kind: graph.EdgeCalls, FilePath: "mono/main.go"}
				g.AddEdge(e)
				r := New(g)
				if tc.name != "replace" {
					g.AddNode(&graph.Node{ID: "other/lib.go::Serve", Name: "Serve", Kind: graph.KindFunction, FilePath: "other/lib.go", RepoPrefix: "other", WorkspaceID: "shared", Language: "go"})
				}
				modules := map[string]string{"mono": tc.module, "other": tc.importPath}
				if route == "master" {
					r.SetGoModules(modules)
					r.ResolveAll()
				} else {
					cr := NewCrossRepo(g)
					cr.SetGoModules(modules)
					cr.ResolveAll()
				}
				if route == "cross_repo" && tc.name == "replace" {
					// Loose directory guesses belong to the ownership-filtered master pass.
					assert.Equal(t, "dep::"+tc.importPath+"::Serve", e.To)
				} else {
					assert.Equal(t, tc.target, e.To)
				}
				assert.False(t, e.CrossRepo)
			})
		}
	}

}

func TestGoModuleDuplicatePrefersCaller(t *testing.T) {
	for _, route := range []string{"master", "cross_repo", "retry"} {
		for _, order := range [][]string{{"mono", "fork", "third"}, {"third", "fork", "mono"}} {
			t.Run(route+"/"+order[0], func(t *testing.T) {
				g := graph.New()
				g.AddNode(&graph.Node{ID: "mono/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "mono/main.go", RepoPrefix: "mono", WorkspaceID: "shared", Language: "go"})
				modules := map[string]string{}
				for _, repo := range order {
					modules[repo] = "example.org/library"
					g.AddNode(&graph.Node{ID: repo + "/sub/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: repo + "/sub/lib.go", RepoPrefix: repo, WorkspaceID: "shared", Language: "go"})
				}
				target := "unresolved::extern::example.org/library/sub::Fn"
				if route == "retry" {
					target = "dep::example.org/library/sub::Fn"
				}
				e := &graph.Edge{From: "mono/main.go::Caller", To: target, Kind: graph.EdgeCalls, FilePath: "mono/main.go"}
				g.AddEdge(e)
				if route == "master" {
					r := New(g)
					r.SetGoModules(modules)
					r.ResolveAll()
				} else {
					cr := NewCrossRepo(g)
					cr.SetGoModules(modules)
					cr.ResolveAll()
				}
				assert.Equal(t, "mono/sub/lib.go::Fn", e.To)
				assert.False(t, e.CrossRepo)
			})
		}
	}
}

func TestGoModuleRetryFormsAndTestFrontier(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				g = s
			}
			g.AddNode(&graph.Node{ID: "a/main_test.go::TestFn", Name: "TestFn", Kind: graph.KindFunction, FilePath: "a/main_test.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
			g.AddNode(&graph.Node{ID: "b/pkg/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: "b/pkg/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
			targets := []string{"dep::backend/pkg::Fn", "a::stdlib::backend/pkg::Fn", "stdlib::backend/pkg::Fn", "a::stdlib::fmt::Fn", "external-call::dep::backend/pkg"}
			for i, target := range targets {
				g.AddEdge(&graph.Edge{From: "a/main_test.go::TestFn", To: target, Kind: graph.EdgeCalls, FilePath: "a/main_test.go", Line: i + 1, Meta: map[string]any{"external_call": true}})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "backend"})
			cr.ResolveAll()
			var edges []*graph.Edge
			for _, e := range g.GetOutEdges("a/main_test.go::TestFn") {
				if e.Kind == graph.EdgeCalls {
					edges = append(edges, e)
				}
			}
			require.Len(t, edges, len(targets))
			for _, e := range edges {
				if e.Line <= 3 {
					assert.Equal(t, "b/pkg/lib.go::Fn", e.To)
					assert.Equal(t, 0.6, e.Confidence)
					assert.Equal(t, graph.OriginASTInferred, e.Origin)
				} else {
					assert.Equal(t, targets[e.Line-1], e.To)
				}
			}
			assert.Equal(t, []string{"a/main_test.go"}, cr.TakeRetargetedTestCallFiles())
		})
	}
}

func TestGoModuleSnapshotOncePerPass(t *testing.T) {
	for _, mode := range []string{"all", "file", "files"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			for line := 1; line <= 8; line++ {
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/library::Gone", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
			}
			r := New(g)
			r.SetDeferGoModuleMisses(true)
			calls := 0
			r.SetGoModuleLookup(func() map[string]string { calls++; return map[string]string{"b": "example.org/library"} })
			run := func() {
				switch mode {
				case "all":
					r.ResolveAll()
				case "file":
					r.ResolveFileAndIncoming("a/main.go")
				case "files":
					r.ResolveFilesAndIncoming([]string{"a/main.go"})
				}
			}
			run()
			assert.Equal(t, 1, calls)
			run()
			assert.Equal(t, 2, calls, "refresh on the next pass, not once per resolver lifetime")
		})
	}
}

func TestGoModuleIncomingExternFrontier(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", RepoPrefix: "a", FilePath: "a/main.go", WorkspaceID: "shared", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/pkg/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", RepoPrefix: "b", FilePath: "b/pkg/lib.go", WorkspaceID: "shared", Language: "go"})
	e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::backend/pkg::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"}
	g.AddEdge(e)
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "backend"})
	cr.ResolveMutationFrontiers([]string{"b/pkg/lib.go"}, []string{"b/pkg/lib.go"}, []string{"b/pkg/lib.go"})
	assert.Equal(t, "b/pkg/lib.go::Fn", e.To)
}

func TestGoModuleSynthesizedRetry(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		module := "example.org/library"
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				g = s
			}
			g.AddNode(&graph.Node{ID: "a/main_test.go::TestFn", Kind: graph.KindFunction, Name: "TestFn", RepoPrefix: "a", FilePath: "a/main_test.go", WorkspaceID: "shared", Language: "go"})
			for i, name := range []string{"Fn", "Other"} {
				g.AddNode(&graph.Node{ID: "b/pkg/lib.go::" + name, Kind: graph.KindFunction, Name: name, RepoPrefix: "b", FilePath: "b/pkg/lib.go", WorkspaceID: "shared", Language: "go"})
				target := "dep::" + module + "/pkg::" + name
				g.AddEdge(&graph.Edge{From: "a/main_test.go::TestFn", To: target, Kind: graph.EdgeCalls, FilePath: "a/main_test.go", Line: i + 1})
			}
			require.Equal(t, 2, SynthesizeExternalCalls(g, true))
			synthesized := g.GetOutEdges("a/main_test.go::TestFn")
			require.Len(t, synthesized, 2)
			for _, e := range synthesized {
				assert.True(t, strings.HasPrefix(e.To, "external-call::"))
				symbol := "Fn"
				if e.Line == 2 {
					symbol = "Other"
				}
				assert.Equal(t, "dep::example.org/library/pkg::"+symbol, e.Meta["go_extern_target"])
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": module})
			cr.ResolveAll()
			for _, e := range g.GetOutEdges("a/main_test.go::TestFn") {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				want := "b/pkg/lib.go::Fn"
				if e.Line == 2 {
					want = "b/pkg/lib.go::Other"
				}
				assert.Equal(t, want, e.To)
				assert.Equal(t, 0.6, e.Confidence)
				assert.Equal(t, graph.OriginASTInferred, e.Origin)
				assert.NotContains(t, e.Meta, "external_call")
				assert.NotContains(t, e.Meta, "go_extern_target")
			}
			assert.Equal(t, []string{"a/main_test.go"}, cr.TakeRetargetedTestCallFiles())
		})
	}
}

func TestGoModuleIncomingFinalizedTarget(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, synthesized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/synth=%t", backend, synthesized), func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", RepoPrefix: "a", FilePath: "a/main.go", WorkspaceID: "shared", Language: "go"})
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/library/pkg::NewFn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
				if synthesized {
					require.Equal(t, 1, SynthesizeExternalCalls(g, true))
				}
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/library"})
				cr.ResolveAll()
				g.AddNode(&graph.Node{ID: "b/pkg/lib.go::NewFn", Kind: graph.KindFunction, Name: "NewFn", RepoPrefix: "b", FilePath: "b/pkg/lib.go", WorkspaceID: "shared", Language: "go"})
				stats := cr.ResolveMutationFrontiers(nil, nil, []string{"b/pkg/lib.go"})
				assert.Equal(t, 1, stats.Resolved)
				assert.Equal(t, 1, stats.CrossRepoEdges)
				assert.Equal(t, map[string]int{"b": 1}, stats.ByRepo)
				var calls []*graph.Edge
				for _, e := range g.GetOutEdges("a/main.go::Caller") {
					if e.Kind == graph.EdgeCalls {
						calls = append(calls, e)
					}
				}
				require.Len(t, calls, 1)
				assert.Equal(t, "b/pkg/lib.go::NewFn", calls[0].To)
				assert.Equal(t, 0.6, calls[0].Confidence)
				assert.Equal(t, graph.OriginASTInferred, calls[0].Origin)
			})
		}
	}
}

type goModuleDroppingStore struct {
	graph.Store
	drop            bool
	scans, maxBatch int
}

func (s *goModuleDroppingStore) ExternalCallCandidateEdgesSeq() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		s.scans++
		externalCallCandidateEdges(s.Store)(func(row externalCallCandidate) bool {
			return yield(row.edge)
		})
	}
}

func (s *goModuleDroppingStore) ReindexEdges(batch []graph.EdgeReindex) { s.ReindexEdgesApplied(batch) }
func (s *goModuleDroppingStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.maxBatch = max(s.maxBatch, len(batch))
	if s.drop {
		s.drop = false
		return false
	}
	s.Store.ReindexEdges(batch)
	return true
}

func TestGoModuleRetryDroppedBatch(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	g := &goModuleDroppingStore{Store: s, drop: true}
	g.AddNode(&graph.Node{ID: "a/test_test.go::TestFn", Kind: graph.KindFunction, Name: "TestFn", RepoPrefix: "a", FilePath: "a/test_test.go", WorkspaceID: "shared", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", RepoPrefix: "b", FilePath: "b/lib.go", WorkspaceID: "shared", Language: "go"})
	for line := 1; line <= 513; line++ {
		g.AddEdge(&graph.Edge{From: "a/test_test.go::TestFn", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/test_test.go", Line: line})
	}
	state := &GoModuleDepRetry{}
	for pass := 0; pass < 2; pass++ {
		cr := NewCrossRepo(g)
		cr.SetGoModules(map[string]string{"b": "example.org/lib"})
		cr.SetGoModuleDepRetry(state, 1)
		cr.ResolveMutationFrontiers(nil, []string{"a/test_test.go"}, nil)
		assert.Equal(t, 1, g.scans, "the next mutation must not repeat the terminal sweep")
		assert.True(t, state.complete, "a dropped batch must not invalidate the terminal sweep")
		assert.Equal(t, 512, g.maxBatch)
		if pass == 0 {
			assert.Len(t, g.GetOutEdges("a/test_test.go::TestFn"), 514, "513 base calls and only one successful cross-repo materialization")
			assert.Len(t, state.pending, 512, "queue only the dropped batch, not the successful remainder")
			for identity, edge := range state.pending {
				assert.Equal(t, "dep::example.org/lib::Fn", edge.To, "retain the original target for live-edge checks")
				assert.Equal(t, graph.EdgeIdentityFor(edge), identity)
			}
		} else {
			assert.Empty(t, state.pending)
			assert.Len(t, g.GetOutEdges("a/test_test.go::TestFn"), 1026, "one base and one derived edge per call after recovery")
			var calls int
			for _, e := range g.GetOutEdges("a/test_test.go::TestFn") {
				assert.Equal(t, "b/lib.go::Fn", e.To)
				if e.Kind == graph.EdgeCalls {
					calls++
				}
			}
			assert.Equal(t, 513, calls, "recover every dropped call without duplicating successful calls")
		}
	}
}

type goModuleBoundedStore struct {
	graph.Store
	loads, maxBatch, batches int
}

func (s *goModuleBoundedStore) AllNodes() []*graph.Node           { panic("whole graph nodes") }
func (s *goModuleBoundedStore) AllEdges() []*graph.Edge           { panic("whole graph edges") }
func (s *goModuleBoundedStore) GetRepoNodes(string) []*graph.Node { panic("whole repo nodes") }
func (s *goModuleBoundedStore) GetRepoEdges(string) []*graph.Edge { panic("whole repo edges") }
func (s *goModuleBoundedStore) GetFileNodesByPaths([]string) map[string][]*graph.Node {
	panic("whole file frontier")
}
func (s *goModuleBoundedStore) FindNodesByName(name string) []*graph.Node {
	s.loads++
	return s.Store.FindNodesByName(name)
}
func (s *goModuleBoundedStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.batches++
	s.maxBatch = max(s.maxBatch, len(batch))
	s.ReindexEdges(batch)
	return true
}
func TestGoModuleDeferredBatches(t *testing.T) {
	g := &goModuleBoundedStore{Store: graph.New()}
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.org/lib"})
	require.NoError(t, cr.ResolveDeferredGoModuleEdges(nil))
	assert.Zero(t, g.loads)
	assert.Zero(t, g.batches)
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	var deferred []*graph.Edge
	for line := 1; line <= 1025; line++ {
		e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line}
		g.AddEdge(e)
		deferred = append(deferred, e)
	}
	g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::Other", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: 1026})
	require.NoError(t, cr.ResolveDeferredGoModuleEdges(deferred))
	assert.Equal(t, 512, g.maxBatch)
	assert.Equal(t, 3, g.batches)
	var bound, unrelated int
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if e.To == "b/lib.go::Fn" {
			bound++
		}
		if e.To == "unresolved::Other" {
			unrelated++
		}
	}
	assert.Equal(t, 1025, bound)
	assert.Equal(t, 1, unrelated)
}

func TestGoModuleExactIdentityBeforeLooseLocal(t *testing.T) {
	for _, route := range []string{"master", "cross_repo", "retry"} {
		for _, tc := range []struct{ module, pkg, name, local, remote string }{
			{"example.org/lib", "example.org/lib/errors", "Wrap", "a/internal/errors/e.go", "lib/errors/errors.go"},
			{"k8s.io/apimachinery", "k8s.io/apimachinery/pkg/apis/meta/v1", "Now", "a/api/v1/types.go", "lib/pkg/apis/meta/v1/time.go"},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				g := graph.New()
				g.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
				for _, file := range []string{tc.local, tc.remote} {
					repo, _, _ := strings.Cut(file, "/")
					g.AddNode(&graph.Node{ID: file + "::" + tc.name, Name: tc.name, Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo, Language: "go"})
				}
				target := "unresolved::extern::" + tc.pkg + "::" + tc.name
				if route == "retry" {
					target = "dep::" + tc.pkg + "::" + tc.name
				}
				e := &graph.Edge{From: "a/main.go::Caller", To: target, Kind: graph.EdgeCalls, FilePath: "a/main.go"}
				g.AddEdge(e)
				modules := map[string]string{"a": "example.org/caller", "lib": tc.module}
				if route == "master" {
					r := New(g)
					r.SetGoModules(modules)
					r.ResolveAll()
				} else {
					cr := NewCrossRepo(g)
					cr.SetGoModules(modules)
					cr.ResolveAll()
				}
				assert.Equal(t, tc.remote+"::"+tc.name, e.To)
			})
		}
	}
}

func TestGoModuleOwnMissIsNotDeferred(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/caller/pkg::Const", Kind: graph.EdgeReferences, FilePath: "a/main.go"}
	g.AddEdge(e)
	r := New(g)
	r.SetGoModules(map[string]string{"a": "example.org/caller"})
	r.SetDeferGoModuleMisses(true)
	r.ResolveAll()
	assert.Equal(t, "dep::example.org/caller/pkg::Const", e.To)
	r.SetDeferGoModuleMisses(false)
	assert.Empty(t, r.TakeDeferredGoModuleEdges())
}

func TestGoModuleDeferredDroppedContinues(t *testing.T) {
	g := &goModuleDroppingStore{Store: graph.New(), drop: true}
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	var edges []*graph.Edge
	for line := 1; line <= 513; line++ {
		e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line}
		g.AddEdge(e)
		edges = append(edges, e)
	}
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.org/lib"})
	assert.NoError(t, cr.ResolveDeferredGoModuleEdges(edges))
	var bound, pending int
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if e.To == "b/lib.go::Fn" {
			bound++
		}
		if graph.IsUnresolvedTarget(e.To) {
			pending++
		}
	}
	assert.Equal(t, 1, bound)
	assert.Equal(t, 512, pending)
	cr = NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.org/lib"})
	cr.ResolveAll()
	bound = 0
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls && e.To == "b/lib.go::Fn" {
			bound++
		}
	}
	assert.Equal(t, 513, bound, "the next pass must recover every dropped pending edge")
}

func TestGoModuleIncomingDroppedRetries(t *testing.T) {
	for _, route := range []string{"mutation", "post_publish"} {
		t.Run(route, func(t *testing.T) {
			s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			g := &goModuleDroppingStore{Store: s}
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
			state := &GoModuleDepRetry{}
			newResolver := func() *CrossRepoResolver {
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/lib"})
				cr.SetGoModuleDepRetry(state, 1)
				return cr
			}
			cr := newResolver()
			cr.ResolveAll()
			require.True(t, state.complete)
			g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			g.drop = true
			if route == "mutation" {
				stats := cr.ResolveMutationFrontiers(nil, nil, []string{"b/lib.go"})
				assert.Equal(t, 1, stats.Resolved, "count the recovery, not the dropped attempt")
				assert.Equal(t, 1, stats.CrossRepoEdges)
				assert.Equal(t, map[string]int{"b": 1}, stats.ByRepo)
			} else {
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("b"))
			}
			assert.True(t, state.complete, "an incoming drop must not invalidate the terminal sweep")
			if route == "post_publish" {
				assert.Len(t, state.pending, 1)
			} else {
				assert.Empty(t, state.pending, "the mutation tail already retries the incoming receipt")
			}
			cr = newResolver()
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			assert.Equal(t, 1, g.scans, "retry incoming receipts without a second terminal sweep")
			assert.Empty(t, state.pending)
			var targets []string
			for _, e := range g.GetOutEdges("a/main.go::Caller") {
				if e.Kind == graph.EdgeCalls {
					targets = append(targets, e.To)
				}
			}
			assert.Equal(t, []string{"b/lib.go::Fn"}, targets)
		})
	}
}

func TestGoModuleDeferredDrainDuringGraphSwap(t *testing.T) {
	r := New(graph.New())
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for range 100 {
			r.TakeDeferredGoModuleEdges()
		}
	}()
	<-started
	for range 100 {
		r.SetGraph(graph.New())
		r.SetDeferGoModuleMisses(true)
		r.SetDeferGoModuleMisses(false)
	}
	<-done
}

func TestGoModuleIncomingWorkspaceLookupBudget(t *testing.T) {
	for _, ws := range []string{"shared", "other"} {
		t.Run(ws, func(t *testing.T) {
			g := &goModuleNameCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
			g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: ws, Language: "go"})
			for line := 1; line <= 20; line++ {
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
			}
			require.Equal(t, 20, SynthesizeExternalCalls(g, true))
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"b": "example.org/lib"})
			cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			// Recreate the unbound incoming bucket after finishing the revision sweep.
			for line := 21; line <= 40; line++ {
				g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
			}
			SynthesizeExternalCalls(g, true)
			g.lookups = 0
			cr.ResolveMutationFrontiers(nil, nil, []string{"b/lib.go"})
			want := 1
			if ws == "other" {
				want = 0
			}
			assert.Equal(t, want, g.lookups, "one memoized lookup for eligible calls; none for workspace-denied calls")
		})
	}
}

func TestGoModuleSameRepoRebindProvenance(t *testing.T) {
	for _, route := range []string{"retry", "incoming"} {
		t.Run(route, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
			g.AddNode(&graph.Node{ID: "a/pkg/p.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "a/pkg/p.go", RepoPrefix: "a", Language: "go"})
			g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/a/pkg::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
			require.Equal(t, 1, SynthesizeExternalCalls(g, true))
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.org/a"})
			if route == "retry" {
				require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			} else {
				require.NoError(t, cr.ResolveGoModuleIncomingForRepo("a"))
			}
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("a/main.go::Caller") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 1)
			assert.Equal(t, "a/pkg/p.go::Fn", calls[0].To)
			assert.Zero(t, calls[0].Confidence)
			assert.Empty(t, calls[0].ConfidenceLabel)
			assert.Empty(t, calls[0].Origin)
			assert.False(t, calls[0].CrossRepo)
		})
	}
}

func TestGoModuleDeferredDropRetryWithoutSweep(t *testing.T) {
	g := &goModuleDroppingStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	state := &GoModuleDepRetry{}
	cr := NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.org/lib"})
	cr.SetGoModuleDepRetry(state, 1)
	require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
	require.True(t, state.complete)
	e := &graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"}
	g.AddEdge(e)
	g.drop = true
	require.NoError(t, cr.ResolveDeferredGoModuleEdges([]*graph.Edge{e}))
	assert.True(t, state.complete, "a deferred drop must not invalidate the terminal sweep")
	cr = NewCrossRepo(g)
	cr.SetGoModules(map[string]string{"b": "example.org/lib"})
	cr.SetGoModuleDepRetry(state, 1)
	require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
	assert.Equal(t, "b/lib.go::Fn", g.GetOutEdges("a/main.go::Caller")[0].To, "the next module tail must recover unresolved edges without a whole pass")
}

func BenchmarkGoModulePendingStdlib(b *testing.B) {
	for _, count := range []int{0, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			modules := make(map[string]string)
			for i := range count {
				modules[fmt.Sprint(i)] = fmt.Sprintf("example.org/module%d", i)
			}
			r := New(graph.New())
			r.SetGoModules(modules)
			r.SetDeferGoModuleMisses(true)
			e := &graph.Edge{From: "a/main.go::Caller", FilePath: "a/main.go", To: "unresolved::extern::fmt::Println"}
			b.ResetTimer()
			for b.Loop() {
				r.pendingGoModuleExtern(e)
			}
		})
	}
}

func TestGoModuleDemotionDroppedBatchRetries(t *testing.T) {
	g := &goModuleDroppingStore{Store: graph.New(), drop: true}
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
	edge := &graph.Edge{From: "a/main.go::Caller", To: "b/lib.go::Hello", FilePath: "a/main.go", Line: 3, Kind: graph.EdgeCalls, CrossRepo: true, Origin: graph.OriginASTInferred, Meta: map[string]any{"go_import": "example.org/library::Hello"}}
	g.AddEdge(edge)
	state := &GoModuleDepRetry{}
	cr := NewCrossRepo(g)
	cr.SetGoModuleDepRetry(state, 1)
	require.NoError(t, cr.ResolveDeferredGoModuleEdges([]*graph.Edge{edge}))
	assert.Equal(t, "b/lib.go::Hello", firstOutEdgeByKind(g, edge.From, graph.EdgeCalls).To)
	g.drop = false
	require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
	assert.Equal(t, "dep::example.org/library::Hello", firstOutEdgeByKind(g, edge.From, graph.EdgeCalls).To)
	assert.Empty(t, state.pending)
}

func TestGoModuleRetryGenerationSkipsBindingRecheck(t *testing.T) {
	for _, modules := range []map[string]string{nil, {"b": "example.org/library"}} {
		name := "without_modules"
		if len(modules) > 0 {
			name = "with_modules"
		}
		t.Run(name, func(t *testing.T) {
			g := &crossRepoPassCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Kind: graph.KindFunction, Name: "Hello", FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			state := &GoModuleDepRetry{}
			cr := NewCrossRepo(g)
			cr.SetGoModules(modules)
			cr.SetGoModuleDepRetry(state, 1)
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			if len(modules) > 0 {
				require.Positive(t, g.dependencyIndexBuilds, "fresh state must seed persisted bindings once")
			} else {
				assert.Zero(t, g.dependencyIndexBuilds, "no tracked modules or persisted Go files need no binding seed")
			}
			g.dependencyIndexBuilds = 0
			cr = NewCrossRepo(g)
			cr.SetGoModules(modules)
			cr.SetGoModuleDepRetry(state, 2)
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			assert.Zero(t, g.dependencyIndexBuilds, "a generation-only bump must not recheck bindings")
		})
	}
	g := &crossRepoPassCountingStore{Store: graph.New()}
	require.NoError(t, NewCrossRepo(g).RetryGoModuleDeps(t.Context()))
	assert.Zero(t, g.dependencyIndexBuilds, "no modules and no retry state need no scan")
}

func TestGoModuleReplacementDenialKeepsPreciseLocal(t *testing.T) {
	g := graph.New()
	caller := &graph.Node{ID: "a/main.go::Caller", RepoPrefix: "a", FilePath: "a/main.go", Kind: graph.KindFunction, Language: "go"}
	g.AddNode(caller)
	for _, target := range []string{"a/staging/src/k8s.io/api/v1/lib.go", "a/vendor/k8s.io/api/v1/lib.go"} {
		t.Run(target, func(t *testing.T) {
			local := &graph.Node{ID: target + "::Hello", RepoPrefix: "a", FilePath: target, Kind: graph.KindFunction, Language: "go"}
			denied := map[string][]GoModuleReplacement{"a": {{Module: "k8s.io/api"}}}
			candidates := []*graph.Node{local}
			r := New(g)
			r.goModuleReplacements = denied
			e := &graph.Edge{From: caller.ID, To: "unresolved::extern::k8s.io/api/v1::Hello", FilePath: caller.FilePath, Kind: graph.EdgeCalls}
			stats := &ResolveStats{}
			assert.True(t, r.resolveGoModuleExtern(e, "k8s.io/api/v1", "a", candidates, stats))
			assert.Equal(t, local.ID, e.To)
			cr := NewCrossRepo(g)
			cr.SetGoModuleReplacements(denied)
			assert.Equal(t, local, cr.goModuleCandidate("k8s.io/api/v1", candidates, "a", ""))
			remote := *local
			remote.RepoPrefix = "b"
			assert.Nil(t, cr.goModuleCandidate("k8s.io/api/v1", []*graph.Node{&remote}, "a", ""), "denial still excludes foreign providers")
		})
	}
}

func TestGoModuleExternDemotionFiles(t *testing.T) {
	g := graph.New()
	var want []string
	for i := range 17 {
		file := fmt.Sprintf("a/caller%d.go", i)
		from := file + "::Caller"
		g.AddNode(&graph.Node{ID: from, RepoPrefix: "a", FilePath: file, Kind: graph.KindFunction, Language: "go"})
		e := &graph.Edge{From: from, To: "unresolved::extern::example.org/library::Hello", FilePath: file, Kind: graph.EdgeCalls}
		if i < 16 {
			e.Meta = map[string]any{"go_import": "example.org/library::Hello"}
			want = append(want, file)
		}
		g.AddEdge(e)
	}
	cr := NewCrossRepo(g)
	_, err := cr.ResolveAllContext(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, want, cr.TakeDemotedGoModuleCallFiles(), "parallel workers must record every marked caller, and no unmarked caller")
	assert.Empty(t, cr.TakeDemotedGoModuleCallFiles(), "draining is idempotent")
}
