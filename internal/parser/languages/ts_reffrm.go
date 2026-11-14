package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitTSReferenceForms walks a parsed TS / TSX file and emits the
// usage-counted reference edges for the TypeScript forms the type-use,
// cast, and render passes don't already cover. Every edge it produces is
// an EdgeReferences / EdgeExtends / EdgeImplements — kinds find_usages
// treats as a usage — so a type or component named only in one of these
// positions still lands cross-file on a no-LSP name-based index:
//
//   - JSX element names (`<App/>`, `<App>…</App>`, `<Foo.Bar/>`) →
//     EdgeReferences, ref_context="jsx". The existing EdgeRendersChild
//     pass records the parent→child component tree but rides on a kind
//     find_usages ignores and only fires inside function bodies; this
//     pass covers file-scope JSX too and emits the usage edge so
//     find_usages(App) surfaces the render site. Capitalised / qualified
//     names are components; lowercase intrinsic HTML elements are skipped.
//   - type-only import bindings (`import type { X }`, `import { type X }`)
//     → EdgeReferences, ref_context="import_type". The name is a type
//     used here, distinct from the module-level EdgeImports dependency.
//   - type-only re-export bindings (`export type { X } from`,
//     `export { type X } from`) → EdgeReferences, ref_context="export_type".
//   - class / interface heritage (`class App extends Base implements I`,
//     `interface Y extends Z`) → EdgeExtends / EdgeImplements.
//
// Decomposition / primitive + container dropping for heritage type
// arguments is delegated to tsTypeRefs (same gate as every other TS
// type-use form), so primitives, builtins, and lowercase non-components
// never become bogus targets. De-duplicated per (owner, kind, name, line).
func emitTSReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	seen := map[string]bool{}
	emit := func(ownerID, name string, kind graph.EdgeKind, refContext string, line int) {
		if ownerID == "" || name == "" {
			return
		}
		key := ownerID + "\x00" + string(kind) + "\x00" + name + "\x00" + strconv.Itoa(line)
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + name,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
			Meta:     map[string]any{"ref_context": refContext},
		})
	}
	ownerFor := func(line int) string {
		if owner := findEnclosingFunc(funcRanges, line); owner != "" {
			return owner
		}
		return fileID
	}

	walkTSNodes(root, func(n *sitter.Node) bool {
		switch n.Type() {
		case "jsx_opening_element", "jsx_self_closing_element":
			// jsx_element wraps a jsx_opening_element child; visiting the
			// opening element directly covers both `<App>…</App>` and
			// `<App/>` without double-emitting (the closing tag carries the
			// same name but the dedup key collapses it anyway — and we only
			// match opening / self-closing here).
			if name := jsxElementName(n, src); name != "" && isJSXComponentName(name) {
				line := int(n.StartPoint().Row) + 1
				emit(ownerFor(line), name, graph.EdgeReferences, "jsx", line)
			}
		case "import_statement":
			emitTSTypeOnlyImportRefs(n, src, fileID, graph.EdgeReferences, "import_type", emit)
		case "export_statement":
			emitTSTypeOnlyExportRefs(n, src, fileID, graph.EdgeReferences, "export_type", emit)
		case "class_declaration", "abstract_class_declaration", "class":
			emitTSClassHeritageRefs(n, src, filePath, emit)
		case "interface_declaration":
			emitTSInterfaceHeritageRefs(n, src, filePath, emit)
		}
		return true
	})
}

// emitTSTypeOnlyImportRefs emits one reference edge per type-only import
// binding. Two shapes carry the `type` modifier:
//
//	import type { Foo, Bar } from "mod"   // statement-level: every binding is a type
//	import { type Foo, value } from "mod" // specifier-level: only `type Foo` is a type
//
// A value import (`import { value }`) is intentionally not referenced —
// `value` is already a call / read at its use sites; only the type-only
// form names a type purely in import position with no other use edge.
func emitTSTypeOnlyImportRefs(importNode *sitter.Node, src []byte, fileID string, kind graph.EdgeKind, refContext string, emit func(ownerID, name string, kind graph.EdgeKind, refContext string, line int)) {
	stmtTypeOnly := tsImportStatementTypeOnly(importNode, src)
	if stmtTypeOnly {
		// Default type-only import: `import type App from "mod"` binds App as a
		// type (a specifier-level `import { type X }` carries no default form).
		if clause := findChildByType(importNode, "import_clause"); clause != nil {
			for i, _nc := 0, int(clause.NamedChildCount()); i < _nc; i++ {
				c := clause.NamedChild(i)
				if c == nil || c.Type() != "identifier" {
					continue
				}
				if name := strings.TrimSpace(c.Content(src)); name != "" && !isTSPrimitive(name) && isTSTypeName(name) {
					emit(fileID, name, kind, refContext, int(c.StartPoint().Row)+1)
				}
				break
			}
		}
	}
	named := jsNamedImportsNode(importNode)
	if named == nil {
		return
	}
	for i, _nc := 0, int(named.NamedChildCount()); i < _nc; i++ {
		spec := named.NamedChild(i)
		if spec == nil || spec.Type() != "import_specifier" {
			continue
		}
		if !stmtTypeOnly && !tsSpecifierTypeOnly(spec, src) {
			continue
		}
		orig, _ := jsSpecifierNames(spec, src)
		if orig == "" || isTSPrimitive(orig) || !isTSTypeName(orig) {
			continue
		}
		emit(fileID, orig, kind, refContext, int(spec.StartPoint().Row)+1)
	}
}

// emitTSTypeOnlyExportRefs mirrors emitTSTypeOnlyImportRefs for re-export
// statements (`export type { X } from`, `export { type X } from`). A
// type-only re-export names the original type, so the barrel file
// references it — find_usages(X) should surface the forwarding site.
func emitTSTypeOnlyExportRefs(exportNode *sitter.Node, src []byte, fileID string, kind graph.EdgeKind, refContext string, emit func(ownerID, name string, kind graph.EdgeKind, refContext string, line int)) {
	clause := findChildByType(exportNode, "export_clause")
	if clause == nil {
		return
	}
	stmtTypeOnly := tsExportStatementTypeOnly(exportNode, src)
	for i, _nc := 0, int(clause.NamedChildCount()); i < _nc; i++ {
		spec := clause.NamedChild(i)
		if spec == nil || spec.Type() != "export_specifier" {
			continue
		}
		if !stmtTypeOnly && !tsSpecifierTypeOnly(spec, src) {
			continue
		}
		orig, _ := jsSpecifierNames(spec, src)
		if orig == "" || isTSPrimitive(orig) || !isTSTypeName(orig) {
			continue
		}
		emit(fileID, orig, kind, refContext, int(spec.StartPoint().Row)+1)
	}
}

// tsImportStatementTypeOnly reports whether an import_statement carries
// the statement-level `type` modifier (`import type { … } from`). The
// grammar exposes it as a bare `type` keyword token between `import` and
// the import_clause rather than a named field, so scan the statement's
// raw children for it, stopping at the clause body.
func tsImportStatementTypeOnly(importNode *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(importNode.ChildCount()); i < _nc; i++ {
		c := importNode.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "import_clause" {
			break
		}
		if c.Type() == "type" || (!c.IsNamed() && c.Content(src) == "type") {
			return true
		}
	}
	return false
}

// tsExportStatementTypeOnly reports whether an export_statement carries
// the statement-level `type` modifier (`export type { … } from`). The
// keyword sits between `export` and the export_clause.
func tsExportStatementTypeOnly(exportNode *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(exportNode.ChildCount()); i < _nc; i++ {
		c := exportNode.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "export_clause" {
			break
		}
		if c.Type() == "type" || (!c.IsNamed() && c.Content(src) == "type") {
			return true
		}
	}
	return false
}

// tsSpecifierTypeOnly reports whether a single import_specifier /
// export_specifier carries an inline `type` modifier (`{ type Foo }`).
func tsSpecifierTypeOnly(spec *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(spec.ChildCount()); i < _nc; i++ {
		c := spec.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "type" {
			return true
		}
		if !c.IsNamed() && c.Content(src) == "type" {
			return true
		}
	}
	return false
}

// emitTSClassHeritageRefs emits EdgeExtends for a class's `extends` base
// and EdgeImplements for each `implements` interface. The base may be a
// bare identifier (`extends Base`), a member expression
// (`extends React.Component`), or carry type arguments
// (`extends Component<Props, State>`) — each surfaces the named base type
// plus any user-defined type arguments via tsTypeRefs.
func emitTSClassHeritageRefs(classNode *sitter.Node, src []byte, filePath string, emit func(ownerID, name string, kind graph.EdgeKind, refContext string, line int)) {
	classID := tsTypeDeclID(classNode, src, filePath)
	if classID == "" {
		return
	}
	heritage := findChildByType(classNode, "class_heritage")
	if heritage == nil {
		return
	}
	for i, _nc := 0, int(heritage.NamedChildCount()); i < _nc; i++ {
		clause := heritage.NamedChild(i)
		if clause == nil {
			continue
		}
		switch clause.Type() {
		case "extends_clause":
			line := int(clause.StartPoint().Row) + 1
			if val := clause.ChildByFieldName("value"); val != nil {
				if name := tsHeritageName(val, src); name != "" {
					emit(classID, name, graph.EdgeExtends, graph.RefContextInherit, line)
				}
			}
			// Type arguments on the base (`extends Component<Props, State>`)
			// are themselves references — decompose them through the same
			// gate as every other type-use form.
			if targs := clause.ChildByFieldName("type_arguments"); targs != nil {
				for _, ref := range tsTypeRefs(targs.Content(src)) {
					emit(classID, ref, graph.EdgeReferences, graph.RefContextGenericArg, line)
				}
			}
		case "implements_clause":
			line := int(clause.StartPoint().Row) + 1
			for j, _nc := 0, int(clause.NamedChildCount()); j < _nc; j++ {
				t := clause.NamedChild(j)
				if t == nil {
					continue
				}
				if name := tsHeritageName(t, src); name != "" {
					emit(classID, name, graph.EdgeImplements, graph.RefContextInherit, line)
				}
			}
		}
	}
}

// emitTSInterfaceHeritageRefs emits EdgeExtends for each base of an
// `interface Y extends Z, W<Foo>` declaration. The interface grammar
// nests the bases under an extends_type_clause (distinct from the class
// extends_clause); each base is a type_identifier or generic_type.
func emitTSInterfaceHeritageRefs(ifaceNode *sitter.Node, src []byte, filePath string, emit func(ownerID, name string, kind graph.EdgeKind, refContext string, line int)) {
	ifaceID := tsTypeDeclID(ifaceNode, src, filePath)
	if ifaceID == "" {
		return
	}
	clause := findChildByType(ifaceNode, "extends_type_clause")
	if clause == nil {
		return
	}
	line := int(clause.StartPoint().Row) + 1
	for i, _nc := 0, int(clause.NamedChildCount()); i < _nc; i++ {
		t := clause.NamedChild(i)
		if t == nil {
			continue
		}
		if name := tsHeritageName(t, src); name != "" {
			emit(ifaceID, name, graph.EdgeExtends, graph.RefContextInherit, line)
		}
		// Type arguments on a base interface (`extends W<Foo>`) reference Foo.
		if t.Type() == "generic_type" {
			if targs := t.ChildByFieldName("type_arguments"); targs != nil {
				for _, ref := range tsTypeRefs(targs.Content(src)) {
					emit(ifaceID, ref, graph.EdgeReferences, graph.RefContextGenericArg, line)
				}
			}
		}
	}
}

// tsHeritageName returns the bare base-type name from a heritage node —
// an identifier / type_identifier (`Base`), a member / nested expression
// (`React.Component` → `Component`), or a generic_type (`W<Foo>` → `W`).
// Returns "" when the name is a primitive (defensive — a heritage base
// is never `string`, but the gate keeps the contract uniform).
func tsHeritageName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier", "type_identifier":
		name := n.Content(src)
		if isTSPrimitive(name) || !isTSTypeName(name) {
			return ""
		}
		return name
	case "member_expression", "nested_type_identifier", "nested_identifier":
		// `React.Component` / `ns.Base` — the last segment is the type.
		txt := strings.TrimSpace(n.Content(src))
		if i := strings.LastIndex(txt, "."); i >= 0 {
			txt = txt[i+1:]
		}
		if isTSPrimitive(txt) || !isTSTypeName(txt) {
			return ""
		}
		return txt
	case "generic_type":
		if name := n.ChildByFieldName("name"); name != nil {
			return tsHeritageName(name, src)
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c != nil && (c.Type() == "type_identifier" || c.Type() == "identifier" || c.Type() == "nested_type_identifier") {
				return tsHeritageName(c, src)
			}
		}
	}
	return ""
}

// tsTypeDeclID returns the graph node ID of a class_declaration /
// interface_declaration — `<filePath>::<Name>`, matching the convention
// emitClass / emitInterface stamp. Returns "" for an anonymous shape.
func tsTypeDeclID(declNode *sitter.Node, src []byte, filePath string) string {
	name := declNode.ChildByFieldName("name")
	if name == nil {
		for i, _nc := 0, int(declNode.NamedChildCount()); i < _nc; i++ {
			c := declNode.NamedChild(i)
			if c != nil && (c.Type() == "type_identifier" || c.Type() == "identifier") {
				name = c
				break
			}
		}
	}
	if name == nil {
		return ""
	}
	return filePath + "::" + name.Content(src)
}
