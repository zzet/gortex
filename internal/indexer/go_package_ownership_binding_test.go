package indexer

import (
	"context"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

func TestGoPackageOwnershipIndexerAndMasterObserveSourceRebind(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "go-ownership-binding")
	builderGit(t, root, "init", "--initial-branch=main")
	builderWriteTree(t, root, map[string]string{
		"go.mod":        "module example.test/first\n\ngo 1.23\n",
		"pkg/a.go":      "package pkg\nfunc Use() {}\n",
		"misc/pkg/b.go": "package pkg\nfunc Use() {}\n",
		"main.go":       "package first\nimport \"example.test/first/pkg\"\nfunc Call() { pkg.Use() }\n",
	})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "first target")
	firstTree := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	builderWriteFile(t, root, "go.mod", "module example.test/second\n\ngo 1.23\n")
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "second target")
	secondTree := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	first, err := source.NewGitTreeSource(ctx, root, firstTree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := source.NewGitTreeSource(ctx, root, secondTree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	idx := &Indexer{repoPrefix: builderRepoPrefix, rootPath: root}
	mi := &MultiIndexer{indexers: map[string]*Indexer{builderRepoPrefix: idx}}
	file := &graph.Node{ID: builderRepoPrefix + "/pkg/a.go", Kind: graph.KindFile, Language: "go", FilePath: builderRepoPrefix + "/pkg/a.go", RepoPrefix: builderRepoPrefix}
	seq := func(yield func(*graph.Node) bool) { yield(file) }
	query := func(module string) resolver.GoImportCandidate {
		return resolver.GoImportCandidate{ImportPath: module + "/pkg", ImporterRepoPrefix: builderRepoPrefix,
			ImporterFilePath: file.FilePath, CandidateRepoPrefix: builderRepoPrefix, CandidateID: file.ID, CandidateFilePath: file.FilePath}
	}
	check := func(label, module string, want resolver.GoPackageOwnershipResult) {
		t.Helper()
		for _, method := range []struct {
			name    string
			prepare resolver.GoPackageOwnershipFactory
		}{
			{"indexer", idx.prepareGoPackageOwnership}, {"master", mi.prepareGoPackageOwnership},
		} {
			lookups, err := method.prepare(ctx, []string{builderRepoPrefix}, seq)
			if err != nil {
				t.Fatal(err)
			}
			got := resolver.GoPackageOwnershipUnknown
			if lookup := lookups[builderRepoPrefix]; lookup != nil {
				got = lookup(query(module))
			}
			if got != want {
				t.Errorf("%s/%s got=%v want=%v", label, method.name, got, want)
			}
		}
	}
	idx.SetContentSource(first)
	check("first target despite dirty second manifest", "example.test/first", resolver.GoPackageOwnershipExact)
	check("first target rejects working-tree module", "example.test/second", resolver.GoPackageOwnershipDifferent)
	idx.SetContentSource(second)
	check("target rebind", "example.test/second", resolver.GoPackageOwnershipExact)
	check("old target not retained", "example.test/first", resolver.GoPackageOwnershipDifferent)
	narrowed := newFileSetSource(first, []string{"pkg/a.go"})
	idx.setContentSourceWithManifests(narrowed, first)
	check("explicit full authority survives parse narrowing", "example.test/first", resolver.GoPackageOwnershipExact)
	idx.SetContentSource(narrowed)
	check("ordinary setter clears stale full authority", "example.test/first", resolver.GoPackageOwnershipUnknown)
	check("unknown explicit source never uses working tree", "example.test/second", resolver.GoPackageOwnershipUnknown)
	idx.SetContentSource(nil)
	if runtime.GOOS == "windows" {
		// The existing safe FilesystemSource regular-reader implementation is
		// intentionally unsupported on Windows. Git snapshot coverage above is
		// still mandatory; fallback must remain Unknown, never stale Git facts.
		check("filesystem unsupported is conservative", "example.test/second", resolver.GoPackageOwnershipUnknown)
	} else {
		check("nil source returns to current filesystem", "example.test/second", resolver.GoPackageOwnershipExact)
	}
	fs, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	idx.SetContentSource(fs)
	if runtime.GOOS != "windows" {
		check("same source initial manifest", "example.test/second", resolver.GoPackageOwnershipExact)
		builderWriteFile(t, root, "go.mod", "module example.test/third\n\ngo 1.23\n")
		check("same source manifest update", "example.test/third", resolver.GoPackageOwnershipExact)
		check("same source old module invalidated", "example.test/second", resolver.GoPackageOwnershipDifferent)
	}
	// Exercise the actual master constructor and pass, not only its factory
	// method. The full Git target stays first while the working tree is third.
	idx.SetContentSource(first)
	g := graph.New()
	mainFile := &graph.Node{ID: builderRepoPrefix + "/main.go", Kind: graph.KindFile, FilePath: builderRepoPrefix + "/main.go", RepoPrefix: builderRepoPrefix, Language: "go"}
	caller := &graph.Node{ID: mainFile.ID + "::Call", Name: "Call", Kind: graph.KindFunction, FilePath: mainFile.FilePath, RepoPrefix: builderRepoPrefix, Language: "go"}
	wrongFile := &graph.Node{ID: builderRepoPrefix + "/misc/pkg/b.go", Kind: graph.KindFile, FilePath: builderRepoPrefix + "/misc/pkg/b.go", RepoPrefix: builderRepoPrefix, Language: "go"}
	wrongDef := &graph.Node{ID: wrongFile.ID + "::Use", Name: "Use", Kind: graph.KindFunction, FilePath: wrongFile.FilePath, RepoPrefix: builderRepoPrefix, Language: "go"}
	definition := &graph.Node{ID: file.ID + "::Use", Name: "Use", Kind: graph.KindFunction, FilePath: file.FilePath, RepoPrefix: builderRepoPrefix, Language: "go"}
	g.AddBatch([]*graph.Node{mainFile, caller, wrongFile, wrongDef, file, definition}, []*graph.Edge{
		{From: caller.ID, To: "unresolved::extern::example.test/first/pkg::Use", Kind: graph.EdgeCalls, FilePath: mainFile.FilePath},
		{From: mainFile.ID, To: "unresolved::import::example.test/first/pkg", Kind: graph.EdgeImports, FilePath: mainFile.FilePath},
	})
	mi.graph = g
	mi.logger = zap.NewNop()
	computed := false
	if err := mi.runMasterResolveHookedContext(ctx, nil, false, func() { computed = true }); err != nil {
		t.Fatal(err)
	}
	if !computed {
		t.Fatal("actual master lifecycle did not complete its compute callback")
	}
	for _, item := range []struct {
		from, to string
		kind     graph.EdgeKind
	}{
		{caller.ID, definition.ID, graph.EdgeCalls}, {mainFile.ID, file.ID, graph.EdgeImports},
	} {
		count := 0
		for _, edge := range g.GetOutEdges(item.from) {
			if edge.Kind == item.kind {
				count++
				if edge.To != item.to {
					t.Errorf("actual master target=%s want=%s", edge.To, item.to)
				}
			}
		}
		if count != 1 {
			t.Errorf("actual master matching edge count=%d", count)
		}
	}
}

func TestGoPackageOwnershipMissingOwnerAndCancellationDoNotEnumerateGraph(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	idx := &Indexer{repoPrefix: "repo"}
	files := func(func(*graph.Node) bool) { t.Fatal("enumerated graph after cancellation") }
	if lookups, err := idx.prepareGoPackageOwnership(ctx, []string{"repo"}, files); err == nil || lookups != nil {
		t.Fatalf("cancellation lost: lookups=%v err=%v", lookups, err)
	}
	mi := &MultiIndexer{}
	if lookups, err := mi.prepareGoPackageOwnership(context.Background(), []string{"missing"}, files); err != nil || lookups != nil {
		t.Fatalf("uninstalled repo gained authority: lookups=%v err=%v", lookups, err)
	}
}
