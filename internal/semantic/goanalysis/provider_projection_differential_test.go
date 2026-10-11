package goanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
)

// TestProjectionNoCallerArtifact is deliberately valid for both the baseline
// and candidate implementations. It invokes the real projection helper with
// compiler-produced Uses from a type/interface-only source whose funcIndex is
// empty, then emits a deterministic artifact for an external harness to diff.
func TestProjectionNoCallerArtifact(t *testing.T) {
	root := t.TempDir()
	fileName := filepath.Join(root, "nocaller.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fileName, "package fixture\nimport \"io\"\ntype Contract interface { io.Reader }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	typed, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	usesBefore := len(info.Uses)
	pkgs := []*packages.Package{{Types: typed, TypesInfo: info, Syntax: []*ast.File{file}}}
	plan, err := projectGoUsesAndReleaseCompilerState(context.Background(), pkgs, fset, root, "", nil, map[types.Object]string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.release()
	if pkgs[0] != nil {
		t.Fatal("projection did not release the package")
	}
	payload := struct {
		InputUses     int  `json:"input_uses"`
		ProjectedUses int  `json:"projected_uses"`
		PackageNil    bool `json:"package_nil"`
	}{usesBefore, plan.len(), pkgs[0] == nil}
	artifact, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	t.Logf("semantic-skip-artifact=%s sha256=%s", artifact, hex.EncodeToString(digest[:]))
}

// TestProjectionVisibleCallerArtifact proves the function/method path still
// uses the exact projection helper in both builds. The caller index is built
// with the production index builder; only the graph persistence layer is
// replaced by a compact target-ID projection.
func TestProjectionVisibleCallerArtifact(t *testing.T) {
	root := t.TempDir()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, "caller.go"), "package fixture\nimport \"fmt\"\nfunc Caller() string { return fmt.Sprint(\"x\") }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	typed, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	funcIndex := buildFileFuncIndexes(map[string][]*graph.Node{
		"caller.go": {{ID: "caller", Kind: graph.KindFunction, StartLine: 1, EndLine: 1000}},
	})
	objToNode := make(map[types.Object]string, len(info.Uses))
	for _, obj := range info.Uses {
		objToNode[obj] = "caller"
		if _, ok := obj.(*types.Func); ok {
			objToNode[obj] = "target"
		}
	}
	pkgs := []*packages.Package{{Types: typed, TypesInfo: info, Syntax: []*ast.File{file}}}
	plan, err := projectGoUsesAndReleaseCompilerState(context.Background(), pkgs, fset, root, "", funcIndex, objToNode, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.release()
	if got := plan.len(); got != 1 {
		t.Fatalf("projected uses = %d, want 1", got)
	}
	payload := struct {
		ProjectedUses int  `json:"projected_uses"`
		PackageNil    bool `json:"package_nil"`
	}{plan.len(), pkgs[0] == nil}
	artifact, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	t.Logf("semantic-skip-visible-artifact=%s sha256=%s", artifact, hex.EncodeToString(digest[:]))
}

type mixedProjectionInput struct {
	pkgs      []*packages.Package
	fset      *token.FileSet
	root      string
	funcIndex map[string]*fileFuncIndex
	objToNode map[types.Object]string
}

func newMixedProjectionInput(t *testing.T) mixedProjectionInput {
	t.Helper()
	root := t.TempDir()
	fset := token.NewFileSet()
	callerFile, err := parser.ParseFile(fset, filepath.Join(root, "caller.go"), "package fixture\nimport \"fmt\"\nfunc Caller() string { return fmt.Sprint(\"x\") }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	callerInfo := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	callerTypes, err := (&types.Config{Importer: importer.Default()}).Check("caller", fset, []*ast.File{callerFile}, callerInfo)
	if err != nil {
		t.Fatal(err)
	}
	nocallerFile, err := parser.ParseFile(fset, filepath.Join(root, "nocaller.go"), "package fixture\nimport \"io\"\ntype Contract interface { io.Reader }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	nocallerInfo := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	nocallerTypes, err := (&types.Config{Importer: importer.Default()}).Check("nocaller", fset, []*ast.File{nocallerFile}, nocallerInfo)
	if err != nil {
		t.Fatal(err)
	}
	funcIndex := buildFileFuncIndexes(map[string][]*graph.Node{
		"caller.go": {{ID: "caller", Kind: graph.KindFunction, StartLine: 1, EndLine: 1000}},
	})
	objToNode := make(map[types.Object]string, len(callerInfo.Uses))
	for _, obj := range callerInfo.Uses {
		objToNode[obj] = "caller"
		if _, ok := obj.(*types.Func); ok {
			objToNode[obj] = "target"
		}
	}
	return mixedProjectionInput{
		pkgs: []*packages.Package{
			{Types: callerTypes, TypesInfo: callerInfo, Syntax: []*ast.File{callerFile}},
			{Types: nocallerTypes, TypesInfo: nocallerInfo, Syntax: []*ast.File{nocallerFile}},
		},
		fset: fset, root: root, funcIndex: funcIndex, objToNode: objToNode,
	}
}

func mixedUseArtifact(plan *goUsePlan) []byte {
	type use struct {
		Caller string `json:"caller"`
		Target string `json:"target"`
		File   string `json:"file"`
		Line   int    `json:"line"`
		Kind   string `json:"kind"`
	}
	uses := make([]use, 0, plan.len())
	for _, packageUses := range plan.packages {
		for _, projected := range packageUses {
			uses = append(uses, use{projected.callerID, projected.targetNodeID, projected.graphPath, projected.line, string(projected.kind)})
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].Caller != uses[j].Caller {
			return uses[i].Caller < uses[j].Caller
		}
		if uses[i].Target != uses[j].Target {
			return uses[i].Target < uses[j].Target
		}
		if uses[i].File != uses[j].File {
			return uses[i].File < uses[j].File
		}
		return uses[i].Line < uses[j].Line
	})
	artifact, _ := json.Marshal(uses)
	return artifact
}

func TestProjectionMixedArtifact(t *testing.T) {
	input := newMixedProjectionInput(t)
	plan, err := projectGoUsesAndReleaseCompilerState(context.Background(), input.pkgs, input.fset, input.root, "", input.funcIndex, input.objToNode, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.release()
	if len(plan.packages) != 2 || plan.len() != 1 || input.pkgs[0] != nil || input.pkgs[1] != nil {
		t.Fatalf("unexpected mixed projection: packages=%d uses=%d released=%t/%t", len(plan.packages), plan.len(), input.pkgs[0] == nil, input.pkgs[1] == nil)
	}
	artifact := mixedUseArtifact(plan)
	digest := sha256.Sum256(artifact)
	t.Logf("semantic-skip-mixed-artifact=%s sha256=%s", artifact, hex.EncodeToString(digest[:]))
}
