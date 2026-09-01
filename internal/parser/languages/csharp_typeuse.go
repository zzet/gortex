package languages

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitCSharpReferenceForms emits the C# reference edges that a pure
// symbol/annotation walk misses — the *expression-site* uses of a type
// that #143's annotation/param/return/field type-use pass (EdgeTypedAs)
// does not cover:
//
//   - INSTANTIATION   `new Foo(...)` / `new Foo[]` / `new Foo { ... }`
//     (object_creation_expression, array_creation_expression,
//     implicit_object_creation_expression) → EdgeInstantiates
//   - CAST / TYPE-TEST `(Foo)x` (cast_expression), `x is Foo` / `x is Foo f`
//     (is_pattern_expression), `x as Foo` (as_expression)
//     → EdgeReferences, Meta["ref_context"]="cast"
//   - STATIC ACCESS   `Foo.Const` / `Foo.Method()` (member_access_expression
//     whose receiver names a type), `typeof(Foo)`, `nameof(Foo)`
//     → EdgeReferences, Meta["ref_context"]="static_access"
//   - ATTRIBUTE TYPE  `[Foo]` / `[Foo(...)]` (attribute) names the type Foo
//     → EdgeReferences, Meta["ref_context"]="attribute"
//   - GENERIC ARG     the type arguments inside a `<…>` (type_argument_list):
//     `List<Foo>`, `Dictionary<string, Bar>`, `Task<Baz>`, nested
//     `Dictionary<int, List<Qux>>`. The annotation / param / return type-use
//     pass canonicalises a generic to its head and drops the arguments, so
//     these element types would otherwise be invisible.
//     → EdgeReferences, Meta["ref_context"]="generic_arg"
//
// Inheritance (`class X : Base, IFoo`) is intentionally NOT handled here:
// emitCSharpBaseList already splits the base_list into EdgeExtends /
// EdgeImplements, and RefContextOf maps both to a "type" reference, so a
// second edge here would double-emit.
//
// Each edge attributes to the enclosing function / method (file node when
// nothing encloses the line) so find_usages lands the reference without a
// language server. Targets are left at the canonical bare-name placeholder
// `unresolved::Foo` for the resolver to bind.
//
// Origin is load-bearing. The structural reference edges (cast / static
// access / attribute) ride graph.OriginASTResolved: the cross-package
// guard (internal/resolver/cross_pkg_guard.go) reverts EdgeReferences /
// EdgeCalls edges to their unresolved placeholder *only* at the two
// weakest tiers (text_matched / ast_inferred), so an ast_resolved edge is
// out of its scope and survives. EdgeInstantiates is not a call-like edge
// the guard polices at all, but it carries the same origin for
// consistency.
//
// Scope discipline keeps the graph clean, but only where a heuristic is
// actually needed. A name in a syntactically guaranteed type position
// (`new T()`, `(T)x`, `x as T`, `x is T`, `typeof(T)`, `[T]`, `List<T>`) is
// emitted on the grammar's authority alone; primitives are still skipped.
// Type-ness is inferred only for a member-access receiver, where `Type.X` and
// `local.X` are the same shape — see csharpStaticReceiverEligible, which
// accepts an uppercase receiver outright and falls back to "is this name
// bound to a value in this file" for scripts that have no case at all.
func emitCSharpReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, funcBytes, funcLines map[string][2]int) {
	if root == nil {
		return
	}
	// The widened owner set, not the methods-only one: an accessor or
	// initializer body must attribute its type references to the same
	// member that owns its calls, or one line of code splits its
	// evidence between two owners (and the fileID fallback below is for
	// top-level positions, not for code inside a member).
	funcRanges := csharpOwnerRanges(result, funcBytes, funcLines)

	// Dedup across the whole file on (owner, type, line, ref_context) so a
	// type referenced twice in the same role on the same line emits one edge.
	seen := map[string]bool{}

	ownerFor := func(line int) string {
		if owner := findEnclosingFunc(funcRanges, line); owner != "" {
			return owner
		}
		return fileID
	}

	// Identifiers this file declares as VALUES. A member access whose
	// receiver is one of them reads an instance, not a type — the thing the
	// Capitalization gate stands in for. Collected once so the caseless-script
	// path can ask the real question instead of the ASCII-shaped proxy.
	valueIdents := csharpDeclaredValueIdents(root, src)

	// emitRef appends an EdgeReferences (cast / static_access / attribute)
	// or EdgeInstantiates edge for a single non-primitive type.
	//
	// typePosition says the grammar already guarantees the name is a type —
	// `new T()`, `(T)x`, `x as T`, `x is T`, `typeof(T)`, `[T]`, `List<T>`.
	// Those positions need no naming heuristic at all, and applying one there
	// dropped every such reference in a codebase whose types are named in a
	// script without case. A receiver read out of `T.Member` is the only
	// position where type-ness must be inferred.
	emit := func(rawType string, line int, kind graph.EdgeKind, refContext string, typePosition bool) {
		canon := canonicalizeCSharpTypeRef(rawType)
		if canon == "" || isCSharpPrimitive(canon) {
			return
		}
		if !typePosition && !csharpStaticReceiverEligible(canon, valueIdents) {
			return
		}
		owner := ownerFor(line)
		if owner == "" {
			return
		}
		key := owner + "\x00" + canon + "\x00" + strconv.Itoa(line) + "\x00" + refContext
		if seen[key] {
			return
		}
		seen[key] = true
		edge := &graph.Edge{
			From:     owner,
			To:       "unresolved::" + canon,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		}
		if refContext != "" {
			edge.Meta = map[string]any{"ref_context": refContext}
		}
		// A qualified spelling names its namespace in the source — keep it
		// as evidence for the resolver's namespace narrowing.
		if fqn := csharpQualifiedTypeRef(rawType); fqn != "" {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["target_fqn"] = fqn
		}
		result.Edges = append(result.Edges, edge)
	}

	walkNodes(root, func(n *sitter.Node) {
		line := int(n.StartPoint().Row) + 1
		switch n.Type() {

		case "object_creation_expression", "implicit_object_creation_expression":
			// `new Foo(...)` / `new Foo { ... }`. The type is the leading
			// named child (identifier / generic_name / qualified_name);
			// implicit_object_creation (`new()`) has no type and is skipped.
			if name := csharpCreationTypeName(n, src); name != "" {
				emit(name, line, graph.EdgeInstantiates, "", true)
			}

		case "array_creation_expression":
			// `new Foo[3]` — the element type lives on the nested array_type.
			if at := csharpFirstChildOfType(n, "array_type"); at != nil {
				if name := csharpArrayElementTypeName(at, src); name != "" {
					emit(name, line, graph.EdgeInstantiates, "", true)
				}
			}

		case "cast_expression":
			// `(Foo)x` — the type is the `type` field (first named child).
			if name := csharpCastTypeName(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "cast", true)
			}

		case "as_expression":
			// `x as Foo` — binary form `value as Type`; the type is the
			// second named child.
			if name := csharpAsExpressionTypeName(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "cast", true)
			}

		case "is_pattern_expression":
			// `x is Foo` (constant_pattern) / `x is Foo f` (declaration_pattern).
			if name := csharpIsPatternTypeName(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "cast", true)
			}

		case "typeof_expression":
			// `typeof(Foo)` — the type is the named child.
			if name := csharpUnaryTypeArgName(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "static_access", true)
			}

		case "member_access_expression":
			// `Foo.Const` / `Foo.Method` — only when the receiver names a
			// type, never `this.x` / `local.X` / chained `a.b.C`.
			if name := csharpStaticAccessReceiver(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "static_access", false)
			}

		case "invocation_expression":
			// `nameof(Foo)` parses as a plain invocation, not a dedicated
			// node — special-case it so the referenced type surfaces.
			if name := csharpNameofTypeArg(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "static_access", false)
			}

		case "attribute":
			// `[Foo]` / `[Foo(...)]` names the attribute type Foo.
			if name := csharpAttributeTypeName(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, "attribute", true)
			}

		case "type_argument_list":
			// The `<…>` of a generic spelling (`List<Foo>`,
			// `Dictionary<string, Bar>`, `Task<Baz>`, `Foo<Bar<Baz>>`).
			// The annotation / param / return type-use pass strips the
			// argument list when it canonicalises a type to its head, so the
			// element types are otherwise invisible to find_usages. Each
			// argument is a direct named child here; emit a reference for the
			// ones that name a user type. A nested generic argument
			// (`List<Qux>` in `Dictionary<int, List<Qux>>`) is its own
			// generic_name whose own type_argument_list this walker visits in
			// turn, so emitting that argument's *head* (List) here and letting
			// the inner list contribute Qux covers every depth without a
			// manual recursion. Predefined primitives (`int`, `string`) parse
			// as predefined_type and are skipped; the canon/primitive/
			// capitalization gate in emit drops everything else lowercase.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				arg := n.NamedChild(i)
				if arg == nil {
					continue
				}
				switch arg.Type() {
				case "predefined_type":
					// `int` / `string` / … — never a user type.
					continue
				case "generic_name":
					// `List<Qux>` — its head names a type; the nested
					// type_argument_list (<Qux>) is visited separately.
					if head := csharpFirstChildOfType(arg, "identifier"); head != nil {
						emit(strings.TrimSpace(head.Content(src)), line, graph.EdgeReferences, "generic_arg", true)
					}
				case "identifier", "qualified_name", "nullable_type", "array_type":
					// `Foo`, `Ns.Foo`, `Foo?`, `Foo[]` — canonicalizeCSharpTypeRef
					// (called inside emit) strips the namespace / nullable /
					// array decoration down to the bare named type.
					emit(strings.TrimSpace(arg.Content(src)), line, graph.EdgeReferences, "generic_arg", true)
				}
			}
		}
	})
}

// csharpStaticReceiverEligible reports whether a bare member-access receiver
// may be read as a type rather than as a value.
//
// An uppercase first rune is the C# convention and is accepted outright, so
// nothing changes for a codebase written in a cased script. A script WITHOUT
// case — Chinese, Japanese, Korean, Hebrew, Arabic, Thai — has no such
// convention available: no identifier in it is ever "Capitalized", so gating
// on capitalisation silently dropped every reference form in such a codebase
// rather than merely being imprecise. For those names the question the gate
// was really asking is answered directly instead: a receiver the file
// declares as a value is an instance access, anything else may be a type.
// A lowercase cased letter is still rejected, and so is anything that is not
// a letter, which keeps `_field.X` and `local.X` out as before.
func csharpStaticReceiverEligible(name string, valueIdents map[string]bool) bool {
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError {
		return false
	}
	if unicode.IsUpper(r) {
		return true
	}
	if unicode.IsLower(r) || !unicode.IsLetter(r) {
		return false
	}
	return !valueIdents[name]
}

// csharpDeclaredValueIdents collects every identifier the file binds to a
// VALUE: locals and fields (variable_declarator), parameters, foreach
// variables, pattern designations (`x is Foo f`), and catch bindings. It is
// deliberately file-scoped rather than block-scoped — a name bound as a value
// anywhere in the file is treated as a value throughout, which errs toward
// dropping a reference rather than inventing one.
func csharpDeclaredValueIdents(root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	add := func(n *sitter.Node) {
		if n == nil {
			return
		}
		if name := strings.TrimSpace(n.Content(src)); name != "" {
			out[name] = true
		}
	}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "variable_declarator", "single_variable_designation":
			add(csharpFirstChildOfType(n, "identifier"))
		case "parameter", "catch_declaration", "foreach_statement":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				add(nameNode)
			} else {
				add(csharpFirstChildOfType(n, "identifier"))
			}
		}
	})
	return out
}

// csharpFirstChildOfType returns the first named child of node whose type
// is nodeType, or nil.
func csharpFirstChildOfType(node *sitter.Node, nodeType string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type() == nodeType {
			return c
		}
	}
	return nil
}

// csharpCreationTypeName extracts the constructed type's spelling from an
// object_creation_expression. The grammar exposes it as the `type` field
// in most revisions; fall back to the first named child that is a type
// token. `new()` (implicit) has no type child and yields "".
func csharpCreationTypeName(n *sitter.Node, src []byte) string {
	if t := n.ChildByFieldName("type"); t != nil {
		return strings.TrimSpace(t.Content(src))
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "generic_name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// csharpArrayElementTypeName extracts the element type from an array_type
// node (`Foo[3]` → "Foo"). The element type is the leading named child;
// the array_rank_specifier ([3]) is dropped.
func csharpArrayElementTypeName(arrayType *sitter.Node, src []byte) string {
	if t := arrayType.ChildByFieldName("type"); t != nil {
		return strings.TrimSpace(t.Content(src))
	}
	for i, _nc := 0, int(arrayType.NamedChildCount()); i < _nc; i++ {
		c := arrayType.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "generic_name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// csharpCastTypeName extracts the target type of a cast_expression
// (`(Foo)x` → "Foo"). The type is the `type` field, or the first named
// child when the field is untagged.
func csharpCastTypeName(n *sitter.Node, src []byte) string {
	if t := n.ChildByFieldName("type"); t != nil {
		return strings.TrimSpace(t.Content(src))
	}
	if n.NamedChildCount() > 0 {
		if c := n.NamedChild(0); c != nil {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// csharpAsExpressionTypeName extracts the target type of an as_expression
// (`x as Foo` → "Foo"). The grammar tags the type as the `right` /
// `type` field; otherwise it is the last named child (the value precedes
// it).
func csharpAsExpressionTypeName(n *sitter.Node, src []byte) string {
	for _, field := range []string{"type", "right"} {
		if t := n.ChildByFieldName(field); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
	}
	if c := n.NamedChild(int(n.NamedChildCount()) - 1); c != nil {
		return strings.TrimSpace(c.Content(src))
	}
	return ""
}

// csharpIsPatternTypeName extracts the tested type from an
// is_pattern_expression. `x is Foo` wraps the type in a constant_pattern;
// `x is Foo f` wraps it in a declaration_pattern (type then binding).
// Returns "" for non-type patterns (`x is null`, `x is > 0`).
func csharpIsPatternTypeName(n *sitter.Node, src []byte) string {
	pat := n.ChildByFieldName("pattern")
	if pat == nil {
		// Last named child is the pattern; the first is the tested value.
		if cnt := int(n.NamedChildCount()); cnt > 0 {
			pat = n.NamedChild(cnt - 1)
		}
	}
	if pat == nil {
		return ""
	}
	switch pat.Type() {
	case "declaration_pattern", "recursive_pattern":
		// `Foo f` — the type is the leading type token.
		for i, _nc := 0, int(pat.NamedChildCount()); i < _nc; i++ {
			c := pat.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "identifier", "generic_name", "qualified_name", "type", "nullable_type", "array_type":
				return strings.TrimSpace(c.Content(src))
			}
		}
	case "constant_pattern", "type_pattern":
		// `Foo` — the whole pattern is the type spelling.
		return strings.TrimSpace(pat.Content(src))
	case "identifier", "generic_name", "qualified_name":
		return strings.TrimSpace(pat.Content(src))
	}
	return ""
}

// csharpUnaryTypeArgName extracts the single type argument from a
// typeof_expression (`typeof(Foo)` → "Foo"): the first type-shaped named
// child.
func csharpUnaryTypeArgName(n *sitter.Node, src []byte) string {
	if t := n.ChildByFieldName("type"); t != nil {
		return strings.TrimSpace(t.Content(src))
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "generic_name", "qualified_name", "type",
			"nullable_type", "array_type", "predefined_type":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// csharpStaticAccessReceiver returns the receiver type name of a
// member_access_expression *only* when the receiver is a bare Capitalized
// identifier — the shape of a static / const access (`Foo.Empty`,
// `Foo.Method`). It returns "" for `this.x`, an instance access on a
// lowercase local (`svc.Foo`), and a chained access (`a.b.C`, whose
// receiver is itself a member_access_expression), so a field read on an
// instance is never misread as a static type reference.
func csharpStaticAccessReceiver(n *sitter.Node, src []byte) string {
	expr := n.ChildByFieldName("expression")
	if expr == nil {
		return ""
	}
	if expr.Type() != "identifier" {
		// this_expression, member_access_expression (chained), invocation,
		// etc. — not a bare type receiver.
		return ""
	}
	return strings.TrimSpace(expr.Content(src))
}

// csharpNameofTypeArg returns the type argument of a `nameof(Foo)`
// invocation, or "" when the invocation is not a nameof or its argument
// is not a bare identifier. nameof(...) has no dedicated grammar node; it
// parses as an invocation_expression whose function identifier is
// `nameof` and whose single argument is the referenced symbol.
func csharpNameofTypeArg(n *sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "nameof" {
		return ""
	}
	args := n.ChildByFieldName("arguments")
	if args == nil {
		args = csharpFirstChildOfType(n, "argument_list")
	}
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		if arg == nil || arg.Type() != "argument" {
			continue
		}
		for j, _nc := 0, int(arg.NamedChildCount()); j < _nc; j++ {
			c := arg.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "identifier", "generic_name":
				return strings.TrimSpace(c.Content(src))
			}
		}
	}
	return ""
}

// csharpAttributeTypeName returns the bare type name an attribute applies
// (`[Foo]` / `[Foo(...)]` → "Foo"). The name is the attribute's `name`
// field, or its first identifier child.
func csharpAttributeTypeName(n *sitter.Node, src []byte) string {
	if nm := n.ChildByFieldName("name"); nm != nil {
		return strings.TrimSpace(nm.Content(src))
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "generic_name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}
