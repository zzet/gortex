package indexer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	gitlinkTestCommitOne = "1111111111111111111111111111111111111111"
	gitlinkTestCommitTwo = "2222222222222222222222222222222222222222"
)

// gitlinkTestCommit commits tree plus the given submodule pointers and returns
// the commit's tree. The pointers are staged straight into the index, so the
// test needs no submodule repository; builderWriteTree leaves none on disk,
// which is also how an uninitialized submodule looks to `git add -A`.
func gitlinkTestCommit(t *testing.T, repoDir string, tree map[string]string, gitlinks map[string]string) string {
	t.Helper()
	builderWriteTree(t, repoDir, tree)
	builderGit(t, repoDir, "add", "-A")
	for p, oid := range gitlinks {
		builderGit(t, repoDir, "update-index", "--add", "--cacheinfo", "160000,"+oid+","+p)
	}
	builderGit(t, repoDir, "commit", "--allow-empty", "-m", "commit")
	return builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
}

func gitlinkTestRenderChanges(changes []LayerPathChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, string(c.Kind)+" "+c.Path)
	}
	return strings.Join(parts, ", ")
}

// A commit-layer change set names only paths the target tree source can
// serve: a submodule pointer is never present, so a bump changes nothing and a
// file replaced by a submodule is deleted (#1458).
func TestDiffTreeChangesLeavesGitlinksOut(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	tree0 := gitlinkTestCommit(t, repoDir, map[string]string{"a.go": "package a\n", "lib": "a file\n"}, nil)
	tree1 := gitlinkTestCommit(t, repoDir, map[string]string{"a.go": "package a\n\nvar X = 1\n"},
		map[string]string{"lib": gitlinkTestCommitOne, "vendor/sub": gitlinkTestCommitOne})
	tree2 := gitlinkTestCommit(t, repoDir, map[string]string{"a.go": "package a\n\nvar X = 1\n"},
		map[string]string{"lib": gitlinkTestCommitTwo})
	tree3 := gitlinkTestCommit(t, repoDir, map[string]string{"a.go": "package a\n\nvar X = 1\n", "lib": "a file again\n"}, nil)

	for _, tc := range []struct {
		name       string
		base, next string
		want       string
	}{
		{"file replaced by submodule, submodule added", tree0, tree1, "modified a.go, deleted lib"},
		{"submodule bumped and removed", tree1, tree2, ""},
		{"submodule replaced by file", tree2, tree3, "added lib"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := diffTreeChanges(context.Background(), repoDir, tc.base, tc.next)
			if err != nil {
				t.Fatalf("diffTreeChanges: %v", err)
			}
			if got := gitlinkTestRenderChanges(changes); got != tc.want {
				t.Errorf("changes = %q, want %q", got, tc.want)
			}
		})
	}
}

// A commit layer across a submodule pointer bump builds instead of refusing
// the gitlink as a present path the target tree does not hold (#1458).
func TestCommitLayerBuildsAcrossASubmoduleBump(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	treeA := gitlinkTestCommit(t, repoDir, builderTreeA(), map[string]string{"3rdparty/lib": gitlinkTestCommitOne})
	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, builderTreeA())
	// A linked worktree holds the submodule as a directory, empty until the
	// submodule is initialized.
	if err := os.MkdirAll(filepath.Join(dirA, "3rdparty", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	treeB := gitlinkTestCommit(t, repoDir, builderTreeB(), map[string]string{"3rdparty/lib": gitlinkTestCommitTwo})
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dirA)

	_, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
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
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer across a submodule bump: %v", err)
	}
	for _, want := range []string{"added.go", "core.go"} {
		if !slices.Contains(report.IndexedPaths, want) {
			t.Errorf("the generation's file set %v misses the changed file %s", report.IndexedPaths, want)
		}
	}
	if slices.Contains(report.IndexedPaths, "3rdparty/lib") {
		t.Errorf("the submodule pointer is in the generation's file set %v", report.IndexedPaths)
	}
}
