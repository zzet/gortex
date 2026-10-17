package goanalysis

import (
	"context"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
)

func TestPackageMayContainGraphVisibleGoCallerRetainsLineDirectives(t *testing.T) {
	root := t.TempDir()
	logical := filepath.Join(root, "logical.go")
	for _, directive := range []string{
		"//line " + logical + ":100",
		"/*line " + logical + ":100*/",
	} {
		fset := token.NewFileSet()
		source := "package fixture\nimport \"fmt\"\n" + directive + "\nfunc Caller() string { return fmt.Sprint(\"x\") }\n"
		file, err := parser.ParseFile(fset, filepath.Join(root, "physical.go"), source, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
		typed, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, []*ast.File{file}, info)
		if err != nil {
			t.Fatal(err)
		}
		pkg := &packages.Package{Types: typed, Syntax: []*ast.File{file}, TypesInfo: info}
		if !packageMayContainGraphVisibleGoCaller(pkg, fset, root, "", nil) {
			t.Fatalf("line-directed source was not conservatively retained: %q", directive)
		}
		logicalKey := ""
		for ident, obj := range info.Uses {
			if _, ok := obj.(*types.Func); ok {
				logicalKey = relativePath(fset.Position(ident.Pos()).Filename, root)
			}
		}
		funcIndex := map[string]*fileFuncIndex(nil)
		if strings.HasPrefix(directive, "//") {
			if logicalKey != "logical.go" {
				t.Fatalf("//line resolved function position to %q, want logical.go", logicalKey)
			}
			funcIndex = buildFileFuncIndexes(map[string][]*graph.Node{
				logicalKey: {{ID: "caller", Kind: graph.KindFunction, StartLine: 1, EndLine: 10000}},
			})
		}
		objToNode := make(map[types.Object]string, len(info.Uses))
		for _, obj := range info.Uses {
			objToNode[obj] = "caller"
			if _, ok := obj.(*types.Func); ok {
				objToNode[obj] = "target"
			}
		}
		if strings.HasPrefix(directive, "//") {
			for ident, obj := range info.Uses {
				if _, ok := obj.(*types.Func); ok {
					if _, ok := resolveGoUse(ident, obj, fset, root, "", funcIndex, objToNode, nil); !ok {
						t.Fatalf("//line function use did not resolve at %+v", fset.Position(ident.Pos()))
					}
				}
			}
		}
		plan, stats, err := projectGoUsesAndReleaseCompilerStateWithStats(
			context.Background(), []*packages.Package{pkg}, fset, root, "",
			funcIndex, objToNode, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.release()
		if stats.packagesScanned != 1 || stats.identsScanned == 0 || stats.packagesSkipped != 0 {
			t.Fatalf("line-directed source was skipped: %+v", stats)
		}
		if directive[:2] == "//" && plan.len() != 1 {
			t.Fatalf("//line caller use was not projected: %d", plan.len())
		}
	}
}

func TestPackageMayContainGraphVisibleGoCallerRetainsUsesWithoutSyntax(t *testing.T) {
	ident := &ast.Ident{Name: "x"}
	pkg := &packages.Package{TypesInfo: &types.Info{Uses: map[*ast.Ident]types.Object{
		ident: types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int]),
	}}}
	if !packageMayContainGraphVisibleGoCaller(pkg, token.NewFileSet(), t.TempDir(), "", nil) {
		t.Fatal("package with Uses but no Syntax was not retained")
	}

	pkg.TypesInfo.Uses = nil
	if packageMayContainGraphVisibleGoCaller(pkg, token.NewFileSet(), t.TempDir(), "", nil) {
		t.Fatal("package without Uses or Syntax was unexpectedly retained")
	}
}

func TestProjectionMixedStats(t *testing.T) {
	input := newMixedProjectionInput(t)
	plan, stats, err := projectGoUsesAndReleaseCompilerStateWithStats(
		context.Background(), input.pkgs, input.fset, input.root, "", input.funcIndex, input.objToNode, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.release()
	if plan.len() != 1 {
		t.Fatalf("projected uses = %d, want 1", plan.len())
	}
	if stats.packagesScanned != 1 || stats.packagesSkipped != 1 || stats.identsScanned == 0 || stats.identsSkipped == 0 {
		t.Fatalf("unexpected mixed projection stats: %+v", stats)
	}
}
