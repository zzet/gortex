package indexer

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestGoModuleCallerSaveHonorsUntrackedReplace(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"reindex", "point", "indexfile"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nrequire example.org/library v1.0.0\nreplace example.org/library => "+filepath.ToSlash(repos[2].Path)+"\n")
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					if _, err := mi.TrackRepoCtx(t.Context(), entry); err != nil {
						t.Fatal(err)
					}
				}
				e := goModuleLifecycleCall(t, g)
				require.Equal(t, "external-call::dep::example.org/library", e.To)
				main := filepath.Join(repos[0].Path, "main.go")
				writeFile(t, main, "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello(); _ = 1 }\n")
				var err error
				switch mode {
				case "reindex":
					_, err = mi.IncrementalReindexRepo("a", []string{main})
				case "point":
					_, err = mi.incrementalPointRepoRaw("a", main)
				case "indexfile":
					err = mi.GetIndexer("a").IndexFile(main)
				}
				if err != nil {
					t.Fatal(err)
				}
				e = goModuleLifecycleCall(t, g)
				require.Equal(t, "external-call::dep::example.org/library", e.To)
			})
		}
	}
}

func TestGoModuleGoModSaveInvalidatesSnapshot(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"reindex", "point", "indexfile"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				mi.goModuleSnapshot() // Warm the cache immediately before the manifest save.
				manifest := filepath.Join(repos[1].Path, "go.mod")
				writeFile(t, manifest, "module example.org/renamed\n")
				var err error
				switch mode {
				case "reindex":
					_, err = mi.IncrementalReindexRepo("b", []string{manifest})
				case "point":
					_, err = mi.incrementalPointRepoRaw("b", manifest)
				default:
					err = mi.GetIndexer("b").IndexFile(manifest)
				}
				require.NoError(t, err)
				require.Equal(t, "external-call::dep::example.org/library", goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

func TestGoModuleBatchHonorsUntrackedReplace(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nreplace example.org/library => "+filepath.ToSlash(repos[2].Path)+"\n")
			mi.BeginBatch()
			for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.EndBatch()
			require.Contains(t, []string{"external-call::dep::example.org/library", "unresolved::extern::example.org/library::Hello"}, goModuleLifecycleCall(t, g).To)
		})
	}
}

type goModuleRetryScanStore struct {
	goModulePublishScanCountingStore
	definitions int
}

func (s *goModuleRetryScanStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.definitions++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func TestGoModuleRetrySkipsUnrelatedWorkspaceChanges(t *testing.T) {
	for _, hasModule := range []bool{false, true} {
		name := "non_go"
		if hasModule {
			name = "unrelated_config"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			lang, file := "javascript", "b/main.js"
			if hasModule {
				writeFile(t, filepath.Join(dir, "go.mod"), "module example.org/library\n")
				lang, file = "go", "b/lib.go"
			}
			cm := newTestConfigManager(t)
			cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: dir}}
			db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			g := &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}
			g.AddNode(&graph.Node{ID: file + "::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: file, Language: lang})
			mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			cr := mi.newCrossRepoResolver()
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			if !hasModule {
				assert.Zero(t, g.definitions, "a non-Go workspace must not seed a binding scan")
				assert.Zero(t, g.scans, "a non-Go workspace must not sweep terminals")
			} else {
				require.Positive(t, g.definitions)
				require.Equal(t, 1, g.scans)
			}
			_, generation := mi.goModuleSnapshot()
			revision := cm.Revision()
			cm.LoadWorkspaceConfig("b", dir)
			require.Greater(t, cm.Revision(), revision)
			_, after := mi.goModuleSnapshot()
			assert.Equal(t, generation, after, "unrelated config must not change module generation")
			g.scans, g.definitions = 0, 0
			cr = mi.newCrossRepoResolver()
			require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
			assert.Zero(t, g.scans, "unchanged module content must not sweep terminals")
			assert.Zero(t, g.definitions, "unchanged module content must not recheck bindings")
		})
	}
}

func TestGoModuleCrossWorkspaceRuleAdded(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, trigger := range []string{"global", "providersave"} {
			t.Run(backend+"/"+trigger, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				repos[0].Workspace, repos[1].Workspace = "w1", "w2"
				other := filepath.Join(repos[1].Path, "other.go")
				writeFile(t, other, "package library\nfunc Other() {}\n")
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				mi.RunGlobalResolve()
				require.Equal(t, "external-call::dep::example.org/library", goModuleLifecycleCall(t, g).To)
				writeFile(t, filepath.Join(repos[0].Path, ".gortex.yaml"), "workspace: w1\ncross_workspace_deps:\n  - workspace: w2\n    modules: [example.org/library]\n    mode: read-only\n")
				mi.RefreshRepoConfigs()
				if trigger == "global" {
					mi.RunGlobalResolve()
				} else {
					writeFile(t, other, "package library\nfunc Other() { println(1) }\n")
					_, err := mi.IncrementalReindexRepo("b", []string{other})
					require.NoError(t, err)
				}
				edge := goModuleLifecycleCall(t, g)
				assert.Equal(t, "b/lib.go::Hello", edge.To, "the new rule must bind the unchanged caller")
				assert.True(t, edge.CrossRepo)
			})
		}
	}
}

func TestGoModuleProviderRemovesSymbolFlags(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"point", "watcher"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				edge := goModuleLifecycleCall(t, g)
				require.Equal(t, "b/lib.go::Hello", edge.To)
				require.True(t, edge.CrossRepo)
				lib := filepath.Join(repos[1].Path, "lib.go")
				writeFile(t, lib, "package library\nfunc Other() {}\n")
				var err error
				if mode == "point" {
					err = mi.GetIndexer("b").IndexFile(lib)
				} else {
					_, err = mi.incrementalPointRepoRaw("b", lib)
				}
				require.NoError(t, err)
				edge = goModuleLifecycleCall(t, g)
				assert.Equal(t, "external-call::dep::example.org/library", edge.To)
				assert.False(t, edge.CrossRepo)
				assert.Equal(t, 0.5, edge.Confidence)
				assert.Equal(t, "INFERRED", edge.ConfidenceLabel)
				assert.Equal(t, graph.OriginTextMatched, edge.Origin)
				mi.RunGlobalResolve()
				edge = goModuleLifecycleCall(t, g)
				assert.Equal(t, "external-call::dep::example.org/library", edge.To)
				assert.False(t, edge.CrossRepo)
				assert.Equal(t, 0.5, edge.Confidence)
				assert.Equal(t, "INFERRED", edge.ConfidenceLabel)
				assert.Equal(t, graph.OriginTextMatched, edge.Origin)
			})
		}
	}
}

func TestGoModuleInCheckoutReplaceRemoved(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			caller := repos[0].Path
			manifest := filepath.Join(caller, "go.mod")
			writeFile(t, manifest, "module example.org/caller\nreplace example.org/library => ./third_party/library\n")
			require.NoError(t, os.MkdirAll(filepath.Join(caller, "third_party", "library"), 0o755))
			writeFile(t, filepath.Join(caller, "third_party", "library", "go.mod"), "module example.org/library\n")
			writeFile(t, filepath.Join(caller, "third_party", "library", "lib.go"), "package library\nfunc Hello() {}\n")
			for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.RunGlobalResolve()
			edge := goModuleLifecycleCall(t, g)
			require.Equal(t, "a/third_party/library/lib.go::Hello", edge.To)
			require.False(t, edge.CrossRepo)
			require.Equal(t, "example.org/library::Hello", edge.Meta["go_import"])
			writeFile(t, manifest, "module example.org/caller\n")
			_, err := mi.IncrementalReindexRepo("a", []string{manifest})
			require.NoError(t, err)
			mi.RunGlobalResolve()
			edge = goModuleLifecycleCall(t, g)
			assert.Equal(t, "b/lib.go::Hello", edge.To)
			assert.True(t, edge.CrossRepo)
			// A cold index of the same state must choose the same provider.
			var coldGraph graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				coldGraph = db
			}
			cold := NewMultiIndexer(coldGraph, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
			t.Cleanup(func() { require.NoError(t, cold.Close(context.Background())) })
			for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
				_, err := cold.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			cold.RunGlobalResolve()
			assert.Equal(t, goModuleLifecycleCall(t, coldGraph).To, edge.To)
		})
	}
}
