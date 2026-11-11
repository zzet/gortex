package languages

import (
	"regexp"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// PHP receiver typing.
//
// Every other AST extractor that records call sites (Go, Java, TypeScript,
// C#, Kotlin, Python) stamps `receiver_type` on a member-call edge so the
// resolver's exact-type passes can bind `x.foo()` to the `foo` that actually
// belongs to x's type. PHP did not: it emitted a bare `unresolved::*.foo` for
// every `$this->foo()` / `$obj->foo()` / `Foo::foo()`, which left the resolver
// with nothing but a repo-wide name match. Because a name-only pick lands at
// the text_matched tier, the cross-package guard then reverts most of them —
// so the calls were not merely imprecise, they were dropped.
//
// phpReceiverEnv is the static approximation of what a receiver expression
// evaluates to, built once per function body from declarations only:
//
//   - `$this`                     → the enclosing class;
//   - `$this->prop`               → the property's declared type, including
//     PHP 8 constructor-promoted properties;
//   - a typed parameter           → its declared type;
//   - `$v = new Foo(...)`         → Foo, but only when every `new` assignment
//     to $v in the body agrees (a variable reassigned to a different class is
//     dropped rather than guessed);
//   - `catch (FooException $e)`   → the caught type, when it is unambiguous;
//   - `/** @var Foo $v */`        → Foo.
//
// Everything else stays untyped. The environment never guesses: an entry is
// only recorded from a declaration the source states outright, so a stamped
// receiver_type is evidence the resolver can bind at the ast_resolved tier.
type phpReceiverEnv struct {
	fn *sitter.Node // preserves parameter spellings for graph-aware property lookup
	// class is the simple name of the enclosing class, or "" inside a free
	// function or a `static` closure (which does not bind $this).
	class string
	// vars maps a bare variable name (no leading `$`) to its written type name.
	vars typeEnv
	// props maps a bare property name to its declared type spelling.
	props map[string]string
}

// lookupVar returns the recorded type of $name, or "".
func (env phpReceiverEnv) lookupVar(name string) string {
	if env.vars == nil {
		return ""
	}
	typ := env.vars[name]
	// A local assigned new self/static names the enclosing declaration,
	// not a class literally named "self" or "static" in its namespace.
	if strings.EqualFold(typ, "self") || strings.EqualFold(typ, "static") {
		return env.class
	}
	return typ
}

// lookupProp returns the recorded type of $this->name, or "".
func (env phpReceiverEnv) lookupProp(name string) string {
	if env.props == nil {
		return ""
	}
	return env.props[name]
}

// childScope returns an environment for a nested closure / arrow function: the
// enclosing bindings stay visible (PHP closures inherit $this, and `use (...)`
// imports outer variables), extended with the closure's own parameters and
// local `new` assignments. A `static` closure drops $this.
func (env phpReceiverEnv) childScope(fn *sitter.Node, src []byte) phpReceiverEnv {
	child := phpReceiverEnv{fn: fn, class: env.class, props: env.props, vars: typeEnv{}}
	for k, v := range env.vars {
		child.vars[k] = v
	}
	if firstChildOfType(fn, "static_modifier") != nil {
		child.class = ""
	}
	collectPHPParamTypes(fn, src, child.vars)
	collectPHPLocalTypes(phpFunctionBody(fn), src, child.vars)
	return child
}

// newPHPReceiverEnv builds the environment for one function / method body.
// class is the enclosing class simple name ("" for a free function) and props
// its declared property types (nil for a free function).
func newPHPReceiverEnv(fn *sitter.Node, src []byte, class string, props map[string]string) phpReceiverEnv {
	env := phpReceiverEnv{fn: fn, class: class, props: props, vars: typeEnv{}}
	collectPHPParamTypes(fn, src, env.vars)
	collectPHPLocalTypes(phpFunctionBody(fn), src, env.vars)
	return env
}

// phpFunctionBody returns the body of a function / method / closure. An arrow
// function's body is a bare expression, not a compound_statement.
func phpFunctionBody(fn *sitter.Node) *sitter.Node {
	if fn == nil {
		return nil
	}
	if b := fn.ChildByFieldName("body"); b != nil {
		return b
	}
	return firstChildOfType(fn, "compound_statement")
}

// collectPHPParamTypes records every typed parameter of fn into vars. Covers
// simple_parameter, variadic_parameter, and property_promotion_parameter (the
// promoted constructor property is also an ordinary parameter inside the
// constructor body).
func collectPHPParamTypes(fn *sitter.Node, src []byte, vars typeEnv) {
	if fn == nil || vars == nil {
		return
	}
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i, n := 0, int(params.NamedChildCount()); i < n; i++ {
		p := params.NamedChild(i)
		switch p.Type() {
		case "simple_parameter", "variadic_parameter", "property_promotion_parameter":
		default:
			continue
		}
		name := phpParamVarName(p, src)
		if name == "" {
			continue
		}
		// A variadic parameter is an array of the declared type, not an
		// instance of it — typing it would misbind `$items->foo()`.
		if p.Type() == "variadic_parameter" {
			continue
		}
		if t := phpSoleTypeAtom(phpParameterType(p, src)); t != "" {
			vars[name] = t
		}
	}
}

// phpParamVarName returns a parameter's variable name without the `$`.
func phpParamVarName(p *sitter.Node, src []byte) string {
	name := p.ChildByFieldName("name")
	if name == nil {
		return ""
	}
	// `&$ref` wraps the variable_name in a by_ref node.
	if name.Type() == "by_ref" {
		if inner := firstChildOfType(name, "variable_name"); inner != nil {
			name = inner
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(name.Content(src)), "$")
}

var phpDocVarRe = regexp.MustCompile(`@var\s+([^\s*]+)\s+\$(\w+)`)

// collectPHPLocalTypes walks a function body recording the type of locals the
// source declares outright: `$v = new Foo`, `catch (Foo $e)`, and
// `/** @var Foo $v */`. A variable assigned two different classes is removed —
// a flow-insensitive environment cannot say which one reaches a given call, and
// a wrong receiver_type is worse than none (it binds the call to the wrong
// method at the ast_resolved tier instead of leaving it for the name-match
// fallback). Nested closures are skipped: they get their own scope.
func collectPHPLocalTypes(body *sitter.Node, src []byte, vars typeEnv) {
	if body == nil || vars == nil {
		return
	}
	conflicted := map[string]bool{}
	record := func(name, typ string) {
		if name == "" || typ == "" || conflicted[name] {
			return
		}
		if prev, ok := vars[name]; ok && prev != typ {
			delete(vars, name)
			conflicted[name] = true
			return
		}
		vars[name] = typ
	}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "anonymous_function", "anonymous_function_creation_expression", "arrow_function":
			return // its own scope
		case "assignment_expression":
			left := n.ChildByFieldName("left")
			right := n.ChildByFieldName("right")
			if left != nil && right != nil && left.Type() == "variable_name" {
				if cls := phpNewExpressionClass(right, src); cls != "" {
					record(strings.TrimPrefix(strings.TrimSpace(left.Content(src)), "$"), cls)
				}
			}
		case "catch_clause":
			name := n.ChildByFieldName("name")
			list := n.ChildByFieldName("type")
			if name != nil && list != nil && list.NamedChildCount() == 1 {
				if t := phpSoleTypeAtom(list.NamedChild(0).Content(src)); t != "" {
					record(strings.TrimPrefix(strings.TrimSpace(name.Content(src)), "$"), t)
				}
			}
		case "comment":
			for _, m := range phpDocVarRe.FindAllStringSubmatch(n.Content(src), -1) {
				if t := phpSoleTypeAtom(m[1]); t != "" {
					record(m[2], t)
				}
			}
		}
		for i, c := 0, int(n.NamedChildCount()); i < c; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
}

// phpNewExpressionClass returns the class named by an `new Foo(...)`
// expression, or "" when the expression is not a `new` of a statically known
// class (`new $cls`, `new class {}`, and `new ($expr)` all return "").
func phpNewExpressionClass(expr *sitter.Node, src []byte) string {
	if expr == nil || expr.Type() != "object_creation_expression" {
		return ""
	}
	for i, n := 0, int(expr.NamedChildCount()); i < n; i++ {
		c := expr.NamedChild(i)
		switch c.Type() {
		case "name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		case "arguments", "attribute_list":
			continue
		default:
			return ""
		}
	}
	return ""
}

// phpSoleTypeAtom reduces a declared type to a single bindable class name.
//
// A UNION (`Foo|Bar`) names alternatives — the value is one of them, and
// picking a branch would bind a call to the wrong class at the resolved tier —
// so it yields "". An INTERSECTION (`Foo&Bar`) is the opposite: the value is
// every branch at once, so any branch is a true statement about it and the
// first non-builtin one is taken. If the method being called lives on a
// different branch, the hierarchy walk from this one simply finds nothing and
// the call stays unresolved — never a wrong bind. This matters because
// `PushoverHandler&MockObject` is the idiomatic type of a mocked collaborator
// in a modern PHP test suite.
//
// `?Foo` is Foo (the null branch has no members to call), and a nullable union
// stays a union. Builtin scalar and pseudo types never name a graph node.
func phpSoleTypeAtom(decl string) string {
	decl = strings.TrimSpace(decl)
	if decl == "" || strings.ContainsRune(decl, '|') {
		return ""
	}
	// PHP 8.2 disjunctive normal form parenthesises intersection groups inside
	// a union; the union check above already rejected those.
	for _, branch := range strings.Split(decl, "&") {
		t := canonicalizePHPTypeRef(branch)
		if t == "" || phpBuiltinType(t) {
			continue
		}
		// Keep the written qualifier until the namespace pass resolves imports.
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(branch), "?"))
	}
	return ""
}

// phpClassPropertyTypes maps each declared property of a class / trait / enum
// body to its written type name — both classic typed properties and PHP 8
// constructor-promoted ones, which are declared in the constructor's parameter
// list and have no property_declaration of their own.
func phpClassPropertyTypes(body *sitter.Node, src []byte) map[string]string {
	if body == nil {
		return nil
	}
	props := map[string]string{}
	for i, n := 0, int(body.NamedChildCount()); i < n; i++ {
		member := body.NamedChild(i)
		switch member.Type() {
		case "property_declaration":
			t := phpSoleTypeAtom(phpPropertyType(member, src))
			if t == "" {
				continue
			}
			for j, m := 0, int(member.NamedChildCount()); j < m; j++ {
				el := member.NamedChild(j)
				if el.Type() != "property_element" {
					continue
				}
				if nameNode := el.ChildByFieldName("name"); nameNode != nil {
					props[strings.TrimPrefix(strings.TrimSpace(nameNode.Content(src)), "$")] = t
				}
			}
		case "method_declaration":
			nameNode := member.ChildByFieldName("name")
			if nameNode == nil || !strings.EqualFold(nameNode.Content(src), "__construct") {
				continue
			}
			params := member.ChildByFieldName("parameters")
			if params == nil {
				continue
			}
			for j, m := 0, int(params.NamedChildCount()); j < m; j++ {
				p := params.NamedChild(j)
				if p.Type() != "property_promotion_parameter" {
					continue
				}
				name := phpParamVarName(p, src)
				t := phpSoleTypeAtom(phpParameterType(p, src))
				if name != "" && t != "" {
					props[name] = t
				}
			}
		}
	}
	collectPHPAssignedPropertyTypes(body, src, props)
	if len(props) == 0 {
		return nil
	}
	return props
}

// phpReceiverTypeFor types the receiver expression of a member call. Returns
// "" when the receiver is not one of the statically known shapes.
func (env phpReceiverEnv) phpReceiverTypeFor(obj *sitter.Node, src []byte) string {
	if obj == nil {
		return ""
	}
	// `(new Foo())->m()` and `($x)->m()` wrap the receiver in parentheses.
	for obj.Type() == "parenthesized_expression" && obj.NamedChildCount() == 1 {
		obj = obj.NamedChild(0)
	}
	switch obj.Type() {
	case "variable_name":
		v := strings.TrimPrefix(strings.TrimSpace(obj.Content(src)), "$")
		if v == "this" {
			return env.class
		}
		return env.lookupVar(v)
	case "member_access_expression", "nullsafe_member_access_expression":
		// `$this->prop->m()` — only a property of the enclosing class is
		// typed; a deeper chain is left to the chain walker.
		inner := obj.ChildByFieldName("object")
		nameNode := obj.ChildByFieldName("name")
		if inner == nil || nameNode == nil || inner.Type() != "variable_name" {
			return ""
		}
		if strings.TrimSpace(inner.Content(src)) != "$this" {
			return ""
		}
		return env.lookupProp(strings.TrimSpace(nameNode.Content(src)))
	case "object_creation_expression":
		// `(new Foo)->m()`
		return phpNewExpressionClass(obj, src)
	}
	return ""
}

// phpScopeReceiverType types the scope of a `Foo::m()` scoped call or a
// `Foo::$prop` / `Foo::CONST` access. A `$var::m()` scope is typed through the
// environment.
//
// `self` / `static` resolve to the enclosing class — late static binding may
// pick a subclass at runtime, but the enclosing class is where the member is
// declared or inherited from, which is what the hierarchy walk needs. `parent`
// returns "" so the existing scope_kind path walks up from the enclosing class
// instead of pinning to it.
func (env phpReceiverEnv) phpScopeReceiverType(scope *sitter.Node, src []byte) string {
	if scope == nil {
		return ""
	}
	if scope.Type() == "relative_scope" {
		switch strings.ToLower(strings.TrimSpace(scope.Content(src))) {
		case "self", "static":
			return env.class
		}
		return ""
	}
	switch scope.Type() {
	case "name", "qualified_name", "relative_name":
		text := strings.TrimSpace(scope.Content(src))
		switch strings.ToLower(text) {
		case "parent", "self", "static":
			return ""
		}
		return phpSoleTypeAtom(text)
	case "variable_name":
		return env.lookupVar(strings.TrimPrefix(strings.TrimSpace(scope.Content(src)), "$"))
	}
	return ""
}

// collectPHPAssignedPropertyTypes types a property that carries no type
// DECLARATION from `$this->prop = new Foo()` in the class body. PHP only got
// typed properties in 7.4, so a large body of code declares `private $handler;`
// and states the type solely by what it assigns — 7.1% of every member call
// laravel/framework leaves untyped reads through such a property.
//
// A declared type always wins, and a property assigned two different classes
// anywhere in the class is dropped, for the same reason a conflicting local
// `new` is: a flow-insensitive answer that is sometimes wrong binds a call to
// the wrong method at the resolved tier.
func collectPHPAssignedPropertyTypes(body *sitter.Node, src []byte, props map[string]string) {
	if body == nil || props == nil {
		return
	}
	assigned := map[string]string{}
	conflicted := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "assignment_expression" {
			left := n.ChildByFieldName("left")
			right := n.ChildByFieldName("right")
			if left != nil && right != nil && left.Type() == "member_access_expression" {
				if name := phpThisPropertyName(left, src); name != "" && !conflicted[name] {
					if cls := phpNewExpressionClass(right, src); cls != "" {
						if prev, ok := assigned[name]; ok && prev != cls {
							delete(assigned, name)
							conflicted[name] = true
						} else {
							assigned[name] = cls
						}
					}
				}
			}
		}
		for i, c := 0, int(n.NamedChildCount()); i < c; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	for name, cls := range assigned {
		if _, declared := props[name]; !declared {
			props[name] = cls
		}
	}
}

// phpThisPropertyName returns the property name of a `$this->name` access, or
// "" for any other receiver or a computed member.
func phpThisPropertyName(access *sitter.Node, src []byte) string {
	inner := access.ChildByFieldName("object")
	nameNode := access.ChildByFieldName("name")
	if inner == nil || nameNode == nil || nameNode.Type() != "name" {
		return ""
	}
	if inner.Type() != "variable_name" || strings.TrimSpace(inner.Content(src)) != "$this" {
		return ""
	}
	return strings.TrimSpace(nameNode.Content(src))
}

// phpPropertyReceiver preserves a property path whose declarations may live in
// another file. Parameter spellings must survive until namespace resolution.
func (env phpReceiverEnv) phpPropertyReceiver(obj *sitter.Node, src []byte) map[string]any {
	var path []string
	for obj != nil && (obj.Type() == "member_access_expression" || obj.Type() == "nullsafe_member_access_expression") {
		name := obj.ChildByFieldName("name")
		if name == nil || name.Type() != "name" {
			return nil
		}
		path = append([]string{name.Content(src)}, path...)
		obj = obj.ChildByFieldName("object")
	}
	if len(path) == 0 || obj == nil || obj.Type() != "variable_name" {
		return nil
	}
	root := ""
	ambiguous := false
	variable := strings.TrimPrefix(obj.Content(src), "$")
	if variable == "this" {
		root = env.class
	} else if env.fn != nil {
		if params := env.fn.ChildByFieldName("parameters"); params != nil {
			for i := 0; i < int(params.NamedChildCount()); i++ {
				p := params.NamedChild(i)
				raw := phpParameterType(p, src)
				if phpParamVarName(p, src) == variable && strings.ContainsAny(raw, "|&") {
					ambiguous = true
				}
				if phpParamVarName(p, src) == variable && p.Type() != "variadic_parameter" && !strings.ContainsAny(raw, "|&") && env.lookupVar(variable) != "" && env.lookupVar(variable) == phpSoleTypeAtom(raw) {
					root = strings.TrimPrefix(strings.TrimSpace(raw), "?")
				}
			}
		}
	}
	return map[string]any{"php_property_root": root, "php_property_path": strings.Join(path, "."), "php_property_ambiguous": ambiguous}
}
