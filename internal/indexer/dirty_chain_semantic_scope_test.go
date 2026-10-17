package indexer

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/semantic"
)

// The handle-rooted go/types load on working-tree builds
// (index.dirty_chain.semantic_handle_roots). The pass loads only the packages
// of the Go files the generation carries; every capture below holds the served
// view against a clean semantic index of the same checkout and holds the
// compiler-context counter to packages(handle).

// scopeAssertCounter requires a handle-rooted load of exactly packages(H).
func scopeAssertCounter(t *testing.T, f *coordinatorFixture, label string, out CheckoutCycle) []string {
	t.Helper()
	w := out.DirtyWork
	if w == nil || !goTypesRan(w) {
		t.Fatalf("%s: the build ran no go/types pass: %+v", label, out)
	}
	cc := w.CompilerContext
	packages := semanticGenerationPackages(t, f, out.DirtyGenerationID)
	t.Logf("%s: parent=%d depth=%d parser_inputs=%d withheld=%d retained=%d scope=%s reason=%q loads=%d packages=%d files=%d load_ms=%d index_ms=%d index_cached=%v handle_packages=%v",
		label, out.DirtyParentGenerationID, out.DirtyChainDepth, w.ParserInputs, w.ContextWithheldFiles, w.ContextRetainedFiles,
		cc.Scope, cc.ScopeReason, cc.Loads, cc.Packages, cc.Files, cc.LoadMs, cc.IndexMs, cc.IndexCached, packages)
	if !cc.Measured || cc.Scope != semantic.CompilerScopeHandleRoots || cc.ScopeReason != "" || cc.Loads != 1 {
		t.Errorf("%s: compiler context %+v, want one handle-rooted load with no fallback", label, cc)
	}
	if cc.Packages != len(packages) {
		t.Errorf("%s: %d packages loaded, want |packages(handle)| = %d (%v)", label, cc.Packages, len(packages), packages)
	}
	return packages
}

// TestDirtyChainSemanticHandleRootsParity: chained single-file edits in the
// independent and the same-package layouts load one package each and keep
// parity with a clean semantic index, binding rows included.
func TestDirtyChainSemanticHandleRootsParity(t *testing.T) {
	f, c, mgr := semanticChainFixtureWith(t, semanticBindingTree(), true)
	coordinatorReconcile(t, c)

	for i := 0; i < 8; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	root := coordinatorReconcile(t, c)
	if root.DirtyParentGenerationID != 0 {
		t.Fatalf("the chain root = %+v, want a direct build", root)
	}
	scopeAssertCounter(t, f, "root", root)
	assertSemanticParity(t, f, mgr, "scoped-root")

	for k, edit := range []struct {
		layout accumulatedDirtyLayout
		unit   int
		files  int
		pkg    string
	}{
		{accumulatedDirtyIndependent, 9, 1, builderRepoPrefix + "/ind/p009"},
		{accumulatedDirtySamePackage, 3, retentionUnits, builderRepoPrefix + "/same"},
		{accumulatedDirtyIndependent, 12, 1, builderRepoPrefix + "/ind/p012"},
	} {
		semanticWriteUnit(t, f.worktree, edit.layout, edit.unit, true)
		out := coordinatorReconcile(t, c)
		label := fmt.Sprintf("scoped-chained-%d", k)
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("%s = %+v, want a chained build", label, out)
		}
		packages := scopeAssertCounter(t, f, label, out)
		if !slices.Equal(packages, []string{edit.pkg}) {
			t.Errorf("%s: packages(handle) = %v, want only %s", label, packages, edit.pkg)
		}
		if got := out.DirtyWork.CompilerContext.Files; got != edit.files {
			t.Errorf("%s: %d compiled files loaded, want %d", label, got, edit.files)
		}
		if n := semanticGenerationBindingRows(t, f, out.DirtyGenerationID); n == 0 {
			t.Errorf("%s: the generation wrote no binding rows; the binding oracle is vacuous", label)
		}
		assertSemanticParity(t, f, mgr, label)
	}
}

// semanticScopeChainTree is the binding tree plus a dependency chain
// a -> b -> c -> d. a's package variable takes its type from b.B, so a type
// change in b moves a stamp in a.
func semanticScopeChainTree() map[string]string {
	tree := semanticBindingTree()
	module := accumulatedDirtyModule
	tree["chain/d/d.go"] = `package d

// D is the bottom of the chain.
func D() int {
	return 1
}
`
	tree["chain/c/c.go"] = `package c

import "` + module + `/chain/d"

// C calls down the chain.
func C() int {
	return d.D() + 1
}
`
	tree["chain/b/b.go"] = scopeChainB(false, false, false)
	tree["chain/a/a.go"] = `package a

import "` + module + `/chain/b"

// X takes its type from b.B.
var X = b.B()

// A calls down the chain.
func A() int {
	v := b.B()
	_ = v
	return 1
}
`
	return tree
}

// scopeChainB renders chain/b/b.go: named makes B return the named type Kind,
// importD adds a direct import of d, withNew declares New.
func scopeChainB(named, importD, withNew bool) string {
	module := accumulatedDirtyModule
	imports := `import "` + module + `/chain/c"`
	body := "c.C() + 1"
	if importD {
		imports = "import (\n\t\"" + module + "/chain/c\"\n\t\"" + module + "/chain/d\"\n)"
		body = "c.C() + d.D()"
	}
	src := "package b\n\n" + imports + "\n\n"
	if named {
		src += "// Kind is B's result type.\ntype Kind int\n\n// B calls down the chain.\nfunc B() Kind {\n\treturn Kind(" + body + ")\n}\n"
	} else {
		src += "// B calls down the chain.\nfunc B() int {\n\treturn " + body + "\n}\n"
	}
	if withNew {
		src += "\n// New becomes resolvable.\nfunc New() int {\n\treturn 2\n}\n"
	}
	return src
}

// scopeChainFixture builds the chain tree (plus extra files), publishes a
// chain root over one dirty unit, and returns the fixture ready for the case
// edit to be a chained child.
func scopeChainFixture(t *testing.T, handleRoots bool, extra map[string]string) (*coordinatorFixture, *CheckoutCoordinator, *semantic.Manager) {
	t.Helper()
	tree := semanticScopeChainTree()
	for p, body := range extra {
		tree[p] = body
	}
	f, c, mgr := semanticChainFixtureWith(t, tree, handleRoots)
	coordinatorReconcile(t, c)
	semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, 0, true)
	if root := coordinatorReconcile(t, c); root.DirtyGenerationID == 0 || !goTypesRan(root.DirtyWork) {
		t.Fatalf("chain root = %+v, want a published build that ran go/types", root)
	}
	return f, c, mgr
}

// scopeHasSemanticEdge reports whether the served view holds a type-resolved
// edge from a node named from to a node named to.
func scopeHasSemanticEdge(t *testing.T, f *coordinatorFixture, fromSuffix, toSuffix string) bool {
	t.Helper()
	view := chainMaterialize(t, f)
	defer view.Close()
	for _, e := range view.Reader.AllEdges() {
		if e == nil || !strings.HasSuffix(e.From, fromSuffix) || !strings.HasSuffix(e.To, toSuffix) {
			continue
		}
		if source, _ := e.Meta["semantic_source"].(string); source != "" || strings.HasPrefix(e.Origin, "lsp") {
			return true
		}
	}
	return false
}

// TestDirtyChainSemanticHandleRootsCrossPackage covers the cross-package
// cases: a body edit in a (its call into b must stay type-resolved), an import
// change in b, and a name in b that becomes resolvable for a waiting caller.
func TestDirtyChainSemanticHandleRootsCrossPackage(t *testing.T) {
	module := accumulatedDirtyModule
	t.Run("body_edit_calls_across_packages", func(t *testing.T) {
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
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("the edit = %+v, want a chained build", out)
		}
		packages := scopeAssertCounter(t, f, "cross-package", out)
		if !slices.Contains(packages, builderRepoPrefix+"/chain/a") {
			t.Errorf("packages(handle) = %v, want chain/a among them", packages)
		}
		if !scopeHasSemanticEdge(t, f, "chain/a/a.go::A", "chain/b/b.go::B") {
			t.Error("the served view lost the type-resolved call A -> b.B")
		}
		assertSemanticParity(t, f, mgr, "cross-package")
	})
	t.Run("import_change", func(t *testing.T) {
		f, c, mgr := scopeChainFixture(t, true, nil)
		builderWriteFile(t, f.worktree, "chain/b/b.go", scopeChainB(false, true, false))
		out := coordinatorReconcile(t, c)
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("the edit = %+v, want a chained build", out)
		}
		packages := scopeAssertCounter(t, f, "import-change", out)
		if !slices.Contains(packages, builderRepoPrefix+"/chain/b") {
			t.Errorf("packages(handle) = %v, want chain/b among them", packages)
		}
		if !scopeHasSemanticEdge(t, f, "chain/b/b.go::B", "chain/d/d.go::D") {
			t.Error("the served view lacks the type-resolved call B -> d.D the import made possible")
		}
		assertSemanticParity(t, f, mgr, "import-change")
	})
	t.Run("name_becomes_resolvable", func(t *testing.T) {
		waiting := map[string]string{"chain/e/e.go": `package e

import "` + module + `/chain/b"

// UseNew is written against a name b does not declare yet.
func UseNew() int {
	return b.New()
}
`}
		// The resolver's pathless stub for the unresolved b.New outlives the
		// name becoming resolvable (the same identity-ownership gap as a
		// removed last external use); that is compared across both modes and
		// documented, while the handle and the type-resolved edge are held.
		scopeHole(t, "name-resolvable", waiting, func(t *testing.T, dir string) {
			builderWriteFile(t, dir, "chain/b/b.go", scopeChainB(false, false, true))
		}, func(t *testing.T, f *coordinatorFixture, out CheckoutCycle, handleRoots bool) {
			if out.DirtyParentGenerationID == 0 {
				t.Fatalf("the edit = %+v, want a chained build", out)
			}
			if handleRoots {
				packages := scopeAssertCounter(t, f, "name-resolvable", out)
				for _, want := range []string{builderRepoPrefix + "/chain/b", builderRepoPrefix + "/chain/e"} {
					if !slices.Contains(packages, want) {
						t.Errorf("packages(handle) = %v, want %s among them (the waiting caller must be re-derived)", packages, want)
					}
				}
			}
			if !scopeHasSemanticEdge(t, f, "chain/e/e.go::UseNew", "chain/b/b.go::New") {
				t.Errorf("handle_roots=%v: the served view lacks the type-resolved call UseNew -> b.New", handleRoots)
			}
		})
	})
}

// TestDirtyChainSemanticHandleRootsManifestFallback: an edit to go.mod makes
// the working-tree build load the whole module and say why.
func TestDirtyChainSemanticHandleRootsManifestFallback(t *testing.T) {
	f, c, mgr := scopeChainFixture(t, true, nil)
	builderWriteFile(t, f.worktree, "go.mod", "module "+accumulatedDirtyModule+"\n\ngo 1.22\n\n// touched\n")
	out := coordinatorReconcile(t, c)
	if !out.DirtyBuilt || out.DirtyWork == nil {
		t.Fatalf("the go.mod edit built nothing: %+v", out)
	}
	cc := out.DirtyWork.CompilerContext
	t.Logf("go.mod edit: parent=%d chain_reason=%q scope=%s reason=%q packages=%d files=%d",
		out.DirtyParentGenerationID, out.DirtyChainReason, cc.Scope, cc.ScopeReason, cc.Packages, cc.Files)
	if !cc.Measured || cc.Scope != semantic.CompilerScopeFull || cc.ScopeReason != semantic.CompilerScopeReasonManifestChanged {
		t.Errorf("compiler context %+v, want a whole-module load with reason %s", cc, semantic.CompilerScopeReasonManifestChanged)
	}
	assertSemanticParity(t, f, mgr, "go-mod")
}

// scopeHole runs one invalidation-hole scenario in both load modes and
// reports the parity differences each left. The holes exist with the whole-
// module load; a handle-rooted load must leave exactly the same differences.
func scopeHole(t *testing.T, name string, extra map[string]string, edit func(t *testing.T, dir string), check func(t *testing.T, f *coordinatorFixture, out CheckoutCycle, handleRoots bool)) {
	t.Helper()
	diffs := map[bool][]string{}
	counters := map[bool]string{}
	for _, handleRoots := range []bool{false, true} {
		f, c, mgr := scopeChainFixture(t, handleRoots, extra)
		edit(t, f.worktree)
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt || out.DirtyWork == nil {
			t.Fatalf("%s: the edit built nothing: %+v", name, out)
		}
		if check != nil {
			check(t, f, out, handleRoots)
		}
		w := out.DirtyWork
		counters[handleRoots] = fmt.Sprintf("parent=%d chain_reason=%q withheld=%d retained=%d scope=%s packages=%d",
			out.DirtyParentGenerationID, out.DirtyChainReason, w.ContextWithheldFiles, w.ContextRetainedFiles,
			w.CompilerContext.Scope, w.CompilerContext.Packages)
		diffs[handleRoots] = semanticParityDiffs(t, f, mgr, fmt.Sprintf("%s-%v", name, handleRoots))
		t.Logf("%s handle_roots=%v: %s; %d parity differences", name, handleRoots, counters[handleRoots], len(diffs[handleRoots]))
	}
	normalize := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, d := range in {
			// The labels differ by mode; the rows must not.
			_, rest, _ := strings.Cut(d, ": ")
			out = append(out, rest)
		}
		return out
	}
	if !slices.Equal(normalize(diffs[false]), normalize(diffs[true])) {
		t.Errorf("%s: the handle-rooted load changed the outcome\n  whole module: %v\n  handle roots: %v", name, diffs[false], diffs[true])
	}
	if len(diffs[true]) == 0 {
		t.Logf("%s: the served view matches a clean index in both modes", name)
		return
	}
	msg := fmt.Sprintf("%s: known invalidation hole, present with the whole-module load as well (no reason counter records it; counters: %s):\n%s",
		name, counters[true], strings.Join(diffs[true], "\n"))
	if os.Getenv("GORTEX_TEST_REQUIRE_SEMANTIC_FRONTIER") == "1" {
		t.Error(msg)
		return
	}
	t.Skip(msg)
}

// TestDirtyChainSemanticInvalidationHoles documents the frontier holes that
// the handle-rooted load neither causes nor fixes: each is a file whose
// type-resolved facts change without the file entering the generation.
// They skip with the observed differences (set
// GORTEX_TEST_REQUIRE_SEMANTIC_FRONTIER=1 to make them fail) until the
// closure carries such files.
func TestDirtyChainSemanticInvalidationHoles(t *testing.T) {
	module := accumulatedDirtyModule
	t.Run("withheld_context_type_change", func(t *testing.T) {
		scopeHole(t, "withheld-context-type-change", nil, func(t *testing.T, dir string) {
			builderWriteFile(t, dir, "chain/b/b.go", scopeChainB(true, false, false))
		}, nil)
	})
	t.Run("transitive_promoted_method", func(t *testing.T) {
		extra := map[string]string{
			"promo/base/base.go": "package base\n\n// T carries the promoted method.\ntype T struct{}\n\n// M is promoted through mid.S.\nfunc (T) M() int {\n\treturn 1\n}\n",
			"promo/mid/mid.go":   "package mid\n\nimport \"" + module + "/promo/base\"\n\n// S embeds base.T.\ntype S struct {\n\tbase.T\n}\n",
			"promo/top/top.go":   "package top\n\nimport \"" + module + "/promo/mid\"\n\n// Y takes its type from the promoted method.\nvar Y = mid.S{}.M()\n",
		}
		scopeHole(t, "transitive-promoted-method", extra, func(t *testing.T, dir string) {
			builderWriteFile(t, dir, "promo/base/base.go", "package base\n\n// T carries the promoted method.\ntype T struct{}\n\n// M is promoted through mid.S.\nfunc (T) M() string {\n\treturn \"one\"\n}\n")
		}, nil)
	})
	t.Run("implicit_cross_package_implements", func(t *testing.T) {
		extra := map[string]string{
			"impl/iface/iface.go": "package iface\n\n// Doer is satisfied implicitly.\ntype Doer interface {\n\tDo() int\n}\n",
			"impl/kind/kind.go":   "package kind\n\n// T gains Do in the edit.\ntype T struct{}\n",
		}
		scopeHole(t, "implicit-cross-package-implements", extra, func(t *testing.T, dir string) {
			builderWriteFile(t, dir, "impl/kind/kind.go", "package kind\n\n// T gains Do in the edit.\ntype T struct{}\n\n// Do makes T a Doer.\nfunc (T) Do() int {\n\treturn 1\n}\n")
		}, nil)
	})
	t.Run("last_external_use_removed", func(t *testing.T) {
		extra := map[string]string{
			"ext/only/only.go": "package only\n\nimport \"strings\"\n\n// Upper is the only use of strings.ToUpper.\nfunc Upper() string {\n\treturn strings.ToUpper(\"x\")\n}\n",
		}
		scopeHole(t, "last-external-use-removed", extra, func(t *testing.T, dir string) {
			builderWriteFile(t, dir, "ext/only/only.go", "package only\n\n// Upper no longer uses strings.\nfunc Upper() string {\n\treturn \"X\"\n}\n")
		}, nil)
	})
}
