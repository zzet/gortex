package indexer

import (
	"context"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A builtin stub (`<repo>::builtin::go::type::int`, ...) lives at no path but
// belongs to the repository that referenced it. The resolver materialises it
// with the workspace and project of the referencing symbol
// (resolver/go_builtins_attribution.go), a sparse generation carries and claims
// it with those columns, and so the whole index of the same tree must serve
// the same row: otherwise no composed view can equal a clean index, and the
// warm-start slug backfill keeps rewriting it.

// builtinStubTree references builtin types and functions from signatures and
// bodies, so the pass materialises several builtin stubs.
func builtinStubTree(edited bool) map[string]string {
	body := "\ts := make([]int, 0)\n\ts = append(s, n)\n\treturn len(s)\n"
	if edited {
		body = "\ts := make([]int, 0, n)\n\ts = append(s, n, n)\n\treturn len(s)\n"
	}
	return map[string]string{
		"core.go":   "package fixture\n\n// Count returns a length.\nfunc Count(n int) int {\n" + body + "}\n",
		"caller.go": "package fixture\n\n// Run calls Count.\nfunc Run() string {\n\t_ = Count(1)\n\treturn \"\"\n}\n",
	}
}

func TestWholeIndexStampsBuiltinStubsWithTheRepositoryBoundary(t *testing.T) {
	store := builderOpenStore(t, "builtin-whole-index")
	dir := builderTempDir(t, "tree")
	builderWriteTree(t, dir, builtinStubTree(false))
	builderIndex(t, store, dir)

	var stubs int
	for _, node := range store.AllNodes() {
		if node == nil || !graph.IsBuiltinStub(node.ID) {
			continue
		}
		stubs++
		if node.WorkspaceID != builderRepoPrefix || node.ProjectID != builderRepoPrefix {
			t.Errorf("builtin stub %q carries workspace %q / project %q, want %q for both",
				node.ID, node.WorkspaceID, node.ProjectID, builderRepoPrefix)
		}
	}
	if stubs == 0 {
		t.Fatal("the fixture materialised no builtin stub — it pins nothing")
	}
	intID := builderRepoPrefix + "::builtin::go::type::int"
	if n := store.GetNode(intID); n == nil || n.WorkspaceID != builderRepoPrefix || n.ProjectID != builderRepoPrefix {
		t.Errorf("GetNode(%s) = %+v, want the repository-stamped stub", intID, n)
	}
}

// TestDirtyLayerBuiltinStubMatchesAWholeIndex is the direct regression of the
// one identity the clean-parity oracle used to find: a working-tree generation
// re-carries the builtin stub stamped, and a clean index of the same checkout
// must serve the identical row.
func TestDirtyLayerBuiltinStubMatchesAWholeIndex(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "builtin-dirty-base")
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, builtinStubTree(false))
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)

	builderWriteTree(t, repoDir, builtinStubTree(true))
	recordLastEditDelta(nil)
	generationID, _, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
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
	if delta := LastEditDeltaReport(); delta == nil || delta.IdentityClaims == 0 {
		t.Fatal("the generation claims no pathless builtin — it pins nothing")
	}

	clean := builderOpenStore(t, "builtin-dirty-clean")
	builderIndex(t, clean, repoDir)
	composed := builderComposed(t, store, generationID)
	for _, id := range []string{
		builderRepoPrefix + "::builtin::go::type::int",
		builderRepoPrefix + "::builtin::go::len",
	} {
		got, want := builderRenderNode(composed.GetNode(id)), builderRenderNode(clean.GetNode(id))
		if got != want {
			t.Errorf("%s: the composed view serves\n  %s\nthe clean index serves\n  %s", id, got, want)
		}
	}
	// Every surface matches a clean index strictly; nodes too, except the
	// clone signature of the edited function, which the per-file delta does
	// not recompute (a documented class: the delta's private indexer holds no
	// corpus to sign against).
	result := assertCleanIndexParity(t, store, generationID, repoDir, "builtin-dirty", false)
	if !result.EdgesEqual || !result.FilesEqual || !result.SemanticEqual || !result.ConstantsEqual ||
		!result.SymbolFTSEqual || !result.MasksValid {
		t.Errorf("the composed view does not match the clean index: %+v", result)
	}
	withoutCloneSig := func(nodes []*graph.Node) []string {
		out := make([]*graph.Node, 0, len(nodes))
		for _, n := range nodes {
			if n != nil && n.Meta["clone_sig"] != nil {
				c := *n
				c.Meta = make(map[string]any, len(n.Meta))
				for k, v := range n.Meta {
					if k != "clone_sig" {
						c.Meta[k] = v
					}
				}
				n = &c
			}
			out = append(out, n)
		}
		return builderRenderNodes(out)
	}
	if got, want := withoutCloneSig(composed.AllNodes()), withoutCloneSig(clean.AllNodes()); !slices.Equal(got, want) {
		t.Errorf("the composed nodes differ from the clean index beyond clone signatures: %v", parityDiff("nodes", got, want))
	}
}
