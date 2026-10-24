package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func moduleRootBuilderTree() map[string]string {
	return map[string]string{
		"go.mod":      "module example.org/app\n\ngo 1.24\n",
		"root.go":     "package app\nfunc Fn() int { return 7 }\n",
		"cmd/main.go": "package main\nfunc Caller() int { return 0 }\n",
	}
}

const moduleRootBuilderCaller = "package main\nimport \"example.org/app\"\nfunc Caller() int { return app.Fn() }\n"

func assertModuleRootBuilderCall(t *testing.T, g graph.Reader) {
	t.Helper()
	var calls []*graph.Edge
	for _, e := range g.GetOutEdges(builderRepoPrefix + "/cmd/main.go::Caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	require.Equal(t, builderRepoPrefix+"/root.go::Fn", calls[0].To, "module-root self import")
}

func TestGoModuleRootSparseBuilder(t *testing.T) {
	treeA := moduleRootBuilderTree()
	treeB := moduleRootBuilderTree()
	treeB["cmd/main.go"] = moduleRootBuilderCaller
	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "root.go", "go.mod")
	assertModuleRootBuilderCall(t, c.composed)
}

func TestGoModuleRootEditDeltaBuilder(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, moduleRootBuilderTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	c.compaction.quiet = -1
	coordinatorReconcile(t, c)
	writeFile(t, filepath.Join(f.worktree, "cmd/main.go"), moduleRootBuilderCaller)
	recordLastEditDelta(nil)
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt || LastEditDeltaReport() == nil {
		t.Fatalf("edit did not use delta builder: %+v", out)
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	assertModuleRootBuilderCall(t, view.Reader)
	// An unchanged reconcile must reuse the same binding.
	coordinatorReconcile(t, c)
	again := chainMaterialize(t, f)
	defer again.Close()
	assertModuleRootBuilderCall(t, again.Reader)
}

func TestGoModuleSaveAndRestartKeepOwnership(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	caller, provider := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/app\n")
	writeFile(t, filepath.Join(caller, "main.go"), "package app\nimport \"example.org/library\"\nfunc Caller() { library.Hello() }\n")
	require.NoError(t, os.MkdirAll(filepath.Join(caller, "wrong", "library"), 0700))
	writeFile(t, filepath.Join(caller, "wrong", "library", "lib.go"), "package library\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(provider, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(provider, "lib.go"), "package library\nfunc Other() {}\n")
	filename := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(filename)
	require.NoError(t, err)
	manager := newTestConfigManager(t)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())); require.NoError(t, store.Close()) })
	repos := []config.RepoEntry{{Name: "b", Path: provider, Workspace: "shared"}, {Name: "a", Path: caller, Workspace: "shared"}}
	for _, entry := range repos {
		_, err = mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	const terminal = "external-call::dep::example.org/library"
	require.Equal(t, terminal, goModuleLifecycleCall(t, store).To)
	writeFile(t, filepath.Join(caller, "main.go"), "package app\nimport \"example.org/library\"\nfunc Caller() { library.Hello(); _ = 1 }\n")
	_, err = mi.IncrementalReindexRepo("a", []string{filepath.Join(caller, "main.go")})
	require.NoError(t, err)
	require.Equal(t, terminal, goModuleLifecycleCall(t, store).To)
	prior := map[string]map[string]int64{"a": mi.FileMtimes("a"), "b": mi.FileMtimes("b")}
	require.NoError(t, mi.Close(context.Background()))
	require.NoError(t, store.Close())
	store, err = store_sqlite.Open(filename)
	require.NoError(t, err)
	mi = NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	for _, entry := range repos {
		_, err = mi.ReconcileRepoCtx(t.Context(), entry, prior[entry.Name])
		require.NoError(t, err)
	}
	mi.RunGlobalResolve()
	require.Equal(t, terminal, goModuleLifecycleCall(t, store).To, "restart must not undo the ownership decision")
}

func TestGoModuleBuilderUsesSnapshotIdentity(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module example.org/dirty\n")
	writeFile(t, filepath.Join(snapshot, "go.mod"), "module example.org/app\n")
	writeFile(t, filepath.Join(snapshot, "root.go"), "package app\nfunc Fn() {}\n")
	g := graph.New()
	caller := &graph.Node{ID: "app/cmd/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "app", FilePath: "app/cmd/main.go", Language: "go"}
	target := &graph.Node{ID: "app/root.go::Fn", Name: "Fn", Kind: graph.KindFunction, RepoPrefix: "app", FilePath: "app/root.go", Language: "go"}
	e := &graph.Edge{From: caller.ID, To: "unresolved::extern::example.org/app::Fn", Kind: graph.EdgeCalls, FilePath: caller.FilePath}
	g.AddBatch([]*graph.Node{caller, target}, []*graph.Edge{e})
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	idx.storeRootPath(checkout)
	idx.SetRepoPrefix("app")
	src := snapshotOf(t, snapshot)
	idx.setContentSourceWithManifests(newFileSetSource(src, []string{"root.go"}), src)
	idx.resolver.ResolveAll()
	require.Equal(t, target.ID, callTargetFrom(t, g, caller.ID), "the full snapshot module, not the dirty checkout or narrowed parse source, owns the binding")
}
