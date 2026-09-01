package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/swift"
)

// qSwiftAll is a single tree-sitter query alternating over every
// pattern the Swift extractor needs. One tree walk per file replaces
// the 9 `parser.RunQuery` calls (plus the duplicated triple-query pass
// the legacy collectTypeBodyRanges performed). Capture names are
// disjoint across patterns so the dispatch in Extract can branch on
// which name is set. Method-vs-function classification is performed
// inline by tracking class/struct/enum line ranges as their match
// arrives — types come before their members in document order, so the
// range table is always complete by the time a function_declaration is
// dispatched.
const qSwiftAll = `
[
  (class_declaration
    name: (type_identifier) @class.name) @class.def

  (class_declaration
    name: (type_identifier) @enum.name
    body: (enum_class_body) @enum.body) @enum.def

  (protocol_declaration
    name: (type_identifier) @proto.name) @proto.def

  (protocol_function_declaration
    name: (simple_identifier) @protomethod.name) @protomethod.def

  (function_declaration
    name: (simple_identifier) @func.name) @func.def

  (property_declaration
    (pattern (simple_identifier) @property.name)) @property.def

  ; init / deinit / subscript are their own grammar rules, not
  ; function_declaration, so none of the patterns above reached them. Left
  ; unmatched they are absent as symbols AND absent from the func-range table,
  ; which silently drops every call edge made from inside their bodies.
  (init_declaration) @init.def

  (deinit_declaration) @deinit.def

  (subscript_declaration) @subscript.def

  (import_declaration) @import.def

  (call_expression
    (simple_identifier) @call.name) @call.expr

  (call_expression
    (navigation_expression
      (navigation_suffix (simple_identifier) @callm.name)) @callm.nav) @callm.expr

  ; A call spelling explicit type arguments is not a call_expression at
  ; all: lowerGeneric<Int>(), obj.doThing<Int>() and MyType<Int>(x:) all
  ; parse as a constructor_expression whose constructed type carries the
  ; whole dotted path. Nothing in this query matched one, so adding type
  ; arguments to any call — free function, member, or initializer —
  ; silently deleted its edge.
  (constructor_expression
    (user_type) @ctor.type) @ctor.expr
]
`

// SwiftExtractor extracts Swift source files into graph nodes and edges.
type SwiftExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewSwiftExtractor() *SwiftExtractor {
	lang := swift.GetLanguage()
	return &SwiftExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qSwiftAll, lang),
	}
}

func (e *SwiftExtractor) Language() string     { return "swift" }
func (e *SwiftExtractor) Extensions() []string { return []string{".swift"} }

// --- Deferred match buffers ----------------------------------------

type swiftDeferredCall struct {
	name     string
	line     int
	isMember bool
	receiver string
}

type swiftTypeRange struct {
	name        string
	startLine   int  // 0-based
	endLine     int  // 0-based
	objcMembers bool // class declared @objcMembers -- exposes all members to ObjC
}

func (e *SwiftExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "swift",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	protoMethods := make(map[string][]string) // protocol name → declared method names
	var typeRanges []swiftTypeRange
	// Extensions aren't captured by qSwiftAll (their name is a user_type, not a
	// type_identifier), so seed their ranges first; members inside an
	// `extension Foo { ... }` then attribute to Foo like any other type member.
	typeRanges = append(typeRanges, swiftExtensionRanges(src)...)
	// Resilience net for parse errors: a tree-sitter error inside a type body
	// (e.g. an unparseable `#if … && !canImport(...)`) can corrupt the enclosing
	// class_declaration so the query never matches it — its members and every
	// reference to the type would then strand on unresolved::Name. Seed the
	// container ranges from a brace-matched text scan so members still attribute;
	// gaps the query misses get a fallback container node after the match loop.
	fallbackTypes := swiftFallbackTypeDecls(src)
	for _, ft := range fallbackTypes {
		typeRanges = append(typeRanges, swiftTypeRange{name: ft.name, startLine: ft.startLine, endLine: ft.endLine})
	}
	var calls []swiftDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitTypeContainer(m, "class", filePath, fileID, src, result, seen, annotationSeen, &typeRanges, nil)

		case m.Captures["enum.def"] != nil:
			// May fire on the same class_declaration as the prior
			// class.def pattern; emitTypeContainer handles the seen
			// dedupe and stamps Meta["kind"]="enum" on the existing
			// node when it does. Walks the captured enum_class_body
			// for case entries.
			body := m.Captures["enum.body"]
			var bodyNode *sitter.Node
			if body != nil {
				bodyNode = body.Node
			}
			e.emitTypeContainer(m, "enum", filePath, fileID, src, result, seen, annotationSeen, &typeRanges, bodyNode)

		case m.Captures["proto.def"] != nil:
			e.emitProtocol(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["protomethod.def"] != nil:
			e.recordProtocolMethod(m, src, protoMethods)

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["property.def"] != nil:
			e.emitProperty(m, filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["init.def"] != nil:
			e.emitTypeMemberDecl(m.Captures["init.def"], "init", filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["deinit.def"] != nil:
			e.emitTypeMemberDecl(m.Captures["deinit.def"], "deinit", filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["subscript.def"] != nil:
			e.emitTypeMemberDecl(m.Captures["subscript.def"], "subscript", filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, swiftDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			recv := ""
			if nav := m.Captures["callm.nav"]; nav != nil && nav.Node != nil && nav.Node.NamedChildCount() > 0 {
				recv = strings.TrimSpace(nav.Node.NamedChild(0).Content(src))
			}
			calls = append(calls, swiftDeferredCall{
				name:     m.Captures["callm.name"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
				receiver: recv,
			})

		case m.Captures["ctor.expr"] != nil:
			expr := m.Captures["ctor.expr"]
			path := swiftUserTypePath(m.Captures["ctor.type"].Node, src)
			if len(path) == 0 {
				break
			}
			call := swiftDeferredCall{
				name: path[len(path)-1],
				line: expr.StartLine + 1,
			}
			// A dotted constructed type is a member call whose receiver
			// the grammar folded into the type path; a bare one is a free
			// function or an initializer, which the non-generic spelling
			// already records the same way.
			if len(path) > 1 {
				call.isMember = true
				call.receiver = strings.Join(path[:len(path)-1], ".")
			}
			calls = append(calls, call)
		}
	})

	// Emit fallback container nodes for class/struct/actor/enum declarations the
	// query missed (parse-error regions). `seen` already holds every container
	// the query emitted, so this only fills gaps and never duplicates.
	for _, ft := range fallbackTypes {
		id := filePath + "::" + ft.name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: ft.name,
			FilePath: filePath, StartLine: ft.startLine + 1, EndLine: ft.endLine + 1,
			Language: "swift",
			Meta:     map[string]any{"visibility": ft.visibility},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: ft.startLine + 1,
		})
	}

	// Stamp protocol method names onto protocol nodes' Meta["methods"].
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := protoMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Resolve calls against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		to := "unresolved::" + c.name
		if c.isMember {
			to = "unresolved::*." + c.name
		}
		edge := &graph.Edge{
			From: callerID, To: to,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if c.isMember && c.receiver != "" {
			stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, nil, result))
		}
		result.Edges = append(result.Edges, edge)
	}

	// React Native native event emits pair with the JS addListener handler.
	mineRNNativeEmits(src, rnSendEventWrapperRe, func(line int) string {
		return findEnclosingFunc(funcRanges, line)
	}, filePath, "swift", result)

	// Closure-collection dispatch: stamp dispatcher/registrar field markers.
	mineSwiftClosureCollections(src, funcRanges, result)

	// Structural reference forms a type-annotation extractor misses:
	// instantiation (`Foo()` / `Foo.init`), inheritance / conformance
	// (`class X: Base, Proto`), casts / type tests (`x as Foo`, `x is Foo`),
	// and static / member access (`Foo.shared`). find_usages then lands these
	// LSP-free. Type-annotation edges are already emitted as EdgeTypedAs above.
	emitSwiftReferenceForms(root, src, filePath, fileID, funcRanges, typeRanges, result)

	// Expo Modules native DSL (Name/Function/AsyncFunction) → synthetic
	// JS-callable method nodes for the Expo bridge synthesizer.
	emitExpoModuleNodes(src, filePath, "swift", fileID, result, seen)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	captureAppleUIRoles(result, root, filePath, src)
	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitTypeContainer emits a class / struct / enum node and records its
// line range so subsequent function_declaration dispatches can classify
// methods by enclosing type. The capture-name prefix selects which
// name/def pair to read. For the "enum" prefix, when the same id is
// already seen (i.e. swQClass already emitted it), stamps
// Meta["kind"]="enum" on the existing node and walks bodyNode for
// case entries instead of emitting a duplicate.
func (e *SwiftExtractor) emitTypeContainer(m parser.QueryResult, prefix, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, typeRanges *[]swiftTypeRange, bodyNode *sitter.Node) {
	var nameKey, defKey string
	switch prefix {
	case "enum":
		nameKey, defKey = "enum.name", "enum.def"
	default:
		nameKey, defKey = "class.name", "class.def"
	}
	name := m.Captures[nameKey].Text
	def := m.Captures[defKey]
	id := filePath + "::" + name

	// Always extend the type-range table — this is what method
	// classification consults. Adding the same id twice (once for
	// class.def, once for enum.def on the same enum) is harmless: the
	// findEnclosingType lookup picks the innermost match by size.
	*typeRanges = append(*typeRanges, swiftTypeRange{
		name:        name,
		startLine:   def.StartLine,
		endLine:     def.EndLine,
		objcMembers: swiftHasAttr(def.Node, "objcMembers", src),
	})

	if !seen[id] {
		seen[id] = true
		meta := map[string]any{"visibility": swiftVisibility(def.Node, src)}
		if prefix == "enum" {
			meta["kind"] = "enum"
		}
		// Structural flavor: the class.* capture covers class/struct/actor
		// alike, so the `prefix` param alone can't tell them apart — read the
		// leading declaration keyword off the decl node. enum is determined
		// by the prefix (its own capture).
		flavor := "class"
		if kw := swiftDeclKeyword(def.Node); kw != "" {
			flavor = kw
		} else if prefix == "enum" {
			flavor = "enum"
		}
		meta["type_flavor"] = flavor
		if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
			meta["doc"] = doc
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
	} else if prefix == "enum" {
		// Backfill enum kind on the existing node.
		for _, n := range result.Nodes {
			if n.ID == id {
				if n.Meta == nil {
					n.Meta = make(map[string]any)
				}
				n.Meta["kind"] = "enum"
				n.Meta["type_flavor"] = "enum"
				break
			}
		}
	}

	// Enum cases — cases with associated values contain nested
	// simple_identifier labels (`case labeled(x: Int)` has `x` as a
	// simple_identifier), so we take *only the first* simple_identifier
	// child of each enum_entry as the case name.
	if prefix != "enum" || bodyNode == nil {
		return
	}
	for i, _nc := 0, int(bodyNode.ChildCount()); i < _nc; i++ {
		entry := bodyNode.Child(i)
		if entry == nil || entry.Type() != "enum_entry" {
			continue
		}
		var caseName string
		for j, _nc := 0, int(entry.ChildCount()); j < _nc; j++ {
			ch := entry.Child(j)
			if ch != nil && ch.Type() == "simple_identifier" {
				caseName = ch.Content(src)
				break
			}
		}
		if caseName == "" {
			continue
		}
		caseID := id + "." + caseName
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: caseID, Kind: graph.KindVariable, Name: caseName,
			FilePath:  filePath,
			StartLine: int(entry.StartPoint().Row) + 1,
			EndLine:   int(entry.EndPoint().Row) + 1,
			Language:  "swift",
			Meta:      map[string]any{"receiver": name, "kind": "enum_case"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: caseID, To: id, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: int(entry.StartPoint().Row) + 1,
		})
	}
}

// swiftDeclKeyword reads the leading declaration keyword token off a
// class.* capture node. tree-sitter-swift folds class/struct/actor/enum
// into one class_declaration rule, so the keyword token is what tells
// them apart. Returns "" when no recognised keyword is a direct child.
func swiftDeclKeyword(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	for i, n := 0, int(node.ChildCount()); i < n; i++ {
		ch := node.Child(i)
		if ch == nil {
			continue
		}
		switch ch.Type() {
		case "class", "struct", "actor", "enum":
			return ch.Type()
		}
	}
	return ""
}

// recordProtocolMethod walks up to the enclosing protocol_declaration
// and appends the method name to its Meta["methods"] entry. Mirrors
// legacy swQProtocolMethod nested capture.
func (e *SwiftExtractor) recordProtocolMethod(m parser.QueryResult, src []byte, protoMethods map[string][]string) {
	def := m.Captures["protomethod.def"]
	if def.Node == nil {
		return
	}
	protoNode := findEnclosingSwiftContainer(def.Node, "protocol_declaration")
	if protoNode == nil {
		return
	}
	nameNode := protoNode.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	protoMethods[nameNode.Content(src)] = append(protoMethods[nameNode.Content(src)], m.Captures["protomethod.name"].Text)
}

func (e *SwiftExtractor) emitProtocol(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["proto.name"].Text
	def := m.Captures["proto.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": swiftVisibility(def.Node, src), "type_flavor": "protocol"}
	if swiftHasAttr(def.Node, "objc", src) {
		meta["objc"] = true
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
}

func (e *SwiftExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, typeRanges []swiftTypeRange) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine := def.StartLine

	doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash)
	visibility := swiftVisibility(def.Node, src)
	sig, returnType, isAsync, isStatic := swiftFunctionDetails(def.Node, src)
	if sig == "" {
		sig = "func " + name + "(...)"
	}

	if tr, ok := findEnclosingSwiftTypeRange(typeRanges, startLine); ok {
		typeName := tr.name
		id, idOK := disambiguateID(seen, filePath+"::"+typeName+"."+name, def.StartLine+1)
		if !idOK {
			return
		}
		meta := map[string]any{
			"receiver":   typeName,
			"signature":  sig,
			"visibility": visibility,
		}
		swiftStampFuncMeta(meta, returnType, isAsync, isStatic)
		if doc != "" {
			meta["doc"] = doc
		}
		if sel := swiftObjCSelectorExposed(def.Node, name, tr.objcMembers, src); sel != "" {
			meta["objc_selector"] = sel
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		typeID := filePath + "::" + typeName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
		emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
		emitSwiftFunctionTypeEdges(id, def.Node, src, filePath, def.StartLine+1, result)
		return
	}

	id, idOK := disambiguateID(seen, filePath+"::"+name, def.StartLine+1)
	if !idOK {
		return
	}
	meta := map[string]any{
		"signature":  sig,
		"visibility": visibility,
	}
	swiftStampFuncMeta(meta, returnType, isAsync, isStatic)
	if doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
	emitSwiftFunctionTypeEdges(id, def.Node, src, filePath, def.StartLine+1, result)
}

// emitProperty extracts a stored property declaration. Inside a type it is a
// field member; at file scope it is a constant (`let`) or variable (`var`).
// When the property carries @objc the Objective-C accessor selectors it is
// exposed under are stamped (`objc_selector` getter, `objc_setter_selector` for
// a mutable var) so the Swift↔ObjC bridge can pair it with native accessors.
func (e *SwiftExtractor) emitProperty(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, typeRanges []swiftTypeRange) {
	nameCap := m.Captures["property.name"]
	def := m.Captures["property.def"]
	if nameCap == nil || def == nil || nameCap.Text == "" {
		return
	}
	name := nameCap.Text
	mutable := swiftPropertyIsMutable(def.Node, src)
	fieldType := swiftPropertyType(def.Node, src)

	tr, enclosed := findEnclosingSwiftTypeRange(typeRanges, def.StartLine)
	typeName := tr.name
	kind := graph.KindField
	id := filePath + "::" + name
	if enclosed {
		id = filePath + "::" + typeName + "." + name
	} else if mutable {
		kind = graph.KindVariable
	} else {
		kind = graph.KindConstant
	}
	if seen[id] {
		return
	}
	seen[id] = true

	meta := map[string]any{"visibility": swiftVisibility(def.Node, src)}
	if mutable {
		meta["mutable"] = true
	}
	if fieldType != "" {
		meta["field_type"] = fieldType
	}
	if enclosed {
		meta["receiver"] = typeName
	}
	if getter, setter := swiftObjCPropertySelectorsExposed(def.Node, name, mutable, enclosed && tr.objcMembers, src); getter != "" {
		meta["objc_selector"] = getter
		if setter != "" {
			meta["objc_setter_selector"] = setter
		}
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if enclosed {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + typeName, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
	if fieldType != "" {
		// Emit the variable / property / local annotation as a type-use
		// edge. Routing through emitSwiftTypeUseEdges skips Swift
		// primitives (no `unresolved::Int` noise), stamps
		// OriginASTInferred, and decomposes composite annotations
		// (`[Foo]`, `Foo?`, `[K: V]`, `Bar<Baz>`) into per-leaf edges so
		// a type used only in annotation position is reachable by
		// find_usages without a language server.
		emitSwiftTypeUseEdges(id, fieldType, filePath, def.StartLine+1, result)
	}
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
}

// emitTypeMemberDecl emits a nameless type member — `init`, `deinit`, or
// `subscript`, named only by its declaring keyword — as a method of its
// enclosing type (a type declaration or an `extension Foo { … }` block, both of
// which seed typeRanges). A declaration with no enclosing type range is a
// protocol requirement or a parse-error fragment and is dropped rather than
// attributed to the file.
//
// An initializer takes the cross-language constructor spelling `<Type>.<init>`
// that Java and C# already emit, so constructor-aware consumers need no Swift
// special case. `deinit` and `subscript` are spelled as written, and two
// subscripts on one type separate by the shared line-suffix disambiguation.
func (e *SwiftExtractor) emitTypeMemberDecl(
	def *parser.CapturedNode, keyword, filePath, fileID string, src []byte,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
	typeRanges []swiftTypeRange,
) {
	if def == nil || def.Node == nil {
		return
	}
	tr, ok := findEnclosingSwiftTypeRange(typeRanges, def.StartLine)
	if !ok {
		return
	}
	name := keyword
	member := tr.name + "." + keyword
	if keyword == "init" {
		name = tr.name + ".<init>"
		member = name
	}
	id, idOK := disambiguateID(seen, filePath+"::"+member, def.StartLine+1)
	if !idOK {
		return
	}

	meta := map[string]any{
		"receiver":   tr.name,
		"kind":       keyword,
		"signature":  swiftDeclHeader(def.Node, src),
		"visibility": swiftVisibility(def.Node, src),
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: filePath + "::" + tr.name, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
}

// swiftDeclHeader returns a declaration's source up to its body brace, folded
// onto one line — the readable signature for a member the grammar gives no
// name field (`init`, `deinit`, `subscript`).
func swiftDeclHeader(decl *sitter.Node, src []byte) string {
	header := decl.Content(src)
	if i := strings.IndexByte(header, '{'); i >= 0 {
		header = header[:i]
	}
	return strings.Join(strings.Fields(header), " ")
}

// swiftPropertyIsMutable reports whether a property is declared with `var`
// (mutable) rather than `let`, reading the value_binding_pattern child.
func swiftPropertyIsMutable(decl *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		if c := decl.NamedChild(i); c != nil && c.Type() == "value_binding_pattern" {
			return strings.Contains(c.Content(src), "var")
		}
	}
	return strings.Contains(decl.Content(src), "var ")
}

// swiftPropertyType returns the base type named in a property's
// type_annotation (`: [Foo]?` → "Foo"), or "" when the type is inferred.
func swiftPropertyType(decl *sitter.Node, src []byte) string {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "type_annotation" {
			continue
		}
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Content(src)), ":"))
		return swiftBaseTypeName(t)
	}
	return ""
}

// swiftBaseTypeName reduces a Swift type expression to its leaf type name,
// stripping opaque/existential markers, optionals, array/dictionary sugar,
// generic arguments and module qualification.
func swiftBaseTypeName(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "some ")
	t = strings.TrimPrefix(t, "any ")
	t = strings.TrimRight(t, "?!")
	t = strings.Trim(t, "[]")
	if idx := strings.IndexByte(t, '<'); idx >= 0 {
		t = t[:idx]
	}
	if idx := strings.IndexByte(t, ':'); idx >= 0 { // dictionary value type
		t = strings.TrimSpace(t[idx+1:])
	}
	if idx := strings.LastIndexByte(t, '.'); idx >= 0 {
		t = t[idx+1:]
	}
	return strings.TrimSpace(t)
}

// swiftFunctionDetails parses a function declaration's header for its real
// signature, return type, and async/static modifier flags. The body is dropped
// at the first brace; modifiers precede `func` and the return type follows `->`.
func swiftFunctionDetails(decl *sitter.Node, src []byte) (signature, returnType string, isAsync, isStatic bool) {
	if decl == nil {
		return "", "", false, false
	}
	header := decl.Content(src)
	if i := strings.IndexByte(header, '{'); i >= 0 {
		header = header[:i]
	}
	header = strings.Join(strings.Fields(header), " ")
	isAsync = swiftHasWord(header, "async")
	fi := strings.Index(header, "func ")
	if fi < 0 {
		return strings.TrimSpace(header), "", isAsync, false
	}
	isStatic = swiftHasWord(header[:fi], "static") || swiftHasWord(header[:fi], "class")
	signature = strings.TrimSpace(header[fi:])
	if ri := strings.Index(signature, "->"); ri >= 0 {
		rt := strings.TrimSpace(signature[ri+2:])
		if wi := strings.Index(rt, " where "); wi >= 0 {
			rt = strings.TrimSpace(rt[:wi])
		}
		rt = strings.TrimPrefix(rt, "some ")
		rt = strings.TrimPrefix(rt, "any ")
		returnType = strings.TrimSpace(rt)
	}
	return signature, returnType, isAsync, isStatic
}

// swiftHasWord reports whether word appears as a standalone space-delimited
// token in s.
func swiftHasWord(s, word string) bool {
	for _, f := range strings.Fields(s) {
		if f == word {
			return true
		}
	}
	return false
}

// swiftStampFuncMeta records the return type and async/static flags on a
// function or method node's Meta when present.
func swiftStampFuncMeta(meta map[string]any, returnType string, isAsync, isStatic bool) {
	if returnType != "" {
		meta["return_type"] = returnType
	}
	if isAsync {
		meta["is_async"] = true
	}
	if isStatic {
		meta["is_static"] = true
	}
}

var swiftExtensionRe = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|private|internal|fileprivate|open|final)[ \t]+)*extension[ \t]+([A-Za-z_][\w.]*)`)

// swiftExtensionRanges returns a type-range per `extension Foo { ... }` block in
// src (Foo collapsed to its last dotted segment), found by a brace-matched text
// scan since the tree-sitter query does not capture extension declarations.
func swiftExtensionRanges(src []byte) []swiftTypeRange {
	s := string(src)
	var ranges []swiftTypeRange
	for _, loc := range swiftExtensionRe.FindAllStringSubmatchIndex(s, -1) {
		typeName := s[loc[2]:loc[3]]
		if i := strings.LastIndexByte(typeName, '.'); i >= 0 {
			typeName = typeName[i+1:]
		}
		rel := strings.IndexByte(s[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		open := loc[1] + rel
		end := swiftMatchBrace(s, open)
		if end < 0 {
			continue
		}
		ranges = append(ranges, swiftTypeRange{
			name:      typeName,
			startLine: strings.Count(s[:open], "\n"),
			endLine:   strings.Count(s[:end], "\n"),
		})
	}
	return ranges
}

// swiftTypeDeclRe matches a class / struct / actor / enum declaration header at
// the start of a line, capturing the type name. Leading attributes (`@objc`)
// and access/other modifiers are skipped. Used as a parse-error resilience net
// (see swiftFallbackTypeDecls).
var swiftTypeDeclRe = regexp.MustCompile(`(?m)^[ \t]*(?:@[A-Za-z_]\w*(?:\([^)]*\))?[ \t]+)*(?:(?:public|private|internal|fileprivate|open|final|indirect)[ \t]+)*(?:class|struct|actor|enum)[ \t]+([A-Za-z_]\w*)`)

type swiftFallbackType struct {
	name       string
	startLine  int // 0-based
	endLine    int // 0-based
	visibility string
}

// swiftFallbackTypeDecls finds class / struct / actor / enum declarations by a
// brace-matched text scan — the resilience net for when a tree-sitter parse
// error (e.g. an unparseable `#if … && !canImport(...)` inside a body) corrupts
// the enclosing class_declaration so the query never matches it. Without this
// the container node is absent and its members + every find_usages reference to
// the type strand on unresolved::Name. Mirrors swiftExtensionRanges.
func swiftFallbackTypeDecls(src []byte) []swiftFallbackType {
	s := string(src)
	var out []swiftFallbackType
	for _, loc := range swiftTypeDeclRe.FindAllStringSubmatchIndex(s, -1) {
		name := s[loc[2]:loc[3]]
		rel := strings.IndexByte(s[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		open := loc[1] + rel
		end := swiftMatchBrace(s, open)
		if end < 0 {
			continue
		}
		vis := VisibilityInternal
		switch prefix := s[loc[0]:loc[1]]; {
		case strings.Contains(prefix, "public"), strings.Contains(prefix, "open"):
			vis = VisibilityPublic
		case strings.Contains(prefix, "private"), strings.Contains(prefix, "fileprivate"):
			vis = VisibilityPrivate
		}
		out = append(out, swiftFallbackType{
			name:       name,
			startLine:  strings.Count(s[:open], "\n"),
			endLine:    strings.Count(s[:end], "\n"),
			visibility: vis,
		})
	}
	return out
}

// swiftMatchBrace returns the index of the '}' that closes the '{' at open, or
// -1 when unbalanced.
func swiftMatchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// swiftVisibility scans a declaration's leading modifier children for
// an access-level keyword. Swift's default is "internal" when no
// modifier is present. The grammar emits modifiers as plain keyword
// children of the declaration node (visibility_modifier etc.).
func swiftVisibility(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return VisibilityInternal
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil {
			continue
		}
		// Stop scanning once we pass the leading modifier band — once
		// we hit `func` / `class` / `struct` / `protocol` etc. there
		// are no more access modifiers ahead.
		t := c.Type()
		if t == "modifiers" {
			// Some grammar versions wrap modifiers; recurse.
			if v := swiftVisibility(c, src); v != VisibilityInternal {
				return v
			}
			continue
		}
		switch strings.TrimSpace(c.Content(src)) {
		case "public":
			return VisibilityPublic
		case "open":
			return VisibilityPublic
		case "private":
			return VisibilityPrivate
		case "fileprivate":
			return VisibilityPrivate
		case "internal":
			return VisibilityInternal
		}
	}
	return VisibilityInternal
}

func (e *SwiftExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	def := m.Captures["import.def"]
	importText := strings.TrimSpace(def.Text)
	importText = strings.TrimPrefix(importText, "import ")
	importText = strings.TrimSpace(importText)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importText,
		Kind: graph.EdgeImports, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingSwiftType returns the innermost type whose line range
// contains the 0-based line. Mirrors the legacy findEnclosingType
// logic — picks the smallest enclosing range so nested types attribute
// correctly.
func findEnclosingSwiftType(ranges []swiftTypeRange, line int) (string, bool) {
	r, ok := findEnclosingSwiftTypeRange(ranges, line)
	if !ok {
		return "", false
	}
	return r.name, true
}

// findEnclosingSwiftTypeRange returns the innermost (smallest) type range
// containing line, so a member can read its enclosing type's attributes
// (e.g. @objcMembers), not just the type name.
func findEnclosingSwiftTypeRange(ranges []swiftTypeRange, line int) (swiftTypeRange, bool) {
	var best swiftTypeRange
	found := false
	bestSize := int(^uint(0) >> 1)
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			size := r.endLine - r.startLine
			if size < bestSize {
				bestSize = size
				best = r
				found = true
			}
		}
	}
	// A type can hold two enclosing ranges -- the brace-matched fallback
	// scan seeded before the query match, and the query match itself. Only
	// the latter carries attributes, so OR the @objcMembers flag across
	// every same-name range that contains the line.
	if found && !best.objcMembers {
		for _, r := range ranges {
			if r.name == best.name && r.objcMembers && line >= r.startLine && line <= r.endLine {
				best.objcMembers = true
				break
			}
		}
	}
	return best, found
}

// findEnclosingSwiftContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingSwiftContainer(n *sitter.Node, t string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == t {
			return p
		}
	}
	return nil
}

// swiftUserTypePath returns the dotted path spelled by a constructed
// type, outermost segment first: `MyType<Int>` yields ["MyType"] and
// `obj.doThing<Int>` yields ["obj", "doThing"]. Only the node's own
// type_identifier children count — the ones nested inside a
// type_arguments list are the type ARGUMENTS, not part of the path.
func swiftUserTypePath(userType *sitter.Node, src []byte) []string {
	if userType == nil {
		return nil
	}
	var path []string
	for i, nc := 0, int(userType.NamedChildCount()); i < nc; i++ {
		c := userType.NamedChild(i)
		if c != nil && c.Type() == "type_identifier" {
			path = append(path, c.Content(src))
		}
	}
	return path
}
