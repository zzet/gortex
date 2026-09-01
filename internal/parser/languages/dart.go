package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/dart"
)

// DartExtractor extracts Dart source files.
type DartExtractor struct {
	lang *sitter.Language
}

func NewDartExtractor() *DartExtractor {
	return &DartExtractor{lang: dart.GetLanguage()}
}

func (e *DartExtractor) Language() string     { return "dart" }
func (e *DartExtractor) Extensions() []string { return []string{".dart"} }

func (e *DartExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "dart",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if dartIsGenerated(filePath) {
		// Generated Dart (build_runner .g.dart, freezed, protobuf .pb.dart, …)
		// is machine-emitted boilerplate that mirrors hand-written declarations.
		// Keep the file node so incremental tracking sees the file, but skip
		// symbol extraction so duplicate generated symbols never pollute
		// search / find_usages. The marker lets callers tell it apart.
		fileNode.Meta = map[string]any{"generated": true}
		return result, nil
	}

	seen := make(map[string]bool)

	// Classes, enums, mixins, extensions — walk the tree to distinguish types.
	e.extractTypes(root, src, filePath, fileNode, result, seen)

	// Methods inside class/mixin/enum/extension bodies.
	e.extractMethods(root, src, filePath, fileNode, result, seen)

	// Fields inside class/mixin/enum/extension bodies. Runs after
	// extractMethods so an accessor keeps the unsuffixed `Type.name` id when a
	// backing field of the same name exists.
	e.extractFields(root, src, filePath, fileNode, result, seen)

	// Top-level functions (function_signature + function_body at program level).
	e.extractTopLevelFunctions(root, src, filePath, fileNode, result, seen)

	// Top-level variables.
	e.extractTopLevelVariables(root, src, filePath, fileNode, result, seen)

	// Imports — also returns the alias map (`import 'pkg:foo/bar.dart'
	// as f;` → "f" → "package:foo/bar.dart") so the call walker can
	// attribute alias-prefixed calls (`f.method()`) to the right URI.
	imports := e.extractImports(root, src, filePath, fileNode, result)

	// Call sites.
	e.extractCalls(root, src, filePath, result, imports)
	e.mineDartFactoryChains(root, src, filePath, result)

	// Cross-file type-usage edges for declaration-position types (fields,
	// parameters, return types, typed locals) — runs after the symbol
	// extractors so the enclosing-owner ranges are populated.
	e.extractTypeUses(root, src, filePath, fileNode, result)

	// Expression-site reference edges — instantiation, inheritance, casts /
	// type-tests, and static access. Runs after the symbol extractors so the
	// enclosing-owner ranges and local-type set are populated.
	e.emitDartReferenceForms(root, src, filePath, fileNode, result)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	return result, nil
}

// extractTypes walks the root for class_definition, enum_declaration, mixin_declaration, extension_declaration.
func (e *DartExtractor) extractTypes(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	walkNodes(root, func(node *sitter.Node) {
		var name string
		var kind graph.NodeKind

		switch node.Type() {
		case "class_definition":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

			// Check for abstract interface class → KindInterface.
			if e.hasChildType(node, "abstract") && e.hasChildType(node, "interface") {
				kind = graph.KindInterface
			}

		case "enum_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

		case "mixin_declaration":
			// mixin_declaration has identifier as a child, not a named field.
			name = e.findChildIdentifier(node, src)
			if name == "" {
				return
			}
			kind = graph.KindType

		case "extension_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				// Anonymous extension — skip.
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

		default:
			return
		}

		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		meta := map[string]any{"visibility": VisibilityByUnderscore(name)}
		if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangSlashSlash); doc != "" {
			meta["doc"] = doc
		}
		if node.Type() == "class_definition" {
			e.dartMarkFlutterWidget(meta, node, src)
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "dart",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
		})
		if node.Type() == "class_definition" {
			e.emitDartMixinEdges(node, src, id, filePath, result)
		}
		if node.Type() == "enum_declaration" {
			e.emitDartEnumConstants(node, src, id, name, filePath, result, seen)
		}
	})
}

// emitDartEnumConstants emits one KindEnumMember per `enum_constant` in an
// enum body, linked to the enum with EdgeMemberOf. A constant of an enhanced
// enum carries an argument_part (`mercury(3.3e23, 2.44e6)`) after its name, so
// only the constant's first identifier child names it.
func (e *DartExtractor) emitDartEnumConstants(
	enumNode *sitter.Node, src []byte, enumID, enumName, filePath string,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	body := dartFirstChildOfType(enumNode, "enum_body")
	if body == nil {
		return
	}
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		constant := body.Child(i)
		if constant == nil || constant.Type() != "enum_constant" {
			continue
		}
		name := e.findChildIdentifier(constant, src)
		if name == "" {
			continue
		}
		startLine := int(constant.StartPoint().Row) + 1
		id := enumID + "." + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindEnumMember, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(constant.EndPoint().Row) + 1,
			Language: "dart",
			Meta:     map[string]any{"receiver": enumName, "enum": enumID},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: enumID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
		})
	}
}

// dartSuperclassName returns the name of a class's `extends` base — the
// direct type_identifier child of the `superclass` node (the `with` /
// `implements` types are nested under mixins / interfaces). Returns ""
// when the class has no extends base.
func (e *DartExtractor) dartSuperclassName(classNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(classNode.ChildCount()); i < _nc; i++ {
		sup := classNode.Child(i)
		if sup == nil || sup.Type() != "superclass" {
			continue
		}
		for j, _nc := 0, int(sup.ChildCount()); j < _nc; j++ {
			c := sup.Child(j)
			if c != nil && c.Type() == "type_identifier" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// dartMarkFlutterWidget stamps the Flutter component marker onto a class
// meta map when the class extends StatelessWidget / StatefulWidget.
func (e *DartExtractor) dartMarkFlutterWidget(meta map[string]any, classNode *sitter.Node, src []byte) {
	switch e.dartSuperclassName(classNode, src) {
	case "StatelessWidget":
		meta["ui_component"] = "flutter"
		meta["component_kind"] = "stateless"
		meta["type_flavor"] = "class"
	case "StatefulWidget":
		meta["ui_component"] = "flutter"
		meta["component_kind"] = "stateful"
		meta["type_flavor"] = "class"
	}
}

// emitDartMixinEdges emits an EdgeExtends edge (Meta via="mixin") from a class
// to each type named in its `with` clause, so a Dart mixin application is a
// first-class subtype relationship in the graph and the mixed-in members are
// reachable through the class — codegraph does not model mixins at all. The
// grammar nests them as class_definition → superclass → mixins → type_identifier.
func (e *DartExtractor) emitDartMixinEdges(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult) {
	for i, _nc := 0, int(classNode.ChildCount()); i < _nc; i++ {
		sup := classNode.Child(i)
		if sup.Type() != "superclass" {
			continue
		}
		for j, _nc := 0, int(sup.ChildCount()); j < _nc; j++ {
			mixins := sup.Child(j)
			if mixins.Type() != "mixins" {
				continue
			}
			for k, _nc := 0, int(mixins.ChildCount()); k < _nc; k++ {
				m := mixins.Child(k)
				if m.Type() != "type_identifier" {
					continue
				}
				name := m.Content(src)
				result.Edges = append(result.Edges, &graph.Edge{
					From: classID, To: "unresolved::" + name,
					Kind: graph.EdgeExtends, FilePath: filePath, Line: int(m.StartPoint().Row) + 1,
					Meta: map[string]any{"via": "mixin"},
				})
			}
		}
	}
}

// extractMethods finds method_signature nodes inside class_body, extension_body, enum_body.
// A constructor with no body (named, const, redirecting factory) is not wrapped
// in method_signature at all — the grammar hangs its signature off a plain
// `declaration`, so both wrappers are visited in the one walk.
func (e *DartExtractor) extractMethods(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	// Collect type body ranges for ownership detection.
	typeBodyRanges := e.collectTypeBodyRanges(root, src)

	walkNodes(root, func(node *sitter.Node) {
		nodeType := node.Type()
		if nodeType != "method_signature" && nodeType != "declaration" {
			return
		}

		// Must be inside a body (class_body, extension_body, enum_body).
		parent := node.Parent()
		if parent == nil {
			return
		}
		parentType := parent.Type()
		if parentType != "class_body" && parentType != "extension_body" && parentType != "enum_body" {
			return
		}

		var name string
		if nodeType == "method_signature" {
			name = e.extractMethodName(node, src)
		} else {
			// Fields and static finals also ride `declaration`; only a
			// constructor signature child yields a name.
			name = dartDeclarationCtorName(node, src)
		}
		if name == "" {
			return
		}

		// Find enclosing type name.
		typeName := ""
		startLine := int(node.StartPoint().Row)
		if tn, ok := e.findEnclosingType(typeBodyRanges, startLine); ok {
			typeName = tn
		}

		// An unnamed constructor (`Foo()` inside class Foo) is matched as a
		// method whose name equals the enclosing type. Emitting a Foo.Foo
		// method creates a phantom that hijacks resolution of `Foo(...)` —
		// which constructs the class, not calls a method. Skip it; extractCalls
		// emits an EdgeInstantiates to the class node instead.
		if typeName != "" && name == typeName {
			return
		}

		methodID := filePath + "::" + typeName + "." + name
		if seen[methodID] {
			methodID = filePath + "::" + typeName + "." + name + "_L" + fmt.Sprint(startLine+1)
		}
		if seen[methodID] {
			return
		}
		seen[methodID] = true
		seen[filePath+"::_method_L"+fmt.Sprint(startLine+1)] = true

		// Dart's tree-sitter grammar splits a method into a leading
		// method_signature node (declaration line) and a following
		// function_body sibling that carries the `{ ... }` block.
		// Using method_signature.EndPoint alone makes every method
		// one line long and breaks downstream tooling that wants to
		// read the body (source viewer, shape extractor,
		// brace-balancing fallbacks, coverage). Stretch EndLine to
		// cover the adjacent function_body when present.
		endLine := int(node.EndPoint().Row)
		if body := nextDartFunctionBody(node); body != nil {
			endLine = int(body.EndPoint().Row)
		}

		methodMeta := map[string]any{
			"receiver":   typeName,
			"visibility": VisibilityByUnderscore(name),
		}
		// Declared return type — seeds the chained-factory receiver walker
		// (helpers_chaintype) so `Widget.create().build()` resolves.
		if rt := dartMethodReturnType(node, src); rt != "" {
			methodMeta["return_type"] = rt
		}
		// A getter and its setter share the bare property name, so the accessor
		// role is the only thing that tells the two nodes apart.
		if acc := dartAccessorKind(node); acc != "" {
			methodMeta["accessor"] = acc
		}
		if doc := ExtractDocAbove(src, startLine, DocLangSlashSlash); doc != "" {
			methodMeta["doc"] = doc
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: methodID, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine + 1, EndLine: endLine + 1,
			Language: "dart",
			Meta:     methodMeta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: methodID, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine + 1,
		})
		if typeName != "" {
			typeID := filePath + "::" + typeName
			result.Edges = append(result.Edges, &graph.Edge{
				From: methodID, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine + 1,
			})
		}
	})
}

// extractFields emits one member node per name bound by a field declaration in
// a class / mixin / enum / extension body. The grammar hangs a field off the
// same `declaration` wrapper it gives a body-less constructor, binding the
// names under initialized_identifier_list (instance, `var`, `final`) or
// static_final_declaration_list (`static final`, `static const`); a
// constructor-signature declaration carries neither and is left to
// extractMethods. One declaration can bind several names (`String? a, b;`), so
// each bound identifier becomes its own node.
func (e *DartExtractor) extractFields(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	typeBodyRanges := e.collectTypeBodyRanges(root, src)

	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "declaration" {
			return
		}
		parent := node.Parent()
		if parent == nil {
			return
		}
		switch parent.Type() {
		case "class_body", "extension_body", "enum_body":
		default:
			return
		}

		typeName, ok := e.findEnclosingType(typeBodyRanges, int(node.StartPoint().Row))
		if !ok {
			return
		}

		fieldType := dartLeadingTypeText(node, src)
		isStatic := e.hasChildType(node, "static")
		// A `const` binding is immutable by grammar; kind it as KindConstant so
		// value-reference impact analysis reaches its readers (mirrors the
		// top-level static_final path and the C# const-field rule).
		kind := graph.KindField
		if e.hasChildType(node, "const_builtin") {
			kind = graph.KindConstant
		}

		for _, bound := range dartFieldBindings(node) {
			name := e.findChildIdentifier(bound, src)
			if name == "" {
				continue
			}
			startLine := int(bound.StartPoint().Row) + 1
			id := filePath + "::" + typeName + "." + name
			if seen[id] {
				continue
			}
			seen[id] = true

			meta := map[string]any{
				"receiver":   typeName,
				"visibility": VisibilityByUnderscore(name),
			}
			if fieldType != "" {
				meta["field_type"] = fieldType
			}
			if isStatic {
				meta["static"] = true
			}
			if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangSlashSlash); doc != "" {
				meta["doc"] = doc
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: kind, Name: name,
				FilePath: filePath, StartLine: startLine, EndLine: int(bound.EndPoint().Row) + 1,
				Language: "dart",
				Meta:     meta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: filePath + "::" + typeName, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
			})
		}
	})
}

// dartFieldBindings returns the per-name binding nodes a field declaration
// carries — `initialized_identifier` under an initialized_identifier_list, or
// `static_final_declaration` under a static_final_declaration_list. Returns nil
// for a declaration that binds no field (a constructor signature).
func dartFieldBindings(decl *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		list := decl.Child(i)
		if list == nil {
			continue
		}
		var want string
		switch list.Type() {
		case "initialized_identifier_list":
			want = "initialized_identifier"
		case "static_final_declaration_list":
			want = "static_final_declaration"
		default:
			continue
		}
		for j, _nc := 0, int(list.ChildCount()); j < _nc; j++ {
			if bound := list.Child(j); bound != nil && bound.Type() == want {
				out = append(out, bound)
			}
		}
	}
	return out
}

// dartIsGenerated reports whether a Dart file path is a code-generator output
// (build_runner, freezed, protobuf, json_serializable, auto_route, injectable).
// Such files mirror hand-written declarations and only add duplicate,
// machine-managed symbols, so they are indexed as a bare (generated-marked)
// file node without their symbols. Delegates to the shared excludes set so the
// Dart suffixes stay in one source of truth.
func dartIsGenerated(filePath string) bool {
	return excludes.IsGenerated(filePath)
}

// dartMethodReturnType returns a Dart method's declared return type — the type
// that precedes the name in its function_signature (`Widget build()` → Widget).
// Returns "" when the method has no leading return type (e.g. a void-inferred
// `build()` or a getter/setter).
func dartMethodReturnType(methodSig *sitter.Node, src []byte) string {
	for i, _nc := 0, int(methodSig.NamedChildCount()); i < _nc; i++ {
		fs := methodSig.NamedChild(i)
		if fs.Type() != "function_signature" {
			continue
		}
		for j, _nc := 0, int(fs.NamedChildCount()); j < _nc; j++ {
			c := fs.NamedChild(j)
			switch c.Type() {
			case "type_identifier", "type", "nullable_type":
				return strings.TrimSpace(c.Content(src))
			case "identifier", "function_type":
				return "" // reached the method name before any return type
			}
		}
	}
	return ""
}

// nextDartFunctionBody returns the function_body sibling that follows
// a method_signature / function_signature node, walking through
// immediate siblings (the Dart grammar occasionally interposes
// whitespace / comment nodes between them). Returns nil when the
// method is abstract or declared with `=>` without a brace block.
func nextDartFunctionBody(sig *sitter.Node) *sitter.Node {
	for s := sig.NextSibling(); s != nil; s = s.NextSibling() {
		if s.Type() == "function_body" {
			return s
		}
		// Stop if we've walked past the declaration grouping — the
		// next signature wipes any chance of finding "this" one's
		// body.
		if s.Type() == "method_signature" || s.Type() == "function_signature" {
			return nil
		}
	}
	return nil
}

// extractTopLevelFunctions finds function_signature nodes that are direct children of program
// (followed by function_body).
func (e *DartExtractor) extractTopLevelFunctions(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i, _nc := 0, int(root.ChildCount()); i < _nc; i++ {
		child := root.Child(i)
		if child.Type() != "function_signature" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(src)
		startLine := int(child.StartPoint().Row) + 1

		// Check if next sibling is function_body to get end line.
		endLine := int(child.EndPoint().Row) + 1
		if i+1 < int(root.ChildCount()) {
			next := root.Child(i + 1)
			if next.Type() == "function_body" {
				endLine = int(next.EndPoint().Row) + 1
			}
		}

		id := filePath + "::" + name
		if seen[id] {
			id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
		}
		if seen[id] {
			continue
		}
		seen[id] = true

		fnMeta := map[string]any{"visibility": VisibilityByUnderscore(name)}
		if doc := ExtractDocAbove(src, startLine-1, DocLangSlashSlash); doc != "" {
			fnMeta["doc"] = doc
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "dart",
			Meta:     fnMeta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
		})
	}
}

// extractTopLevelVariables finds top-level initialized_variable_definition and
// static_final_declaration_list nodes at program level.
func (e *DartExtractor) extractTopLevelVariables(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i, _nc := 0, int(root.ChildCount()); i < _nc; i++ {
		child := root.Child(i)
		switch child.Type() {
		case "initialized_variable_definition":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			name := nameNode.Content(src)
			startLine := int(child.StartPoint().Row) + 1
			id := filePath + "::" + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath: filePath, StartLine: startLine, EndLine: int(child.EndPoint().Row) + 1,
				Language: "dart",
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
			})

		case "static_final_declaration_list":
			// Walk children for static_final_declaration nodes.
			for j, _nc := 0, int(child.ChildCount()); j < _nc; j++ {
				decl := child.Child(j)
				if decl.Type() != "static_final_declaration" {
					continue
				}
				name := e.findChildIdentifier(decl, src)
				if name == "" {
					continue
				}
				startLine := int(decl.StartPoint().Row) + 1
				id := filePath + "::" + name
				if seen[id] {
					continue
				}
				seen[id] = true
				// A static_final_declaration is a `const` / `final` / `static
				// final` binding — immutable by grammar. A distinctive-named one
				// is a Dart value constant; kind it as KindConstant so
				// value-reference impact analysis reaches its readers (mirrors
				// the Scala/Ruby constant handling).
				declKind := graph.KindVariable
				if isDistinctiveConstName(name) {
					declKind = graph.KindConstant
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: declKind, Name: name,
					FilePath: filePath, StartLine: startLine, EndLine: int(decl.EndPoint().Row) + 1,
					Language: "dart",
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
				})
			}
		}
	}
}

// extractImports walks `import_or_export` nodes, emits the import
// edge, and returns the per-file alias map captured from `as
// <alias>` clauses. The map is consumed by `extractCalls` so calls
// like `f.method(args)` (where `f` was bound by `import '…' as f;`)
// attribute to the originating URI rather than landing as the
// unresolved-method fallback.
func (e *DartExtractor) extractImports(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) map[string]string {
	imports := map[string]string{}
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "import_or_export" {
			return
		}

		text := node.Content(src)
		uri := extractDartImportURI(text)
		if uri == "" {
			return
		}

		startLine := int(node.StartPoint().Row) + 1
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + uri,
			Kind: graph.EdgeImports, FilePath: filePath, Line: startLine,
		})

		if alias := extractDartImportAlias(text); alias != "" {
			// Re-binding the same alias to two different URIs in
			// one file would be a Dart compile error; keep
			// last-write-wins so a freshly-edited file with a
			// half-typed import doesn't poison earlier state.
			imports[alias] = uri
		}
	})
	return imports
}

// extractCalls finds function/method call sites and emits EdgeCalls
// edges to the resolver-stub target.
//
// Dart's tree-sitter grammar exposes both a flat `bareCall()` and a
// chained `fl.runApp()` as siblings under the surrounding scope:
//
//	bareCall():
//	  identifier "bareCall"
//	  selector
//	    argument_part(...)
//
//	fl.runApp():
//	  identifier "fl"
//	  selector
//	    unconditional_assignable_selector
//	      "."
//	      identifier "runApp"
//	  selector
//	    argument_part(...)
//
// We anchor on the leading identifier (the one that is NOT itself a
// child of a selector / unconditional_assignable_selector) and scan
// forward through `selector` siblings, collecting `.method`
// segments until a selector with `argument_part` confirms the
// call. The trailing-most identifier is the call name; the leading
// identifier is the receiver. When the receiver matches an import
// alias the edge attributes to `unresolved::extern::<uri>::<name>`
// so the resolver-side module-attribution pass can land it on a
// KindModule; otherwise we fall back to the legacy name-only stub.
func (e *DartExtractor) extractCalls(
	root *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult,
	imports map[string]string,
) {
	funcRanges := buildFuncRanges(result)

	// Local type names — a bare call to one is a construction, not a function
	// call. Populated from the types extractTypes already emitted (it runs
	// before extractCalls).
	localTypes := map[string]bool{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface) {
			localTypes[n.Name] = true
		}
	}

	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "identifier" {
			return
		}
		// Skip identifiers that live inside a selector chain; the
		// chain is reconstructed once from the leading identifier
		// below. Without this guard we would emit duplicate
		// edges (one per identifier in the chain).
		if p := node.Parent(); p != nil {
			switch p.Type() {
			case "unconditional_assignable_selector",
				"conditional_assignable_selector",
				"selector":
				return
			}
		}

		methodChain := []string{node.Content(src)}
		isCall := false
		for sib := node.NextSibling(); sib != nil && !isCall; sib = sib.NextSibling() {
			if sib.Type() != "selector" {
				break
			}
			for j, _nc := 0, int(sib.ChildCount()); j < _nc; j++ {
				child := sib.Child(j)
				switch child.Type() {
				case "argument_part", "arguments":
					isCall = true
				case "unconditional_assignable_selector",
					"conditional_assignable_selector":
					if id := firstIdentifierChild(child); id != nil {
						methodChain = append(methodChain, id.Content(src))
					}
				}
			}
		}
		if !isCall {
			return
		}

		line := int(node.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			return
		}

		callName := methodChain[len(methodChain)-1]
		leadingReceiver := ""
		if len(methodChain) > 1 {
			leadingReceiver = methodChain[0]
		}

		// Instantiation: a bare call to a known local type name (`Widget()`,
		// also `new`/`const` Widget()) constructs the class. Emit a typed
		// EdgeInstantiates to the class node rather than a flat unresolved call
		// — impact/trace then see the construction and the resolver binds it to
		// the real type. (codegraph emits only a flat call edge here.)
		if len(methodChain) == 1 && localTypes[callName] {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: filePath + "::" + callName,
				Kind: graph.EdgeInstantiates, FilePath: filePath, Line: line,
				Origin: graph.OriginASTResolved,
			})
			return
		}

		target := "unresolved::*." + callName
		aliased := false
		if leadingReceiver != "" {
			if uri, ok := imports[leadingReceiver]; ok {
				target = "unresolved::extern::" + uri + "::" + callName
				aliased = true
			}
		}
		edge := &graph.Edge{
			From: callerID, To: target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		}
		// `TiledAtlas.fromTiledMap(1)` — a Capitalized chain head names the type
		// the callee lives on, so the resolver binds the call to that type's
		// member instead of any same-named symbol. Only a two-segment chain
		// qualifies: in `Foo.bar.baz()` the head types `bar`, not `baz`. An
		// import alias is excluded — it is a library prefix, not a type, and its
		// attribution already rides the extern target.
		if !aliased && len(methodChain) == 2 && isDartTypeNameCapitalized(leadingReceiver) {
			edge.Meta = map[string]any{"receiver_type": leadingReceiver}
		}
		result.Edges = append(result.Edges, edge)
	})
}

// firstIdentifierChild returns the first direct child of node whose
// type is "identifier", or nil. Used to pull the method name out of
// a Dart selector wrapper.
func firstIdentifierChild(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		c := node.Child(i)
		if c != nil && c.Type() == "identifier" {
			return c
		}
	}
	return nil
}

// --- helpers ---

func (e *DartExtractor) hasChildType(node *sitter.Node, typeName string) bool {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		if node.Child(i).Type() == typeName {
			return true
		}
	}
	return false
}

func (e *DartExtractor) findChildIdentifier(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// extractMethodName extracts the name from a method_signature node.
// method_signature wraps function_signature, getter_signature, setter_signature,
// constructor_signature, operator_signature, factory_constructor_signature.
func (e *DartExtractor) extractMethodName(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		switch child.Type() {
		case "function_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "getter_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "setter_signature":
			// The bare property name, not the `set foo` source spelling: a
			// search or a `.foo = x` write site names the property. The
			// getter/setter pair collides on `Type.foo`; the caller's
			// line-suffix disambiguation separates the second one.
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "constructor_signature", "constant_constructor_signature",
			"factory_constructor_signature", "redirecting_factory_constructor_signature":
			return dartCtorName(child, src)
		case "operator_signature":
			// operator + binary_operator child
			for j, _nc := 0, int(child.ChildCount()); j < _nc; j++ {
				c := child.Child(j)
				if c.Type() == "binary_operator" || c.Type() == "tilde_operator" {
					return "operator " + strings.TrimSpace(c.Content(src))
				}
			}
		}
	}
	return ""
}

// dartAccessorKind returns "getter" / "setter" when a method_signature wraps a
// getter_signature / setter_signature, and "" for every other member shape.
func dartAccessorKind(methodSig *sitter.Node) string {
	for i, _nc := 0, int(methodSig.ChildCount()); i < _nc; i++ {
		child := methodSig.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "getter_signature":
			return "getter"
		case "setter_signature":
			return "setter"
		}
	}
	return ""
}

// dartDeclarationCtorName returns the constructor name carried by a class-body
// `declaration` wrapper — the shape the grammar gives a body-less constructor
// (named, const, redirecting factory). Returns "" for every other declaration
// (fields, static finals).
func dartDeclarationCtorName(decl *sitter.Node, src []byte) string {
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		child := decl.Child(i)
		switch child.Type() {
		case "constructor_signature", "constant_constructor_signature",
			"factory_constructor_signature", "redirecting_factory_constructor_signature":
			return dartCtorName(child, src)
		}
	}
	return ""
}

// dartCtorName returns a constructor signature's declared name. Dart spells a
// named constructor `Type.name(...)`, and the grammar's `name` field points at
// the leading TYPE identifier, so the constructor's own name is the identifier
// that follows the dot. An unnamed constructor has no dot — the type identifier
// is returned so the caller's unnamed-constructor guard recognises and drops it.
func dartCtorName(sig *sitter.Node, src []byte) string {
	typeName := ""
	afterDot := false
	for i, _nc := 0, int(sig.ChildCount()); i < _nc; i++ {
		child := sig.Child(i)
		switch child.Type() {
		case ".":
			afterDot = true
		case "identifier":
			if afterDot {
				return child.Content(src)
			}
			if typeName == "" {
				typeName = child.Content(src)
			}
		case "formal_parameter_list":
			// The parameter list closes the name; a dot inside it
			// (`Foo(this.n)`) is not a name separator.
			return typeName
		}
	}
	return typeName
}

type dartTypeRange struct {
	typeName  string
	startLine int // 0-based
	endLine   int // 0-based
}

func (e *DartExtractor) collectTypeBodyRanges(root *sitter.Node, src []byte) []dartTypeRange {
	var ranges []dartTypeRange
	walkNodes(root, func(node *sitter.Node) {
		var name string
		switch node.Type() {
		case "class_definition":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		case "enum_declaration":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		case "mixin_declaration":
			name = e.findChildIdentifier(node, src)
		case "extension_declaration":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		default:
			return
		}
		if name == "" {
			return
		}
		ranges = append(ranges, dartTypeRange{
			typeName:  name,
			startLine: int(node.StartPoint().Row),
			endLine:   int(node.EndPoint().Row),
		})
	})
	return ranges
}

func (e *DartExtractor) findEnclosingType(ranges []dartTypeRange, line int) (string, bool) {
	best := ""
	bestSize := int(^uint(0) >> 1)
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			size := r.endLine - r.startLine
			if size < bestSize {
				bestSize = size
				best = r.typeName
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// extractDartImportAlias pulls the prefix bound by ` as <ident>`
// from an `import_or_export` statement's text. Returns empty when
// the import has no alias clause. The Dart grammar guarantees
// `as <ident>` follows the URI; we scan the suffix after the
// closing quote so we don't confuse `as` inside the URI itself
// (e.g. `package:as_a_string/...`).
func extractDartImportAlias(text string) string {
	closing := -1
	for _, q := range []byte{'\'', '"'} {
		start := strings.IndexByte(text, q)
		if start < 0 {
			continue
		}
		end := strings.IndexByte(text[start+1:], q)
		if end < 0 {
			continue
		}
		closing = start + 1 + end
		break
	}
	if closing < 0 || closing+1 >= len(text) {
		return ""
	}
	rest := text[closing+1:]
	idx := strings.Index(rest, " as ")
	if idx < 0 {
		return ""
	}
	tail := strings.TrimLeft(rest[idx+len(" as "):], " \t")
	end := 0
	for end < len(tail) {
		c := tail[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
			end++
			continue
		}
		break
	}
	return tail[:end]
}

// extractDartImportURI extracts the URI string from an import/export statement text.
// e.g. "import 'package:flutter/material.dart';" → "package:flutter/material.dart"
func extractDartImportURI(text string) string {
	// Find content between quotes.
	for _, q := range []byte{'\'', '"'} {
		start := strings.IndexByte(text, q)
		if start < 0 {
			continue
		}
		end := strings.IndexByte(text[start+1:], q)
		if end < 0 {
			continue
		}
		return text[start+1 : start+1+end]
	}
	return ""
}

// mineDartFactoryChains emits a member call per chained `.method(...)` whose
// receiver is itself a call -- a factory chain like builder().withX().build().
// extractCalls anchors on the leading identifier and stops at the first call,
// so these inner segments are otherwise dropped. Gated on a call-bearing
// receiver (so plain recv.method() is left to extractCalls, not double-counted)
// and carries the receiver text through the shared chained-receiver walker.
func (e *DartExtractor) mineDartFactoryChains(root *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	funcRanges := buildFuncRanges(result)
	if len(funcRanges) == 0 {
		return
	}
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "identifier" {
			return
		}
		if p := node.Parent(); p != nil {
			switch p.Type() {
			case "unconditional_assignable_selector", "conditional_assignable_selector", "selector":
				return
			}
		}
		var pending *sitter.Node
		for sib := node.NextSibling(); sib != nil; sib = sib.NextSibling() {
			if sib.Type() != "selector" {
				break
			}
			var assignable *sitter.Node
			hasArgs := false
			for j, _nc := 0, int(sib.ChildCount()); j < _nc; j++ {
				switch sib.Child(j).Type() {
				case "argument_part", "arguments":
					hasArgs = true
				case "unconditional_assignable_selector", "conditional_assignable_selector":
					assignable = sib.Child(j)
				}
			}
			if assignable != nil {
				pending = assignable
				continue
			}
			if hasArgs && pending != nil {
				if id := firstIdentifierChild(pending); id != nil {
					receiver := strings.TrimSpace(string(src[node.StartByte():pending.StartByte()]))
					if strings.Contains(receiver, "(") {
						line := int(pending.StartPoint().Row) + 1
						if callerID := findEnclosingFunc(funcRanges, line); callerID != "" {
							edge := &graph.Edge{
								From: callerID, To: "unresolved::*." + id.Content(src),
								Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
							}
							stampFactoryChainReceiver(edge, receiver, resolveChainType(receiver, nil, result))
							result.Edges = append(result.Edges, edge)
						}
					}
				}
				pending = nil
			}
		}
	})
}
