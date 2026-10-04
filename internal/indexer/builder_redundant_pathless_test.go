package indexer

import (
	"context"
	"slices"
	"testing"
)

// stubAdjacencyTree is two files that both return a value out of the same
// stdlib function, so the stdlib stub has out-edges (value flows) recorded in
// each of them.
func stubAdjacencyTree(edited bool) map[string]string {
	body := "\treturn fmt.Errorf(\"a\")\n"
	if edited {
		body = "\t_ = 1\n" + body
	}
	return map[string]string{
		"go.mod": "module example.com/fixture\n\ngo 1.22\n",
		"a.go":   "package fixture\n\nimport \"fmt\"\n\n// A errs.\nfunc A() error {\n" + body + "}\n",
		"b.go":   "package fixture\n\nimport \"fmt\"\n\n// B errs.\nfunc B() error {\n\treturn fmt.Errorf(\"b\")\n}\n",
	}
}

// TestBodyEditKeepsOtherFilesEdgesOutOfASharedStub: a body edit in a.go
// re-derives a.go's reference to fmt.Errorf, and with it the stdlib stub. The
// generation must not carry that stub as its own (a carried pathless node owns
// its whole adjacency), or b.go's value flow out of the stub — recorded in a
// file the generation never touched — disappears from the composed view. The
// composed view answers the stub's adjacency exactly as a clean index does.
func TestBodyEditKeepsOtherFilesEdgesOutOfASharedStub(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "stub-adjacency")
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, stubAdjacencyTree(false))
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)

	builderWriteTree(t, repoDir, stubAdjacencyTree(true))
	generationID, report, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildDirtyLayer: %v", err)
	}
	propAssertRedundantPathlessPruned(t, store, generationID, store.AtGeneration(0))
	clean := builderOpenStore(t, "stub-adjacency-clean")
	builderIndex(t, clean, repoDir)
	composed := builderComposed(t, store, generationID)

	stub := builderRepoPrefix + "::stdlib::fmt::Errorf"
	want := builderRenderEdges(clean.GetOutEdges(stub))
	if len(want) < 2 {
		t.Fatalf("the clean index holds %d edges out of %s; the fixture needs one per file: %v", len(want), stub, want)
	}
	got := builderRenderEdges(composed.GetOutEdges(stub))
	if !slices.Equal(got, want) {
		t.Errorf("edges out of %s (edge sources %d, tombstones %d):\n composed %v\n clean    %v",
			stub, report.EdgeSourceMarkers, report.NodeTombstones, got, want)
	}
	if got, want := builderRenderEdges(composed.AllEdges()), builderRenderEdges(clean.AllEdges()); !slices.Equal(got, want) {
		t.Errorf("AllEdges differs from a clean index:\n composed %v\n clean    %v", got, want)
	}
}
