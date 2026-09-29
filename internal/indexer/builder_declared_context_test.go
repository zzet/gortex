package indexer

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// cloneLeakTree is a changed file calling into five context files whose
// functions are large enough to carry a clone signature.
func cloneLeakTree(edited bool) map[string]string {
	body := func(name string, n int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "func %s(x int) int {\n\ttotal := 0\n", name)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "\tif x > %d {\n\t\ttotal += x * %d\n\t} else {\n\t\ttotal -= %d\n\t}\n", i, i+1, i)
		}
		b.WriteString("\treturn total\n}\n")
		return b.String()
	}
	tree := map[string]string{}
	var calls strings.Builder
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("Ctx%d", i)
		tree[fmt.Sprintf("ctx%d.go", i)] = "package fixture\n\n" + body(name, 12+i)
		fmt.Fprintf(&calls, "\tsum += %s(%d)\n", name, i)
	}
	extra := ""
	if edited {
		extra = "\tsum++\n"
	}
	tree["changed.go"] = "package fixture\n\nfunc Changed() int {\n\tsum := 0\n" + calls.String() + extra + "\treturn sum\n}\n"
	return tree
}

// TestWithheldContextLeavesNoCloneCorpusRows: the generation's clone corpus
// holds rows only for nodes the generation carries — a withheld context
// file's functions leave their shingle rows behind with them.
func TestWithheldContextLeavesNoCloneCorpusRows(t *testing.T) {
	store := builderOpenStore(t, "clone-leak")
	_, generationID, report := builderContextGeneration(t, store, cloneLeakTree(false), cloneLeakTree(true))
	if len(report.ContextPaths) == 0 {
		t.Fatalf("no context was withheld: %+v", report.ClosurePaths)
	}
	db := parityOpenRaw(t, store)
	orphans := parityRows(t, db, `SELECT c.node_id, '' FROM clone_shingles c WHERE c.repo_prefix = ? AND c.view_gen = ?
		AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = c.node_id AND n.view_gen = c.view_gen)`, generationID)
	all := parityRows(t, db, `SELECT node_id, '' FROM clone_shingles WHERE repo_prefix = ? AND view_gen = ?`, generationID)
	t.Logf("withheld %d paths; clone rows %d, orphaned %d", len(report.ContextPaths), len(all), len(orphans))
	if len(all) == 0 {
		t.Fatal("the generation wrote no clone rows at all; the fixture no longer exercises the corpus")
	}
	if len(orphans) > 0 {
		t.Errorf("the generation's clone corpus holds %d rows for nodes it does not carry: %v", len(orphans), orphans)
	}
}

// memberChainTree is a changed file that reaches a declared method through a
// declared constructor's result type, across three files of one package.
func memberChainTree(edited bool) map[string]string {
	extra := ""
	if edited {
		extra = "\tn++\n"
	}
	return map[string]string{
		"go.mod":   "module example.com/fixture\n\ngo 1.22\n",
		"types.go": "package fixture\n\n// T holds a size.\ntype T struct {\n\tsize int\n}\n\n// Unused is never named by the change.\nfunc Unused() int {\n\treturn 7\n}\n",
		"size.go":  "package fixture\n\n// Size reports the size.\nfunc (t *T) Size() int {\n\treturn t.size\n}\n",
		"new.go":   "package fixture\n\n// New builds a T.\nfunc New() *T {\n\treturn &T{size: 1}\n}\n",
		"use.go":   "package fixture\n\n// Use calls through the constructor's result.\nfunc Use() int {\n\tn := New().Size()\n" + extra + "\treturn n\n}\n",
	}
}

// TestDeclaredContextKeepsCleanIndexParity: a body edit that reaches a
// declared method through a declared constructor's result type reads that
// context, withholds it, and still serves exactly what a clean index serves.
func TestDeclaredContextKeepsCleanIndexParity(t *testing.T) {
	store := builderOpenStore(t, "declared-context")
	repoDir, generationID, report := builderContextGeneration(t, store, memberChainTree(false), memberChainTree(true))
	if len(report.ClosureDeclaredPaths) == 0 {
		t.Fatalf("no declared context was read: closure=%v", report.ClosurePaths)
	}
	assertCleanIndexParity(t, store, generationID, repoDir, "declared-context", true)
}

// TestDeclaredContextSeedParsesOnlyTheChangeSet: with declared context seeded
// from the layer below, a body edit parses the edited file and the manifest
// alone, the declared file it calls into is neither parsed nor written, the
// type-resolved call into it survives, and the served view matches a clean
// semantic index.
func TestDeclaredContextSeedParsesOnlyTheChangeSet(t *testing.T) {
	t.Setenv("GORTEX_CLOSURE_SEED_DECLARED", "on")
	module := accumulatedDirtyModule
	f, c, mgr := scopeChainFixture(t, true, nil)
	builderWriteFile(t, f.worktree, "chain/a/a.go", `package a

import "`+module+`/chain/b"

// X takes its type from b.B.
var X = b.B()

// A calls down the chain.
func A() int {
	v := b.B()
	_ = v
	v++
	return 1
}
`)
	out := coordinatorReconcile(t, c)
	if out.DirtyParentGenerationID == 0 || out.DirtyWork == nil {
		t.Fatalf("the edit = %+v, want a chained build", out)
	}
	w := out.DirtyWork
	t.Logf("parser_inputs=%d paths=%v withheld=%d retained=%d", w.ParserInputs, w.ParserInputPaths, w.ContextWithheldFiles, w.ContextRetainedFiles)
	if want := []string{"chain/a/a.go", "go.mod"}; !slices.Equal(w.ParserInputPaths, want) {
		t.Errorf("parsed %v, want only the edited file and the manifest %v", w.ParserInputPaths, want)
	}
	if w.ContextRetainedFiles != 0 {
		t.Errorf("a body edit retained %d context files", w.ContextRetainedFiles)
	}
	if !scopeHasSemanticEdge(t, f, "chain/a/a.go::A", "chain/b/b.go::B") {
		t.Error("the served view lost the type-resolved call A -> b.B")
	}
	assertSemanticParity(t, f, mgr, "seeded-declared-context")
}

// TestNameScopedSeedKeepsCleanIndexParity: with the seed scoped to the
// declarations the change set names (the default), a body edit that reaches a
// declared method through a declared constructor's result type still serves
// exactly what a clean index serves.
func TestNameScopedSeedKeepsCleanIndexParity(t *testing.T) {
	for _, scope := range []string{"names", "files"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv("GORTEX_CLOSURE_SEED_SCOPE", scope)
			store := builderOpenStore(t, "name-seed-"+scope)
			repoDir, generationID, report := builderContextGeneration(t, store, memberChainTree(false), memberChainTree(true))
			if len(report.DeclaredContextSeeded) == 0 {
				t.Fatalf("nothing was seeded: declared=%v", report.ClosureDeclaredPaths)
			}
			assertCleanIndexParity(t, store, generationID, repoDir, "name-seed-"+scope, true)
		})
	}
}
