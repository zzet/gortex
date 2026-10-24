package resolver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCrossRepoTrackedNpmPackage(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, target := range []string{"external::p::f", "unresolved::import::p::f"} {
			for _, mode := range []string{"all", "repo", "file", "mutation", "import only"} {
				t.Run(backend+"/"+target+"/"+mode, func(t *testing.T) {
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, store.Close()) })
						g = store
					}
					g.AddBatch([]*graph.Node{
						{ID: "app/src/app.ts", Kind: graph.KindFile, FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
						{ID: "app/src/app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
						{ID: "lib/src/index.ts", Kind: graph.KindFile, FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript"},
						{ID: "lib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
					}, nil)
					imp := &graph.Edge{From: "app/src/app.ts", To: target, Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1, Meta: map[string]any{"test_marker": "preserved"}}
					g.AddEdge(imp)
					if mode != "import only" {
						g.AddEdge(&graph.Edge{From: "app/src/app.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: 2})
					}
					cr := NewCrossRepo(g)
					cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
					cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
						if spec != "p" {
							return nil
						}
						return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/src/index.ts", "lib"}}}
					})
					var stats *CrossRepoStats
					switch mode {
					case "all", "import only":
						stats = cr.ResolveAll()
					case "repo":
						stats = cr.ResolveForRepo("app")
					case "file":
						stats = cr.ResolveForFile("app", "src/app.ts")
					case "mutation":
						stats = cr.ResolveFilesAndIncoming([]string{"app/src/app.ts"})
					}
					wantResolved := 2
					if mode == "import only" {
						wantResolved = 1
					}
					assert.Equal(t, wantResolved, stats.Resolved)
					assert.Equal(t, wantResolved, stats.CrossRepoEdges)
					imports := g.GetOutEdges("app/src/app.ts")
					require.Len(t, imports, 1)
					var found bool
					for _, e := range imports {
						if e.Kind != graph.EdgeImports {
							continue
						}
						found = true
						assert.Equal(t, "lib/src/index.ts::f", e.To)
						assert.True(t, e.CrossRepo)
						assert.Equal(t, "preserved", e.Meta["test_marker"])
					}
					assert.True(t, found)
					if mode != "import only" {
						var calls []*graph.Edge
						for _, e := range g.GetOutEdges("app/src/app.ts::run") {
							if e.Kind == graph.EdgeCalls {
								calls = append(calls, e)
							}
						}
						require.Len(t, calls, 1)
						assert.Equal(t, "lib/src/index.ts::f", calls[0].To)
						assert.True(t, calls[0].CrossRepo)
					}
					// A second pass is idempotent and leaves no old target bucket.
					assert.Zero(t, cr.ResolveAll().Resolved)
					assert.Empty(t, g.GetInEdges(target))
				})
			}
		}
	}
}

func TestCrossRepoTrackedNpmPackageAmbiguousOrUndeclared(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "undeclared", true: "ambiguous"}[declared], func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app", Language: "typescript"})
			g.AddNode(&graph.Node{ID: "one/index.ts", Kind: graph.KindFile, FilePath: "one/index.ts", RepoPrefix: "one", Language: "typescript"})
			g.AddNode(&graph.Node{ID: "two/index.ts", Kind: graph.KindFile, FilePath: "two/index.ts", RepoPrefix: "two", Language: "typescript"})
			imp := &graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}
			g.AddEdge(imp)
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return declared })
			cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
				targets := map[string]NpmPackageTarget{"one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}
				if declared {
					targets["two"] = NpmPackageTarget{Entries: []string{"two/index.ts"}}
				}
				return targets
			})
			assert.Zero(t, cr.ResolveAll().Resolved)
			assert.Equal(t, "external::p", imp.To)
			assert.False(t, imp.CrossRepo)
		})
	}
}

func TestCrossRepoTrackedNpmPackageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, visibility string
		dirs             map[string]NpmPackageTarget
		want             string
	}{
		{"own package missing index", "private", map[string]NpmPackageTarget{"app/packages/p": {Entries: []string{"app/packages/p/missing"}}, "one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}, "external::p::f"},
		{"own root package", "private", map[string]NpmPackageTarget{"app": {Entries: []string{"app/missing"}}, "one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}, "external::p::f"},
		{"same workspace ambiguous", "private", map[string]NpmPackageTarget{"one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}, "two": {WorkspaceID: "shared", Entries: []string{"two/index.ts"}}}, "external::p::f"},
		{"private symbol", "private", map[string]NpmPackageTarget{"one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}, "one/index.ts"},
		{"public symbol", "public", map[string]NpmPackageTarget{"one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}, "one/index.ts::f"},
		{"unknown visibility", "", map[string]NpmPackageTarget{"one": {WorkspaceID: "shared", Entries: []string{"one/index.ts"}}}, "one/index.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app", WorkspaceID: "shared"})
			for _, repo := range []string{"one", "two"} {
				file := repo + "/index.ts"
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, WorkspaceID: "shared"})
				g.AddNode(&graph.Node{ID: file + "::f", Kind: graph.KindFunction, Name: "f", FilePath: file, RepoPrefix: repo, WorkspaceID: "shared", Meta: map[string]any{"visibility": tc.visibility}})
			}
			imp := &graph.Edge{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}
			g.AddEdge(imp)
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget { return tc.dirs })
			cr.ResolveAll()
			assert.Equal(t, tc.want, imp.To)
			assert.Equal(t, tc.want != "external::p::f", imp.CrossRepo)
		})
	}
}

type npmProbeCountingStore struct {
	graph.Store
	providerReads int
}

func (g *npmProbeCountingStore) GetNode(id string) *graph.Node {
	if len(id) >= 4 && id[:4] == "lib/" {
		g.providerReads++
	}
	return g.Store.GetNode(id)
}

func TestCrossRepoTrackedNpmPackageProbeMemo(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "undeclared", true: "missing target"}[declared], func(t *testing.T) {
			g := &npmProbeCountingStore{Store: graph.New()}
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app"})
			for line := 1; line <= 100; line++ {
				g.AddEdge(&graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line})
			}
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return declared })
			cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
				return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/missing"}}}
			})
			for pass := 0; pass < 2; pass++ {
				g.providerReads = 0
				cr.ResolveAll()
				if declared {
					assert.Equal(t, 2*len(jsTSImportExts), g.providerReads)
				} else {
					assert.Zero(t, g.providerReads)
				}
			}
		})
	}
}

type npmHydrationCountingStore struct {
	graph.Store
	sites, maxSites int
	reads           map[string]int
}

func (g *npmHydrationCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	g.sites += len(sites)
	g.maxSites = max(g.maxSites, len(sites))
	return g.Store.GetEdgeCandidates(endpoints, sites)
}

func (g *npmHydrationCountingStore) GetNode(id string) *graph.Node {
	g.reads[id]++
	return g.Store.GetNode(id)
}

func TestCrossRepoTrackedNpmPackageHydration(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"undeclared", "alias", "declared"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				var store graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					store = db
				}
				g := &npmHydrationCountingStore{Store: store, reads: map[string]int{}}
				g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"})
				g.AddNode(&graph.Node{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"})
				g.AddNode(&graph.Node{ID: "lib/index.ts::f", Kind: graph.KindFunction, RepoPrefix: "lib", Meta: map[string]any{"visibility": "public"}})
				var edges []*graph.Edge
				for line := 1; line <= resolvePendingPageRows+3; line++ {
					e := &graph.Edge{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line}
					edges = append(edges, e)
					g.AddEdge(e)
				}
				cr := NewCrossRepo(g)
				cr.SetNpmDependencyLookup(func(_, _ string) bool { return mode != "undeclared" })
				if mode == "alias" {
					cr.SetNpmAliasResolver(func(_, _ string) string { return "other" })
				}
				lookups := 0
				cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
					lookups++
					return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/index.ts"}}}
				})
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				require.NoError(t, cr.resolveTrackedNpmImports(context.Background(), graph.EdgesLightSeq(store, graph.EdgeImports), stats))
				if mode != "declared" {
					assert.Zero(t, g.sites)
					assert.Zero(t, lookups, "rejected imports must not trigger manifest lookup")
					assert.Zero(t, stats.Resolved)
				} else {
					assert.Equal(t, len(edges), g.sites)
					assert.LessOrEqual(t, g.maxSites, resolvePendingPageRows)
					assert.Equal(t, len(edges), stats.Resolved)
					assert.Equal(t, 1, g.reads["app/a.ts"])
					assert.Equal(t, 1, g.reads["lib/index.ts::f"])
				}
			})
		}
	}
}

// Cancellation must stop the npm prepass, not only the residual resolver.
type npmCancelHydrationStore struct {
	graph.Store
	cancel context.CancelFunc
	pages  int
}

func (g *npmCancelHydrationStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	g.pages++
	g.cancel()
	return g.Store.GetEdgeCandidates(endpoints, sites)
}

func TestCrossRepoTrackedNpmPackageCancellation(t *testing.T) {
	for _, phase := range []string{"before", "scan", "hydrate"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := &npmCancelHydrationStore{Store: graph.New(), cancel: cancel}
			g.AddNode(&graph.Node{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"})
			g.AddNode(&graph.Node{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"})
			for line := 1; line <= resolvePendingPageRows+3; line++ {
				g.AddEdge(&graph.Edge{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: line})
			}
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			lookups := 0
			cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
				lookups++
				if phase == "scan" {
					cancel()
				}
				return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/index.ts"}}}
			})
			if phase == "before" {
				cancel()
			}
			stats, err := cr.ResolveAllContext(ctx)
			require.ErrorIs(t, err, context.Canceled)
			if phase == "hydrate" {
				assert.Equal(t, 1, g.pages, "must not hydrate another page after cancellation")
				assert.LessOrEqual(t, stats.Resolved, resolvePendingPageRows)
			} else {
				assert.Zero(t, g.pages)
				assert.Zero(t, stats.Resolved)
				if phase == "scan" {
					assert.Equal(t, 1, lookups, "must stop scanning after cancellation")
				} else {
					assert.Zero(t, lookups)
				}
			}
		})
	}
}

func TestTrackedNpmCallUsesImportBinding(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			g.AddBatch([]*graph.Node{
				{ID: "app/a.ts", Kind: graph.KindFile, FilePath: "app/a.ts", RepoPrefix: "app", Language: "typescript"},
				{ID: "app/a.ts::run", Kind: graph.KindFunction, FilePath: "app/a.ts", RepoPrefix: "app", Language: "typescript"},
				{ID: "lib/z.ts", Kind: graph.KindFile, FilePath: "lib/z.ts", RepoPrefix: "lib", Language: "typescript"},
				{ID: "lib/z.ts::f", Name: "f", Kind: graph.KindFunction, FilePath: "lib/z.ts", RepoPrefix: "lib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
				{ID: "lib/a/helpers.ts::f", Name: "f", Kind: graph.KindFunction, FilePath: "lib/a/helpers.ts", RepoPrefix: "lib", Language: "typescript"},
				{ID: "lib/internal.ts::debounce", Name: "debounce", Kind: graph.KindFunction, FilePath: "lib/internal.ts", RepoPrefix: "lib", Language: "typescript"},
			}, []*graph.Edge{
				{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1},
				{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1},
				{From: "app/a.ts", To: "external::lodash::debounce", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 2},
				{From: "app/a.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts", Line: 3},
				{From: "app/a.ts::run", To: "unresolved::debounce", Kind: graph.EdgeCalls, FilePath: "app/a.ts", Line: 4},
			})
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
				if spec == "p" {
					return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/z.ts"}}}
				}
				return nil
			})
			cr.ResolveAll()
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("app/a.ts::run") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 2)
			for _, e := range calls {
				if e.Line == 3 {
					assert.Equal(t, "lib/z.ts::f", e.To)
				} else {
					assert.Equal(t, "unresolved::debounce", e.To)
					assert.False(t, e.CrossRepo)
				}
			}
		})
	}
}

func TestTrackedNpmAmbiguityCountsUnindexedManifests(t *testing.T) {
	for _, workspace := range []string{"shared", "foreign"} {
		t.Run(workspace, func(t *testing.T) {
			g := graph.New()
			g.AddBatch([]*graph.Node{
				{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app", WorkspaceID: "shared"},
				{ID: "fork/index.ts", Kind: graph.KindFile, RepoPrefix: "fork", WorkspaceID: workspace},
			}, []*graph.Edge{{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}})
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
			cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
				return map[string]NpmPackageTarget{"real": {WorkspaceID: workspace, Entries: []string{"real/dist/index.js"}}, "fork": {WorkspaceID: workspace, Entries: []string{"fork/index.ts"}}}
			})
			cr.ResolveAll()
			imports := g.GetOutEdges("app/a.ts")
			require.Len(t, imports, 1)
			assert.Equal(t, "external::p", imports[0].To)
			assert.False(t, imports[0].CrossRepo)
		})
	}
}

func TestTrackedNpmLinkedImportsRechecked(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, change := range []string{"renamed", "blocked", "dependency removed", "alias", "repointed"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				g.AddBatch([]*graph.Node{
					{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"},
					{ID: "app/a.ts::run", Kind: graph.KindFunction, FilePath: "app/a.ts", RepoPrefix: "app"},
					{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"},
					{ID: "lib/index.ts::f", Name: "f", Kind: graph.KindFunction, RepoPrefix: "lib", Meta: map[string]any{"visibility": "public"}},
					{ID: "next/index.ts", Kind: graph.KindFile, RepoPrefix: "next"},
					{ID: "next/index.ts::f", Name: "f", Kind: graph.KindFunction, RepoPrefix: "next", Meta: map[string]any{"visibility": "public"}},
				}, []*graph.Edge{
					{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1},
					{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1},
					{From: "app/a.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts", Line: 2},
				})
				changed := false
				cr := NewCrossRepo(g)
				cr.SetNpmDependencyLookup(func(_, _ string) bool { return !changed || change != "dependency removed" })
				cr.SetNpmAliasResolver(func(_, _ string) string {
					if changed && change == "alias" {
						return "other"
					}
					return ""
				})
				cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
					if changed && change == "repointed" {
						return map[string]NpmPackageTarget{"next": {Entries: []string{"next/index.ts"}}}
					}
					if changed && change == "renamed" {
						return nil
					}
					if changed && change == "blocked" {
						return map[string]NpmPackageTarget{"lib": {}}
					}
					return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/index.ts"}}}
				})
				cr.ResolveAll()
				for _, e := range g.GetOutEdges("app/a.ts") {
					require.True(t, e.CrossRepo)
					assert.NotEmpty(t, e.Meta["npm_import"])
				}
				for _, e := range g.GetOutEdges("app/a.ts::run") {
					if e.Kind == graph.EdgeCalls {
						require.Equal(t, "lib/index.ts::f", e.To)
					}
				}
				changed = true
				cr.ResolveAll()
				if change == "repointed" {
					calls := g.GetOutEdges("app/a.ts::run")
					require.Len(t, calls, 2, "one base call and one parallel")
					for _, e := range calls {
						assert.Equal(t, "next/index.ts::f", e.To, "base and parallel must both target the new provider")
					}
					assert.Empty(t, g.GetInEdges("lib/index.ts::f"))
					return
				}
				var targets []string
				for _, e := range g.GetOutEdges("app/a.ts") {
					targets = append(targets, e.To)
					assert.False(t, e.CrossRepo)
				}
				assert.ElementsMatch(t, []string{"external::p", "external::p::f"}, targets)
				for _, e := range g.GetOutEdges("app/a.ts::run") {
					assert.NotEqual(t, graph.EdgeCrossRepoCalls, e.Kind, "restubbed call must not keep its old parallel")
					if e.Kind == graph.EdgeCalls {
						assert.Equal(t, "unresolved::f", e.To)
						assert.False(t, e.CrossRepo)
					}
				}
				assert.Zero(t, cr.ResolveAll().Resolved, "repeated rejection is idempotent")
			})
		}
	}
}

func TestTrackedNpmOwnWorkspaceWins(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app", WorkspaceID: "shared"},
		{ID: "local/index.ts", Kind: graph.KindFile, RepoPrefix: "local", WorkspaceID: "shared"},
		{ID: "foreign/index.ts", Kind: graph.KindFile, RepoPrefix: "foreign", WorkspaceID: "elsewhere"},
	}, []*graph.Edge{{From: "app/a.ts", To: "external::p", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1}})
	cr := NewCrossRepo(g)
	cr.SetCrossWorkspaceDepLookup(func(_ string) []CrossWorkspaceDepRule {
		return []CrossWorkspaceDepRule{{Workspace: "elsewhere", Modules: []string{"p"}}}
	})
	cr.SetNpmDependencyLookup(func(_, _ string) bool { return true })
	cr.SetNpmPackageLookup(func(_ string) map[string]NpmPackageTarget {
		return map[string]NpmPackageTarget{"local": {WorkspaceID: "shared", Entries: []string{"local/index.ts"}}, "foreign": {WorkspaceID: "elsewhere", Entries: []string{"foreign/index.ts"}}}
	})
	cr.ResolveAll()
	imports := g.GetOutEdges("app/a.ts")
	require.Len(t, imports, 1)
	assert.Equal(t, "local/index.ts", imports[0].To)
	assert.True(t, imports[0].CrossRepo)
}

func TestTrackedNpmRenamedImportDoesNotExposeOriginal(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			g.AddBatch([]*graph.Node{
				{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"},
				{ID: "app/a.ts::run", Kind: graph.KindFunction, FilePath: "app/a.ts", RepoPrefix: "app"},
				{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"},
				{ID: "lib/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/index.ts", RepoPrefix: "lib", Meta: map[string]any{"visibility": "public"}},
			}, []*graph.Edge{
				{From: "app/a.ts", To: "external::p::f", Kind: graph.EdgeImports, Alias: "renamed", Meta: map[string]any{"import_alias": "renamed"}, FilePath: "app/a.ts", Line: 1},
				{From: "app/a.ts", To: "external::lodash::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 2},
				{From: "app/a.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts", Line: 3},
				{From: "app/a.ts::run", To: "unresolved::renamed", Kind: graph.EdgeCalls, FilePath: "app/a.ts", Line: 4},
			})
			cr := NewCrossRepo(g)
			cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" || spec == "lodash" })
			cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
				if spec == "p" {
					return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/index.ts"}}}
				}
				return nil
			})
			cr.ResolveAll()
			// The provider changes, but the consumer's marked import is outside
			// the outgoing frontier. Its incoming bare f() must stay unresolved.
			cr.ResolveMutationFrontiers([]string{"lib/index.ts"}, nil, []string{"lib/index.ts"})
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("app/a.ts::run") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 2)
			for _, call := range calls {
				if call.Line == 3 {
					assert.Equal(t, "unresolved::f", call.To)
					assert.False(t, call.CrossRepo)
				} else {
					assert.Equal(t, "lib/index.ts::f", call.To)
					assert.True(t, call.CrossRepo)
				}
			}
		})
	}
}

func TestJSTSCrossRepoFileImportFallback(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			g.AddBatch([]*graph.Node{
				{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"},
				{ID: "app/a.ts::run", Kind: graph.KindFunction, FilePath: "app/a.ts", RepoPrefix: "app"},
				{ID: "lib/index.ts", Kind: graph.KindFile, RepoPrefix: "lib"},
				{ID: "lib/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/index.ts", RepoPrefix: "lib"},
			}, []*graph.Edge{
				{From: "app/a.ts", To: "lib/index.ts", Kind: graph.EdgeImports, FilePath: "app/a.ts", CrossRepo: true},
				{From: "app/a.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts"},
			})
			NewCrossRepo(g).ResolveAll()
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("app/a.ts::run") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 1)
			assert.Equal(t, "lib/index.ts::f", calls[0].To)
			assert.True(t, calls[0].CrossRepo)
		})
	}
}

func TestJSTSRenamedImportPreservesSameRepoCall(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, importedRepo := range []string{"lib", "app"} {
			t.Run(backend+"/"+importedRepo, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				target := importedRepo + "/x.ts::f"
				importedName := "f"
				if importedRepo == "app" {
					importedName = "" // Do not add a second same-repo name candidate.
				}
				g.AddBatch([]*graph.Node{
					{ID: "app/a.ts", Kind: graph.KindFile, RepoPrefix: "app"},
					{ID: "app/a.ts::run", Kind: graph.KindFunction, FilePath: "app/a.ts", RepoPrefix: "app"},
					{ID: "app/b.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "app/b.ts", RepoPrefix: "app"},
					{ID: target, Kind: graph.KindFunction, Name: importedName, FilePath: importedRepo + "/x.ts", RepoPrefix: importedRepo},
				}, []*graph.Edge{
					{From: "app/a.ts", To: target, Kind: graph.EdgeImports, Alias: "g", Meta: map[string]any{"import_alias": "g"}, FilePath: "app/a.ts", CrossRepo: importedRepo != "app"},
					{From: "app/a.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts"},
				})
				NewCrossRepo(g).ResolveAll()
				var calls []*graph.Edge
				for _, edge := range g.GetOutEdges("app/a.ts::run") {
					if edge.Kind == graph.EdgeCalls {
						calls = append(calls, edge)
					}
				}
				require.Len(t, calls, 1)
				assert.Equal(t, "app/b.ts::f", calls[0].To)
				assert.False(t, calls[0].CrossRepo)
			})
		}
	}
}
