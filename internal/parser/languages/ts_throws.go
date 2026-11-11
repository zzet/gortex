package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitTSThrowsEdges walks a TypeScript function body for throw_statement
// nodes and emits one EdgeThrows per distinct exception type. The
// extractor recognises the three idiomatic shapes:
//
//	throw new MyError("msg")     -> "MyError"
//	throw MyError                -> "MyError"
//	throw errs.MyError           -> "MyError" (trailing identifier of the
//	                                 member-expression chain)
//
// String / numeric / template-literal throws (`throw "oops"`) are
// skipped — they don't carry a referenceable type and would just create
// `unresolved::string` noise in the graph. Re-throws (`throw caught` in
// a catch clause that captured a typed name) fall through to the
// identifier branch and resolve like any other.
//
// Origin is OriginASTInferred because TS doesn't enforce a checked-
// exception contract — the body scan is a best-effort summary of every
// type that can propagate, not a proof. The resolver upgrades to
// OriginLSPResolved when the language server confirms the target.
//
// Nested function / arrow / method bodies are skipped: their throws
// belong to those inner scopes, not the enclosing function. This
// matches the Python and Rust emitters' walk policy.
func emitTSThrowsEdges(funcNode *sitter.Node, src []byte, fromID, filePath string, result *parser.ExtractionResult) {
	if funcNode == nil || fromID == "" {
		return
	}
	body := tsFunctionBody(funcNode)
	if body == nil {
		return
	}
	seen := map[string]bool{}
	tsWalkThrows(body, src, fromID, filePath, seen, result)
}

// tsWalkThrows is the recursive descent helper for emitTSThrowsEdges.
// Pulled out to keep the entry-point readable and to make the
// "don't descend into nested function bodies" rule visible at the
// single place that enforces it.
func tsWalkThrows(node *sitter.Node, src []byte, fromID, filePath string, seen map[string]bool, result *parser.ExtractionResult) {
	if node == nil {
		return
	}
	if node.Type() == "throw_statement" {
		name := tsThrowTypeName(node, src)
		if name != "" && !seen[name] {
			seen[name] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fromID,
				To:       "unresolved::" + name,
				Kind:     graph.EdgeThrows,
				FilePath: filePath,
				Line:     int(node.StartPoint().Row) + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
		return
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		// Skip nested scopes — their throws belong to those scopes.
		switch c.Type() {
		case "function_declaration", "function_expression", "arrow_function",
			"method_definition", "generator_function_declaration",
			"class_declaration", "abstract_class_declaration", "class_expression":
			continue
		}
		tsWalkThrows(c, src, fromID, filePath, seen, result)
	}
}

// tsThrowTypeName returns the typed name being thrown, or "" when the
// throw expression isn't a referenceable type. Handles:
//
//	throw new Foo(...) / throw new ns.Foo(...) — new_expression
//	throw Foo / throw Foo.SubFoo                — identifier / member_expression
//
// Anything else (string literal, template string, number, undefined,
// computed property access) returns "".
func tsThrowTypeName(throwNode *sitter.Node, src []byte) string {
	if throwNode == nil {
		return ""
	}
	for i, _nc := 0, int(throwNode.NamedChildCount()); i < _nc; i++ {
		c := throwNode.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "new_expression":
			ctor := c.ChildByFieldName("constructor")
			if ctor == nil {
				continue
			}
			return tsTrailingIdentifier(ctor, src)
		case "identifier":
			return c.Content(src)
		case "member_expression":
			// `errs.MyError.Variant` → return the trailing identifier.
			// Captures the exception's leaf name regardless of
			// namespace depth.
			return tsTrailingIdentifier(c, src)
		case "call_expression":
			// `throw makeError(...)` — record the constructor-style
			// identifier so the resolver can still upgrade origin once
			// the call's return type is known. Skip when the callee
			// isn't a bare identifier (avoids `throw a.b.c()` producing
			// `c` as a throw type, which is misleading).
			fn := c.ChildByFieldName("function")
			if fn == nil || fn.Type() != "identifier" {
				continue
			}
			return fn.Content(src)
		}
	}
	return ""
}

// tsTrailingIdentifier walks a member_expression / nested_identifier
// chain returning the last (rightmost) identifier component. For
// `errs.kinds.MyError` returns "MyError"; for bare `Foo` returns
// "Foo". Returns "" for any node that isn't an identifier or a chain.
func tsTrailingIdentifier(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier", "type_identifier":
		return node.Content(src)
	case "member_expression":
		// Walk the property side first (the rightmost segment of the chain).
		if prop := node.ChildByFieldName("property"); prop != nil {
			if name := tsTrailingIdentifier(prop, src); name != "" {
				return name
			}
		}
	case "nested_identifier":
		// `ns.Inner.Leaf` — last NamedChild is the leaf.
		count := int(node.NamedChildCount())
		if count == 0 {
			return ""
		}
		return tsTrailingIdentifier(node.NamedChild(count-1), src)
	}
	// Fall back to the verbatim content so unusual shapes still
	// surface SOMETHING the resolver can dedupe against.
	return node.Content(src)
}
