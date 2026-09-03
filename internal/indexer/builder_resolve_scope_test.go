package indexer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A sparse generation build resolves its own closure, not the corpus's backlog.
//
// The build runs the ordinary index pipeline against a generation-pinned store
// handle, so its resolve pass enumerates pending work through that handle. The
// rows come back generation-filtered either way; what this case pins is the
// bound rather than the filter — the window the pass declares must sit entirely
// above the corpus beneath it, so a change touching two files never walks a
// workspace-sized frontier to find its own handful of unresolved references.

// resolveScopeBacklogFiles is how many corpus files park a reference on a name
// nothing defines. They are the backlog a build must not walk: every one is
// unresolved in the base corpus, and none is reachable from the change.
const resolveScopeBacklogFiles = 150

func resolveScopeTree(core string) map[string]string {
	tree := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go":  core,
		"helper.go": `package fixture

func Helper() Result {
	return Result{}
}
`,
	}
	for i := 0; i < resolveScopeBacklogFiles; i++ {
		tree[fmt.Sprintf("island%03d.go", i)] = fmt.Sprintf(`package fixture

func Island%03d() {
	Nowhere%03d()
}
`, i, i)
	}
	return tree
}

// resolveScopeCoreA and resolveScopeCoreB both park a reference on a name the
// tree does not define, so the generation the change produces carries pending
// work of its own — without it the bound would be vacuous.
const resolveScopeCoreA = `package fixture

func Compute(o Options) Result {
	Absent()
	return Helper()
}
`

const resolveScopeCoreB = `package fixture

func Calculate(o Options, again Options) Result {
	Absent()
	return Helper()
}
`

func unresolvedEdgeCount(t *testing.T, reader interface{ AllEdges() []*graph.Edge }) int {
	t.Helper()
	count := 0
	for _, edge := range reader.AllEdges() {
		if edge != nil && graph.IsUnresolvedTarget(edge.To) {
			count++
		}
	}
	return count
}

func TestSparseBuildResolvesWithinItsOwnGeneration(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, resolveScopeTree(resolveScopeCoreA))
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	baseTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dir)

	corpus := store.AtGeneration(0)
	backlogBefore := unresolvedEdgeCount(t, corpus)
	if backlogBefore <= resolveScopeBacklogFiles {
		t.Fatalf("the corpus carries %d unresolved edges, want more than the %d the fixture parks",
			backlogBefore, resolveScopeBacklogFiles)
	}
	baseScan, err := corpus.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatalf("BeginUnresolvedEdgeScan on the corpus: %v", err)
	}
	if baseScan.LowWaterID != 0 {
		t.Fatalf("the corpus's own scan starts at %d, want 0", baseScan.LowWaterID)
	}

	builderWriteTree(t, dir, resolveScopeTree(resolveScopeCoreB))
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "B")
	targetTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")
	commitOID := builderGit(t, dir, "rev-parse", "HEAD")

	generationID, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-resolve-scope",
			LayerID:             "layer-" + targetTree,
			CheckoutID:          "checkout-resolve-scope",
			ProvenanceCommitOID: commitOID,
		},
		Base:          store,
		RepoDir:       dir,
		BaseTreeOID:   baseTree,
		TargetTreeOID: targetTree,
		RootPath:      dir,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d files — not what this case is about", report.ClosureCap)
	}
	for _, path := range report.IndexedPaths {
		if strings.HasPrefix(path, "island") {
			t.Fatalf("the generation's file set reaches the backlog: %v", report.IndexedPaths)
		}
	}
	if !slices.Contains(report.IndexedPaths, "core.go") {
		t.Fatalf("the generation's file set %v does not carry the changed file", report.IndexedPaths)
	}

	// The corpus's backlog is the base resolver's to work through. A build that
	// consumed or rewrote any of it would have resolved work it does not own.
	if after := unresolvedEdgeCount(t, corpus); after != backlogBefore {
		t.Fatalf("the corpus's unresolved backlog moved from %d to %d during a sparse build",
			backlogBefore, after)
	}

	derived := store.AtGeneration(generationID)
	genScan, err := derived.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatalf("BeginUnresolvedEdgeScan on the generation: %v", err)
	}
	if genScan.HighWaterID == 0 {
		t.Fatal("the generation carries no pending work — the bound below would prove nothing")
	}
	if genScan.LowWaterID <= baseScan.HighWaterID {
		t.Fatalf("the generation's resolve window starts at %d, at or below the corpus's last "+
			"unresolved row %d — a two-file change walks the whole backlog",
			genScan.LowWaterID, baseScan.HighWaterID)
	}

	page, err := derived.ReadUnresolvedEdgePage(t.Context(), genScan, 0, 16<<10, 16<<20)
	if err != nil {
		t.Fatalf("ReadUnresolvedEdgePage on the generation: %v", err)
	}
	if len(page.Edges) == 0 {
		t.Fatal("the generation's first page is empty")
	}
	for _, edge := range page.Edges {
		rel, owned := builderRelPath(builderRepoPrefix, edge.FilePath)
		if !owned || !slices.Contains(report.IndexedPaths, rel) {
			t.Fatalf("the generation's pending page carries %s, which is outside its file set %v",
				edge.FilePath, report.IndexedPaths)
		}
	}
}
