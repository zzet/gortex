package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitGoTypeArgReferences captures the composite/generic type positions
// whose element / argument types are dropped by the primary type-name
// passes, so a find_usages of a type surfaces every place the type is
// mentioned — not just the bare-name declarations.
//
// The primary passes attribute a single name per type position:
// canonicalizeGoTypeRef reduces `Foo[Bar]` to `Foo`, and returns "" for
// `map[K]V` and `chan E` entirely. That means the argument `Bar`, the
// map key `K` and value `V`, and the channel element `E` produce no
// reference edge at all. This pass walks the three node kinds whose
// inner types are otherwise lost and emits one EdgeReferences per
// element/argument type:
//
//   - generic_type   `List[Foo]` / `Map[K, V]` — every type_arguments
//     entry. Mirrors the Kotlin type_arguments handling.
//   - map_type       `map[K]V` — both the key and the value type.
//   - channel_type   `chan E` / `<-chan E` / `chan<- E` — the element.
//
// Nested composites are walked transitively (`map[string][]Foo`,
// `chan Option[Bar]`, `List[map[K]V]`), so the innermost named types
// surface regardless of how deeply they are wrapped. Slice / array /
// pointer ELEMENT types are already canonicalised to a name by the
// existing param / return / field passes, so the bare wrappers are not
// re-emitted here on their own — but they are descended into when they
// appear inside one of the three lost positions above.
//
// Zero false positives: only type_identifier / qualified_type leaves
// are emitted, run through canonicalizeGoTypeRef (which drops
// primitives, package qualifiers, and the generic suffix). Builtins
// (`isGoBuiltinOrKeyword`) and type-parameter names declared anywhere
// in the file (collected from type_parameter_list) are skipped, so a
// generic instantiation `List[T]` inside a `func F[T any]` never emits
// a reference to a phantom type `T`. Edges are deduped per
// (owner, type, line, ref_context) and attributed to the enclosing
// function (or the file node for package-level declarations).
func emitGoTypeArgReferences(work goTailWork, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	typeParams, containers := work.typeParams, work.typeContainers
	seen := map[string]bool{}
	emit := func(name string, line int) {
		t := canonicalizeGoTypeRef(name)
		if t == "" || isGoBuiltinOrKeyword(t) || typeParams[t] {
			return
		}
		ownerID := findEnclosingFunc(funcRanges, line)
		if ownerID == "" {
			ownerID = fileID
		}
		if ownerID == "" {
			return
		}
		key := ownerID + "\x00" + t + "\x00" + intKey(line)
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     graph.EdgeReferences,
			FilePath: filePath,
			Line:     line,
			// OriginASTResolved (not Inferred): these are structural type
			// references read straight off the AST — a generic argument,
			// a map key/value, or a channel element. The name binds to
			// the (unique) type of that name in the repo with no
			// ambiguity, exactly like the type-position EdgeTypedAs /
			// EdgeReturns edges, so ast_resolved keeps the cross-package
			// name-match guard from dropping them as name-only guesses.
			Origin: graph.OriginASTResolved,
			Meta:   map[string]any{"ref_context": "generic_arg"},
		})
	}

	// emitTypeLeaves descends a type subtree and emits a reference for
	// every named-type leaf it finds, recursing through nested
	// composites. Used on the inner positions of the three lost node
	// kinds.
	var emitTypeLeaves func(n *sitter.Node)
	emitTypeLeaves = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "type_identifier", "qualified_type":
			emit(n.Content(src), int(n.StartPoint().Row)+1)
		default:
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				emitTypeLeaves(n.NamedChild(i))
			}
		}
	}

	for _, n := range containers {
		switch n.Type() {
		case "type_arguments":
			// Generic instantiation arguments: the bracketed list under a
			// generic_type. Each entry is a type_elem (or a bare type
			// node on older grammars); descend into all of them.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				emitTypeLeaves(n.NamedChild(i))
			}
		case "map_type":
			// Both the key and the value type are otherwise lost. The
			// grammar exposes them via the key/value fields when present;
			// fall back to all named children so older grammar shapes
			// (two bare type_identifier children) are still covered.
			key := n.ChildByFieldName("key")
			value := n.ChildByFieldName("value")
			if key != nil || value != nil {
				emitTypeLeaves(key)
				emitTypeLeaves(value)
			} else {
				for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
					emitTypeLeaves(n.NamedChild(i))
				}
			}
		case "channel_type":
			// The element type of a channel. The value field names it
			// when present; otherwise the sole named child is the
			// element.
			if value := n.ChildByFieldName("value"); value != nil {
				emitTypeLeaves(value)
			} else {
				for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
					emitTypeLeaves(n.NamedChild(i))
				}
			}
		}
	}
}
