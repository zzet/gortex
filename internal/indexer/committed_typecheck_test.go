package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// committedTreeA and committedTreeB are two commits of a small module. The
// commit layer from A to B carries p/a.go and p/c.go, both changed.
func committedTreeA() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"p/a.go": "package p\n\nimport \"fmt\"\n\nfunc A() error { return fmt.Errorf(\"a\") }\n",
		"p/c.go": "package p\n\nimport \"fmt\"\n\nfunc C() error { return fmt.Errorf(\"c\") }\n",
		"q/q.go": "package q\n\nimport \"example.com/m/p\"\n\nfunc Q() error { return p.C() }\n",
	}
}

func committedTreeB() map[string]string {
	tree := committedTreeA()
	tree["p/a.go"] = "package p\n\nimport \"fmt\"\n\nfunc A() error { _ = C(); return fmt.Errorf(\"a2\") }\n"
	tree["p/c.go"] = "package p\n\nimport \"fmt\"\n\nfunc C() error { return fmt.Errorf(\"c2\") }\n"
	return tree
}

// committedWorkingCopy leaves the checkout's working copy away from its
// committed tree in every way the overlay handles: p/a.go modified (it no
// longer calls fmt.Errorf), p/c.go deleted, an untracked p/b.go whose
// package clause would break package p if the load saw it, an untracked
// package made only of untracked files, and a file added to the index only.
func committedWorkingCopy(t *testing.T, dir string) {
	t.Helper()
	builderWriteFile(t, dir, "p/a.go", "package p\n\nfunc A() error { return B() }\n")
	if err := os.Remove(filepath.Join(dir, "p", "c.go")); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, dir, "p/b.go", "package other\n\nfunc B() error { return nil }\n")
	committedWriteNew(t, dir, "d/d.go", "package d\n\nfunc D() {}\n")
	builderWriteFile(t, dir, "p/e.go", "package p\n\nfunc E() {}\n")
	builderGit(t, dir, "add", "p/e.go")
	// A workspace file the tree does not hold, naming a module that does not
	// exist: a load that read it would fail.
	builderWriteFile(t, dir, "go.work", "go 1.22\n\nuse (\n\t.\n\t./missing\n)\n")
}

// committedWriteNew writes a file, creating its directory; builderWriteTree
// would replace the whole tree.
func committedWriteNew(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, dir, name, body)
}

func TestCommittedOverlayReadsTheRootAsTheTree(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "committed-overlay")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, committedTreeB())
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-q", "-m", "B")
	tree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")
	committedWorkingCopy(t, dir)
	// A module manifest in a directory the tree has no Go files under
	// changes nothing the load reads.
	committedWriteNew(t, dir, "tool-cache/go.mod", "module cache\n")
	committedWriteNew(t, dir, "tool-cache/x.go", "package cache\n")

	overlay, err := buildCommittedOverlay(context.Background(), dir, tree)
	if err != nil {
		t.Fatalf("buildCommittedOverlay: %v", err)
	}
	if overlay.refusal != "" {
		t.Fatalf("refused: %s", overlay.refusal)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"p/a.go":          committedTreeB()["p/a.go"],
		"p/c.go":          committedTreeB()["p/c.go"],
		"p/b.go":          string(hiddenGoFile),
		"d/d.go":          string(hiddenGoFile),
		"p/e.go":          string(hiddenGoFile),
		"tool-cache/x.go": string(hiddenGoFile),
	}
	if len(overlay.files) != len(want) {
		t.Errorf("overlay holds %d files, want %d: %v", len(overlay.files), len(want), overlayKeys(overlay.files))
	}
	for rel, content := range want {
		got, ok := overlay.files[filepath.Join(abs, filepath.FromSlash(rel))]
		if !ok {
			t.Errorf("overlay has no %s", rel)
			continue
		}
		if string(got) != content {
			t.Errorf("%s reads %q, want %q", rel, got, content)
		}
	}
	if overlay.restored != 2 || overlay.hidden != 4 {
		t.Errorf("restored %d hidden %d, want 2 and 4", overlay.restored, overlay.hidden)
	}
	if !overlay.goWorkOff {
		t.Error("the tree holds no go.work, but the loads would read the working copy's")
	}

	// A module manifest over Go files the tree holds moves them into another
	// module, which no overlay can undo: refused.
	builderWriteFile(t, dir, "q/go.mod", "module other\n")
	overlay, err = buildCommittedOverlay(context.Background(), dir, tree)
	if err != nil {
		t.Fatalf("buildCommittedOverlay: %v", err)
	}
	if !strings.Contains(overlay.refusal, "q/go.mod") {
		t.Errorf("refusal %q, want one naming q/go.mod", overlay.refusal)
	}
}

func overlayKeys(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for k := range files {
		out = append(out, k)
	}
	return out
}

// TestCommitLayerTypeChecksTheCommittedTree builds a commit layer whose
// checkout's working copy has a modified, a deleted and an untracked file,
// and checks that the type checker read the committed tree: the deleted
// file's and the modified file's committed calls are at the type checker's
// level, and the untracked file, whose package clause would break the
// package, is not seen.
// buildCommittedFixtureLayer builds the commit layer from committedTreeA to
// committedTreeB over a checkout whose working copy committedWorkingCopy
// changed, after extra has changed it further.
func buildCommittedFixtureLayer(t *testing.T, extra func(dir string)) (*store_sqlite.Store, int64, BuildReport) {
	t.Helper()
	builderIsolateGit(t)
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
	// The go command's own default: a workspace file found from the load's
	// directory upwards is read, unless the stage turns it off.
	t.Setenv("GOWORK", "")
	repoDir := builderTempDir(t, "committed-repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, committedTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	dirA := builderTempDir(t, "committed-a")
	builderWriteTree(t, dirA, committedTreeA())
	builderWriteTree(t, repoDir, committedTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")
	committedWorkingCopy(t, repoDir)
	if extra != nil {
		extra(repoDir)
	}

	store := builderOpenStore(t, "committed-base")
	builderIndex(t, store, dirA) // the base: no type checker
	builder := builderNewBuilder(store)
	builder.Semantic = goTypesManager(t)
	generationID, report, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-fixture",
			LayerID:             "layer-" + treeB,
			CheckoutID:          "checkout-fixture",
			ProvenanceCommitOID: commitB,
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   treeA,
		TargetTreeOID: treeB,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	return store, generationID, report
}

func TestCommitLayerTypeChecksTheCommittedTree(t *testing.T) {
	store, generationID, report := buildCommittedFixtureLayer(t, nil)
	stage := report.CommittedTypecheck
	t.Logf("stage: %+v", stage)
	if !stage.Requested || !stage.Ran {
		t.Fatalf("the committed tree's stage did not run: %+v", stage)
	}
	if stage.Restored != 2 || stage.Hidden != 3 {
		t.Errorf("restored %d hidden %d, want 2 and 3", stage.Restored, stage.Hidden)
	}
	view := dirtyChainComposed(t, store, []int64{generationID})
	calls := func(file, fn string) string {
		var out []string
		for _, e := range view.GetOutEdges(builderGraphPath(builderRepoPrefix, file) + "::" + fn) {
			if e.Kind == "calls" {
				out = append(out, e.To)
			}
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ file, fn string }{{"p/a.go", "A"}, {"p/c.go", "C"}} {
		got := calls(tc.file, tc.fn)
		t.Logf("%s %s calls: %s", tc.file, tc.fn, got)
		if !strings.Contains(got, "ext::go:fmt") {
			t.Errorf("%s %s is not at the type checker's level: %q", tc.file, tc.fn, got)
		}
	}
	states, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	var semanticRow *store_sqlite.ProducerCompleteness
	for i := range states {
		if states[i].Producer == string(graphview.CapSemantic) {
			semanticRow = &states[i]
		}
	}
	if semanticRow == nil || semanticRow.State != store_sqlite.ProducerStateComplete {
		t.Errorf("graph.semantic row %+v, want complete", semanticRow)
	}
}

// A stage the working copy refuses (a module manifest the tree does not hold,
// over Go files the tree holds) runs nothing, and the layer declares
// graph.semantic incomplete with the reason.
func TestCommitLayerDeclaresARefusedStageIncomplete(t *testing.T) {
	store, generationID, report := buildCommittedFixtureLayer(t, func(dir string) {
		committedWriteNew(t, dir, "q/go.mod", "module other\n")
	})
	if report.CommittedTypecheck.Ran {
		t.Fatalf("the stage ran over a working copy that moves q into another module: %+v", report.CommittedTypecheck)
	}
	states, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	for _, row := range states {
		if row.Producer != string(graphview.CapSemantic) {
			continue
		}
		if row.State != store_sqlite.ProducerStateIncomplete || !strings.Contains(row.Reason, "q/go.mod") {
			t.Fatalf("graph.semantic row %+v, want incomplete naming q/go.mod", row)
		}
		return
	}
	t.Fatal("the layer declares no graph.semantic row")
}

// A commit layer that carries no Go file has nothing for the type checker to
// add: it declares graph.semantic complete without loading anything.
func TestCommitLayerWithoutGoFilesIsComplete(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "committed-docs")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	tree := committedTreeA()
	tree["README.md"] = "# one\n"
	builderWriteTree(t, repoDir, tree)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	dirA := builderTempDir(t, "committed-docs-a")
	builderWriteTree(t, dirA, tree)
	builderWriteFile(t, repoDir, "README.md", "# two\n")
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "committed-docs-base")
	builderIndex(t, store, dirA)
	builder := builderNewBuilder(store)
	builder.Semantic = goTypesManager(t)
	generationID, report, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
			LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture", ProvenanceCommitOID: commitB},
		Base: store, RepoDir: repoDir, BaseTreeOID: treeA, TargetTreeOID: treeB,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if !report.CommittedTypecheck.NothingToCheck || report.CommittedTypecheck.Compiler != nil {
		t.Errorf("stage %+v, want nothing to check and no load", report.CommittedTypecheck)
	}
	states, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	for _, row := range states {
		if row.Producer == string(graphview.CapSemantic) && row.State != store_sqlite.ProducerStateComplete {
			t.Errorf("graph.semantic row %+v, want complete", row)
		}
	}
}
