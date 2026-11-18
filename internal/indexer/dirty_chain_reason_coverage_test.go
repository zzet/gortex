package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// dirtyChainReasonProducers names, for every fallback reason, the test that
// produces it and asserts it was produced. Parent selection produces all but
// one; fold_unverified comes only from the inline fold's background check.
var dirtyChainReasonProducers = map[string]string{
	dirtyChainFallbackCleanCheckout:             "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackNoParent:                  "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackParentManifestMissing:     "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackPolicyChanged:             "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackDependencyManifestChanged: "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackHeadOrBaseMoved:           "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackChainDepthExhausted:       "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackDeltaNotSmaller:           "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackClosureTruncatedParent:    "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackSymlinkOrSubmoduleChanged: "TestSelectDirtyParentReasonCodes",
	dirtyChainFallbackFoldUnverified:            "TestUnverifiedInlineFoldSendsTheNextBuildDirect",
}

// Every fallback reason has a producing test, and that test exists in the
// package and names the reason's constant (the test itself asserts it was
// produced). A reason with no entry, an entry for no reason, or a test that
// does not name its reason fails here.
func TestEveryFallbackReasonHasAProducingTest(t *testing.T) {
	constOf := map[string]string{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// The constant each reason is spelled by, and every test's body.
	bodies := map[string]string{} // test name -> body source
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if strings.HasSuffix(name, "_test.go") && strings.HasPrefix(d.Name.Name, "Test") && d.Body != nil {
					bodies[d.Name.Name] = string(src[fset.Position(d.Body.Pos()).Offset:fset.Position(d.Body.End()).Offset])
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, n := range vs.Names {
						if !strings.HasPrefix(n.Name, "dirtyChainFallback") || i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
							constOf[strings.Trim(lit.Value, `"`)] = n.Name
						}
					}
				}
			}
		}
	}
	listed := map[string]bool{}
	for _, reason := range dirtyChainFallbackReasons {
		listed[reason] = true
		producer, ok := dirtyChainReasonProducers[reason]
		if !ok {
			t.Errorf("reason %q has no producing test in dirtyChainReasonProducers", reason)
			continue
		}
		body, ok := bodies[producer]
		if !ok {
			t.Errorf("reason %q: producing test %s does not exist", reason, producer)
			continue
		}
		if !strings.Contains(body, constOf[reason]) {
			t.Errorf("reason %q: producing test %s never names %s", reason, producer, constOf[reason])
		}
	}
	for reason := range dirtyChainReasonProducers {
		if !listed[reason] {
			t.Errorf("dirtyChainReasonProducers names %q, which is not a fallback reason", reason)
		}
	}
}
