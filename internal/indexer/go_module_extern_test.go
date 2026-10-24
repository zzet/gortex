package indexer

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func TestMultiIndexer_GoModuleExtern(t *testing.T) {
	testGoModuleExtern(t, []string{"memory", "sqlite"}, "all")
}

func TestMultiIndexer_GoModuleExternTrackMemory(t *testing.T) {
	testGoModuleExtern(t, []string{"memory"}, "track")
}

func TestMultiIndexer_GoModuleExternTrackSQLite(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "track")
}

func TestMultiIndexer_GoModuleExternTrackReverse(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "reverse")
}

func TestMultiIndexer_GoModuleExternTrackParallel(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "parallel")
}

func TestMultiIndexer_GoModuleExternRuntime(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "runtime")
}

func TestMultiIndexer_GoModuleExternIndexRepo(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "indexrepo")
}

func TestMultiIndexer_GoModuleExternReconcile(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "reconcile")
}

func TestMultiIndexer_GoModuleExternIndexCtx(t *testing.T) {
	testGoModuleExtern(t, []string{"sqlite"}, "indexctx")
}

func TestMultiIndexer_GoModuleExternWatcher(t *testing.T) {
	testGoModuleExtern(t, []string{"memory", "sqlite"}, "watcher")
}

func TestMultiIndexer_GoModuleExternLegacyDeps(t *testing.T) {
	testGoModuleExtern(t, []string{"memory", "sqlite"}, "legacy")
}

func testGoModuleExtern(t *testing.T, backends []string, mode string) {
	t.Helper()
	for _, backend := range backends {
		for _, tc := range []struct {
			name, module, prefix, workspace, declaration         string
			manifest, wantCalls, decoy, replace, differentModule bool
		}{
			{name: "dotless", module: "backend", prefix: "b", workspace: "shared", manifest: true, wantCalls: true},
			{name: "renamed", module: "example.org/library", prefix: "b", workspace: "shared", manifest: true, wantCalls: true},
			{name: "public_shaped", module: "go.uber.org/multierr", prefix: "libsource", workspace: "shared", manifest: true, wantCalls: true},
			{name: "module_before_suffix", module: "example.org/library", prefix: "b", workspace: "shared", manifest: true, wantCalls: true, decoy: true},
			{name: "separate_workspace", module: "example.org/library", prefix: "b", workspace: "other", manifest: true},
			{name: "declared_workspace", module: "example.org/library", prefix: "b", workspace: "other", declaration: "example.org/library", manifest: true, wantCalls: true},
			{name: "wrong_declared_module", module: "example.org/library", prefix: "b", workspace: "other", declaration: "example.org/library2", manifest: true},
			{name: "local_replace", module: "example.org/library", prefix: "b", workspace: "shared", manifest: true, replace: true, wantCalls: true},
			{name: "suffix_nonmatching_module", module: "example.org/library", prefix: "library", workspace: "shared", manifest: true, differentModule: true},
			{name: "suffix_without_manifest", module: "example.org/library", prefix: "library", workspace: "shared", wantCalls: true},
			{name: "suffix_separate_workspace", module: "example.org/library", prefix: "library", workspace: "other"},
		} {
			if (mode == "legacy" || mode == "watcher" || (mode != "all" && backend == "sqlite")) && (!tc.manifest || tc.differentModule) {
				continue
			}
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				caller, library := t.TempDir(), t.TempDir()
				writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/consumer\n\ngo 1.27.0\n\nrequire "+tc.module+" v1.0.0\n")
				writeFile(t, filepath.Join(caller, "main.go"), fmt.Sprintf(`package consumer
import library %q
import child %q
func Caller() { library.Hello(); child.Nested() }
`, tc.module, tc.module+"/sub"))
				writeFile(t, filepath.Join(library, "library.go"), "package library\nfunc Hello() {}\n")
				require.NoError(t, os.Mkdir(filepath.Join(library, "sub"), 0o755))
				writeFile(t, filepath.Join(library, "sub", "sub.go"), "package sub\nfunc Nested() {}\n")
				if tc.manifest {
					module := tc.module
					if tc.differentModule {
						module = "example.org/unrelated"
					}
					writeFile(t, filepath.Join(library, "go.mod"), "module "+module+"\n\ngo 1.27.0\n")
				}
				if tc.replace {
					goMod := "module example.org/consumer\n\ngo 1.27.0\n\nrequire " + tc.module + " v1.0.0\nreplace " + tc.module + " => " + filepath.ToSlash(library) + "\n"
					writeFile(t, filepath.Join(caller, "go.mod"), goMod)
				}
				if tc.declaration != "" {
					writeFile(t, filepath.Join(caller, ".gortex.yaml"), "cross_workspace_deps:\n  - workspace: other\n    modules: ["+tc.declaration+"]\n    mode: read-only\n")
				}
				repos := []config.RepoEntry{
					{Path: caller, Name: "a", Workspace: "shared"},
					{Path: library, Name: tc.prefix, Workspace: tc.workspace},
				}
				if tc.decoy {
					decoy := t.TempDir()
					writeFile(t, filepath.Join(decoy, "library.go"), "package library\nfunc Hello() {}\n")
					repos = append(repos, config.RepoEntry{Path: decoy, Name: "library", Workspace: "shared"})
				}
				cm := newTestConfigManager(t)
				cm.Global().Repos = repos
				if mode == "runtime" {
					cm.Global().Repos = nil
				}
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				var err error
				if mode == "watcher" {
					mi.BeginParallelBatch()
					t.Cleanup(mi.EndBatch)
				}
				switch mode {
				case "track", "reverse", "watcher", "runtime", "indexrepo", "reconcile", "indexctx":
					order := []config.RepoEntry{repos[1], repos[0]}
					if mode == "reverse" || mode == "runtime" {
						order[0], order[1] = order[1], order[0]
					}
					for _, entry := range order {
						_, err = mi.TrackRepoCtx(context.Background(), entry)
						require.NoError(t, err)
					}
				case "parallel":
					errors := make(chan error, len(repos))
					for _, entry := range repos {
						go func() { _, e := mi.TrackRepoCtx(context.Background(), entry); errors <- e }()
					}
					for range repos {
						require.NoError(t, <-errors)
					}
				default:
					_, err = mi.IndexAll()
				}
				if mode == "legacy" {
					// Simulate a legacy store loaded before its first module sweep.
					mi.goModuleRetry = resolver.GoModuleDepRetry{}
					var batch []graph.EdgeReindex
					for _, edge := range g.GetOutEdges("a/main.go::Caller") {
						if edge.Kind != graph.EdgeCalls {
							continue
						}
						target := g.GetNode(edge.To)
						if target == nil || target.RepoPrefix != tc.prefix {
							continue
						}
						oldTo := edge.To
						pkg := tc.module
						if target.Name == "Nested" {
							pkg += "/sub"
						}
						edge.To = "dep::" + pkg + "::" + target.Name
						edge.Origin, edge.Confidence, edge.ConfidenceLabel = "", 0, ""
						batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
					}
					g.ReindexEdges(batch)
				}
				if mode == "legacy" || mode == "watcher" {
					mi.RunGlobalResolve()
				}
				require.NoError(t, err)

				if mode == "reconcile" {
					prior := mi.FileMtimes("a")
					mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
					t.Cleanup(func() { require.NoError(t, mi2.Close(context.Background())) })
					_, err = mi2.ReconcileRepoCtx(context.Background(), repos[1], mi.FileMtimes(tc.prefix))
					require.NoError(t, err)
					t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
					res, e := mi2.ReconcileRepoCtx(context.Background(), repos[0], prior)
					require.NoError(t, e)
					require.True(t, res.FullRetrack)
					mi = mi2
				}
				if mode == "indexctx" {
					_, err = mi.GetIndexer("a").IndexCtx(context.Background(), caller)
					require.NoError(t, err)
				}
				if mode == "indexrepo" {
					_, err = mi.IndexRepo("a")
					require.NoError(t, err)
				}
				callerID := "a/main.go::Caller"
				checkCalls := func() {
					var targets []string
					for _, edge := range g.GetOutEdges(callerID) {
						if edge.Kind != graph.EdgeCalls {
							continue
						}
						n := g.GetNode(edge.To)
						if n == nil || n.RepoPrefix == "" || n.RepoPrefix == "a" {
							continue
						}
						targets = append(targets, edge.To)
						assert.True(t, edge.CrossRepo, "edge=%+v source=%+v", edge, g.GetNode(edge.From))
						if tc.manifest && !tc.differentModule {
							assert.Equal(t, graph.OriginASTInferred, edge.Origin)
							assert.NotEqual(t, "EXTRACTED", edge.ConfidenceLabel)
							assert.Greater(t, edge.Confidence, 0.0)
							assert.Less(t, edge.Confidence, 0.9)
						}
					}
					if tc.wantCalls {
						assert.ElementsMatch(t, []string{tc.prefix + "/library.go::Hello", tc.prefix + "/sub/sub.go::Nested"}, targets)
					} else {
						assert.Empty(t, targets, "no source call may cross an undeclared workspace boundary")
					}
				}
				checkCalls()
				content, err := os.ReadFile(filepath.Join(caller, "main.go"))
				require.NoError(t, err)
				edited := "// edited\n" + string(content)
				if mode == "watcher" {
					edited = strings.Replace(edited, "func Caller()", "func Edited()", 1)
					callerID = "a/main.go::Edited"
				}
				writeFile(t, filepath.Join(caller, "main.go"), edited)
				if mode == "watcher" {
					_, err = mi.GetIndexer("a").incrementalPointWatcherPath(caller, filepath.Join(caller, "main.go"))
				} else {
					err = mi.GetIndexer("a").IndexFile(filepath.Join(caller, "main.go"))
				}
				require.NoError(t, err)
				checkCalls()
			})
		}
	}
}

func TestReadGoModModule(t *testing.T) {
	for _, source := range []string{
		"module example.org/library\n",
		"module\texample.org/library\n",
		"module \"example.org/library\" // module identity\n",
	} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), source)
			assert.Equal(t, "example.org/library", readGoModModule(root))
		})
	}
	for _, source := range []string{"", "module", "module \"unterminated"} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), source)
		assert.Empty(t, readGoModModule(root))
	}
	assert.Empty(t, readGoModModule(t.TempDir()))
}

func TestTrackedGoModuleCacheRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/old\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: root}}
	mi := &MultiIndexer{configMgr: cm, repos: map[string]*RepoMetadata{"b": {RootPath: root}}}
	modules, generation := mi.goModuleSnapshot()
	require.Equal(t, "example.org/old", modules["b"])
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/old\nrequire example.org/dep v1.2.3\n")
	mi.goModuleChecked = time.Time{}
	same, sameGeneration := mi.goModuleSnapshot()
	assert.Equal(t, modules, same)
	assert.Equal(t, generation, sameGeneration, "require-only edits must not schedule a new terminal sweep")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/new\n")
	rateLimited, rateGeneration := mi.goModuleSnapshot()
	assert.Equal(t, "example.org/old", rateLimited["b"], "no stat refresh inside the snapshot interval")
	assert.Equal(t, generation, rateGeneration)
	mi.goModuleChecked = time.Time{}
	cached, cachedGeneration := mi.goModuleSnapshot()
	assert.Equal(t, "example.org/new", cached["b"], "root go.mod edits refresh without config reload")
	assert.Greater(t, cachedGeneration, generation)
	unchanged, unchangedGeneration := mi.goModuleSnapshot()
	assert.Equal(t, cached, unchanged)
	assert.Equal(t, cachedGeneration, unchangedGeneration)
	cm.LoadWorkspaceConfig("b", root)
	refreshed, refreshedGeneration := mi.goModuleSnapshot()
	assert.Equal(t, "example.org/new", refreshed["b"])
	assert.Greater(t, refreshedGeneration, generation)
	assert.Equal(t, "example.org/old", modules["b"], "published maps are immutable")
}

func TestMultiIndexer_GoModuleRuntimeTestProjection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	caller, library := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/caller\n")
	writeFile(t, filepath.Join(caller, "main_test.go"), "package caller\nimport \"example.org/library\"\nfunc TestHello() { library.Hello() }\n")
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\n")
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, e := range []config.RepoEntry{{Path: caller, Name: "a", Workspace: "shared"}, {Path: library, Name: "b", Workspace: "shared"}} {
		_, err = mi.TrackRepoCtx(context.Background(), e)
		require.NoError(t, err)
	}
	var calls, tests []string
	for _, e := range s.GetOutEdges("a/main_test.go::TestHello") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e.To)
		}
		if e.Kind == graph.EdgeTests {
			tests = append(tests, e.To)
		}
	}
	assert.Equal(t, []string{"b/lib.go::Hello"}, calls)
	assert.Equal(t, []string{"b/lib.go::Hello"}, tests, "the legacy retry must reconcile the test-caller frontier")
}

func TestMultiIndexer_GoModuleSameRepoProvenance(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"all", "track"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				repo := t.TempDir()
				writeFile(t, filepath.Join(repo, "go.mod"), "module example.org/caller\n")
				require.NoError(t, os.Mkdir(filepath.Join(repo, "util"), 0o755))
				writeFile(t, filepath.Join(repo, "main.go"), "package caller\nimport \"example.org/caller/util\"\nfunc Caller() { util.Help() }\n")
				writeFile(t, filepath.Join(repo, "util", "u.go"), "package util\nfunc Help() {}\n")
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				cm := newTestConfigManager(t)
				entry := config.RepoEntry{Path: repo, Name: "a", Workspace: "shared"}
				mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				if mode == "all" {
					cm.Global().Repos = []config.RepoEntry{entry}
					_, err := mi.IndexAll()
					require.NoError(t, err)
				} else {
					_, err := mi.TrackRepoCtx(context.Background(), entry)
					require.NoError(t, err)
				}
				var calls []*graph.Edge
				for _, e := range g.GetOutEdges("a/main.go::Caller") {
					if e.Kind == graph.EdgeCalls {
						calls = append(calls, e)
					}
				}
				require.Len(t, calls, 1)
				assert.Equal(t, "a/util/u.go::Help", calls[0].To)
				assert.Zero(t, calls[0].Confidence)
				assert.Empty(t, calls[0].Origin)
				assert.False(t, calls[0].CrossRepo)
			})
		}
	}
}

func TestMultiIndexer_GoModuleIdentityEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	caller, library := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/caller\n")
	writeFile(t, filepath.Join(caller, "main.go"), "package caller\nimport \"example.org/library\"\nfunc Caller() { library.Hello() }\n")
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\n")
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, e := range []config.RepoEntry{{Path: library, Name: "b", Workspace: "shared"}, {Path: caller, Name: "a", Workspace: "shared"}} {
		_, err = mi.TrackRepoCtx(context.Background(), e)
		require.NoError(t, err)
	}
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/library/v2\n")
	writeFile(t, filepath.Join(caller, "main.go"), "package caller\nimport library \"example.org/library/v2\"\nfunc Caller() { library.Hello() }\n")
	mi.goModuleChecked = time.Time{}
	_, err = mi.IndexRepo("a")
	require.NoError(t, err)
	var calls []*graph.Edge
	for _, e := range s.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "b/lib.go::Hello", calls[0].To)
	assert.Equal(t, 0.6, calls[0].Confidence)
}

func TestMultiIndexer_GoModuleDeferredEmpty(t *testing.T) {
	// No ConfigManager or graph is needed when the shadow deferred no edges.
	mi := &MultiIndexer{}
	idx := &Indexer{resolver: resolver.New(graph.New())}
	require.NoError(t, mi.resolveDeferredGoModules(idx))
	require.NoError(t, mi.resolveIncomingGoModules("a"))
}

func TestMultiIndexer_GoModuleProviderAddsSymbol(t *testing.T) {
	for _, mode := range []string{"watcher", "indexrepo"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			caller, library := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/caller\n")
			writeFile(t, filepath.Join(caller, "main.go"), "package caller\nimport \"example.org/library\"\nfunc Caller() { library.NewFn() }\n")
			writeFile(t, filepath.Join(library, "go.mod"), "module example.org/library\n")
			writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\n")
			s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			cm := newTestConfigManager(t)
			mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			for _, e := range []config.RepoEntry{{Path: library, Name: "b", Workspace: "shared"}, {Path: caller, Name: "a", Workspace: "shared"}} {
				_, err = mi.TrackRepoCtx(context.Background(), e)
				require.NoError(t, err)
			}
			mi.RunGlobalResolve() // finish the revision's legacy sweep before adding the symbol
			writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\nfunc NewFn() {}\n")
			if mode == "watcher" {
				// MultiWatcher.pointReindexRaw uses the shared MultiIndexer executor.
				_, err = mi.incrementalPointRepoRaw("b", filepath.Join(library, "lib.go"))
			} else {
				_, err = mi.IndexRepo("b")
			}
			require.NoError(t, err)
			var calls []*graph.Edge
			for _, e := range s.GetOutEdges("a/main.go::Caller") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 1)
			assert.Equal(t, "b/lib.go::NewFn", calls[0].To)
			assert.Equal(t, 0.6, calls[0].Confidence)
			assert.Equal(t, graph.OriginASTInferred, calls[0].Origin)
		})
	}
}

func TestMultiIndexer_GoModuleDeferredPublicationInterleave(t *testing.T) {
	g := graph.New()
	cm := newTestConfigManager(t)
	library := t.TempDir()
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library}}
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	shadow := graph.New()
	shadow.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	shadow.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
	r := resolver.New(shadow)
	r.SetGoModules(map[string]string{"b": "example.org/lib"})
	r.SetDeferGoModuleMisses(true)
	r.ResolveAll()
	idx := &Indexer{resolver: r}
	mi.mu.Lock()
	mi.indexers["a"] = idx
	mi.mu.Unlock()
	// Repo B's global pass interleaves after A resolves its shadow, before A drains it.
	require.NoError(t, mi.runCrossRepoResolveContext(context.Background(), false))
	for _, n := range shadow.AllNodes() {
		g.AddNode(n)
	}
	for _, e := range shadow.AllEdges() {
		g.AddEdge(e)
	}
	r.SetGraph(g)
	r.SetDeferGoModuleMisses(false)
	require.NoError(t, mi.resolveDeferredGoModules(idx))
	var targets []string
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			targets = append(targets, e.To)
		}
	}
	assert.Equal(t, []string{"b/lib.go::Fn"}, targets, "the in-flight shadow receipt must survive another repo's global pass")
}

type goModulePublishDroppingStore struct {
	graph.Store
	drop bool
}

func (s *goModulePublishDroppingStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	if s.drop {
		s.drop = false
		return false
	}
	s.ReindexEdges(batch)
	return true
}

func TestMultiIndexer_GoModuleDeferredDrop(t *testing.T) {
	g := &goModulePublishDroppingStore{Store: graph.New(), drop: true}
	library := t.TempDir()
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library}}
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	shadow := graph.New()
	shadow.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	for line := 1; line <= 513; line++ {
		shadow.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
	}
	r := resolver.New(shadow)
	r.SetGoModules(map[string]string{"b": "example.org/lib"})
	r.SetDeferGoModuleMisses(true)
	r.ResolveAll()
	for _, n := range shadow.AllNodes() {
		g.AddNode(n)
	}
	for _, e := range shadow.AllEdges() {
		g.AddEdge(e)
	}
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	r.SetGraph(g)
	r.SetDeferGoModuleMisses(false)
	idx := &Indexer{resolver: r}
	require.NoError(t, mi.resolveDeferredGoModules(idx), "writer contention must not fail an already published index")
	count := func() (int, int) {
		bound, pending := 0, 0
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
		return bound, pending
	}
	bound, pending := count()
	assert.Equal(t, 1, bound)
	assert.Equal(t, 512, pending)
	require.NoError(t, mi.runCrossRepoResolveContext(context.Background(), false))
	bound, pending = count()
	assert.Equal(t, 513, bound)
	assert.Zero(t, pending)
}

func TestMultiIndexer_GoModuleColdShadowPrecise(t *testing.T) {
	for _, tc := range []struct{ module, pkg, name, local, remote string }{
		{"example.org/lib", "example.org/lib/errors", "Wrap", "internal/errors/e.go", "errors/e.go"},
		{"k8s.io/apimachinery", "k8s.io/apimachinery/pkg/apis/meta/v1", "Now", "api/v1/t.go", "pkg/apis/meta/v1/t.go"},
		{"example.org/library", "example.org/library", "Fn", "internal/library/l.go", "l.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			caller, provider := t.TempDir(), t.TempDir()
			own := "example.org/caller"
			if tc.name == "Fn" {
				own = "example.org"
			}
			writeFile(t, filepath.Join(caller, "go.mod"), "module "+own+"\n")
			writeFile(t, filepath.Join(provider, "go.mod"), "module "+tc.module+"\n")
			writeFile(t, filepath.Join(caller, "main.go"), fmt.Sprintf("package caller\nimport lib %q\nfunc Caller() { lib.%s() }\n", tc.pkg, tc.name))
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(caller, tc.local)), 0o755))
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(provider, tc.remote)), 0o755))
			if tc.name != "Fn" {
				writeFile(t, filepath.Join(caller, tc.local), "package local\nfunc "+tc.name+"() {}\n")
			}
			writeFile(t, filepath.Join(provider, tc.remote), "package lib\nfunc "+tc.name+"() {}\n")
			s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			cm := newTestConfigManager(t)
			cm.Global().Repos = []config.RepoEntry{{Name: "a", Path: caller, Workspace: "shared"}, {Name: "lib", Path: provider, Workspace: "shared"}}
			mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			_, err = mi.IndexAll()
			require.NoError(t, err)
			// Complete the module revision's retry sweep before re-indexing the caller.
			mi.RunGlobalResolve()
			_, err = mi.IndexRepo("a")
			require.NoError(t, err)
			var targets []string
			for _, e := range s.GetOutEdges("a/main.go::Caller") {
				if e.Kind == graph.EdgeCalls {
					targets = append(targets, e.To)
				}
			}
			assert.Equal(t, []string{"lib/" + tc.remote + "::" + tc.name}, targets)
		})
	}
}

type goModuleCancelPublishedStore struct {
	graph.Store
	published func() bool
	cancel    context.CancelFunc
}

func (s *goModuleCancelPublishedStore) ExternalCallCandidateEdgesSeq() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		if s.published() {
			s.cancel()
		}
		for _, e := range s.AllEdges() {
			if !yield(e) {
				return
			}
		}
	}
}
func TestMultiIndexer_GoModuleTrackTailCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.org/lib\n")
	writeFile(t, filepath.Join(repo, "lib.go"), "package lib\nfunc Fn() {}\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &goModuleCancelPublishedStore{Store: graph.New(), cancel: cancel}
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	g.published = func() bool { return mi.GetIndexer("b") != nil }
	result, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: repo, Name: "b", Workspace: "shared"})
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "fixture must cancel after publication")
	assert.NoError(t, err, "a best-effort follow-up cannot fail the installed track")
	assert.NotNil(t, result)
	assert.NotNil(t, mi.GetIndexer("b"))
	assert.NotNil(t, g.GetNode("b/lib.go::Fn"))
}

type goModulePublishDuringScanStore struct {
	graph.Store
	afterSnapshot func()
}

func (s *goModulePublishDuringScanStore) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	scan, err := s.Store.(graph.UnresolvedEdgePager).BeginUnresolvedEdgeScan(ctx)
	if err == nil && s.afterSnapshot != nil {
		publish := s.afterSnapshot
		s.afterSnapshot = nil
		publish()
	}
	return scan, err
}
func (s *goModulePublishDuringScanStore) ReadUnresolvedEdgePage(ctx context.Context, scan graph.UnresolvedEdgeScan, after int64, rows, bytes int) (graph.UnresolvedEdgePage, error) {
	return s.Store.(graph.UnresolvedEdgePager).ReadUnresolvedEdgePage(ctx, scan, after, rows, bytes)
}
func TestMultiIndexer_GoModuleDeferredPublishDuringPass(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModulePublishDuringScanStore{Store: db}
	library := t.TempDir()
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library}}
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	shadow := graph.New()
	shadow.AddNode(&graph.Node{ID: "a/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	shadow.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
	r := resolver.New(shadow)
	r.SetGoModules(map[string]string{"b": "example.org/lib"})
	r.SetDeferGoModuleMisses(true)
	r.ResolveAll()
	idx := &Indexer{resolver: r}
	mi.indexers["a"] = idx
	// Publication happens after this pass pins its high water, but before it returns.
	g.afterSnapshot = func() {
		for _, n := range shadow.AllNodes() {
			g.AddNode(n)
		}
		for _, e := range shadow.AllEdges() {
			g.AddEdge(e)
		}
		r.SetDeferGoModuleMisses(false)
	}
	require.NoError(t, mi.runCrossRepoResolveContext(context.Background(), false))
	require.Nil(t, g.afterSnapshot, "the snapshot hook must have published")
	require.NoError(t, mi.resolveDeferredGoModules(idx))
	var targets []string
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			targets = append(targets, e.To)
		}
	}
	assert.Equal(t, []string{"b/lib.go::Fn"}, targets, "a pass cannot drain rows published outside its snapshot")
}

func TestMultiIndexer_GoModuleDeferredBeforeCancelledGlobalPass(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	library := t.TempDir()
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library}}
	mi := NewMultiIndexer(db, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	shadow := graph.New()
	shadow.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	shadow.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "unresolved::extern::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
	r := resolver.New(shadow)
	r.SetGoModules(map[string]string{"b": "example.org/lib"})
	r.SetDeferGoModuleMisses(true)
	r.ResolveAll()
	for _, n := range shadow.AllNodes() {
		db.AddNode(n)
	}
	for _, e := range shadow.AllEdges() {
		db.AddEdge(e)
	}
	db.AddNode(&graph.Node{ID: "b/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	r.SetGraph(db)
	r.SetDeferGoModuleMisses(false)
	idx := &Indexer{resolver: r}
	mi.indexers["a"] = idx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, mi.runCrossRepoResolveContext(ctx, false), context.Canceled)
	require.NoError(t, mi.resolveDeferredGoModules(idx))
	var calls []*graph.Edge
	for _, e := range db.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "b/lib.go::Fn", calls[0].To, "a global pass must resolve receipts it drains, even if its later scan is cancelled")
}

type goModulePublishScanCountingStore struct {
	graph.Store
	scans int
}

func (s *goModulePublishScanCountingStore) ExternalCallCandidateEdgesSeq() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		s.scans++
		for e := range s.Store.(graph.ExternalCallCandidateIterator).ExternalCallCandidateEdgesSeq() {
			if !yield(e) {
				return
			}
		}
	}
}

func TestMultiIndexer_GoModuleNonModulePublishRetries(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModulePublishScanCountingStore{Store: db}
	library := t.TempDir()
	writeFile(t, filepath.Join(library, "go.mod"), "module example.org/lib\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: library}}
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", WorkspaceID: "shared", Language: "go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Fn", Name: "Fn", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "shared", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/lib::Fn", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
	idx := &Indexer{resolver: resolver.New(g), repoPrefix: "non-go"}
	for range 2 {
		require.NoError(t, mi.postPublishGoModules(t.Context(), idx, true))
	}
	assert.Equal(t, 1, g.scans, "repeated publication with the same module revision must not repeat the sweep")
	var calls []*graph.Edge
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "b/lib.go::Fn", calls[0].To)
}
