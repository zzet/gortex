package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/mql"
)

// qMqlAll is the single combined tree-sitter query for MQL (.mq4/.mq5/.mqh).
// One tree walk per file replaces per-pattern query compilations; capture
// names are disjoint so the Extract dispatch can branch on which name is set.
//
// The grammar (github.com/davalillo/tree-sitter-mql5) is tree-sitter-cpp plus
// MQL5 extensions, so node kinds match the C++ extractor's vocabulary and the
// shared cpp* helpers in this package apply unchanged. MQL has no namespaces,
// so there are no namespace/qualified-call patterns; the MQL-only surface
// (interface_specifier, input/sinput variables) is covered here and in the
// post-walks below.
const qMqlAll = `
[
  (class_specifier
    name: (type_identifier) @class.name) @class.def

  (struct_specifier
    name: (type_identifier) @struct.name) @struct.def

  (enum_specifier
    name: (type_identifier) @enum.name) @enum.def

  (interface_specifier
    name: (type_identifier) @iface.name) @iface.def

  (function_definition
    declarator: (function_declarator
      declarator: [(identifier) (qualified_identifier)] @func.name)) @func.def

  (template_declaration
    (function_definition) @tmplfn.inner) @tmplfn.def

  (preproc_include
    path: (_) @include.path) @include.def

  (preproc_def
    name: (identifier) @macro.name) @macro.def

  (preproc_function_def
    name: (identifier) @macrofn.name) @macrofn.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (field_expression
      field: (field_identifier) @callm.method)) @callm.expr

  (call_expression
    function: (template_function
      name: (identifier) @call.name)) @call.expr

  (call_expression
    function: (field_expression
      field: (template_method
        name: (field_identifier) @callm.method))) @callm.expr
]
`

// mql5Markers are MQL5-exclusive tokens used for content sniffing on .mqh
// files — the same marker list mql-language-server's LanguageDetection uses
// (`using` and `final` deliberately excluded: valid in MQL4 too). A .mqh
// using none of them falls back to mql4, the server's documented default.
//
// Shared contract: this list must mirror
// mql-language-server src/Lsp/Server/LanguageDetection.cs (Mql5Tokens —
// the server's single source of truth). Last verified identical against
// v2.4.2 (nullptr, #resource, union, "pack(", "enum class"). If the server
// changes its list, update both sides in the same change and re-verify the
// dialect tests below.
var mql5Markers = []string{"nullptr", "#resource", "union", "pack(", "enum class"}

// mqlDialect stamps the file's dialect: mql4/mql5 from the extension, content
// sniffing for headers. The dialect is semantic metadata (filters, PR2 LSP
// routing hints) — the grammar itself is dialect-agnostic.
func mqlDialect(filePath string, src []byte) string {
	switch {
	case strings.HasSuffix(filePath, ".mq5"):
		return "mql5"
	case strings.HasSuffix(filePath, ".mq4"):
		return "mql4"
	default:
		for _, m := range mql5Markers {
			if strings.Contains(string(src), m) {
				return "mql5"
			}
		}
		return "mql4"
	}
}

// MQLExtractor extracts MQL4/MQL5 source files into graph nodes and edges.
type MQLExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewMQLExtractor() *MQLExtractor {
	lang := mql.GetLanguage()
	return &MQLExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qMqlAll, lang),
	}
}

func (e *MQLExtractor) Language() string     { return "mql" }
func (e *MQLExtractor) Extensions() []string { return []string{".mq4", ".mq5", ".mqh"} }

func (e *MQLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	dialect := mqlDialect(filePath, src)
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "mql",
		Meta:     map[string]any{"dialect": dialect},
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []cppDeferredCall

	root.WithScratch(func() {
		parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
			switch {

			case m.Captures["class.def"] != nil:
				e.emitMqlType(m, "class.def", "class.name", "class", filePath, fileID, src, result, seen)

			case m.Captures["struct.def"] != nil:
				e.emitMqlType(m, "struct.def", "struct.name", "struct", filePath, fileID, src, result, seen)

			case m.Captures["enum.def"] != nil:
				e.emitMqlEnum(m, filePath, fileID, result, seen)

			case m.Captures["iface.def"] != nil:
				e.emitMqlInterface(m, filePath, fileID, src, result, seen)

			case m.Captures["tmplfn.def"] != nil:
				e.emitMqlTemplateFunction(m, filePath, fileID, src, result, seen)

			case m.Captures["func.def"] != nil:
				e.emitMqlFunction(m, filePath, fileID, src, result, seen)

			case m.Captures["include.def"] != nil:
				e.emitMqlInclude(m, filePath, fileID, result)

			case m.Captures["macro.def"] != nil:
				emitCMacro(m.Captures["macro.def"].Node, false, filePath, fileID, "mql", src, result, seen)

			case m.Captures["macrofn.def"] != nil:
				emitCMacro(m.Captures["macrofn.def"].Node, true, filePath, fileID, "mql", src, result, seen)

			case m.Captures["callm.expr"] != nil:
				expr := m.Captures["callm.expr"]
				calls = append(calls, cppDeferredCall{
					name:     m.Captures["callm.method"].Text,
					line:     expr.StartLine + 1,
					isMember: true,
					receiver: cppCallReceiverText(expr.Node, src),
					argTypes: extractCppCallArgTypes(expr.Node, src),
				})

			case m.Captures["call.expr"] != nil:
				expr := m.Captures["call.expr"]
				calls = append(calls, cppDeferredCall{
					name:     m.Captures["call.name"].Text,
					line:     expr.StartLine + 1,
					argTypes: extractCppCallArgTypes(expr.Node, src),
				})
			}
		})
	})

	// MQL-only surface: input / sinput / extern variable declarations at
	// translation-unit scope. `input` parameters are an EA's user-facing
	// configuration surface, so they earn KindVariable nodes with the
	// storage class on Meta. Deeper declarations (inside functions) are
	// locals — not extracted, matching the C++ extractor.
	root.WithScratch(func() {
		e.emitMqlInputs(root, src, filePath, fileID, result, seen)
	})

	// Attribute deferred calls to their enclosing function/method.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			From: callerID,
		}
		if c.isMember {
			edge.To = "unresolved::*." + c.name
		} else {
			edge.To = "unresolved::" + c.name
		}
		if len(c.argTypes) > 0 {
			edge.Meta = map[string]any{
				"scope_arg_types": strings.Join(c.argTypes, ","),
			}
		}
		if c.isMember && c.receiver != "" {
			stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, nil, result))
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitMqlType covers class and struct definitions. MQL5 classes carry an
// optional base class (`: public CObject` style) recorded as scope_parent,
// matching the C++ extractor's convention.
func (e *MQLExtractor) emitMqlType(m parser.QueryResult, defCap, nameCap, flavor, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures[nameCap].Text
	def := m.Captures[defCap]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	seen[cppTypeMarker(filePath, name)] = true
	meta := map[string]any{"type_flavor": flavor}
	if parent := extractCppParentClass(def.Node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "mql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	e.walkMqlTypeBody(def.Node, src, filePath, fileID, name, id, seen, result)
}

// walkMqlTypeBody extracts methods (definitions with bodies) and field
// type-uses from a class/struct/interface body.
func (e *MQLExtractor) walkMqlTypeBody(typeNode *sitter.Node, src []byte, filePath, fileID, typeName, typeID string, seen map[string]bool, result *parser.ExtractionResult) {
	var body *sitter.Node
	for i, nc := 0, int(typeNode.NamedChildCount()); i < nc; i++ {
		child := typeNode.NamedChild(i)
		if child.Type() == "field_declaration_list" {
			body = child
			break
		}
	}
	if body == nil {
		return
	}
	typeSeen := make(map[string]bool)
	for i, nc := 0, int(body.NamedChildCount()); i < nc; i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "access_specifier":
			continue
		case "function_definition":
			e.addMqlMethodFromNode(child, src, filePath, fileID, typeName, typeID, seen, result)
		case "field_declaration":
			// Member/field type-use: a `CFoo bar;` member references CFoo.
			line := int(child.StartPoint().Row) + 1
			if tn := child.ChildByFieldName("type"); tn != nil {
				emitCppTypeUseEdges(typeID, tn.Content(src), filePath, line, result, typeSeen)
			}
		case "declaration_list":
			for j, nc := 0, int(child.NamedChildCount()); j < nc; j++ {
				gc := child.NamedChild(j)
				if gc.Type() == "function_definition" {
					e.addMqlMethodFromNode(gc, src, filePath, fileID, typeName, typeID, seen, result)
				}
			}
		}
	}
}

// addMqlMethodFromNode emits an in-body method definition with a member_of
// edge to its owning type. The "_method_L<line>" seen marker tells the
// function_definition dispatcher this line is already claimed.
func (e *MQLExtractor) addMqlMethodFromNode(funcNode *sitter.Node, src []byte, filePath, fileID, typeName, typeID string, seen map[string]bool, result *parser.ExtractionResult) {
	methodName := extractFuncName(funcNode, src)
	if methodName == "" {
		return
	}
	startLine := int(funcNode.StartPoint().Row) + 1
	endLine := int(funcNode.EndPoint().Row) + 1

	id := filePath + "::" + typeName + "." + methodName
	if seen[id] {
		id = filePath + "::" + typeName + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	seen[filePath+"::_method_L"+fmt.Sprint(startLine)] = true

	meta := map[string]any{"receiver": typeName, "scope_class": typeName}
	if rt := cppReturnType(funcNode, src); rt != "" {
		meta["return_type"] = rt
	}
	stampCppSignature(meta, funcNode, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "mql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})
}

// emitMqlEnum covers named enum declarations.
func (e *MQLExtractor) emitMqlEnum(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "mql", Meta: map[string]any{"type_flavor": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitMqlInterface covers MQL5 `interface Name { ... };` declarations. Unlike
// classes, interface members are declarations without bodies
// (field_declaration -> function_declarator), so method names are stamped
// onto Meta["methods"] (the Java convention) for implementation matching
// instead of minting method nodes.
func (e *MQLExtractor) emitMqlInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["iface.name"].Text
	def := m.Captures["iface.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	var methods []string
	var body *sitter.Node
	for i, nc := 0, int(def.Node.NamedChildCount()); i < nc; i++ {
		child := def.Node.NamedChild(i)
		if child.Type() == "field_declaration_list" {
			body = child
			break
		}
	}
	if body != nil {
		for i, nc := 0, int(body.NamedChildCount()); i < nc; i++ {
			fd := body.NamedChild(i)
			if fd.Type() != "field_declaration" {
				continue
			}
			for j, nc2 := 0, int(fd.NamedChildCount()); j < nc2; j++ {
				decl := fd.NamedChild(j)
				if decl.Type() != "function_declarator" {
					continue
				}
				d := decl.ChildByFieldName("declarator")
				if d != nil {
					methods = append(methods, d.Content(src))
				}
			}
		}
	}

	meta := map[string]any{"type_flavor": "interface"}
	if len(methods) > 0 {
		meta["methods"] = methods
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "mql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitMqlTemplateFunction emits a free template function (`template<typename T>
// T Max3(T a, T b) {…}`). A template member inside a type body keeps its
// method emission path.
func (e *MQLExtractor) emitMqlTemplateFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def, inner := m.Captures["tmplfn.def"], m.Captures["tmplfn.inner"]
	if def == nil || inner == nil || inner.Node == nil || cppInsideTypeBody(inner.Node) {
		return
	}
	e.emitMqlFreeFunction(cppFreeTemplateFuncName(inner.Node, src), inner.Node,
		def.StartLine+1, def.EndLine+1, filePath, fileID, src, result, seen)
}

// emitMqlFunction emits a free function. Skips lines already claimed by the
// type-body walk, and skips definitions owned by a template_declaration.
func (e *MQLExtractor) emitMqlFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["func.def"]
	startLine := def.StartLine + 1
	if seen[filePath+"::_method_L"+fmt.Sprint(startLine)] {
		return
	}
	if cppTemplateOwnsDefinition(def.Node) {
		return
	}
	e.emitMqlFreeFunction(m.Captures["func.name"].Text, def.Node, startLine, def.EndLine+1,
		filePath, fileID, src, result, seen)
}

func (e *MQLExtractor) emitMqlFreeFunction(name string, fnNode *sitter.Node, startLine, endLine int, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{}
	if rt := cppReturnType(fnNode, src); rt != "" {
		meta["return_type"] = rt
	}
	stampCppSignature(meta, fnNode, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "mql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
}

// emitMqlInclude records `#include "file.mqh"` / `#include <Trade\Trade.mqh>`
// as an imports edge to an unresolved target — binding to actual .mqh files
// is resolver work.
func (e *MQLExtractor) emitMqlInclude(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	pathCap := m.Captures["include.path"]
	raw := strings.TrimSpace(pathCap.Text)
	kind := "system"
	if strings.HasPrefix(raw, `"`) {
		kind = "quoted"
	}
	includePath := strings.Trim(raw, `"<>`)
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + includePath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
		Meta:     map[string]any{"include_kind": kind},
	})
}

// mqlDeclaratorEntry is one declarator of a top-level declaration: its base
// identifier and start line.
type mqlDeclaratorEntry struct {
	name string
	line int
}

// mqlDeclaratorEntries collects one entry per declarator of a top-level
// declaration. MQL input declarations surface in every tree-sitter-c
// declarator shape: plain identifiers (`extern int A, B;` — multiple
// `declarator` fields, no init), array declarators (`input double
// Rates[][6];`), pointer declarators (`extern CArrayObj *Ptr;`) and
// initialized declarators (`input int X = 5;`). Function declarators
// (`extern void f();`) are declarations of functions, not variables, and are
// skipped.
func mqlDeclaratorEntries(decl *sitter.Node, src []byte) []mqlDeclaratorEntry {
	var out []mqlDeclaratorEntry
	for i, nc := 0, int(decl.NamedChildCount()); i < nc; i++ {
		child := decl.NamedChild(i)
		switch child.Type() {
		// Type-only filter: a declaration's named children of these kinds are
		// always declarators (types surface as type_identifier/primitive_type/
		// struct_specifier, never as bare identifiers). Do NOT gate on
		// FieldNameForChild == "declarator": with comma-separated declarators
		// only the first carries the field name in the tree-sitter-c grammar.
		case "identifier", "init_declarator", "array_declarator", "pointer_declarator":
			if name := mqlDeclaratorName(child, src); name != "" {
				out = append(out, mqlDeclaratorEntry{name: name, line: int(child.StartPoint().Row) + 1})
			}
		}
	}
	return out
}

// mqlDeclaratorName resolves a declarator node to its base identifier,
// descending through init/array/pointer declarators.
func mqlDeclaratorName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src)
	case "init_declarator", "array_declarator", "pointer_declarator":
		return mqlDeclaratorName(n.ChildByFieldName("declarator"), src)
	default:
		return ""
	}
}

// emitMqlInputs extracts top-level input / sinput / extern variable
// declarations — an EA's user-facing configuration surface — as KindVariable
// nodes with the storage class on Meta. Every declarator of the declaration
// mints its own variable (see mqlDeclaratorEntries).
func (e *MQLExtractor) emitMqlInputs(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	for i, nc := 0, int(root.NamedChildCount()); i < nc; i++ {
		decl := root.NamedChild(i)
		if decl.Type() != "declaration" {
			continue
		}
		storage := ""
		for j, nc2 := 0, int(decl.NamedChildCount()); j < nc2; j++ {
			child := decl.NamedChild(j)
			if child.Type() == "storage_class_specifier" {
				storage = strings.TrimSpace(child.Content(src))
				break
			}
		}
		if storage != "input" && storage != "sinput" && storage != "extern" {
			continue
		}
		for _, entry := range mqlDeclaratorEntries(decl, src) {
			if seen[filePath+"::"+entry.name] {
				continue
			}
			seen[filePath+"::"+entry.name] = true
			id := filePath + "::" + entry.name
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: entry.name,
				FilePath: filePath, StartLine: entry.line, EndLine: int(decl.EndPoint().Row) + 1,
				Language: "mql", Meta: map[string]any{"storage_class": storage},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: entry.line,
			})
		}
	}
}
