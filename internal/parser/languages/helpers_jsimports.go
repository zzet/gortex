package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// A same-named declaration is not necessarily the value imported by name:
// `export { other as work }` can coexist with a private `work`.
func jsNamedExportMeta(decl *sitter.Node, name string, localExports map[string]bool, meta map[string]any) map[string]any {
	if decl == nil {
		return meta
	}
	parent := decl.Parent()
	if parent == nil {
		return meta
	}
	exported := false
	switch {
	case parent.Type() == "export_statement" && findChildByType(parent, "default") == nil:
		if scope := parent.Parent(); scope != nil && scope.Type() == "program" {
			exported = true
		}
	case localExports[name] && decl.Type() != "function_expression" && decl.Type() != "generator_function":
		if parent.Type() == "program" {
			exported = true
		} else if parent.Type() == "export_statement" && parent.Parent() != nil && parent.Parent().Type() == "program" {
			exported = true
		}
	}
	if !exported {
		return meta
	}
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["js_named_export"] = true
	return meta
}

// jsLocalExportSpecifierName returns the local binding name of a local
// `export_specifier` when it is exported under its own name (`export { init }`,
// `export { init as init }`, `export { init as "init" }`), or "" when renamed
// (including `export { init as default }` and `export { init as "other" }`).
func jsLocalExportSpecifierName(sp *sitter.Node, src []byte) string {
	if sp == nil {
		return ""
	}
	nameNode := sp.ChildByFieldName("name")
	if nameNode == nil {
		for i := 0; i < int(sp.NamedChildCount()); i++ {
			if c := sp.NamedChild(i); c != nil && c.Type() == "identifier" {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil || nameNode.Type() != "identifier" {
		return ""
	}
	orig := nameNode.Content(src)
	if orig == "" {
		return ""
	}
	aliasNode := sp.ChildByFieldName("alias")
	hasAs := aliasNode != nil
	if !hasAs {
		seenAs := false
		for i := 0; i < int(sp.ChildCount()); i++ {
			c := sp.Child(i)
			if c == nil || c.Type() == "comment" {
				continue
			}
			if !seenAs {
				if !c.IsNamed() && c.Type() == "as" {
					seenAs = true
					hasAs = true
				}
				continue
			}
			aliasNode = c
			break
		}
	}
	if !hasAs {
		return orig
	}
	if aliasNode == nil {
		return ""
	}
	var aliasName string
	switch aliasNode.Type() {
	case "identifier":
		aliasName = aliasNode.Content(src)
	case "string":
		aliasName = jsSingleStringContent(aliasNode, src)
	default:
		aliasName = aliasNode.Content(src)
	}
	if aliasName != orig {
		return ""
	}
	return orig
}

// jsLocalExportClauseNames collects the names exported unrenamed by top-level
// local `export { foo }` statements (without `from`). Returns nil when no such
// clauses exist in root.
func jsLocalExportClauseNames(root *sitter.Node, src []byte) map[string]bool {
	if root == nil {
		return nil
	}
	var exported map[string]bool
	for i := 0; i < int(root.NamedChildCount()); i++ {
		stmt := root.NamedChild(i)
		if stmt == nil || stmt.Type() != "export_statement" || stmt.ChildByFieldName("source") != nil {
			continue
		}
		if tsExportStatementTypeOnly(stmt, src) {
			continue
		}
		clause := findChildByType(stmt, "export_clause")
		if clause == nil {
			continue
		}
		for _, sp := range jsCollectChildren(clause, "export_specifier") {
			if tsSpecifierTypeOnly(sp, src) {
				continue
			}
			orig := jsLocalExportSpecifierName(sp, src)
			if orig == "" {
				continue
			}
			if exported == nil {
				exported = make(map[string]bool)
			}
			exported[orig] = true
		}
	}
	return exported
}

func visitJSPattern(n *sitter.Node, src []byte, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "identifier", "type_identifier", "shorthand_property_identifier_pattern":
		visit(n.Content(src))
	case "required_parameter", "optional_parameter":
		visitJSPattern(n.ChildByFieldName("pattern"), src, visit)
	case "pair_pattern":
		visitJSPattern(n.ChildByFieldName("value"), src, visit)
	case "assignment_pattern", "object_assignment_pattern":
		visitJSPattern(n.ChildByFieldName("left"), src, visit)
	case "formal_parameters", "object_pattern", "array_pattern", "rest_pattern":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			visitJSPattern(n.NamedChild(i), src, visit)
		}
	}
}

// jsSingleStringContent returns the non-empty text of a plain string literal
// with a single string_fragment child, or "".
func jsSingleStringContent(n *sitter.Node, src []byte) string {
	if n == nil || n.Type() != "string" || n.NamedChildCount() != 1 {
		return ""
	}
	frag := n.NamedChild(0)
	if frag == nil || frag.Type() != "string_fragment" {
		return ""
	}
	return frag.Content(src)
}

// jsStaticRequireCall returns the static string specifier of a single-argument
// require("...") call, ignoring comments in the argument list.
func jsStaticRequireCall(call *sitter.Node, src []byte) string {
	if call == nil || call.Type() != "call_expression" {
		return ""
	}
	callee := call.ChildByFieldName("function")
	if callee == nil || callee.Type() != "identifier" || callee.Content(src) != "require" {
		return ""
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	var arg *sitter.Node
	for i := 0; i < int(args.NamedChildCount()); i++ {
		c := args.NamedChild(i)
		if c == nil || c.Type() == "comment" {
			continue
		}
		if arg != nil {
			return ""
		}
		arg = c
	}
	return jsSingleStringContent(arg, src)
}

func visitJSImportBindings(importStmt *sitter.Node, src []byte, visit func(string)) {
	if importStmt == nil || importStmt.Type() != "import_statement" || tsImportStatementTypeOnly(importStmt, src) {
		return
	}
	visitNamed := func(named *sitter.Node) {
		for _, sp := range jsCollectChildren(named, "import_specifier") {
			if tsSpecifierTypeOnly(sp, src) {
				continue
			}
			orig, alias := jsSpecifierNames(sp, src)
			if alias != "" {
				visit(alias)
			} else if orig != "" {
				visit(orig)
			}
		}
	}
	if clause := findChildByType(importStmt, "import_clause"); clause != nil {
		for i := 0; i < int(clause.NamedChildCount()); i++ {
			c := clause.NamedChild(i)
			switch c.Type() {
			case "identifier":
				visit(c.Content(src))
			case "namespace_import":
				if id := findChildByType(c, "identifier"); id != nil {
					visit(id.Content(src))
				}
			case "named_imports":
				visitNamed(c)
			}
		}
	} else if named := findChildByType(importStmt, "named_imports"); named != nil {
		visitNamed(named)
	}
	if reqClause := findChildByType(importStmt, "import_require_clause"); reqClause != nil {
		if id := findChildByType(reqClause, "identifier"); id != nil {
			visit(id.Content(src))
		}
	}
}

func firstNonCommentNamedChild(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(i); c != nil && c.Type() != "comment" {
			return c
		}
	}
	return nil
}

// tsSingleUsingAssignment returns the assignment_expression of a single
// TypeScript `using x = ...` or `await using x = ...` binding (tree-sitter-typescript
// attaches an anonymous "using" token to assignment_expression instead of
// emitting JS's using_declaration).
func tsSingleUsingAssignment(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() == "await_expression" {
		n = firstNonCommentNamedChild(n)
	}
	if n == nil || n.Type() != "assignment_expression" {
		return nil
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		c := n.Child(i)
		if c == nil || c.Type() == "comment" {
			continue
		}
		if !c.IsNamed() && c.Type() == "using" {
			return n
		}
		return nil
	}
	return nil
}

func flattenJSSequence(n *sitter.Node, out *[]*sitter.Node) {
	if n == nil {
		return
	}
	if n.Type() == "sequence_expression" {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c != nil && c.Type() != "comment" {
				flattenJSSequence(c, out)
			}
		}
		return
	}
	*out = append(*out, n)
}

// visitTSUsingAssignments visits every assignment_expression belonging to a
// single- or multi-declarator TypeScript `using` / `await using` statement
// (including `sequence_expression` chains like `using a = mk(), b = mk()`).
func visitTSUsingAssignments(n *sitter.Node, visit func(*sitter.Node)) bool {
	if n == nil {
		return false
	}
	if n.Type() == "expression_statement" {
		n = firstNonCommentNamedChild(n)
	}
	if n == nil {
		return false
	}
	if n.Type() == "sequence_expression" {
		var elems []*sitter.Node
		flattenJSSequence(n, &elems)
		if len(elems) == 0 {
			return false
		}
		first := tsSingleUsingAssignment(elems[0])
		if first == nil {
			return false
		}
		visit(first)
		for _, el := range elems[1:] {
			if el != nil && el.Type() == "assignment_expression" {
				visit(el)
			}
		}
		return true
	}
	if assign := tsSingleUsingAssignment(n); assign != nil {
		visit(assign)
		return true
	}
	return false
}

func visitJSNamespaceName(n *sitter.Node, src []byte, visit func(string)) {
	for n != nil && (n.Type() == "nested_identifier" || n.Type() == "member_expression") {
		next := n.ChildByFieldName("object")
		if next == nil {
			next = firstNonCommentNamedChild(n)
		}
		n = next
	}
	if n != nil && n.Type() == "identifier" {
		visit(n.Content(src))
	}
}

func isJSBlockVarScope(block *sitter.Node) bool {
	p := block.Parent()
	if p == nil {
		return false
	}
	switch p.Type() {
	case "class_static_block", "internal_module", "module":
		return true
	}
	return false
}

// visitJSAllDeclarations visits every binding declared by n without exempting
// require() initializers, so checking whether `require` itself is shadowed
// never recurses into require matching.
func visitJSAllDeclarations(n *sitter.Node, src []byte, includeAmbient bool, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "ambient_declaration":
		if includeAmbient {
			visitJSAllDeclarations(firstNonCommentNamedChild(n), src, includeAmbient, visit)
		}
	case "export_statement":
		visitJSAllDeclarations(n.ChildByFieldName("declaration"), src, includeAmbient, visit)
	case "expression_statement":
		if visitTSUsingAssignments(n, func(assign *sitter.Node) {
			visitJSPattern(assign.ChildByFieldName("left"), src, visit)
		}) {
			return
		}
		if child := firstNonCommentNamedChild(n); child != nil && (child.Type() == "internal_module" || child.Type() == "module") {
			visitJSNamespaceName(child.ChildByFieldName("name"), src, visit)
		}
	case "sequence_expression", "await_expression", "assignment_expression":
		visitTSUsingAssignments(n, func(assign *sitter.Node) {
			visitJSPattern(assign.ChildByFieldName("left"), src, visit)
		})
	case "internal_module", "module":
		visitJSNamespaceName(n.ChildByFieldName("name"), src, visit)
	case "import_alias":
		if id := findChildByType(n, "identifier"); id != nil {
			visit(id.Content(src))
		}
	case "function_signature":
		if includeAmbient {
			visitJSPattern(n.ChildByFieldName("name"), src, visit)
		}
	case "function_declaration", "generator_function_declaration", "class_declaration", "abstract_class_declaration", "enum_declaration":
		visitJSPattern(n.ChildByFieldName("name"), src, visit)
	case "lexical_declaration", "variable_declaration", "using_declaration":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			v := n.NamedChild(i)
			if v.Type() == "variable_declarator" {
				visitJSPattern(v.ChildByFieldName("name"), src, visit)
			}
		}
	}
}

func visitJSAllHoistedVars(n *sitter.Node, src []byte, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "variable_declaration":
		visitJSAllDeclarations(n, src, false, visit)
		return
	case "for_in_statement":
		if kind := n.ChildByFieldName("kind"); kind != nil && kind.Type() == "var" {
			visitJSPattern(n.ChildByFieldName("left"), src, visit)
		}
	case "program", "export_statement", "statement_block", "if_statement", "else_clause",
		"switch_statement", "switch_body", "switch_case", "switch_default",
		"for_statement", "while_statement", "do_statement",
		"try_statement", "catch_clause", "finally_clause", "with_statement", "labeled_statement":
	default:
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		visitJSAllHoistedVars(n.NamedChild(i), src, visit)
	}
}

func visitJSRequireObjectPattern(pat *sitter.Node, spec string, src []byte, local func(string), required func(string, string)) {
	for i := 0; i < int(pat.NamedChildCount()); i++ {
		c := pat.NamedChild(i)
		switch c.Type() {
		case "shorthand_property_identifier_pattern":
			required(c.Content(src), spec)
		case "object_assignment_pattern":
			left := c.ChildByFieldName("left")
			if left != nil && left.Type() == "shorthand_property_identifier_pattern" {
				required(left.Content(src), spec)
			} else {
				visitJSPattern(left, src, local)
			}
		case "pair_pattern":
			key := c.ChildByFieldName("key")
			val := c.ChildByFieldName("value")
			target := val
			if target != nil && target.Type() == "assignment_pattern" {
				target = target.ChildByFieldName("left")
			}
			var keyName string
			if key != nil {
				switch key.Type() {
				case "property_identifier":
					keyName = key.Content(src)
				case "string":
					keyName = jsSingleStringContent(key, src)
				}
			}
			if keyName != "" && target != nil && target.Type() == "identifier" && target.Content(src) == keyName {
				required(keyName, spec)
			} else {
				visitJSPattern(val, src, local)
			}
		default:
			visitJSPattern(c, src, local)
		}
	}
}

func (cache *jsScopeCache) isRequireShadowed(expr *sitter.Node, src []byte) bool {
	inDecorator := false
	for child, n := expr, expr.Parent(); n != nil; child, n = n, n.Parent() {
		if child.Type() == "decorator" {
			inDecorator = true
		}
		kind := n.Type()
		if kind == "class_body" {
			inDecorator = false
			continue
		}
		switch kind {
		case "program", "statement_block", "switch_body", "class",
			"function_declaration", "generator_function_declaration", "method_definition",
			"function_expression", "generator_function", "arrow_function",
			"catch_clause", "for_statement", "for_in_statement":
		default:
			continue
		}
		body := child.Type() == "statement_block"
		// Computed method keys, method decorators, and parameter decorators
		// evaluate outside the method's parameter scope; class decorators
		// evaluate outside the class name.
		if kind == "method_definition" && (inDecorator || (!body && child.Type() != "formal_parameters")) {
			inDecorator = false
			continue
		}
		if kind == "class" && child.Type() == "decorator" {
			inDecorator = false
			continue
		}
		inDecorator = false
		key := jsScopeKey{n.StartByte(), n.EndByte(), kind, body}
		shadowed, ok := cache.requireShadow[key]
		if !ok {
			check := func(id string) {
				if id == "require" {
					shadowed = true
				}
			}
			switch kind {
			case "program":
				visitJSAllHoistedVars(n, src, check)
				for i := 0; i < int(n.NamedChildCount()) && !shadowed; i++ {
					s := n.NamedChild(i)
					if s.Type() == "import_statement" {
						visitJSImportBindings(s, src, check)
						continue
					}
					if s.Type() != "variable_declaration" {
						visitJSAllDeclarations(s, src, false, check)
					}
				}
			case "statement_block":
				if isJSBlockVarScope(n) {
					visitJSAllHoistedVars(n, src, check)
				}
				for i := 0; i < int(n.NamedChildCount()) && !shadowed; i++ {
					s := n.NamedChild(i)
					if s.Type() != "variable_declaration" {
						visitJSAllDeclarations(s, src, true, check)
					}
				}
			case "switch_body":
				for i := 0; i < int(n.NamedChildCount()) && !shadowed; i++ {
					clause := n.NamedChild(i)
					for j := 0; j < int(clause.NamedChildCount()) && !shadowed; j++ {
						s := clause.NamedChild(j)
						if s.Type() != "variable_declaration" {
							visitJSAllDeclarations(s, src, true, check)
						}
					}
				}
			case "for_statement", "for_in_statement":
				if init := n.ChildByFieldName("initializer"); init != nil && init.Type() != "variable_declaration" {
					visitJSAllDeclarations(init, src, true, check)
				}
				if k := n.ChildByFieldName("kind"); k != nil && k.Type() != "var" {
					visitJSPattern(n.ChildByFieldName("left"), src, check)
				}
			case "class":
				visitJSPattern(n.ChildByFieldName("name"), src, check)
			default:
				if body && kind != "catch_clause" {
					visitJSAllHoistedVars(child, src, check)
				}
				if kind == "function_expression" || kind == "generator_function" {
					visitJSPattern(n.ChildByFieldName("name"), src, check)
				}
				visitJSPattern(n.ChildByFieldName("parameters"), src, check)
				visitJSPattern(n.ChildByFieldName("parameter"), src, check)
			}
			if cache.requireShadow == nil {
				cache.requireShadow = map[jsScopeKey]bool{}
			}
			cache.requireShadow[key] = shadowed
		}
		if shadowed {
			return true
		}
	}
	return false
}

// staticRequireSpec returns the static module specifier of call only when
// `require` is not lexically shadowed in any enclosing scope (including
// module scope) and receives a single non-empty string literal.
func (cache *jsScopeCache) staticRequireSpec(call *sitter.Node, src []byte) string {
	spec := jsStaticRequireCall(call, src)
	if spec == "" || cache.isRequireShadowed(call, src) {
		return ""
	}
	return spec
}

func (cache *jsScopeCache) visitBinding(name, value *sitter.Node, src []byte, local func(string), required func(string, string)) {
	if name == nil {
		return
	}
	if value != nil {
		switch value.Type() {
		case "call_expression":
			if spec := cache.staticRequireSpec(value, src); spec != "" {
				switch name.Type() {
				case "identifier":
					required(name.Content(src), spec)
					return
				case "object_pattern":
					visitJSRequireObjectPattern(name, spec, src, local, required)
					return
				}
			}
		case "member_expression":
			prop := value.ChildByFieldName("property")
			if name.Type() == "identifier" && prop != nil && prop.Type() == "property_identifier" && name.Content(src) == prop.Content(src) {
				if spec := cache.staticRequireSpec(value.ChildByFieldName("object"), src); spec != "" {
					required(name.Content(src), spec)
					return
				}
			}
		}
	}
	visitJSPattern(name, src, local)
}

func (cache *jsScopeCache) visitDeclarator(v *sitter.Node, src []byte, local func(string), required func(string, string)) {
	cache.visitBinding(v.ChildByFieldName("name"), v.ChildByFieldName("value"), src, local, required)
}

func (cache *jsScopeCache) visitDeclaration(n *sitter.Node, src []byte, local func(string), required func(string, string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "ambient_declaration":
		cache.visitDeclaration(firstNonCommentNamedChild(n), src, local, required)
	case "export_statement":
		cache.visitDeclaration(n.ChildByFieldName("declaration"), src, local, required)
	case "expression_statement":
		if visitTSUsingAssignments(n, func(assign *sitter.Node) {
			cache.visitBinding(assign.ChildByFieldName("left"), assign.ChildByFieldName("right"), src, local, required)
		}) {
			return
		}
		if child := firstNonCommentNamedChild(n); child != nil && (child.Type() == "internal_module" || child.Type() == "module") {
			visitJSNamespaceName(child.ChildByFieldName("name"), src, local)
		}
	case "sequence_expression", "await_expression", "assignment_expression":
		visitTSUsingAssignments(n, func(assign *sitter.Node) {
			cache.visitBinding(assign.ChildByFieldName("left"), assign.ChildByFieldName("right"), src, local, required)
		})
	case "internal_module", "module":
		visitJSNamespaceName(n.ChildByFieldName("name"), src, local)
	case "import_alias":
		if id := findChildByType(n, "identifier"); id != nil {
			local(id.Content(src))
		}
	case "function_signature", "function_declaration", "generator_function_declaration", "class_declaration", "abstract_class_declaration", "enum_declaration":
		visitJSPattern(n.ChildByFieldName("name"), src, local)
	case "lexical_declaration", "variable_declaration", "using_declaration":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			v := n.NamedChild(i)
			if v.Type() == "variable_declarator" {
				cache.visitDeclarator(v, src, local, required)
			}
		}
	}
}

// var crosses statement-block boundaries but not function or class boundaries.
// Only descend through statement containers: expressions cannot introduce a
// var in this scope, even when they contain an entire function expression.
func (cache *jsScopeCache) visitHoistedVars(n *sitter.Node, src []byte, local func(string), required func(string, string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "variable_declaration":
		cache.visitDeclaration(n, src, local, required)
		return
	case "for_in_statement":
		if kind := n.ChildByFieldName("kind"); kind != nil && kind.Type() == "var" {
			visitJSPattern(n.ChildByFieldName("left"), src, local)
		}
	case "export_statement", "statement_block", "if_statement", "else_clause", "switch_statement", "switch_body",
		"switch_case", "switch_default", "for_statement", "while_statement", "do_statement",
		"try_statement", "catch_clause", "finally_clause", "with_statement", "labeled_statement":
	default:
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		cache.visitHoistedVars(n.NamedChild(i), src, local, required)
	}
}

func (cache *jsScopeCache) visitBlockDeclaration(n *sitter.Node, src []byte, local func(string), required func(string, string)) {
	if n != nil && n.Type() != "variable_declaration" {
		cache.visitDeclaration(n, src, local, required)
	}
}

// Cache direct declarations once per scope. A sibling function's declarations
// cannot shadow this call; parameters of every enclosing function can. Module
// scope is not recorded: an ES import cannot share it with a same-named
// declaration, and the resolver's same-file tier already handles the rest.
type jsScopeKey struct {
	start, end uint32
	kind       string
	body       bool
}
type jsBinding struct {
	kind string
	spec string
}
type jsScopeCache struct {
	scopes        map[jsScopeKey]map[string]jsBinding
	requireShadow map[jsScopeKey]bool
}

func (cache *jsScopeCache) stamp(edge *graph.Edge, expr *sitter.Node, name string, src []byte) {
	inDecorator := false
	for child, n := expr, expr.Parent(); n != nil; child, n = n, n.Parent() {
		if child.Type() == "decorator" {
			inDecorator = true
		}
		kind := n.Type()
		if kind == "class_body" {
			inDecorator = false
			continue
		}
		switch kind {
		case "statement_block", "switch_body", "class",
			"function_declaration", "generator_function_declaration", "method_definition",
			"function_expression", "generator_function", "arrow_function",
			"catch_clause", "for_statement", "for_in_statement":
		default:
			continue
		}
		// The body sees hoisted var declarations; parameter initializers do
		// not. Cache the two environments separately for the same function.
		body := child.Type() == "statement_block"
		if kind == "method_definition" && (inDecorator || (!body && child.Type() != "formal_parameters")) {
			inDecorator = false
			continue
		}
		if kind == "class" && child.Type() == "decorator" {
			inDecorator = false
			continue
		}
		inDecorator = false
		key := jsScopeKey{n.StartByte(), n.EndByte(), kind, body}
		names, ok := cache.scopes[key]
		if !ok {
			add := func(id string, b jsBinding) {
				if names == nil {
					names = map[string]jsBinding{}
				}
				if prev, exists := names[id]; exists && prev != b {
					if prev.kind == graph.JSPendingParameter || b.kind == graph.JSPendingParameter {
						b = jsBinding{kind: graph.JSPendingParameter}
					} else {
						// Conflicting local/require or differing require specs
						// cannot be trusted to name a single module binding.
						b = jsBinding{kind: graph.JSLexicalLocal}
					}
				}
				names[id] = b
			}
			local := func(id string) { add(id, jsBinding{kind: graph.JSLexicalLocal}) }
			required := func(id, spec string) { add(id, jsBinding{kind: graph.JSLexicalRequire, spec: spec}) }
			switch kind {
			case "statement_block":
				// Class static blocks and TS namespaces are their own var scopes.
				if isJSBlockVarScope(n) {
					cache.visitHoistedVars(n, src, local, required)
				}
				for i := 0; i < int(n.NamedChildCount()); i++ {
					cache.visitBlockDeclaration(n.NamedChild(i), src, local, required)
				}
			case "switch_body":
				for i := 0; i < int(n.NamedChildCount()); i++ {
					clause := n.NamedChild(i)
					for j := 0; j < int(clause.NamedChildCount()); j++ {
						cache.visitBlockDeclaration(clause.NamedChild(j), src, local, required)
					}
				}
			case "for_statement", "for_in_statement":
				cache.visitBlockDeclaration(n.ChildByFieldName("initializer"), src, local, required)
				if k := n.ChildByFieldName("kind"); k != nil && k.Type() != "var" {
					visitJSPattern(n.ChildByFieldName("left"), src, local)
				}
			case "class":
				visitJSPattern(n.ChildByFieldName("name"), src, local)
			default:
				param := func(id string) { add(id, jsBinding{kind: graph.JSPendingParameter}) }
				if body && kind != "catch_clause" {
					cache.visitHoistedVars(child, src, local, required)
				}
				if kind == "function_expression" || kind == "generator_function" {
					visitJSPattern(n.ChildByFieldName("name"), src, local)
				}
				visitJSPattern(n.ChildByFieldName("parameters"), src, param)
				visitJSPattern(n.ChildByFieldName("parameter"), src, param)
			}
			if cache.scopes == nil {
				cache.scopes = map[jsScopeKey]map[string]jsBinding{}
			}
			cache.scopes[key] = names
		}
		if binding, found := names[name]; found {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta[graph.JSLexicalBindingMeta] = binding.kind
			if binding.spec != "" {
				edge.Meta[graph.JSLexicalRequireSpecMeta] = binding.spec
			}
			return
		}
	}
}

func jsLexicalRank(e *graph.Edge) int {
	switch e.Meta[graph.JSLexicalBindingMeta] {
	case nil:
		return 0
	case graph.JSLexicalRequire:
		return 1
	case graph.JSLexicalLocal:
		return 2
	default:
		return 3
	}
}

// Calls with one From/To/Line identity collapse into one stored edge. When a
// less-shadowed call shares that site with a shadowed one, normalize every
// colliding edge to the same deterministic least-shadowed binding so last-wins
// in-memory stores and first-wins SQLite stores agree.
func clearCollapsedJSShadows(edges []*graph.Edge) {
	var hasStamped bool
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && e.Meta[graph.JSLexicalBindingMeta] != nil {
			hasStamped = true
			break
		}
	}
	if !hasStamped {
		return
	}
	best := make(map[graph.EdgeIdentity]*graph.Edge)
	byID := make(map[graph.EdgeIdentity][]*graph.Edge)
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		id := graph.EdgeIdentityFor(e)
		byID[id] = append(byID[id], e)
		if cur, ok := best[id]; !ok || jsLexicalRank(e) < jsLexicalRank(cur) {
			best[id] = e
		}
	}
	for id, group := range byID {
		if len(group) < 2 {
			continue
		}
		winner := best[id]
		winnerRank := jsLexicalRank(winner)
		if winnerRank == 3 {
			continue
		}
		// Two colliding local require() calls on one line that disagree on
		// their module specifier cannot pick one deterministically across
		// stores; downgrade the whole group to JSLexicalLocal.
		if winnerRank == 1 {
			spec, _ := winner.Meta[graph.JSLexicalRequireSpecMeta].(string)
			ambiguousSpec := false
			for _, e := range group {
				if jsLexicalRank(e) != 1 {
					continue
				}
				if s, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string); s != spec {
					ambiguousSpec = true
					break
				}
			}
			if ambiguousSpec {
				for _, e := range group {
					if e.Meta == nil {
						e.Meta = map[string]any{}
					}
					e.Meta[graph.JSLexicalBindingMeta] = graph.JSLexicalLocal
					delete(e.Meta, graph.JSLexicalRequireSpecMeta)
				}
				continue
			}
		}
		for _, e := range group {
			if jsLexicalRank(e) <= winnerRank {
				continue
			}
			switch winnerRank {
			case 0:
				delete(e.Meta, graph.JSLexicalBindingMeta)
				delete(e.Meta, graph.JSLexicalRequireSpecMeta)
			case 1:
				e.Meta[graph.JSLexicalBindingMeta] = winner.Meta[graph.JSLexicalBindingMeta]
				e.Meta[graph.JSLexicalRequireSpecMeta] = winner.Meta[graph.JSLexicalRequireSpecMeta]
			case 2:
				e.Meta[graph.JSLexicalBindingMeta] = graph.JSLexicalLocal
				delete(e.Meta, graph.JSLexicalRequireSpecMeta)
			}
		}
	}
}

// jsImportBindingCap bounds how many per-binding import / re-export edges a
// single statement may emit. Barrel files (`export { ... } from`) routinely
// re-export hundreds of names; without a cap one statement could add hundreds
// of edges and dwarf the rest of the file's graph. Over the cap we keep only
// the module-level edge so the dependency is still recorded.
const jsImportBindingCap = 64

// jsSpecifierNames reads an import_specifier / export_specifier and returns its
// upstream (original) name and its renamed name. The grammar lays both out
// positionally: the first identifier is the name in the source module, the
// second (present only for `x as y`) is the local alias (imports) or the
// exported name (re-exports). Returns ("", "") when no identifier is present.
func jsSpecifierNames(spec *sitter.Node, src []byte) (orig, alias string) {
	var ids []*sitter.Node
	for i, _nc := 0, int(spec.NamedChildCount()); i < _nc; i++ {
		if c := spec.NamedChild(i); c != nil && c.Type() == "identifier" {
			ids = append(ids, c)
		}
	}
	if len(ids) > 0 {
		orig = ids[0].Content(src)
	}
	if len(ids) > 1 {
		alias = ids[1].Content(src)
	}
	return orig, alias
}

// jsCollectChildren returns every direct child of node whose type matches t.
func jsCollectChildren(node *sitter.Node, t string) []*sitter.Node {
	var out []*sitter.Node
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		if c := node.Child(i); c != nil && c.Type() == t {
			out = append(out, c)
		}
	}
	return out
}

// jsNamedImportsNode locates the named_imports node of an import_statement.
// The grammar nests it under import_clause (`import { a } from …`), but tolerate
// it appearing directly under the statement across grammar revisions.
func jsNamedImportsNode(importNode *sitter.Node) *sitter.Node {
	if n := findChildByType(importNode, "named_imports"); n != nil {
		return n
	}
	if clause := findChildByType(importNode, "import_clause"); clause != nil {
		return findChildByType(clause, "named_imports")
	}
	return nil
}

// emitJSPerBindingImports emits one EdgeImports per named binding of an import
// statement — `import { foo, bar as baz } from "mod"` yields an edge to
// unresolved::import::mod::foo and one to unresolved::import::mod::bar with
// Alias "baz". The module-level import edge is emitted separately by the
// caller and left untouched; these per-binding edges let find_usages and
// dependency analysis answer "who imports `foo` from mod" instead of only
// "who imports mod". Over jsImportBindingCap bindings the per-binding edges are
// skipped (the module edge still records the dependency).
func emitJSPerBindingImports(importNode *sitter.Node, importPath, fileID, filePath string, src []byte, result *parser.ExtractionResult) {
	// Vue template tags refer to the local default binding, which need not
	// match the component filename. Retain it on the module edge through resolve.
	if strings.Contains(filePath, ".vue#script:") {
		if clause := findChildByType(importNode, "import_clause"); clause != nil {
			for _, id := range jsCollectChildren(clause, "identifier") {
				for i := len(result.Edges) - 1; i >= 0; i-- {
					edge := result.Edges[i]
					if edge.Kind == graph.EdgeImports && edge.From == fileID && edge.To == "unresolved::import::"+importPath {
						if edge.Meta == nil {
							edge.Meta = map[string]any{}
						}
						edge.Meta["vue_default_import"] = id.Content(src)
						break
					}
				}
			}
		}
	}
	named := jsNamedImportsNode(importNode)
	if named == nil {
		return
	}
	specs := jsCollectChildren(named, "import_specifier")
	if len(specs) == 0 || len(specs) > jsImportBindingCap {
		return
	}
	for _, sp := range specs {
		orig, alias := jsSpecifierNames(sp, src)
		if orig == "" {
			continue
		}
		edge := &graph.Edge{
			From: fileID, To: "unresolved::import::" + importPath + "::" + orig,
			Kind: graph.EdgeImports, FilePath: filePath, Line: int(sp.StartPoint().Row) + 1,
		}
		if alias != "" && alias != orig {
			edge.Alias = alias
			edge.Meta = map[string]any{"import_alias": alias}
		}
		result.Edges = append(result.Edges, edge)
	}
}

// emitJSReExport emits EdgeReExports edges for an `export ... from "mod"`
// statement — the re-export forms a barrel file uses to forward another
// module's surface. Three shapes:
//
//   - named   `export { a, b as c } from "mod"` — one edge per specifier to
//     unresolved::import::mod::<original>, Alias set to the exported name when
//     renamed (`b as c` → Alias "c"). Over jsImportBindingCap specifiers it
//     collapses to a single module-level re-export edge.
//   - namespace `export * as ns from "mod"` — one module-level edge, Alias "ns".
//   - wildcard `export * from "mod"` — one module-level edge, no Alias.
//
// For each named (value) specifier within the cap it also mints a queryable
// node at the barrel site under the exported (post-alias) name, so the public
// façade a consumer imports (`import { persist } from 'zustand/middleware'`)
// resolves as a real symbol whose id follows the standard `<file>::<name>`
// scheme. The node carries a `reexport` marker and a node→canonical
// EdgeReExports edge (resolved through the same import machinery as the
// file-level edge) so find_usages can delegate to the canonical declaration's
// usages. Type-only specifiers are left as reference edges by
// emitTSTypeOnlyExportRefs and never mint a value node here.
func emitJSReExport(exportNode *sitter.Node, importPath, fileID, filePath, lang string, src []byte, result *parser.ExtractionResult) {
	line := int(exportNode.StartPoint().Row) + 1

	if clause := findChildByType(exportNode, "export_clause"); clause != nil {
		specs := jsCollectChildren(clause, "export_specifier")
		if len(specs) > jsImportBindingCap {
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: "unresolved::import::" + importPath,
				Kind: graph.EdgeReExports, FilePath: filePath, Line: line,
			})
			return
		}
		stmtTypeOnly := tsExportStatementTypeOnly(exportNode, src)
		mintedNode := map[string]bool{}
		for _, sp := range specs {
			orig, alias := jsSpecifierNames(sp, src)
			if orig == "" {
				continue
			}
			specLine := int(sp.StartPoint().Row) + 1
			edge := &graph.Edge{
				From: fileID, To: "unresolved::import::" + importPath + "::" + orig,
				Kind: graph.EdgeReExports, FilePath: filePath, Line: specLine,
			}
			if alias != "" && alias != orig {
				edge.Alias = alias
			}
			result.Edges = append(result.Edges, edge)

			// A type-only re-export (`export type { X } from` or
			// `export { type X } from`) names a type, not a value — it is
			// forwarded as a reference edge by emitTSTypeOnlyExportRefs, so
			// no value node is minted for it.
			if stmtTypeOnly || tsSpecifierTypeOnly(sp, src) {
				continue
			}
			// The public binding name is the alias when renamed, else the
			// original — that is the name a consumer imports.
			publicName := orig
			if alias != "" {
				publicName = alias
			}
			nodeID := fileID + "::" + publicName
			if mintedNode[nodeID] {
				continue
			}
			mintedNode[nodeID] = true
			meta := map[string]any{
				"reexport":        true,
				"reexport_source": importPath,
			}
			if publicName != orig {
				meta["reexport_original"] = orig
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: nodeID, Kind: graph.KindVariable, Name: publicName,
				FilePath: filePath, StartLine: specLine, EndLine: specLine,
				Language: lang, Meta: meta,
			})
			// File → binding: EdgeDefines attaches the binding to the barrel
			// file (get_file_summary and friends walk defines/contains).
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: nodeID, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: specLine,
			})
			// Binding → canonical: a second EdgeReExports edge, this one
			// sourced from the node, gives the barrel binding a direct link
			// to the declaration it forwards once the resolver rewrites the
			// unresolved target. find_usages walks it to delegate.
			forward := &graph.Edge{
				From: nodeID, To: "unresolved::import::" + importPath + "::" + orig,
				Kind: graph.EdgeReExports, FilePath: filePath, Line: specLine,
			}
			if publicName != orig {
				forward.Alias = publicName
			}
			result.Edges = append(result.Edges, forward)
		}
		return
	}

	edge := &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeReExports, FilePath: filePath, Line: line,
	}
	// `export * as ns from "mod"` — record the namespace alias.
	if ns := findChildByType(exportNode, "namespace_export"); ns != nil {
		if id := findChildByType(ns, "identifier"); id != nil {
			edge.Alias = id.Content(src)
		}
	}
	result.Edges = append(result.Edges, edge)
}
