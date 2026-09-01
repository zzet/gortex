package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// extractTypeUses emits cross-file type-usage edges (EdgeTypedAs to
// "unresolved::"+Type, Origin OriginASTInferred) for every Dart type that
// appears in a declaration position the symbol extractors don't already
// cover:
//
//   - class / mixin / extension fields (`final Foo foo;`)
//   - function & method parameter types (`void f(Foo x)`)
//   - function & method return types (`Foo build()`)
//   - typed local variables (`Foo x = ...`, `final Foo x = ...`)
//
// This is the LSP-free guarantee: a Dart type named in a declaration always
// produces a usage edge, so find_usages lands it without a language server.
// Inferred bindings (`var x = ...`, `final x = ...`) carry no written type
// and are skipped — there is nothing to attribute.
//
// Fields attribute to the file node: one declaration can bind several names
// (`String? a, b;`) and therefore several field nodes, so a per-field owner
// would have to either duplicate the edge or pick one binding arbitrarily.
// Parameters and return types attribute to the enclosing function / method
// node. Typed locals attribute to the enclosing function via the same
// funcRange mechanism extractCalls uses, falling back to the file node when no
// function encloses the line (e.g. a typed local in a field initialiser).
func (e *DartExtractor) extractTypeUses(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	funcRanges := buildFuncRanges(result)

	// Dedup key across the whole file so a type that appears in two positions
	// attributed to the same owner emits a single edge (no double-emit).
	seen := map[string]bool{}
	emit := func(ownerID, typeText string, line int) {
		emitDartTypeUseEdges(ownerID, typeText, filePath, line, result, seen)
	}

	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "declaration":
			// Class / mixin / extension field: `[final|const|static] Foo bar;`.
			// Only treat it as a field when it lives in a type body — a bare
			// `declaration` elsewhere (forward signatures) has no owning field
			// node to attribute to.
			parent := node.Parent()
			if parent == nil {
				return
			}
			switch parent.Type() {
			case "class_body", "mixin_body", "extension_body", "enum_body":
			default:
				return
			}
			if t := dartLeadingTypeText(node, src); t != "" {
				emit(fileNode.ID, t, int(node.StartPoint().Row)+1)
			}

		case "local_variable_declaration":
			// `Foo x = ...;` inside a function/method body. The grammar wraps
			// the binding in initialized_variable_definition; the leading type
			// precedes the bound identifier.
			ivd := dartFirstChildOfType(node, "initialized_variable_definition")
			if ivd == nil {
				return
			}
			t := dartLeadingTypeText(ivd, src)
			if t == "" {
				return
			}
			line := int(node.StartPoint().Row) + 1
			owner := findEnclosingFunc(funcRanges, line)
			if owner == "" {
				owner = fileNode.ID
			}
			emit(owner, t, line)

		case "formal_parameter":
			// Parameter type: `Foo x` / `Foo? x` / `List<Foo> x`. Attribute to
			// the enclosing function / method. Default-value-only parameters
			// (`this.x`, `super.x`, untyped) carry no leading type → skipped.
			t := dartLeadingTypeText(node, src)
			if t == "" {
				return
			}
			line := int(node.StartPoint().Row) + 1
			owner := findEnclosingFunc(funcRanges, line)
			if owner == "" {
				owner = fileNode.ID
			}
			emit(owner, t, line)

		case "function_signature":
			// Declared return type — the type that precedes the function name.
			// Attribute to the function / method node enclosing the signature
			// line. dartMethodReturnType only captured the bare head; we want
			// the full `Future<Response>` so generic args surface too.
			t := dartReturnTypeText(node, src)
			if t == "" {
				return
			}
			line := int(node.StartPoint().Row) + 1
			owner := findEnclosingFunc(funcRanges, line)
			if owner == "" {
				owner = fileNode.ID
			}
			emit(owner, t, line)
		}
	})
}

// emitDartTypeUseEdges canonicalizes typeText to its bare named type(s) and
// appends one EdgeTypedAs (To "unresolved::"+type, Origin OriginASTInferred)
// per distinct, non-primitive type, deduped on (ownerID, type) via seen.
//
// Nullable suffixes (`?`) are stripped; generic / container wrappers
// (`List<Foo>`, `Future<Foo>`, `Map<K,Foo>`) are unwrapped to every named
// type argument so a container-typed declaration still references the element
// type. Dart primitives / builtins (int, double, num, bool, String, void,
// dynamic, Object, var, …) carry no useful target and are skipped.
func emitDartTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult, seen map[string]bool) {
	if ownerID == "" {
		return
	}
	for _, t := range dartNamedTypes(typeText) {
		if t == "" || isDartPrimitive(t) {
			continue
		}
		key := ownerID + "\x00" + t
		if seen[key] {
			continue
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     graph.EdgeTypedAs,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
		})
	}
}

// dartLeadingTypeText returns the verbatim source of the leading type
// annotation of a declaration-shaped node (class field declaration,
// initialized_variable_definition, formal_parameter). It returns "" when the
// declaration is type-inferred (`var` / `final` with no explicit type) or has
// no annotation. The leading type is whatever precedes the first bound
// identifier; a `nullable_type "?"` sibling is folded back in so `Foo?` is
// reported as `Foo?`.
func dartLeadingTypeText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	typeText := ""
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "final_builtin", "const_builtin", "static", "late", "covariant",
			"metadata", "documentation_comment", "comment":
			// modifiers / annotations that precede the type — skip.
			continue
		case "inferred_type":
			// `var` — no explicit type to attribute.
			return ""
		case "type_identifier", "type", "function_type", "record_type":
			typeText = strings.TrimSpace(c.Content(src))
		case "type_arguments":
			// Generic args follow the head type_identifier; the head's
			// Content does not include them, so append.
			if typeText != "" {
				typeText += strings.TrimSpace(c.Content(src))
			}
		case "nullable_type":
			if typeText != "" {
				typeText += "?"
			}
		case "initialized_identifier_list", "initialized_identifier", "identifier":
			// Reached the bound name — the type (if any) is complete.
			return typeText
		}
	}
	return typeText
}

// dartReturnTypeText returns the full declared return type of a
// function_signature, including generic arguments (`Future<Response>`). It
// returns "" when the signature has no leading return type (an inferred-void
// `build()`, a getter/setter, or an operator). The return type is whatever
// precedes the function name identifier.
func dartReturnTypeText(fnSig *sitter.Node, src []byte) string {
	if fnSig == nil {
		return ""
	}
	typeText := ""
	for i, _nc := 0, int(fnSig.NamedChildCount()); i < _nc; i++ {
		c := fnSig.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "type", "void_type":
			typeText = strings.TrimSpace(c.Content(src))
		case "type_arguments":
			if typeText != "" {
				typeText += strings.TrimSpace(c.Content(src))
			}
		case "nullable_type":
			if typeText != "" {
				typeText += "?"
			}
		case "identifier", "function_type":
			// Reached the function name — return type complete.
			return typeText
		}
	}
	return typeText
}

// dartFirstChildOfType returns the first named child of node whose type is
// nodeType, or nil.
func dartFirstChildOfType(node *sitter.Node, nodeType string) *sitter.Node {
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

// dartNamedTypes canonicalizes a Dart type-text into the set of named types it
// references. It strips nullable `?`, unwraps generic containers
// (`List<Foo>`, `Future<Foo>`, `Map<K, Foo>`) to every named type argument,
// and drops library prefixes (`prefix.Foo` → `Foo`). A simple `Foo` yields
// `[Foo]`; `Map<String, User>` yields `[String, User]` (String filtered out
// later as a primitive). Returns nil for empty / function / record types,
// which have no single named target.
func dartNamedTypes(typeText string) []string {
	typeText = strings.TrimSpace(typeText)
	if typeText == "" {
		return nil
	}
	// Strip trailing nullable markers.
	typeText = strings.TrimRight(typeText, "?")
	typeText = strings.TrimSpace(typeText)
	if typeText == "" {
		return nil
	}
	// Function / record types have no single named target.
	if strings.ContainsAny(typeText, "(") || strings.Contains(typeText, "Function") {
		return nil
	}

	var out []string
	// Split off generic arguments: head<args>.
	head := typeText
	args := ""
	if lt := strings.IndexByte(typeText, '<'); lt >= 0 && strings.HasSuffix(typeText, ">") {
		head = typeText[:lt]
		args = typeText[lt+1 : len(typeText)-1]
	}
	if h := dartBareTypeName(head); h != "" {
		out = append(out, h)
	}
	if args != "" {
		for _, a := range splitDartTypeArgs(args) {
			out = append(out, dartNamedTypes(a)...)
		}
	}
	return out
}

// dartBareTypeName strips a library prefix (`p.Foo` → `Foo`), nullable
// markers, and surrounding whitespace from a single (non-generic) type token.
func dartBareTypeName(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimRight(t, "?")
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	return strings.TrimSpace(t)
}

// splitDartTypeArgs splits a generic argument list at top-level commas,
// respecting nested `<…>` so `Map<String, List<User>>` splits into
// `String` and `List<User>`.
func splitDartTypeArgs(args string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '<', '(':
			depth++
		case '>', ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, args[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, args[start:])
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// isDartPrimitive reports whether t names a Dart builtin / primitive type that
// doesn't need a usage edge — emitting these would just clutter the graph with
// unresolved::int / unresolved::String edges that never land on a workspace
// declaration.
func isDartPrimitive(t string) bool {
	switch t {
	case "", "int", "double", "num", "bool", "String", "void", "dynamic",
		"Object", "Null", "Never", "var", "this":
		return true
	}
	return false
}
