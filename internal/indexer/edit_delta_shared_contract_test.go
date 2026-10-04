package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// sharedContractTree has two files registering one route (one contract ID,
// two records) and one of them registering a second route of its own.
func sharedContractTree() map[string]string {
	return map[string]string{
		"go.mod":        "module example.com/fx\n\ngo 1.22\n",
		"api/routes.go": "package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc setupRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsers)\n\tr.POST(\"/api/things\", createItem)\n}\n\nfunc listUsers()  {}\nfunc createItem() {}\n",
		"api/more.go":   "package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc moreRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsersToo)\n}\n\nfunc listUsersToo() {}\n",
	}
}

// renderContractRows is a view's contract nodes and the owner edges into
// them (provides, consumes, handles_route): identity, file and endpoints.
func renderContractRows(r graph.Reader) []string {
	var out []string
	var ids []string
	for n := range r.NodesByKind(graph.KindContract) {
		if n == nil {
			continue
		}
		out = append(out, fmt.Sprintf("node %s @%s", n.ID, n.FilePath))
		ids = append(ids, n.ID)
	}
	for _, id := range ids {
		for _, e := range r.GetInEdges(id) {
			switch e.Kind {
			case graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeHandlesRoute:
				// The source of an owner edge into an ID two files share is
				// whichever record the registry picked, which differs between
				// two whole indexes of the same tree; it is left out.
				out = append(out, fmt.Sprintf("edge -%s-> %s @%s", e.Kind, e.To, e.FilePath))
			}
		}
	}
	sort.Strings(out)
	return out
}

func contractRowsDiff(got, want []string) (onlyGot, onlyWant []string) {
	have := map[string]int{}
	for _, l := range want {
		have[l]++
	}
	for _, l := range got {
		if have[l] > 0 {
			have[l]--
			continue
		}
		onlyGot = append(onlyGot, l)
	}
	for l, n := range have {
		for ; n > 0; n-- {
			onlyWant = append(onlyWant, l)
		}
	}
	sort.Strings(onlyWant)
	return onlyGot, onlyWant
}

// A single working-tree edit on a clean checkout that deletes one of two
// files sharing a contract ID leaves the other file's contracts as a clean
// index of the tree holds them: its own records, the shared ID's surviving
// record, and every owner edge into them.
func TestEditDeltaDeletingASharedContractFileKeepsTheOthersContracts(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "shared-contract")
	repoDir := builderTempDir(t, "checkout-shared-contract")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, sharedContractTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)

	if err := os.Remove(filepath.Join(repoDir, "api", "more.go")); err != nil {
		t.Fatal(err)
	}
	id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	view := dirtyChainComposed(t, store, []int64{id})
	clean := builderOpenStore(t, "shared-contract-clean")
	builderIndex(t, clean, repoDir)
	got, want := renderContractRows(view), renderContractRows(clean)
	if onlyGot, onlyWant := contractRowsDiff(got, want); len(onlyGot)+len(onlyWant) > 0 {
		t.Fatalf("the edit's view differs from a clean index in its contracts\nonly the view: %s\nonly the clean index: %s",
			strings.Join(onlyGot, "; "), strings.Join(onlyWant, "; "))
	}
}

// The same over a chain: an earlier edit re-derived routes.go (a route of its
// own renamed), and a chained edit then deletes more.go. The chain's composed
// view must hold routes.go's contracts as a clean index does.
func TestEditDeltaChainDeletingASharedContractFileKeepsTheOthersContracts(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "shared-contract-chain")
	repoDir := builderTempDir(t, "checkout-shared-contract-chain")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, sharedContractTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	check := func(label string) {
		t.Helper()
		view := dirtyChainComposed(t, store, h.chain)
		clean := builderOpenStore(t, "shared-contract-chain-clean-"+label)
		builderIndex(t, clean, repoDir)
		if onlyGot, onlyWant := contractRowsDiff(renderContractRows(view), renderContractRows(clean)); len(onlyGot)+len(onlyWant) > 0 {
			t.Fatalf("%s: the chain's view differs from a clean index in its contracts\nonly the view: %s\nonly the clean index: %s",
				label, strings.Join(onlyGot, "; "), strings.Join(onlyWant, "; "))
		}
	}
	routes := sharedContractTree()["api/routes.go"]
	builderWriteFile(t, repoDir, "api/routes.go", strings.Replace(routes, "/api/things", "/api/stuff", 1))
	h.build()
	check("rename_route")
	if err := os.Remove(filepath.Join(repoDir, "api", "more.go")); err != nil {
		t.Fatal(err)
	}
	_, report, chain := h.build()
	if len(chain) != 2 || report.ParentGenerationID == 0 {
		t.Fatalf("the delete was not chained: chain %v, fallback %q", chain, report.ChainFallbackReason)
	}
	check("delete_sharing_file")
}
