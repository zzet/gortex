package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// The append-leaf cell adds one numeric, zero-argument leaf declaration. A
// comment, string literal containing a call, or an actual outgoing call cannot
// stand in for a leaf. Other fixture declarations may coexist in a file read.
func editProbeLeafSource(file, source, name string) bool {
	switch filepath.Ext(file) {
	case ".ts":
		valid, correct := editProbeTypeScriptFixture(source, name, "")
		return valid && correct
	case ".py":
		valid, correct := editProbePythonFixture(source, name, "")
		return valid && correct
	case ".go":
		parsed, err := parser.ParseFile(token.NewFileSet(), "leaf.go", source, 0)
		if err != nil {
			parsed, err = parser.ParseFile(token.NewFileSet(), "leaf.go", "package fixture\n"+source, 0)
		}
		if err != nil {
			return false
		}
		matches := 0
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name {
				continue
			}
			if fn.Recv != nil || fn.Type.Params.NumFields() != 0 || fn.Body == nil || len(fn.Body.List) != 1 {
				return false
			}
			ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return false
			}
			value, ok := ret.Results[0].(*ast.BasicLit)
			if !ok || value.Kind != token.INT || strings.Trim(value.Value, "0123456789") != "" {
				return false
			}
			matches++
		}
		return matches == 1
	}
	return false
}

func editProbeLeafRelations(body map[string]any, c Corpus, step editProbeStep) bool {
	truncated, ok := body["truncated"].(bool)
	if !ok || truncated {
		return false
	}
	nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
	edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
	if !nok || !eok || len(nodes) != 1 || len(edges) != 0 {
		return false
	}
	node, ok := nodes[0].(map[string]any)
	if !ok {
		return false
	}
	id, _ := node["id"].(string)
	return editProbeSourceID(id, c.File, step.Marker)
}
