package languages

import (
	"maps"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// jsImportBindingCap bounds how many per-binding import / re-export edges a
// single statement may emit. Barrel files (`export { ... } from`) routinely
// re-export hundreds of names; without a cap one statement could add hundreds
// of edges and dwarf the rest of the file's graph. Over the cap we keep only
// the module-level edge so the dependency is still recorded.
const jsImportBindingCap = 64

// jsSpecifierNames reads an import_specifier / export_specifier and returns its
// upstream (original) name and its renamed name. Handles named `name`/`alias`
// fields, anonymous `as` / `default` tokens, and single-fragment string aliases
// (e.g. `export { init as "other" }`, `export { init as default }`). Returns
// ("", "") when no valid binding name is present or an explicit `as` alias
// cannot be decoded cleanly.
func jsSpecifierNames(spec *sitter.Node, src []byte) (orig, alias string) {
	if spec == nil {
		return "", ""
	}
	nameNode := spec.ChildByFieldName("name")
	if nameNode == nil {
		for i, _nc := 0, int(spec.NamedChildCount()); i < _nc; i++ {
			if c := spec.NamedChild(i); c != nil && (c.Type() == "identifier" || c.Type() == "string") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return "", ""
	}
	switch nameNode.Type() {
	case "identifier":
		orig = nameNode.Content(src)
	case "string":
		orig = jsSingleStringContent(nameNode, src)
	default:
		orig = nameNode.Content(src)
	}
	if orig == "" {
		return "", ""
	}
	aliasNode := spec.ChildByFieldName("alias")
	hasAs := aliasNode != nil
	if !hasAs {
		seenAs := false
		for i, _nc := 0, int(spec.ChildCount()); i < _nc; i++ {
			c := spec.Child(i)
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
		return orig, ""
	}
	if aliasNode == nil {
		return "", ""
	}
	switch aliasNode.Type() {
	case "identifier":
		alias = aliasNode.Content(src)
	case "string":
		alias = jsSingleStringContent(aliasNode, src)
	default:
		alias = aliasNode.Content(src)
	}
	if alias == "" {
		return "", ""
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

// visitJSPattern enumerates bindings, not property keys, types or initializers.
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
// require() initializers, so export recording and checking whether `require`
// itself is shadowed never recurse into require matching.
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

// visitJSDeclaration visits every declared name, as export records need.
func visitJSDeclaration(n *sitter.Node, src []byte, visit func(string)) {
	visitJSAllDeclarations(n, src, true, visit)
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

// Only unconditional, top-level writes are evidence: a nested assignment may
// never execute. Track whether the local exports alias still names the module.
type jsCommonExports struct {
	named    map[string]string
	def      string
	interop  bool
	detached bool
	// replaced counts module.exports replacements, so a property write whose
	// object was captured before its right-hand side ran is not misattributed.
	replaced int
	// objects holds top-level locals still bound to an object literal. Such a
	// module.exports is a container, so it cannot compete with a callable .default.
	objects map[string]bool
}

func jsModuleExports(n *sitter.Node, src []byte) bool {
	if n == nil || n.Type() != "member_expression" {
		return false
	}
	obj, prop := n.ChildByFieldName("object"), n.ChildByFieldName("property")
	return obj != nil && obj.Type() == "identifier" && obj.Content(src) == "module" &&
		prop != nil && prop.Type() == "property_identifier" && prop.Content(src) == "exports"
}

func (c *jsCommonExports) activeObject(n *sitter.Node, src []byte) bool {
	return jsModuleExports(n, src) || !c.detached && n != nil && n.Type() == "identifier" && n.Content(src) == "exports"
}

func (c *jsCommonExports) property(left *sitter.Node, src []byte) string {
	if left == nil || left.Type() != "member_expression" || !c.activeObject(left.ChildByFieldName("object"), src) {
		return ""
	}
	if prop := left.ChildByFieldName("property"); prop != nil && prop.Type() == "property_identifier" {
		return prop.Content(src)
	}
	return ""
}

// Assignment expressions evaluate to their RHS. Retain the storage identity
// as well as the value so either ordering of a chained alias reset works.
func (c *jsCommonExports) assign(left, value *sitter.Node, src []byte) (local string, module, alias bool) {
	if left == nil || value == nil {
		return
	}
	// JavaScript resolves the target object before evaluating the value.
	name, replaced := c.property(left, src), c.replaced
	switch value.Type() {
	case "assignment_expression":
		local, module, alias = c.assign(value.ChildByFieldName("left"), value.ChildByFieldName("right"), src)
	case "identifier":
		local = value.Content(src)
		alias = local == "exports"
		module = alias && !c.detached
	case "function_expression", "generator_function":
		if name := value.ChildByFieldName("name"); name != nil {
			local = name.Content(src)
		}
	default:
		module = jsModuleExports(value, src)
		alias = module && !c.detached
	}
	switch {
	case jsModuleExports(left, src):
		if !module {
			c.def, c.named, c.interop = local, nil, false
			c.replaced++
		}
		c.detached, module = !alias, true
	case left.Type() == "identifier" && left.Content(src) == "exports":
		c.detached, alias = !module, true
	default:
		switch {
		case name == "" || c.replaced != replaced:
		case name == "__esModule":
			c.interop = value.Type() == "true"
		case local == "":
			delete(c.named, name)
		default:
			if c.named == nil {
				c.named = make(map[string]string)
			}
			c.named[name] = local
		}
	}
	return
}

// tsc and Babel also emit Object.defineProperty(exports, "__esModule", {value:true}).
func (c *jsCommonExports) defineProperty(call *sitter.Node, src []byte) {
	fn, args := call.ChildByFieldName("function"), call.ChildByFieldName("arguments")
	if fn == nil || fn.Type() != "member_expression" || args == nil || args.NamedChildCount() != 3 {
		return
	}
	obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
	name, descriptor := args.NamedChild(1), args.NamedChild(2)
	if obj == nil || obj.Content(src) != "Object" || prop == nil || prop.Content(src) != "defineProperty" ||
		!c.activeObject(args.NamedChild(0), src) || name.Type() != "string" || descriptor.Type() != "object" {
		return
	}
	if marker := name.Content(src); marker != `"__esModule"` && marker != "'__esModule'" {
		return
	}
	for i := 0; i < int(descriptor.NamedChildCount()); i++ {
		pair := descriptor.NamedChild(i)
		key, val := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
		if key != nil && key.Content(src) == "value" && val != nil {
			c.interop = val.Type() == "true"
		}
	}
}

// jsTopLevelBindingNames collects names bound at module scope (top-level
// declarations, hoisted vars, and top-level imports) so a local `export { x }`
// clause never claims a nested function, nested arrow, named function
// expression, or TS namespace member as a module export.
func jsTopLevelBindingNames(root *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			names[name] = true
		}
	}
	visitJSAllHoistedVars(root, src, add)
	for i := 0; i < int(root.NamedChildCount()); i++ {
		s := root.NamedChild(i)
		if s == nil {
			continue
		}
		if s.Type() == "import_statement" {
			visitJSImportBindings(s, src, add)
			continue
		}
		visitJSAllDeclarations(s, src, true, add)
		decl := s
		if decl.Type() == "export_statement" {
			decl = decl.ChildByFieldName("declaration")
		}
		if decl != nil && (decl.Type() == "interface_declaration" || decl.Type() == "type_alias_declaration") {
			visitJSPattern(decl.ChildByFieldName("name"), src, add)
		}
	}
	return names
}

// Read direct module statements in execution order; do not infer control flow.
func recordJSExports(root *sitter.Node, src []byte, file *graph.Node) {
	var bindings strings.Builder
	var common jsCommonExports
	var topBindings map[string]bool
	for i := 0; i < int(root.NamedChildCount()); i++ {
		s := root.NamedChild(i)
		if s.Type() == "expression_statement" {
			n := s.NamedChild(0)
			if n != nil {
				switch n.Type() {
				case "assignment_expression":
					if left := n.ChildByFieldName("left"); left != nil && left.Type() == "identifier" {
						delete(common.objects, left.Content(src))
					}
					common.assign(n.ChildByFieldName("left"), n.ChildByFieldName("right"), src)
				case "call_expression":
					common.defineProperty(n, src)
				case "unary_expression":
					if op := n.ChildByFieldName("operator"); op != nil && op.Type() == "delete" {
						if jsModuleExports(n.ChildByFieldName("argument"), src) {
							common.def, common.named, common.interop, common.detached = "", nil, false, true
							common.replaced++
							continue
						}
						name := common.property(n.ChildByFieldName("argument"), src)
						delete(common.named, name)
						if name == "__esModule" {
							common.interop = false
						}
					}
				}
			}
			continue
		}
		if s.Type() == "variable_declaration" || s.Type() == "lexical_declaration" {
			for j := 0; j < int(s.NamedChildCount()); j++ {
				v := s.NamedChild(j)
				if name := v.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
					if value := v.ChildByFieldName("value"); value != nil && value.Type() == "object" {
						if common.objects == nil {
							common.objects = make(map[string]bool)
						}
						common.objects[name.Content(src)] = true
					} else {
						delete(common.objects, name.Content(src))
					}
				}
				if name := v.ChildByFieldName("name"); name != nil && name.Type() == "identifier" && name.Content(src) == "exports" {
					if value := v.ChildByFieldName("value"); value != nil {
						common.assign(name, value, src)
					} else if s.Type() == "lexical_declaration" {
						common.detached = true
					}
				}
			}
			continue
		}
		if s.Type() != "export_statement" || s.ChildByFieldName("source") != nil || tsExportStatementTypeOnly(s, src) {
			continue
		}
		decl := s.ChildByFieldName("declaration")
		if findChildByType(s, "default") != nil {
			var local string
			if value := s.ChildByFieldName("value"); value != nil && value.Type() == "identifier" {
				local = value.Content(src)
			} else {
				visitJSDeclaration(decl, src, func(n string) { local = n })
			}
			if local != "" {
				if file.Meta == nil {
					file.Meta = map[string]any{}
				}
				file.Meta[graph.DefaultExportMeta] = local
			}
			continue
		}
		visitJSDeclaration(decl, src, func(n string) { graph.AppendJSBinding(&bindings, n, n) })
		if clause := findChildByType(s, "export_clause"); clause != nil {
			if topBindings == nil {
				topBindings = jsTopLevelBindingNames(root, src)
			}
			for _, sp := range jsCollectChildren(clause, "export_specifier") {
				orig, alias := jsSpecifierNames(sp, src)
				if orig == "" || tsSpecifierTypeOnly(sp, src) || !topBindings[orig] {
					continue
				}
				if alias == "" {
					alias = orig
				}
				if strings.ContainsAny(alias, " \t\r\n:") || strings.ContainsAny(orig, " \t\r\n:") {
					continue
				}
				graph.AppendJSBinding(&bindings, alias, orig)
			}
		}
	}
	if common.interop {
		graph.AppendJSBinding(&bindings, graph.JSInteropMarkerBinding, graph.JSInteropMarkerBinding)
	}
	for _, name := range slices.Sorted(maps.Keys(common.named)) {
		key := name
		if name == graph.JSDefaultBinding {
			if !common.interop {
				continue
			}
			key = graph.JSInteropDefaultBinding
		}
		graph.AppendJSBinding(&bindings, key, common.named[name])
	}
	if common.interop && common.objects[common.def] {
		common.def = ""
	}
	if common.def != "" {
		graph.AppendJSBinding(&bindings, graph.JSDefaultBinding, common.def)
	}
	if bindings.Len() != 0 {
		if file.Meta == nil {
			file.Meta = map[string]any{}
		}
		file.Meta[graph.JSLocalExportsMeta] = bindings.String()
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
	case "program", "statement_block", "if_statement", "else_clause", "switch_statement", "switch_body",
		"switch_case", "switch_default", "for_statement", "while_statement", "do_statement",
		"try_statement", "catch_clause", "finally_clause", "with_statement", "labeled_statement", "export_statement":
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
// names hide generated globals without shadowing explicit ES imports; genuine
// static require() bindings resolve strictly against their required module.
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
	// called holds the names of the bare calls stamp will look up. Scope maps
	// keep only those names: a declaration no call looks up can't shadow one.
	// Nil keeps every name.
	called map[string]struct{}
}

// noteCall records a bare call name before stamping begins.
func (cache *jsScopeCache) noteCall(name string) {
	if cache.called == nil {
		cache.called = map[string]struct{}{}
	}
	cache.called[name] = struct{}{}
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
		case "program", "statement_block", "switch_body", "class",
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
				if cache.called != nil {
					if _, ok := cache.called[id]; !ok {
						return
					}
				}
				if names == nil {
					names = map[string]jsBinding{}
				}
				if prev, exists := names[id]; exists && prev != b {
					if prev.kind == graph.JSPendingParameter || b.kind == graph.JSPendingParameter {
						b = jsBinding{kind: graph.JSPendingParameter}
					} else if kind == "program" {
						b = jsBinding{kind: graph.JSShadowModule}
					} else {
						// Conflicting local/require or differing require specs
						// cannot be trusted to name a single module binding.
						b = jsBinding{kind: graph.JSLexicalLocal}
					}
				}
				names[id] = b
			}
			local := func(id string) { add(id, jsBinding{kind: graph.JSLexicalLocal}) }
			if kind == "program" {
				local = func(id string) { add(id, jsBinding{kind: graph.JSShadowModule}) }
			}
			required := func(id, spec string) { add(id, jsBinding{kind: graph.JSLexicalRequire, spec: spec}) }
			switch kind {
			case "program", "statement_block":
				// Program, class static blocks, and TS namespaces are their own var scopes.
				if kind == "program" || isJSBlockVarScope(n) {
					cache.visitHoistedVars(n, src, local, required)
				}
				for i := 0; i < int(n.NamedChildCount()); i++ {
					s := n.NamedChild(i)
					if s.Type() == "export_statement" {
						s = s.ChildByFieldName("declaration")
					}
					cache.visitBlockDeclaration(s, src, local, required)
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
			if binding.kind == graph.JSShadowModule {
				edge.Meta[graph.JSAmbientShadowMeta] = binding.kind
			} else {
				edge.Meta[graph.JSLexicalBindingMeta] = binding.kind
				if binding.spec != "" {
					edge.Meta[graph.JSLexicalRequireSpecMeta] = binding.spec
				}
			}
			return
		}
	}
}

func jsShadowRank(e *graph.Edge) int {
	switch e.Meta[graph.JSLexicalBindingMeta] {
	case nil:
		if e.Meta[graph.JSAmbientShadowMeta] == graph.JSShadowModule {
			return 2
		}
		if e.Meta[graph.JSAmbientShadowMeta] != nil {
			return 1
		}
		return 0
	case graph.JSLexicalRequire:
		return 1
	case graph.JSLexicalLocal:
		return 3
	default:
		return 4
	}
}

// Calls with one From/To/Line identity collapse into one stored edge. When a
// less-shadowed call shares that site with a shadowed one, normalize every
// colliding edge to the same deterministic least-shadowed binding so last-wins
// in-memory stores and first-wins SQLite stores agree.
func clearCollapsedJSShadows(edges []*graph.Edge) {
	shadowed := make(map[graph.EdgeIdentity][]*graph.Edge)
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && jsShadowRank(e) != 0 {
			id := graph.EdgeIdentityFor(e)
			shadowed[id] = append(shadowed[id], e)
		}
	}
	if len(shadowed) == 0 {
		return
	}
	weakest := make(map[graph.EdgeIdentity]*graph.Edge, len(shadowed))
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		id := graph.EdgeIdentityFor(e)
		if _, ok := shadowed[id]; !ok {
			continue
		}
		if w := weakest[id]; w == nil || jsShadowRank(e) < jsShadowRank(w) {
			weakest[id] = e
		}
	}
	for id, group := range shadowed {
		winner := weakest[id]
		winnerRank := jsShadowRank(winner)
		// Two colliding local require() calls on one line that disagree on
		// their module specifier cannot pick one deterministically across
		// stores; downgrade the whole group to JSLexicalLocal.
		if winnerRank == 1 && winner.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
			spec, _ := winner.Meta[graph.JSLexicalRequireSpecMeta].(string)
			ambiguousSpec := false
			for _, e := range group {
				if e.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
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
					delete(e.Meta, graph.JSAmbientShadowMeta)
				}
				continue
			}
		}
		lexical := winner.Meta[graph.JSLexicalBindingMeta]
		reqSpec := winner.Meta[graph.JSLexicalRequireSpecMeta]
		ambient := winner.Meta[graph.JSAmbientShadowMeta]
		for _, s := range group {
			delete(s.Meta, graph.JSLexicalBindingMeta)
			delete(s.Meta, graph.JSLexicalRequireSpecMeta)
			delete(s.Meta, graph.JSAmbientShadowMeta)
			if lexical != nil {
				s.Meta[graph.JSLexicalBindingMeta] = lexical
			}
			if reqSpec != nil {
				s.Meta[graph.JSLexicalRequireSpecMeta] = reqSpec
			}
			if ambient != nil {
				s.Meta[graph.JSAmbientShadowMeta] = ambient
			}
		}
	}
}

// emitJSImports retains value bindings on the module edge: a per-symbol edge
// can converge onto that edge when resolution finds the file but not the export.
func emitJSImports(importNode *sitter.Node, importPath, fileID, filePath string, line int, src []byte, result *parser.ExtractionResult) {
	module := &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	}
	result.Edges = append(result.Edges, module)
	if importNode == nil {
		return
	}
	var locals strings.Builder
	typeOnly := tsImportStatementTypeOnly(importNode, src)
	if clause := findChildByType(importNode, "import_clause"); clause != nil && !typeOnly {
		if id := findChildByType(clause, "identifier"); id != nil {
			name := id.Content(src)
			graph.AppendJSBinding(&locals, name, graph.JSDefaultBinding)
		}
		if ns := findChildByType(clause, "namespace_import"); ns != nil {
			if id := findChildByType(ns, "identifier"); id != nil {
				graph.AppendJSBinding(&locals, id.Content(src), graph.JSNamespaceBinding)
			}
		}
	}
	var specs []*sitter.Node
	if named := jsNamedImportsNode(importNode); named != nil {
		specs = jsCollectChildren(named, "import_specifier")
	}
	for _, sp := range specs {
		orig, alias := jsSpecifierNames(sp, src)
		if orig == "" {
			continue
		}
		if !typeOnly && !tsSpecifierTypeOnly(sp, src) {
			if alias == "" {
				graph.AppendJSBinding(&locals, orig, orig)
			} else {
				graph.AppendJSBinding(&locals, alias, orig)
			}
		}
		if len(specs) > jsImportBindingCap {
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
	if locals.Len() != 0 {
		bindings := locals.String()
		module.Meta = map[string]any{graph.ImportBindingsMeta: bindings, graph.JSImportSourceMeta: importPath}
		if len(result.Nodes) > 0 && result.Nodes[0].Kind == graph.KindFile {
			meta := result.Nodes[0].Meta
			exports, _ := meta[graph.JSLocalExportsMeta].(string)
			def, _ := meta[graph.DefaultExportMeta].(string)
			if graph.JSExportsImport(exports, def, bindings) {
				module.Meta[graph.JSForwardsImportMeta] = true
			}
		}
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
			var bindings strings.Builder
			if !tsExportStatementTypeOnly(exportNode, src) {
				for _, sp := range specs {
					orig, alias := jsSpecifierNames(sp, src)
					if orig == "" || tsSpecifierTypeOnly(sp, src) {
						continue
					}
					if alias == "" {
						alias = orig
					}
					graph.AppendJSBinding(&bindings, alias, orig)
				}
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: "unresolved::import::" + importPath,
				Kind: graph.EdgeReExports, FilePath: filePath, Line: line,
				Meta: map[string]any{graph.JSImportSourceMeta: importPath, graph.ImportBindingsMeta: bindings.String()},
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
				Meta: map[string]any{graph.JSImportSourceMeta: importPath + "::" + orig},
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
				Meta: map[string]any{graph.JSImportSourceMeta: importPath + "::" + orig},
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
		Meta: map[string]any{graph.JSImportSourceMeta: importPath},
	}
	// `export * as ns from "mod"` — record the namespace alias.
	if ns := findChildByType(exportNode, "namespace_export"); ns != nil {
		if id := findChildByType(ns, "identifier"); id != nil {
			edge.Alias = id.Content(src)
		}
	} else if !tsExportStatementTypeOnly(exportNode, src) {
		edge.Meta[graph.StarReExportMeta] = true
	}
	result.Edges = append(result.Edges, edge)
}
