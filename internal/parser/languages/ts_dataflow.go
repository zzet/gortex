package languages

import (
	"strconv"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// emitTSLocalBindings walks a TypeScript / JavaScript function body
// and materialises a KindLocal node for every introduced binding
// (`let x = …`, `const x = …`, `var x = …`, destructured shorthand,
// for-in/for-of induction vars, catch clause bindings, ...). Each
// binding gets:
//
//   - ID `<ownerID>#local:<name>@+<offsetFromOwnerStartLine>`
//     (function-relative offset like the Go walker, so an edit
//     above the function leaves the IDs stable),
//   - Name = the identifier,
//   - FilePath / StartLine = the binding's source position,
//   - EdgeMemberOf back to the enclosing function so the resolver's
//     scope-aware bare-name binding (#81) can find it by walking
//     the function's inbound EdgeMemberOf of KindLocal.
//
// TS doesn't (yet) have a dataflow walker analogous to
// emitGoDataflow, so no value_flow / arg_of / returns_to edges
// target these locals today. Their value is semantic parity with
// Go: every introduced binding is a first-class graph node with
// stable identity, ready for the dataflow / scope-resolution
// passes downstream. KindLocal is excluded from BM25 search via
// shouldIndexForSearch so the materialisation doesn't pollute name
// lookups with per-function `err` / `data` / `i` rows.
//
// Mirrors emitGoDataflow's bindLocal helper for the
// node-emission side; the walk shape is TypeScript-specific
// (different AST node types).
func emitTSLocalBindings(ownerID string, ownerStartLine int, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil || ownerID == "" {
		return
	}
	w := &tsBindingWalker{
		ownerID:        ownerID,
		ownerStartLine: ownerStartLine,
		filePath:       filePath,
		src:            src,
		result:         result,
		emitted:        map[string]struct{}{},
	}
	w.walk(body)
}

type tsBindingWalker struct {
	ownerID        string
	ownerStartLine int
	filePath       string
	src            []byte
	result         *parser.ExtractionResult
	emitted        map[string]struct{}
}

func (w *tsBindingWalker) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "function_declaration", "method_definition", "function", "arrow_function", "generator_function", "generator_function_declaration", "function_expression":
		// Don't descend into nested functions — their bindings
		// belong to the inner function's scope. The TS extractor's
		// own pass handles each inner function separately.
		return
	case "lexical_declaration", "variable_declaration":
		w.handleVarDecl(n)
		// Fall through to children for any nested expressions
		// (e.g. an initializer that contains a destructure pattern
		// is already captured by handleVarDecl; no extra walk).
		return
	case "for_in_statement", "for_of_statement":
		w.handleForInOf(n)
		// Continue into the body to pick up nested declarations.
		if body := n.ChildByFieldName("body"); body != nil {
			w.walk(body)
		}
		return
	case "catch_clause":
		w.handleCatchClause(n)
		if body := n.ChildByFieldName("body"); body != nil {
			w.walk(body)
		}
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		w.walk(n.NamedChild(i))
	}
}

// handleVarDecl visits `let`, `const`, `var` declarations and emits
// a KindLocal node per declarator. Each declarator's `name` field
// is either an identifier (simplest case) or a destructure pattern
// (object_pattern / array_pattern) — for patterns we descend and
// emit one node per shorthand identifier.
func (w *tsBindingWalker) handleVarDecl(decl *sitter.Node) {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "variable_declarator" {
			continue
		}
		name := c.ChildByFieldName("name")
		if name == nil {
			continue
		}
		w.emitFromPattern(name, int(decl.StartPoint().Row)+1)
	}
}

// handleForInOf visits `for (const x of items)` / `for (let k in obj)`
// and materialises the induction var(s) declared on the LHS.
func (w *tsBindingWalker) handleForInOf(n *sitter.Node) {
	left := n.ChildByFieldName("left")
	if left == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	switch left.Type() {
	case "lexical_declaration", "variable_declaration":
		w.handleVarDecl(left)
	case "identifier":
		w.bindLocal(left.Content(w.src), line)
	default:
		w.emitFromPattern(left, line)
	}
}

// handleCatchClause materialises the catch parameter (`catch (err)
// { ... }`). TS supports both an identifier and a destructure
// pattern as the catch binding.
func (w *tsBindingWalker) handleCatchClause(n *sitter.Node) {
	param := n.ChildByFieldName("parameter")
	if param == nil {
		return
	}
	w.emitFromPattern(param, int(n.StartPoint().Row)+1)
}

// emitFromPattern recursively visits a binding pattern (identifier
// at the leaf; object_pattern / array_pattern in the middle) and
// emits a KindLocal node for every leaf identifier. Shorthand
// (`{ a, b }`) and renamed (`{ a: aliased }`) both produce
// identifier leaves the walker handles uniformly.
func (w *tsBindingWalker) emitFromPattern(node *sitter.Node, line int) {
	visitJSPattern(node, w.src, func(name string) { w.bindLocal(name, line) })
}

// bindLocal emits the KindLocal node + owner edge. Idempotent on
// the binding ID so a name visited through more than one walk path
// produces exactly one node row.
func (w *tsBindingWalker) bindLocal(name string, line int) {
	if name == "" || name == "_" {
		return
	}
	offset := line
	if w.ownerStartLine > 0 {
		offset = line - w.ownerStartLine + 1
	}
	id := w.ownerID + "#local:" + name + "@+" + strconv.Itoa(offset)
	if _, ok := w.emitted[id]; ok {
		return
	}
	w.emitted[id] = struct{}{}
	// Language tag mirrors the file's source language; the
	// extractor's caller passes the file path so we recover it
	// from the suffix. Defaults to typescript when ambiguous.
	lang := "typescript"
	switch {
	case hasSuffix(w.filePath, ".tsx"):
		lang = "tsx"
	case hasSuffix(w.filePath, ".jsx"):
		lang = "javascript"
	case hasSuffix(w.filePath, ".js"), hasSuffix(w.filePath, ".mjs"), hasSuffix(w.filePath, ".cjs"):
		lang = "javascript"
	}
	w.result.Nodes = append(w.result.Nodes, &graph.Node{
		ID:        id,
		Kind:      graph.KindLocal,
		Name:      name,
		FilePath:  w.filePath,
		StartLine: line,
		EndLine:   line,
		Language:  lang,
	})
	w.result.Edges = append(w.result.Edges, &graph.Edge{
		From:     id,
		To:       w.ownerID,
		Kind:     graph.EdgeMemberOf,
		FilePath: w.filePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
	})
}

func hasSuffix(s, suf string) bool {
	if len(s) < len(suf) {
		return false
	}
	return s[len(s)-len(suf):] == suf
}
