package languages

import (
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

func visitJSDeclaration(n *sitter.Node, src []byte, visit func(string)) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "function_declaration", "generator_function_declaration", "class_declaration", "enum_declaration":
		visitJSPattern(n.ChildByFieldName("name"), src, visit)
	case "lexical_declaration", "variable_declaration":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			v := n.NamedChild(i)
			if v.Type() == "variable_declarator" {
				visitJSPattern(v.ChildByFieldName("name"), src, visit)
			}
		}
	}
}

// Only the program's export statements define the module's value surface.
func recordJSExports(root *sitter.Node, src []byte, file *graph.Node) {
	var bindings strings.Builder
	for i := 0; i < int(root.NamedChildCount()); i++ {
		s := root.NamedChild(i)
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
			for _, sp := range jsCollectChildren(clause, "export_specifier") {
				orig, alias := jsSpecifierNames(sp, src)
				if orig != "" && !tsSpecifierTypeOnly(sp, src) {
					if alias == "" {
						alias = orig
					}
					graph.AppendJSBinding(&bindings, alias, orig)
				}
			}
		}
	}
	if bindings.Len() != 0 {
		if file.Meta == nil {
			file.Meta = map[string]any{}
		}
		file.Meta[graph.JSLocalExportsMeta] = bindings.String()
	}
}

// Cache direct declarations once per scope. A sibling function's declarations
// cannot shadow this call; parameters of every enclosing function can.
type jsScopeKey struct {
	start, end uint32
	kind       string
}
type jsScopeCache map[jsScopeKey]map[string]string

func (cache *jsScopeCache) stamp(edge *graph.Edge, expr *sitter.Node, name string, src []byte) {
	for n := expr.Parent(); n != nil; n = n.Parent() {
		kind := n.Type()
		switch kind {
		case "program", "statement_block", "function_declaration", "generator_function_declaration", "method_definition", "function_expression", "generator_function", "arrow_function", "catch_clause", "for_statement", "for_in_statement":
		default:
			continue
		}
		key := jsScopeKey{n.StartByte(), n.EndByte(), kind}
		names, ok := (*cache)[key]
		if !ok {
			add := func(name, value string) {
				if names == nil {
					names = map[string]string{}
				}
				names[name] = value
			}
			switch kind {
			case "program", "statement_block":
				for i := 0; i < int(n.NamedChildCount()); i++ {
					s := n.NamedChild(i)
					if s.Type() == "export_statement" {
						s = s.ChildByFieldName("declaration")
					}
					visitJSDeclaration(s, src, func(id string) { add(id, "local") })
				}
			case "for_statement", "for_in_statement":
				visitJSDeclaration(n.ChildByFieldName("initializer"), src, func(id string) { add(id, "local") })
				if n.ChildByFieldName("kind") != nil {
					visitJSPattern(n.ChildByFieldName("left"), src, func(id string) { add(id, "local") })
				}
			default:
				param := func(id string) { add(id, graph.JSPendingParameter) }
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
