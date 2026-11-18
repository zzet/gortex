package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allEdgesWrappers are the readers whose own AllEdges forwards to the reader
// they wrap and filters the answer. They are definitions rather than scans:
// each runs only when one of the sites below asks for every edge.
var allEdgesWrappers = map[string]int{
	"contract_core_composition.go|(*contractCoreEdges).AllEdges": 1,
	"view_request.go|(*baseGraphReader).AllEdges":                1,
}

// allEdgesDocumentedSites are this package's whole-store edge scans, counted
// per function. Each reads every edge on purpose — a whole-corpus analysis, or
// the fallback for a reader without the aggregate it would otherwise ask for —
// and says so in a comment directly above the statement that scans.
var allEdgesDocumentedSites = map[string]int{
	"contract_core_composition.go|(*contractAnalysisContext).composedReader": 1,
	"tools_analyze_edge_audit.go|(*Server).handleAnalyzeEdgeAudit":           1,
	"tools_architecture.go|architectureCrossRepo":                            1,
	"tools_outline.go|(*Server).buildRepoOutline":                            1,
	"tools_outline.go|mostImportedFiles":                                     1,
	"tools_surprising.go|(*Server).collectSurprisingEdges":                   2,
	"view_request.go|(*baseGraphReader).derivedStats":                        1,
}

// TestNonTestAllEdgesCallsAreDocumented keeps the whole-store edge scans in
// this package's production code countable. AllEdges reads every edge of
// every repository the store holds, so a tool that reaches it pays for each
// repository added, which is the cost scoped reads exist to avoid.
//
// Every non-test reference outside a wrapper's own AllEdges must therefore sit
// in a statement with a comment on the line directly above it, saying why the
// whole store has to be read, and the sites are pinned per function. A new
// scan fails here until it is replaced by a scoped read, or documented and
// listed.
func TestNonTestAllEdgesCallsAreDocumented(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	fset := token.NewFileSet()
	wrappers := make(map[string]int)
	sites := make(map[string]int)
	var undocumented []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		commentEnds := make(map[int]bool, len(file.Comments))
		for _, group := range file.Comments {
			commentEnds[fset.Position(group.End()).Line] = true
		}
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			selector, ok := n.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "AllEdges" {
				return true
			}
			decl, anchor := allEdgesSiteContext(stack)
			key := path + "|" + allEdgesFuncName(decl)
			if decl != nil && decl.Recv != nil && decl.Name.Name == "AllEdges" {
				wrappers[key]++
				return true
			}
			sites[key]++
			if line := fset.Position(anchor.Pos()).Line; !commentEnds[line-1] {
				undocumented = append(undocumented, path+":"+strconv.Itoa(fset.Position(selector.Pos()).Line))
			}
			return true
		})
	}
	if len(undocumented) > 0 {
		t.Errorf("whole-store edge scans with no comment above them saying why they must read every edge:\n  %s",
			strings.Join(undocumented, "\n  "))
	}
	if !maps.Equal(wrappers, allEdgesWrappers) {
		t.Errorf("AllEdges wrapper definitions = %v, want %v", wrappers, allEdgesWrappers)
	}
	if !maps.Equal(sites, allEdgesDocumentedSites) {
		t.Errorf("whole-store edge scans per function = %v, want %v; replace a new scan with a scoped read, or document it and list it",
			sites, allEdgesDocumentedSites)
	}
}

// allEdgesSiteContext finds, on the path from the file down to a reference,
// the function declaring it and the innermost statement or declaration holding
// it, which is what a comment documents.
func allEdgesSiteContext(stack []ast.Node) (*ast.FuncDecl, ast.Node) {
	var decl *ast.FuncDecl
	var anchor ast.Node
	for _, node := range stack {
		switch node := node.(type) {
		case *ast.FuncDecl:
			decl = node
			anchor = node
		case ast.Stmt, ast.Decl:
			anchor = node
		}
	}
	return decl, anchor
}

// allEdgesFuncName names a declaration the way the pinned maps spell it:
// "(*T).Method", "T.Method", or the bare function name.
func allEdgesFuncName(decl *ast.FuncDecl) string {
	if decl == nil {
		return "<package scope>"
	}
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	recv := decl.Recv.List[0].Type
	pointer := false
	if star, ok := recv.(*ast.StarExpr); ok {
		pointer, recv = true, star.X
	}
	switch generic := recv.(type) {
	case *ast.IndexExpr:
		recv = generic.X
	case *ast.IndexListExpr:
		recv = generic.X
	}
	name := "?"
	if ident, ok := recv.(*ast.Ident); ok {
		name = ident.Name
	}
	if pointer {
		return "(*" + name + ")." + decl.Name.Name
	}
	return name + "." + decl.Name.Name
}
