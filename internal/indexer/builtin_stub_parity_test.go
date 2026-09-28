package indexer

import (
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
