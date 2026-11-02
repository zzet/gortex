package languages

import sitter "github.com/zzet/gortex/internal/parser/tsitter"

// fnRefSpec is the per-grammar value-position capture spec for the
// function-as-value walk. It names which AST node types constitute a callable
// token in value position (idNodeTypes), how to peel address-of / eta wrappers
// (unwrapForms), how a call is disambiguated from a value (dispatch), and
// whether the gate may resolve the captured name cross-module (ungated).
//
// The default spec reproduces the original grammar-agnostic behaviour — a bare
// `identifier` leaf disambiguated by the next source byte — so a language with
// no entry regresses to nothing.
type fnRefSpec struct {
	// idNodeTypes are the node types whose content (or trailing segment, for a
	// qualified path) names a function in value position.
	idNodeTypes []string
	// unwrapForms maps a parent node type that wraps the callable token to the
	// `fn_ref_form` tag it records (`address_of` for `&fn`/`@fn`, `eta` for
	// Scala's `f _`).
	unwrapForms map[string]string
	// dispatch selects the call-vs-value disambiguator.
	dispatch fnRefDispatch
	// ungated lets the gate resolve the captured name cross-module (for a
	// qualified path like Rust `m::f` or Go `pkg.Fn`), not just same-file.
	ungated bool
}

type fnRefDispatch int

const (
	// dispatchByteParen is the original disambiguator: an identifier whose next
	// non-whitespace source byte is '(' or '`' is a callee, not a value.
	dispatchByteParen fnRefDispatch = iota
	// astCalleeField additionally rejects a token that is the callee child of a
	// call node in the parse tree, catching calls the byte heuristic misses
	// because a non-'(' token sits between the name and its '(' — optional
	// chaining `f?.()`, non-null `f!()`, generic instantiation `f<T>()`. It only
	// ever classifies more tokens as calls than dispatchByteParen, never fewer,
	// so it strictly tightens precision (fewer spurious value candidates).
	astCalleeField
)

func (s fnRefSpec) matchesIDNode(t string) bool {
	for _, x := range s.idNodeTypes {
		if x == t {
			return true
		}
	}
	return false
}

var defaultFnRefSpec = fnRefSpec{idNodeTypes: []string{"identifier"}, dispatch: dispatchByteParen}

// fnRefSpecFor returns the value-position capture spec for a language, falling
// back to the default bare-identifier spec.
func fnRefSpecFor(lang string) fnRefSpec {
	switch lang {
	case "rust":
		// `let f = m::func;` — a path value whose trailing segment names the fn.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "scoped_identifier"},
			dispatch:    dispatchByteParen,
			ungated:     true,
		}
	case "go":
		// `cb := pkg.Fn` — a selector value whose field names the fn. Cross-pkg,
		// so ungated.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "selector_expression"},
			dispatch:    dispatchByteParen,
			ungated:     true,
		}
	case "swift", "kotlin":
		// The callable token is `simple_identifier`, not `identifier`.
		return fnRefSpec{idNodeTypes: []string{"identifier", "simple_identifier"}, dispatch: dispatchByteParen}
	case "scala":
		// Eta-expansion `f _` wraps the identifier in a postfix expression.
		return fnRefSpec{
			idNodeTypes: []string{"identifier"},
			unwrapForms: map[string]string{"postfix_expression": "eta"},
			dispatch:    dispatchByteParen,
		}
	case "pascal":
		// `@Fn` address-of: the `@` is an exprUnary wrapper around the ident.
		return fnRefSpec{
			idNodeTypes: []string{"identifier"},
			unwrapForms: map[string]string{"exprUnary": "address_of", "unary_expression": "address_of"},
			dispatch:    dispatchByteParen,
		}
	case "javascript", "typescript", "tsx":
		// `el.addEventListener('x', this.onX)` / `setTimeout(mod.tick)` — a
		// member value whose property names a same-file fn. AST dispatch rejects
		// `f?.()` / `f!()` / `f<T>()` calls the byte heuristic would miss.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "member_expression"},
			dispatch:    astCalleeField,
		}
	case "python":
		// `signal.connect(self.on_change)` — an attribute value whose trailing
		// attribute names the fn.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "attribute"},
			dispatch:    astCalleeField,
		}
	case "csharp":
		// `list.ForEach(this.Handle)` — a member-access value.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "member_access_expression"},
			dispatch:    astCalleeField,
		}
	case "java", "c", "dart":
		// Bare-identifier value idiom; AST dispatch tightens the call check
		// (Java `method_invocation`, C/Dart `call_expression`).
		return fnRefSpec{idNodeTypes: []string{"identifier"}, dispatch: astCalleeField}
	case "cpp":
		// Bare-identifier value idiom plus the C++ address-of forms: a `&fn`
		// value-arg (pointer_expression) and a `&Cls::method` pointer-to-member
		// (qualified_identifier). The pointer-to-member names a method of
		// another class, so it resolves ungated.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "qualified_identifier", "scoped_identifier"},
			unwrapForms: map[string]string{"pointer_expression": "address_of", "unary_expression": "address_of"},
			dispatch:    astCalleeField,
			ungated:     true,
		}
	case "php", "ruby":
		// First-class refs are dominated by the special forms (PHP string
		// callables, Ruby `method(:sym)` / `&:sym`); the bare path stays on the
		// byte heuristic.
		return fnRefSpec{idNodeTypes: []string{"identifier"}, dispatch: dispatchByteParen}
	case "lua", "luau":
		// Lua functions are first-class values: a bare `setCallback(onClick)`
		// and a table-member `register(handlers.onTick)` both pass a function
		// by value. A dot_index_expression resolves to its trailing field name.
		return fnRefSpec{idNodeTypes: []string{"identifier", "dot_index_expression"}, dispatch: dispatchByteParen}
	}
	return defaultFnRefSpec
}

// fnRefNodeName returns the function name a value-position node refers to: the
// node's own content for a leaf identifier, or the trailing segment for a
// qualified path (Rust `scoped_identifier`, Go `selector_expression`). Returns
// "" when no name can be recovered.
func fnRefNodeName(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "scoped_identifier":
		if name := n.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "selector_expression":
		if field := n.ChildByFieldName("field"); field != nil {
			return field.Content(src)
		}
	case "member_expression":
		if prop := n.ChildByFieldName("property"); prop != nil {
			return prop.Content(src)
		}
	case "attribute":
		if attr := n.ChildByFieldName("attribute"); attr != nil {
			return attr.Content(src)
		}
	case "member_access_expression":
		if name := n.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "dot_index_expression":
		// Lua `tbl.method` -- the trailing identifier names the function.
		if f := n.ChildByFieldName("field"); f != nil {
			return f.Content(src)
		}
		if k := int(n.NamedChildCount()); k > 0 {
			return n.NamedChild(k - 1).Content(src)
		}
	}
	return n.Content(src)
}

// qualifiedFnRefNodeTypes are the value-position node types that name a function
// through a path / member access rather than a bare leaf — the forms a spec may
// resolve cross-module (ungated) when the trailing name is not file-local.
var qualifiedFnRefNodeTypes = map[string]bool{
	"scoped_identifier":        true,
	"qualified_identifier":     true,
	"selector_expression":      true,
	"member_expression":        true,
	"attribute":                true,
	"member_access_expression": true,
}

func isQualifiedFnRefNode(t string) bool { return qualifiedFnRefNodeTypes[t] }

// nodeIsCallCallee reports whether n sits in callee position of a call node —
// the `function` / `name` child of a call/invocation — across the grammars the
// fn-value walk targets. Used by astCalleeField dispatch to reject calls whose
// `(` is separated from the callee by `?.`, `!`, or `<T>`.
func nodeIsCallCallee(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	var callee *sitter.Node
	switch p.Type() {
	case "call_expression", "call", "invocation_expression":
		callee = p.ChildByFieldName("function")
	case "method_invocation":
		callee = p.ChildByFieldName("name")
	}
	if callee == nil {
		return false
	}
	return callee.StartByte() == n.StartByte() && callee.EndByte() == n.EndByte()
}

// fnRefStartsCall reports whether the value-position node n is actually the
// callee of a call (so not a function-as-value). It always applies the original
// byte heuristic and, under astCalleeField dispatch, additionally consults the
// parse tree so modern call syntax (`f?.()`, `f!()`, `f<T>()`) is not mistaken
// for a value reference.
func fnRefStartsCall(spec fnRefSpec, n *sitter.Node, src []byte) bool {
	if spec.dispatch == astCalleeField && nodeIsCallCallee(n) {
		return true
	}
	return byteAfterIdentStartsCall(src, int(n.EndByte()))
}

// fnRefIsCallableDeclName reports whether n is the NAME of a callable
// declaration rather than a use of one.
//
// The byte heuristic leans on the `(` that follows a declaration's name to
// exclude the declaration itself, and that holds only while nothing sits
// between them. A generic declaration puts the type parameters there —
// `AddFooDecorator<TInterface, TImpl>(` — so the method's own name read as a
// function-as-value and landed a placeholder reference edge stamped at its
// declaration line, which the gate then bound to a sibling overload.
//
// Widening the byte rule to `<` is not available: `f < g` is a comparison in
// JavaScript, TypeScript and every C-family language the capture serves. The
// tree answers exactly, and for every language at once — a node that fills its
// parent's `name` field where that parent also declares `parameters` is a
// declaration however it is spelled.
func fnRefIsCallableDeclName(n *sitter.Node) bool {
	return fnRefIsCallableDeclNameWithParent(n, n.Parent())
}

func fnRefIsCallableDeclNameWithParent(n, p *sitter.Node) bool {
	if p == nil {
		return false
	}
	name := p.ChildByFieldName("name")
	if name == nil || name.StartByte() != n.StartByte() || name.EndByte() != n.EndByte() {
		return false
	}
	return p.ChildByFieldName("parameters") != nil
}

// fnRefForm reports the wrapper form (address_of / eta) a captured node sits in
// per the spec's unwrapForms, or "" for a plain value position.
func (s fnRefSpec) fnRefForm(n *sitter.Node) string {
	if len(s.unwrapForms) == 0 || n == nil {
		return ""
	}
	if p := n.Parent(); p != nil {
		if form, ok := s.unwrapForms[p.Type()]; ok {
			return form
		}
	}
	return ""
}
