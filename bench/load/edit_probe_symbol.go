package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// The first symbol cell replaces an actual stable declaration with itself and
// the new marker sibling. The product already supports this text splice; the
// fixture does not invent a symbol-append operation or reuse marker presence.
func editProbeSymbolRequest(c Corpus, marker string) request {
	return request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": c.Symbol}, "match": c.EditAnchor, "replacement": c.EditAnchor + strings.ReplaceAll(c.EditTemplate, "{{marker}}", marker)}}
}
func editProbeSymbolAnchor(c Corpus) error {
	parts := strings.Split(c.Symbol, "::")
	if len(parts) != 2 || parts[0] != c.File || !token.IsIdentifier(parts[1]) {
		return errors.New("edit.symbol fixture requires an exact same-file top-level declaration")
	}
	name := parts[1]
	switch filepath.Ext(c.File) {
	case ".go":
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, c.File, c.EditInitialContent, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				source := c.EditInitialContent[set.Position(fn.Pos()).Offset:set.Position(fn.End()).Offset]
				if strings.TrimSpace(source) == strings.TrimSpace(c.EditAnchor) {
					return nil
				}
			}
		}
	case ".ts":
		valid, correct := editProbeTypeScriptFixture(c.EditAnchor, name, "")
		if valid && correct && strings.Count(c.EditAnchor, "function ") == 1 {
			return nil
		}
	case ".py":
		valid, correct := editProbePythonFixture(c.EditAnchor, name, "")
		if valid && correct && len(strings.Split(strings.TrimSpace(c.EditAnchor), "\n")) == 2 {
			return nil
		}
	}
	return errors.New("edit.symbol anchor is not the exact selected fixture declaration")
}
