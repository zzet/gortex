package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitJSXRenderEdges walks a function/method/arrow body for JSX child
// components rendered inside it and emits one EdgeRendersChild per
// unique child component. The parent ID is the enclosing function's
// graph ID (passed in by the caller).
//
// Detection:
//   - jsx_element  → `<Foo>...</Foo>` — read opening element's name.
//   - jsx_self_closing_element → `<Foo />` — read its name.
//
// Naming convention: capital-first-letter element names are component
// references; lowercase names map to HTML/SVG primitives and are
// skipped (rendering edges to <div> and <span> would be pure noise).
// Member-expression names (`Foo.Bar`) preserve the qualified shape so
// the resolver can land them on `unresolved::Foo.Bar`.
//
// Dedup: the same component rendered in multiple branches of the same
// function emits one edge — the dependency is "this parent renders
// this child", and counting branches is a different question.
func emitJSXRenderEdges(parentID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil || parentID == "" {
		return
	}
	seen := make(map[string]int) // child name → first line
	walkAST(body, func(n *sitter.Node) bool {
		if n == nil {
			return true
		}
		switch n.Type() {
		case "jsx_element":
			opening := n.NamedChild(0)
			if opening != nil && opening.Type() == "jsx_opening_element" {
				if name := jsxElementName(opening, src); name != "" {
					if _, dup := seen[name]; !dup {
						seen[name] = int(n.StartPoint().Row) + 1
					}
				}
			}
		case "jsx_self_closing_element":
			if name := jsxElementName(n, src); name != "" {
				if _, dup := seen[name]; !dup {
					seen[name] = int(n.StartPoint().Row) + 1
				}
			}
		}
		return true
	})
	for name, line := range seen {
		if !isJSXComponentName(name) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     parentID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeRendersChild,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"child_name": name,
			},
		})
	}
}

// jsxElementName returns the bare or qualified element name from a
// jsx_opening_element / jsx_self_closing_element node. Returns "" for
// shapes the resolver can't act on (jsx_namespace_name, fragments
// `<>...</>`, member expressions deeper than two segments).
func jsxElementName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		// Fallback: walk children for the name token. tree-sitter
		// grammars vary on whether the field is exposed.
		for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
			c := node.NamedChild(i)
			if c != nil && (c.Type() == "identifier" || c.Type() == "member_expression" || c.Type() == "nested_identifier") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return ""
	}
	switch nameNode.Type() {
	case "identifier":
		return nameNode.Content(src)
	case "member_expression", "nested_identifier":
		// Two-segment qualified name: `Foo.Bar`.
		return strings.TrimSpace(nameNode.Content(src))
	}
	return ""
}

// isJSXComponentName reports whether name is a component reference (as
// opposed to an HTML/SVG primitive). React's convention: capital first
// letter or contains a `.` (member-access components).
func isJSXComponentName(name string) bool {
	if name == "" {
		return false
	}
	if strings.Contains(name, ".") {
		return true
	}
	first := name[0]
	return first >= 'A' && first <= 'Z'
}

// isPascalComponentName is the stricter marking gate for component
// detection: the function's own (last-dotted-segment) name must start
// with a capital letter. Unlike isJSXComponentName it does NOT treat a
// `.` as a free pass — an object-field arrow named `api.health` must be
// judged on `health`, not on the qualifying owner. This is what kills
// hooks (`useX`), handlers (`handleX`), and helpers (`renderRow`) by
// construction.
func isPascalComponentName(name string) bool {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

// functionDirectlyRendersJSX reports whether a function / arrow body
// renders JSX in its OWN scope. It is a SHALLOW walk: it returns true on
// the first jsx_element / jsx_self_closing_element / jsx_fragment, but it
// does NOT descend into a nested named-function scope (a
// function_declaration, method_definition, class_declaration, or an
// arrow / function expression bound to a variable_declarator) — those
// render into their own component. It DOES descend through inline
// anonymous arrows (`.map(i => <Item/>)`), conditionals, and
// jsx_expression_container, because those render in the parent's scope.
//
// This resolves the ancestor false positive: in
// `function App(){ const W = () => <div/>; return null }` App skips W's
// subtree, sees no JSX of its own, and is not marked; W is marked by its
// own emit site. It is a separate predicate from emitJSXRenderEdges,
// which keeps its full descent for EdgeRendersChild.
func functionDirectlyRendersJSX(body *sitter.Node) bool {
	// The emit sites pass varied entry nodes — a statement_block, a
	// concise arrow body, or (for the TS arrow query) the whole
	// arrow_function. Unwrap a function/arrow entry to its body so the
	// variable-bound-arrow stop below applies only to NESTED scopes, not
	// to the component we are testing itself.
	body = unwrapFunctionBody(body)
	var walk func(n *sitter.Node) bool
	walk = func(n *sitter.Node) bool {
		if n == nil {
			return false
		}
		switch n.Type() {
		case "jsx_element", "jsx_self_closing_element", "jsx_fragment":
			return true
		case "function_declaration", "generator_function_declaration",
			"method_definition", "class_declaration", "abstract_class_declaration", "class":
			// A nested named-function / class scope is its own component.
			return false
		case "arrow_function", "function_expression":
			// An arrow / function expression bound to a variable is its own
			// component (it has its own emit site); an inline anonymous one
			// (a `.map` callback, an event handler) renders in the parent.
			if isVariableBoundFunction(n) {
				return false
			}
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			if walk(n.Child(i)) {
				return true
			}
		}
		return false
	}
	return walk(body)
}

// isVariableBoundFunction reports whether an arrow / function expression
// is the value of a variable_declarator (`const W = () => …`), as
// opposed to an inline anonymous callback.
func isVariableBoundFunction(n *sitter.Node) bool {
	p := n.Parent()
	return p != nil && p.Type() == "variable_declarator"
}

// unwrapFunctionBody returns the body of a function/arrow node, or the
// node itself when it is not a function wrapper (already a body block or
// a concise expression).
func unwrapFunctionBody(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	switch n.Type() {
	case "arrow_function", "function_expression", "function",
		"function_declaration", "generator_function_declaration", "method_definition":
		if b := n.ChildByFieldName("body"); b != nil {
			return b
		}
	}
	return n
}

// jsxImportSource returns the module string of an import_statement with
// its surrounding quotes stripped, or "" when the source is absent.
func jsxImportSource(importStmt *sitter.Node, src []byte) string {
	s := importStmt.ChildByFieldName("source")
	if s == nil {
		return ""
	}
	return strings.Trim(s.Content(src), "'\"`")
}

// jsxFrameworkForSource maps an import module specifier to its
// component framework, or "" when the module is not a recognised
// JSX/component framework.
func jsxFrameworkForSource(s string) string {
	switch {
	case s == "react" || strings.HasPrefix(s, "react/") || s == "react-dom" || strings.HasPrefix(s, "react-dom/"):
		return "react"
	case s == "preact" || strings.HasPrefix(s, "preact/"):
		return "preact"
	case s == "solid-js" || strings.HasPrefix(s, "solid-js/"):
		return "solid"
	case strings.HasPrefix(s, "@builder.io/qwik"):
		return "qwik"
	case strings.HasPrefix(s, "@stencil/core"):
		return "stencil"
	}
	return ""
}

// jsxFrameworkFromImports resolves the UI-component framework for a file
// from its top-level import sources. JSX is not React-exclusive, so when
// no known framework import resolves it returns the neutral "jsx" — that
// still satisfies flavor:component but does not mis-attribute the file
// to React.
func jsxFrameworkFromImports(node *sitter.Node, src []byte) string {
	root := node
	for root != nil && root.Type() != "program" {
		p := root.Parent()
		if p == nil {
			break
		}
		root = p
	}
	if root == nil {
		return "jsx"
	}
	for i, _nc := 0, int(root.NamedChildCount()); i < _nc; i++ {
		stmt := root.NamedChild(i)
		if stmt == nil || stmt.Type() != "import_statement" {
			continue
		}
		if fw := jsxFrameworkForSource(jsxImportSource(stmt, src)); fw != "" {
			return fw
		}
	}
	return "jsx"
}

// markFunctionComponent stamps ui_component + component_kind onto a
// just-built function/arrow meta map when the symbol is a JSX-rendering
// PascalCase function component. root is any node in the file (used to
// resolve the framework from imports). No-op otherwise.
func markFunctionComponent(meta map[string]any, name string, body, root *sitter.Node, src []byte, componentKind string) {
	if meta == nil || !isPascalComponentName(name) || !functionDirectlyRendersJSX(body) {
		return
	}
	meta["ui_component"] = jsxFrameworkFromImports(root, src)
	meta["component_kind"] = componentKind
}

// jsxClassExtendsName returns the last-dotted-segment name of a class's
// `extends` base (`React.Component` → "Component"), across both the TS
// grammar (extends_clause wrapper) and the JS grammar (the base
// expression sits directly under class_heritage). Returns "" when the
// class has no extends base.
func jsxClassExtendsName(classNode *sitter.Node, src []byte) string {
	heritage := findChildByType(classNode, "class_heritage")
	if heritage == nil {
		return ""
	}
	lastSeg := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.LastIndexByte(s, '.'); i >= 0 {
			s = s[i+1:]
		}
		return s
	}
	for i, _nc := 0, int(heritage.NamedChildCount()); i < _nc; i++ {
		c := heritage.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "extends_clause" {
			if val := c.ChildByFieldName("value"); val != nil {
				return lastSeg(val.Content(src))
			}
		}
		if c.Type() == "identifier" || c.Type() == "member_expression" {
			return lastSeg(c.Content(src))
		}
	}
	return ""
}

// tsClassHasComponentDecorator reports whether a class declaration
// carries an `@Component` decorator (Angular). Decorators sit either as
// PrevSiblings of the class node or as its leading children, depending
// on the grammar version — scan both.
func tsClassHasComponentDecorator(classNode *sitter.Node, src []byte) bool {
	isComponent := func(dec *sitter.Node) bool {
		if dec == nil || dec.Type() != "decorator" {
			return false
		}
		name, _ := tsDecoratorNameAndArgs(dec, src)
		return name == "Component"
	}
	for sib := classNode.PrevSibling(); sib != nil && sib.Type() == "decorator"; sib = sib.PrevSibling() {
		if isComponent(sib) {
			return true
		}
	}
	for i, _nc := 0, int(classNode.ChildCount()); i < _nc; i++ {
		if isComponent(classNode.Child(i)) {
			return true
		}
	}
	return false
}

// classHeritageComponentUI maps a class's extends-base name to its UI
// component framework + sub-kind: React-family base classes resolve the
// framework from imports (component_kind=class); the Web-Components base
// classes are webcomponent (component_kind=custom_element). Returns
// ("","") for any other base.
func classHeritageComponentUI(extendsName string, classNode *sitter.Node, src []byte) (ui, kind string) {
	switch extendsName {
	case "Component", "PureComponent":
		return jsxFrameworkFromImports(classNode, src), "class"
	case "HTMLElement", "LitElement":
		return "webcomponent", "custom_element"
	}
	return "", ""
}
