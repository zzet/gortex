package indexer

import (
	"fmt"
	"strings"
	"testing"
)

// claimSurvivorTree is a type whose methods live in other files of its
// package, and callers in another package: an edit of the type's file evicts
// and re-adds the type, and the rows other files hold into it (member_of,
// calls) are edge-claimed.
func claimSurvivorTree(methods, callers int, store string) map[string]string {
	tree := map[string]string{
		"go.mod":         "module example.com/fixture\n\ngo 1.24\n",
		"store/store.go": store,
	}
	for i := 0; i < methods; i++ {
		tree[fmt.Sprintf("store/m%02d.go", i)] = fmt.Sprintf("package store\n\nfunc (s *Store) M%02d() int {\n\treturn s.n\n}\n", i)
	}
	for i := 0; i < callers; i++ {
		tree[fmt.Sprintf("app/c%02d.go", i)] = fmt.Sprintf(
			"package app\n\nimport \"example.com/fixture/store\"\n\nfunc C%02d(s *store.Store) int {\n\treturn s.M%02d() + store.Helper(1) + store.Named() + store.Dropped()\n}\n", i, i%methods)
	}
	return tree
}

func claimSurvivorStore(body int, helperSig, named string, dropped bool) string {
	var b strings.Builder
	b.WriteString("package store\n\ntype Store struct{ n int }\n\nfunc configure() int {\n")
	b.WriteString(strings.Repeat("\t_ = 0\n", body))
	b.WriteString("\treturn 1\n}\n\n")
	b.WriteString("func Helper(" + helperSig + ") int {\n\treturn configure()\n}\n\n")
	b.WriteString("func " + named + "() int {\n\treturn 2\n}\n")
	if dropped {
		b.WriteString("\nfunc Dropped() int {\n\treturn 3\n}\n")
	}
	return b.String()
}

// Edits of a type's file, on a chain, each through the MCP edit: a body-only
// change, a signature change, a rename and a removal. After every edit the
// served view equals a clean index of the working tree: the rows other files
// hold into the re-derived file's identities stay visible where they still
// hold, and go where their target went. A body-only edit claims no caller
// whole.
func TestEditsOfATypesFileKeepTheRowsOtherFilesHoldIntoIt(t *testing.T) {
	const methods, callers = 6, 8
	f, _, l := mcpChainFixture(t, claimSurvivorTree(methods, callers, claimSurvivorStore(0, "x int", "Named", true)), false)
	steps := []struct {
		label string
		store string
	}{
		{"body-only", claimSurvivorStore(3, "x int", "Named", true)},
		{"signature", claimSurvivorStore(3, "x, y int", "Named", true)},
		{"rename", claimSurvivorStore(3, "x, y int", "Renamed", true)},
		{"removal", claimSurvivorStore(3, "x, y int", "Renamed", false)},
	}
	for i, step := range steps {
		out := mcpEdit(t, l, f, func() { builderWriteFile(t, f.worktree, "store/store.go", step.store) })
		delta := LastEditDeltaReport()
		if delta == nil {
			t.Fatalf("%s: no edit delta", step.label)
		}
		t.Logf("%s: depth %d edge_claims=%d promoted=%d payload_edges=%d", step.label, out.DirtyChainDepth, delta.EdgeClaims, delta.EdgeClaimsPromoted, delta.PayloadEdges)
		if i == 0 && delta.EdgeClaimsPromoted != 0 {
			t.Errorf("a body-only edit claimed %d sources whole", delta.EdgeClaimsPromoted)
		}
		view := chainMaterialize(t, f)
		kindParityCheck(t, "claim-survivors/"+step.label, f.worktree, view.Reader,
			generationCovered(t, f.store, out.DirtyGenerationID), []string{"store/store.go"})
		view.Close()
	}
}
