package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// A same-named declaration is not necessarily the value imported by name:
// `export { other as work }` can coexist with a private `work`.
func jsNamedExportMeta(decl *sitter.Node, meta map[string]any) map[string]any {
	parent := decl.Parent()
	if parent == nil || parent.Type() != "export_statement" || findChildByType(parent, "default") != nil {
		return meta
	}
	if scope := parent.Parent(); scope == nil || scope.Type() != "program" {
		return meta
	}
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["js_named_export"] = true
	return meta
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

// A local initialized from require() still names that module's export, so it
// stays eligible for import evidence.
func jsRequireInitializer(declarator *sitter.Node, src []byte) bool {
	value := declarator.ChildByFieldName("value")
	for value != nil && value.Type() == "member_expression" {
		value = value.ChildByFieldName("object")
	}
	if value == nil || value.Type() != "call_expression" {
		return false
	}
	callee := value.ChildByFieldName("function")
	return callee != nil && callee.Content(src) == "require"
}

func visitJSDeclaration(n *sitter.Node, src []byte, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "ambient_declaration":
		visitJSDeclaration(n.NamedChild(0), src, visit)
	case "function_signature", "function_declaration", "generator_function_declaration", "class_declaration", "enum_declaration":
		visitJSPattern(n.ChildByFieldName("name"), src, visit)
	case "lexical_declaration", "variable_declaration":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			v := n.NamedChild(i)
			if v.Type() == "variable_declarator" && !jsRequireInitializer(v, src) {
				visitJSPattern(v.ChildByFieldName("name"), src, visit)
			}
		}
	}
}

// var crosses statement-block boundaries but not function or class boundaries.
// Only descend through statement containers: expressions cannot introduce a
// var in this scope, even when they contain an entire function expression.
func visitJSHoistedVars(n *sitter.Node, src []byte, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "variable_declaration":
		visitJSDeclaration(n, src, visit)
		return
	case "for_in_statement":
		if kind := n.ChildByFieldName("kind"); kind != nil && kind.Type() == "var" {
			visitJSPattern(n.ChildByFieldName("left"), src, visit)
		}
	case "statement_block", "if_statement", "else_clause", "switch_statement", "switch_body",
		"switch_case", "switch_default", "for_statement", "while_statement", "do_statement",
		"try_statement", "catch_clause", "finally_clause", "with_statement", "labeled_statement":
	default:
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		visitJSHoistedVars(n.NamedChild(i), src, visit)
	}
}

func visitJSBlockDeclaration(n *sitter.Node, src []byte, visit func(string)) {
	if n != nil && n.Type() != "variable_declaration" {
		visitJSDeclaration(n, src, visit)
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
type jsScopeCache map[jsScopeKey]map[string]string

func (cache *jsScopeCache) stamp(edge *graph.Edge, expr *sitter.Node, name string, src []byte) {
	for child, n := expr, expr.Parent(); n != nil; child, n = n, n.Parent() {
		kind := n.Type()
		switch kind {
		case "statement_block", "switch_body", "function_declaration", "generator_function_declaration", "method_definition", "function_expression", "generator_function", "arrow_function", "catch_clause", "for_statement", "for_in_statement":
		default:
			continue
		}
		// The body sees hoisted var declarations; parameter initializers do
		// not. Cache the two environments separately for the same function.
		body := child.Type() == "statement_block"
		key := jsScopeKey{n.StartByte(), n.EndByte(), kind, body}
		names, ok := (*cache)[key]
		if !ok {
			add := func(name, value string) {
				if names == nil {
					names = map[string]string{}
				}
				names[name] = value
			}
			local := func(id string) { add(id, graph.JSLexicalLocal) }
			switch kind {
			case "statement_block":
				// A class static block is its own var scope.
				if p := n.Parent(); p != nil && p.Type() == "class_static_block" {
					visitJSHoistedVars(n, src, local)
				}
				for i := 0; i < int(n.NamedChildCount()); i++ {
					visitJSBlockDeclaration(n.NamedChild(i), src, local)
				}
			case "switch_body":
				for i := 0; i < int(n.NamedChildCount()); i++ {
					clause := n.NamedChild(i)
					for j := 0; j < int(clause.NamedChildCount()); j++ {
						visitJSBlockDeclaration(clause.NamedChild(j), src, local)
					}
				}
			case "for_statement", "for_in_statement":
				visitJSBlockDeclaration(n.ChildByFieldName("initializer"), src, local)
				if k := n.ChildByFieldName("kind"); k != nil && k.Type() != "var" {
					visitJSPattern(n.ChildByFieldName("left"), src, local)
				}
			default:
				param := func(id string) { add(id, graph.JSPendingParameter) }
				if body && kind != "catch_clause" {
					visitJSHoistedVars(child, src, local)
				}
				if kind == "function_expression" || kind == "generator_function" {
					visitJSPattern(n.ChildByFieldName("name"), src, local)
				}
				visitJSPattern(n.ChildByFieldName("parameters"), src, param)
				visitJSPattern(n.ChildByFieldName("parameter"), src, param)
			}
			if *cache == nil {
				*cache = jsScopeCache{}
			}
			(*cache)[key] = names
		}
		if binding := names[name]; binding != "" {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta[graph.JSLexicalBindingMeta] = binding
			return
		}
	}
}

// Calls with one From/To/Line identity collapse into one stored edge. When a
// real call shares that site with a shadowed one, the stored edge must stay
// eligible for the real call's binding.
func clearCollapsedJSShadows(edges []*graph.Edge) {
	shadowed := make(map[graph.EdgeIdentity][]*graph.Edge)
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && e.Meta[graph.JSLexicalBindingMeta] != nil {
			id := graph.EdgeIdentityFor(e)
			shadowed[id] = append(shadowed[id], e)
		}
	}
	if len(shadowed) == 0 {
		return
	}
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.Meta[graph.JSLexicalBindingMeta] != nil {
			continue
		}
		id := graph.EdgeIdentityFor(e)
		for _, s := range shadowed[id] {
			delete(s.Meta, graph.JSLexicalBindingMeta)
		}
		delete(shadowed, id)
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
