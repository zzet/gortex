package indexer

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"slices"
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

func goModuleLifecycleFixture(t *testing.T, backend string) (*MultiIndexer, graph.Store, []config.RepoEntry) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	caller, library, fork := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/caller\n")
	writeFile(t, filepath.Join(caller, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello() }\n")
	for _, root := range []string{library, fork} {
		writeFile(t, filepath.Join(root, "go.mod"), "module example.org/library\n")
		writeFile(t, filepath.Join(root, "lib.go"), "package library\nfunc Hello() {}\n")
	}
	var g graph.Store = graph.New()
	if backend == "sqlite" {
		s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		g = s
	}
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	return mi, g, []config.RepoEntry{{Path: caller, Name: "a", Workspace: "shared"}, {Path: library, Name: "b", Workspace: "shared"}, {Path: fork, Name: "fork", Workspace: "shared"}}
}

func goModuleLifecycleCall(t *testing.T, g graph.Store) *graph.Edge {
	t.Helper()
	var calls []*graph.Edge
	for _, e := range g.GetOutEdges("a/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1, "the unchanged caller's call must survive provider eviction")
	return calls[0]
}

func TestGoModuleProviderLifecycle(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"point", "watcher", "indexrepo", "reconcile", "untrack"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				// A same-named foreign definition must not attract a restubbed bare name.
				decoy := t.TempDir()
				writeFile(t, filepath.Join(decoy, "lib.go"), "package decoy\nfunc Hello() {}\n")
				_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: decoy, Name: "decoy", Workspace: "shared"})
				require.NoError(t, err)
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Hello() { println(1) }\n")
				switch mode {
				case "point":
					err = mi.GetIndexer("b").IndexFile(filepath.Join(repos[1].Path, "lib.go"))
				case "watcher":
					_, err = mi.incrementalPointRepoRaw("b", filepath.Join(repos[1].Path, "lib.go"))
				case "indexrepo":
					_, err = mi.IndexRepo("b")
				case "reconcile":
					t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
					prior := mi.FileMtimes("b")
					restored := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), mi.configMgr, zap.NewNop())
					t.Cleanup(func() { require.NoError(t, restored.Close(context.Background())) })
					var result *IndexResult
					result, err = restored.ReconcileRepoCtx(t.Context(), repos[1], prior)
					require.NoError(t, err)
					require.True(t, result.FullRetrack)
				case "untrack":
					_, _, err = mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
					edge := goModuleLifecycleCall(t, g)
					assert.NotEqual(t, "b/lib.go::Hello", edge.To)
					assert.Zero(t, edge.Confidence)
					_, err = mi.TrackRepoCtx(t.Context(), repos[1])
				}
				require.NoError(t, err)
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To, "provider mutation must settle without a global pass")
				mi.RunGlobalResolve()
				edge := goModuleLifecycleCall(t, g)
				assert.Equal(t, "b/lib.go::Hello", edge.To)
				assert.Equal(t, 0.6, edge.Confidence)
				assert.Equal(t, graph.OriginASTInferred, edge.Origin)
			})
		}
	}
}

func TestGoModuleMapRechecksBoundCalls(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"fork_first", "fork_last", "rename", "remove", "rename_restart", "remove_all_restart"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				order := []config.RepoEntry{repos[1], repos[0]}
				if mode == "fork_first" {
					order = []config.RepoEntry{repos[1], repos[2], repos[0]}
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode != "fork_first" {
					resolver.DetectCrossRepoEdges(g)
					require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				}
				switch mode {
				case "fork_last":
					_, err := mi.TrackRepoCtx(t.Context(), repos[2])
					require.NoError(t, err)
				case "rename", "rename_restart":
					writeFile(t, filepath.Join(repos[1].Path, "go.mod"), "module example.org/renamed\n")
				case "remove", "remove_all_restart":
					require.NoError(t, os.Remove(filepath.Join(repos[1].Path, "go.mod")))
					if mode == "remove_all_restart" {
						require.NoError(t, os.Remove(filepath.Join(repos[0].Path, "go.mod")))
					}
				}
				if mode == "rename_restart" || mode == "remove_all_restart" {
					mi.goModuleRetry = resolver.GoModuleDepRetry{}
				}
				mi.goModuleChecked = time.Time{}
				mi.RunGlobalResolve()
				edge := goModuleLifecycleCall(t, g)
				assert.NotEqual(t, "b/lib.go::Hello", edge.To)
				assert.NotEqual(t, "fork/lib.go::Hello", edge.To)
				for _, derived := range g.GetOutEdges("a/main.go::Caller") {
					if derived.Kind == graph.EdgeCrossRepoCalls {
						assert.NotEqual(t, "b/lib.go::Hello", derived.To)
					}
				}
				assert.NotEqual(t, graph.OriginASTInferred, edge.Origin)
			})
		}
	}
}

func TestGoModuleDeferredTerminalParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"missing", "variable", "workspace_denied", "disabled"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				if mode == "variable" {
					writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nvar Hello = func() {}\n")
				}
				if mode == "missing" || mode == "disabled" {
					writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
				}
				if mode == "workspace_denied" {
					repos[1].Workspace = "other"
				}
				if mode == "disabled" {
					writeFile(t, filepath.Join(repos[0].Path, ".gortex.yaml"), "index:\n  synthesize_external_calls: false\n")
				}
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				edge := goModuleLifecycleCall(t, g)
				want := "external-call::dep::example.org/library"
				if mode == "disabled" {
					want = "dep::example.org/library::Hello"
				}
				assert.Equal(t, want, edge.To)
			})
		}
	}
}

func TestGoModuleUntrackedReplacement(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"absolute", "relative", "versioned", "edit"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				target := repos[2].Path // fork exists but is not tracked
				if mode == "relative" {
					var err error
					target, err = filepath.Rel(repos[0].Path, target)
					require.NoError(t, err)
				}
				module := "example.org/library"
				if mode == "versioned" {
					module += " v1.0.0"
				}
				replaced := "module example.org/caller\nrequire example.org/library v1.0.0\nreplace " + module + " => " + filepath.ToSlash(target) + "\n"
				if mode != "edit" {
					writeFile(t, filepath.Join(repos[0].Path, "go.mod"), replaced)
				}
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode == "edit" {
					require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
					writeFile(t, filepath.Join(repos[0].Path, "go.mod"), replaced)
					mi.goModuleChecked = time.Time{}
				}
				mi.RunGlobalResolve()
				edge := goModuleLifecycleCall(t, g)
				assert.NotEqual(t, "b/lib.go::Hello", edge.To)
				assert.NotEqual(t, graph.OriginASTInferred, edge.Origin)
			})
		}
	}
}

func TestGoModuleSameRepoIncremental(t *testing.T) {
	for _, mode := range []string{"point", "watcher"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "go.mod"), "module example.org/local\n")
			require.NoError(t, os.Mkdir(filepath.Join(dir, "pkg"), 0o755))
			provider := filepath.Join(dir, "pkg", "lib.go")
			writeFile(t, provider, "package pkg\nfunc Hello() {}\n")
			writeFile(t, filepath.Join(dir, "main.go"), "package main\nimport \"example.org/local/pkg\"\nfunc Caller() { pkg.Hello() }\n")
			g := graph.New()
			idx := newTestIndexer(g)
			idx.SetRepoPrefix("local")
			t.Cleanup(idx.Close)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			calls := func() []*graph.Edge {
				var result []*graph.Edge
				for _, e := range g.GetOutEdges("local/main.go::Caller") {
					if e.Kind == graph.EdgeCalls {
						result = append(result, e)
					}
				}
				return result
			}
			require.Len(t, calls(), 1)
			require.Equal(t, "local/pkg/lib.go::Hello", calls()[0].To)
			require.False(t, calls()[0].CrossRepo)
			writeFile(t, provider, "package pkg\nfunc Hello() { println(1) }\n")
			if mode == "point" {
				err = idx.IndexFile(provider)
			} else {
				_, err = idx.incrementalWatcherPaths(dir, []string{provider}, incrementalPathMode{forceExplicitFiles: true})
			}
			require.NoError(t, err)
			require.Len(t, calls(), 1)
			// #806 preserves an unchanged local definition through a body save.
			assert.Equal(t, "local/pkg/lib.go::Hello", calls()[0].To)
			assert.False(t, calls()[0].CrossRepo)
			assert.Zero(t, calls()[0].Confidence)
		})
	}
}

type goModuleEvictionCountingStore struct {
	graph.Store
	definitionScans int
}

func (s *goModuleEvictionCountingStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	// Count only the Go module restub's definition scan. The npm restub
	// also scans an evicted repository, with a wider kind list that
	// includes files, and it is expected to run for a JavaScript repo.
	if slices.Equal(kinds, []graph.NodeKind{graph.KindFunction, graph.KindType, graph.KindInterface}) {
		s.definitionScans++
	}
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func (s *goModuleEvictionCountingStore) PurgeRepo(repo string) error {
	_, _, err := evictRepoAllGenerations(s.Store, repo)
	return err
}

func TestNonGoRepoEvictionSkipsModuleRestub(t *testing.T) {
	g := &goModuleEvictionCountingStore{Store: graph.New()}
	g.AddNode(&graph.Node{ID: "js/main.js::Hello", RepoPrefix: "js", FilePath: "js/main.js", Kind: graph.KindFunction, Language: "javascript", Name: "Hello"})
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, _, err := mi.purgeRepositoryPayload("js", &RepoMetadata{RootPath: t.TempDir()})
	require.NoError(t, err)
	assert.Zero(t, g.definitionScans, "non-Go eviction must not run the module incoming-edge scan")
	assert.Nil(t, g.GetNode("js/main.js::Hello"))
}

func TestGoModuleSymlinkReplacement(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			alias := filepath.Join(t.TempDir(), "library")
			require.NoError(t, os.Symlink(repos[1].Path, alias))
			writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nreplace example.org/library => "+filepath.ToSlash(alias)+"\n")
			for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			assert.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
		})
	}
}

func TestGoModuleInCheckoutReplacement(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"staging", "monorepo", "foreign", "foreign_subpath", "missing"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				module, imported, target := "k8s.io/kubernetes", "k8s.io/api/v1", "staging/src/k8s.io/api/v1"
				replacement, moduleRoot := "./staging/src/k8s.io/api", "k8s.io/api"
				if mode == "monorepo" {
					module, imported, target = "example.org/mono", "example.org/mono/lib", "lib"
					replacement, moduleRoot = "./lib", imported
				}
				if mode == "foreign" || mode == "foreign_subpath" || mode == "missing" {
					module, imported, target = "example.org/caller", "example.org/library", "third_party/library"
					replacement, moduleRoot = "./third_party/library", imported
					if mode == "foreign_subpath" {
						imported += "/sub"
						target += "/sub"
						require.NoError(t, os.Mkdir(filepath.Join(repos[1].Path, "sub"), 0o755))
						writeFile(t, filepath.Join(repos[1].Path, "sub", "lib.go"), "package pkg\nfunc Hello() {}\n")
					}
				}
				writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module "+module+"\nreplace "+moduleRoot+" => "+replacement+"\n")
				writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport pkg \""+imported+"\"\nfunc Caller() { pkg.Hello() }\n")
				require.NoError(t, os.MkdirAll(filepath.Join(repos[0].Path, target), 0o755))
				writeFile(t, filepath.Join(repos[0].Path, filepath.FromSlash(replacement), "go.mod"), "module "+moduleRoot+"\n")
				declaration := "package pkg\nfunc Hello() {}\n"
				if mode == "missing" {
					declaration = "package pkg\nfunc Other() {}\n"
				}
				writeFile(t, filepath.Join(repos[0].Path, target, "lib.go"), declaration)
				if mode != "missing" {
					decoy := "aaa/" + filepath.Base(target)
					require.NoError(t, os.MkdirAll(filepath.Join(repos[0].Path, decoy), 0o755))
					writeFile(t, filepath.Join(repos[0].Path, decoy, "lib.go"), "package pkg\nfunc Hello() {}\n")
				}
				// The daemon's configured roots include the caller before its cold parse.
				require.NoError(t, mi.configMgr.Global().AddRepo(repos[0]))
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				want := "a/" + target + "/lib.go::Hello"
				if mode == "missing" {
					want = "external-call::dep::example.org/library"
				}
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
				mi.RunGlobalResolve()
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

func TestGoModuleMissingProviderFiles(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		modes := []string{"deleted_checkout", "removed_module_reindex"}
		if backend == "sqlite" {
			modes = append(modes, "removed_module_restart")
		}
		for _, mode := range modes {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				prior := mi.FileMtimes("b")
				if mode == "deleted_checkout" {
					require.NoError(t, os.RemoveAll(repos[1].Path))
				} else {
					require.NoError(t, os.Remove(filepath.Join(repos[1].Path, "go.mod")))
				}
				// Do not let the one-second module cache hide the missing disk identity.
				mi.goModuleChecked = time.Time{}
				_ = mi.trackedGoModules()
				switch mode {
				case "deleted_checkout":
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
				case "removed_module_reindex":
					_, err := mi.IndexRepo("b")
					require.NoError(t, err)
				case "removed_module_restart":
					t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
					restored := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), mi.configMgr, zap.NewNop())
					t.Cleanup(func() { require.NoError(t, restored.Close(context.Background())) })
					result, err := restored.ReconcileRepoCtx(t.Context(), repos[1], prior)
					require.NoError(t, err)
					require.True(t, result.FullRetrack)
				}
				edge := goModuleLifecycleCall(t, g)
				assert.NotEqual(t, "b/lib.go::Hello", edge.To)
				assert.Contains(t, edge.To, "example.org/library")
				assert.Equal(t, "example.org/library::Hello", edge.Meta["go_import"])
			})
		}
	}
}

func TestGoModuleDemotionTerminalParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"rename", "remove", "fork", "replace", "disabled", "unchanged"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				if mode == "disabled" {
					writeFile(t, filepath.Join(repos[0].Path, ".gortex.yaml"), "index:\n  synthesize_external_calls: false\n")
				}
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				switch mode {
				case "unchanged":
					// A repeated frontier without module edits must retain the binding.
				case "remove":
					require.NoError(t, os.Remove(filepath.Join(repos[1].Path, "go.mod")))
				case "fork":
					_, err := mi.TrackRepoCtx(t.Context(), repos[2])
					require.NoError(t, err)
				case "replace":
					writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nreplace example.org/library => "+filepath.ToSlash(repos[2].Path)+"\n")
				default:
					writeFile(t, filepath.Join(repos[1].Path, "go.mod"), "module example.org/renamed\n")
				}
				mi.goModuleChecked = time.Time{}
				// Only the provider is in the frontier. The caller file is unchanged.
				mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
				want := "external-call::dep::example.org/library"
				switch mode {
				case "disabled":
					want = "dep::example.org/library::Hello"
				case "unchanged":
					want = "b/lib.go::Hello"
				}
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

func TestGoModuleReplacementModulePath(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"different_path", "same_path_version"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				target := "example.org/otherfork"
				want := "external-call::dep::example.org/library"
				if mode == "same_path_version" {
					target = "example.org/library"
					want = "b/lib.go::Hello"
				}
				writeFile(t, filepath.Join(repos[0].Path, "go.mod"), "module example.org/caller\nrequire example.org/library v1.0.0\nreplace example.org/library => "+target+" v1.2.0\n")
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
				mi.RunGlobalResolve()
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

func TestGoModuleProviderLossTerminalParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"untrack", "removed_symbol_reindex", "unchanged"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
				switch mode {
				case "untrack":
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
				case "removed_symbol_reindex":
					writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
					_, err := mi.IndexRepo("b")
					require.NoError(t, err)
				}
				// Eviction can park a qualified extern until the next global pass.
				mi.RunGlobalResolve()
				want := "external-call::dep::example.org/library"
				if mode == "unchanged" {
					want = "b/lib.go::Hello"
				}
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
				mi.RunGlobalResolve()
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}
